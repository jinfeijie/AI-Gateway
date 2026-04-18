package proxy

import (
	"encoding/json"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"ai-gateway/model"
	"ai-gateway/store"
)

type sessionBinding struct {
	UpstreamID string
	AccessedAt time.Time
	RawUserID  string // 原始 metadata.user_id JSON
	ReqCount   int    // 会话期间请求数
}

type Balancer struct {
	store *store.Store

	// 会话亲和: sessionKey -> binding
	sessions     map[string]*sessionBinding
	sessionsMu   sync.RWMutex
	sessionTTL   time.Duration
	sessionsPath string // 持久化路径

	// 负载追踪: upstreamID -> active request count
	load sync.Map // upstreamID -> *int64

	// 冷却: upstreamID -> 冷却到期时间
	cooldowns sync.Map // upstreamID -> *time.Time
}

func NewBalancer(s *store.Store) *Balancer {
	b := &Balancer{
		store:      s,
		sessions:   make(map[string]*sessionBinding),
		sessionTTL: 5 * time.Minute,
	}
	go b.cleanupLoop()
	return b
}

// SetSessionsPath 设置会话持久化路径并加载已有数据
func (b *Balancer) SetSessionsPath(dataPath string) {
	b.sessionsPath = filepath.Join(filepath.Dir(dataPath), "sessions.json")
	b.loadSessions()
}

func (b *Balancer) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		b.sessionsMu.Lock()
		now := time.Now()
		for k, v := range b.sessions {
			if now.Sub(v.AccessedAt) > b.sessionTTL {
				delete(b.sessions, k)
			}
		}
		b.sessionsMu.Unlock()
		b.saveSessions()
	}
}

type sessionFile struct {
	Key        string `json:"key"`
	UpstreamID string `json:"upstream_id"`
	AccessedAt int64  `json:"accessed_at"`
	RawUserID  string `json:"raw_user_id,omitempty"`
	ReqCount   int    `json:"req_count,omitempty"`
}

func (b *Balancer) loadSessions() {
	if b.sessionsPath == "" {
		return
	}
	data, err := os.ReadFile(b.sessionsPath)
	if err != nil {
		return
	}
	var entries []sessionFile
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	now := time.Now()
	b.sessionsMu.Lock()
	defer b.sessionsMu.Unlock()
	loaded := 0
	for _, e := range entries {
		at := time.Unix(e.AccessedAt, 0)
		if now.Sub(at) < b.sessionTTL {
			b.sessions[e.Key] = &sessionBinding{
				UpstreamID: e.UpstreamID,
				AccessedAt: at,
				RawUserID:  e.RawUserID,
				ReqCount:   e.ReqCount,
			}
			loaded++
		}
	}
	if loaded > 0 {
		log.Printf("[balancer] loaded %d session bindings from %s", loaded, b.sessionsPath)
	}
}

func (b *Balancer) saveSessions() {
	if b.sessionsPath == "" {
		return
	}
	b.sessionsMu.RLock()
	entries := make([]sessionFile, 0, len(b.sessions))
	for k, v := range b.sessions {
		entries = append(entries, sessionFile{
			Key:        k,
			UpstreamID: v.UpstreamID,
			AccessedAt: v.AccessedAt.Unix(),
			RawUserID:  v.RawUserID,
			ReqCount:   v.ReqCount,
		})
	}
	b.sessionsMu.RUnlock()
	data, _ := json.Marshal(entries)
	os.WriteFile(b.sessionsPath, data, 0644)
}

// InCooldown 检查上游是否在冷却中
func (b *Balancer) InCooldown(upstreamID string) bool {
	val, ok := b.cooldowns.Load(upstreamID)
	if !ok {
		return false
	}
	until := val.(*time.Time)
	if time.Now().Before(*until) {
		return true
	}
	b.cooldowns.Delete(upstreamID)
	return false
}

// SetCooldown 设置上游冷却
func (b *Balancer) SetCooldown(upstreamID string, d time.Duration) {
	until := time.Now().Add(d)
	b.cooldowns.Store(upstreamID, &until)
}

// Pick 选择上游: 模型匹配 → 会话亲和 → 最少负载（跳过冷却中的上游）
func (b *Balancer) Pick(groupID string, sessionKey string, rawUserID string, reqModel string, exclude map[string]bool) *model.Upstream {
	cfg := b.store.Get()

	// 查找分组配置
	var groupModels []string
	var groupMaxConcurrency int
	for _, g := range cfg.Groups {
		if g.ID == groupID {
			groupModels = g.Models
			groupMaxConcurrency = g.MaxConcurrency
			break
		}
	}

	// 分组级并发限流
	if groupMaxConcurrency > 0 && b.GroupLoad(groupID, cfg) >= int64(groupMaxConcurrency) {
		return nil
	}

	var actives []model.Upstream
	for _, u := range cfg.Upstreams {
		if u.GroupID == groupID && (u.Status == "active" || u.Status == "half_open") && !exclude[u.ID] && !b.InCooldown(u.ID) {
			// Weight=0 暂停流量，不参与任何选取（含会话亲和）
			if u.EffectiveWeight() == 0 {
				continue
			}
			// 并发限流：超过最大并发数时跳过
			if u.MaxConcurrency > 0 && b.GetLoad(u.ID) >= int64(u.MaxConcurrency) {
				continue
			}
			// 模型过滤：上游有配置用上游的，否则用分组的，都没有则不过滤
			if reqModel != "" {
				models := u.Models
				if len(models) == 0 {
					models = groupModels
				}
				if len(models) > 0 && !containsStr(models, reqModel) {
					continue
				}
			}
			actives = append(actives, u)
		}
	}
	if len(actives) == 0 {
		return nil
	}

	// 1. 会话亲和：查找已有绑定
	if sessionKey != "" {
		b.sessionsMu.RLock()
		binding, exists := b.sessions[sessionKey]
		b.sessionsMu.RUnlock()

		if exists {
			for _, u := range actives {
				if u.ID == binding.UpstreamID {
					b.sessionsMu.Lock()
					binding.AccessedAt = time.Now()
					binding.ReqCount++
					if rawUserID != "" {
						binding.RawUserID = rawUserID
					}
					b.sessionsMu.Unlock()
					return &u
				}
			}
		}
	}

	// 2. 最少负载
	picked := b.pickLeastLoad(actives)

	if sessionKey != "" {
		b.sessionsMu.Lock()
		b.sessions[sessionKey] = &sessionBinding{
			UpstreamID: picked.ID,
			AccessedAt: time.Now(),
			RawUserID:  rawUserID,
			ReqCount:   1,
		}
		b.sessionsMu.Unlock()
	}

	return &picked
}

func (b *Balancer) pickLeastLoad(actives []model.Upstream) model.Upstream {
	minLoad := int64(1<<63 - 1)
	for _, u := range actives {
		load := b.GetLoad(u.ID)
		if load < minLoad {
			minLoad = load
		}
	}
	// 收集所有最小负载的上游，按权重加权随机选取
	var candidates []int
	totalWeight := 0
	for i, u := range actives {
		if b.GetLoad(u.ID) == minLoad {
			candidates = append(candidates, i)
			totalWeight += u.EffectiveWeight()
		}
	}
	// 加权随机
	r := rand.Intn(totalWeight)
	for _, ci := range candidates {
		w := actives[ci].EffectiveWeight()
		r -= w
		if r < 0 {
			return actives[ci]
		}
	}
	// fallback（理论上不会到这里）
	return actives[candidates[len(candidates)-1]]
}

// IncLoad 请求开始时增加负载计数
func (b *Balancer) IncLoad(upstreamID string) {
	val, _ := b.load.LoadOrStore(upstreamID, new(int64))
	atomic.AddInt64(val.(*int64), 1)
}

// DecLoad 请求结束时减少负载计数
func (b *Balancer) DecLoad(upstreamID string) {
	if val, ok := b.load.Load(upstreamID); ok {
		atomic.AddInt64(val.(*int64), -1)
	}
}

// GetLoad 获取上游当前负载
func (b *Balancer) GetLoad(upstreamID string) int64 {
	if val, ok := b.load.Load(upstreamID); ok {
		return atomic.LoadInt64(val.(*int64))
	}
	return 0
}

// GroupLoad 获取分组内所有上游的总并发数
func (b *Balancer) GroupLoad(groupID string, cfg model.Config) int64 {
	var total int64
	for _, u := range cfg.Upstreams {
		if u.GroupID == groupID {
			total += b.GetLoad(u.ID)
		}
	}
	return total
}

// SessionEntry 会话绑定信息（用于 API 返回）
type SessionEntry struct {
	SessionKey string `json:"session_key"`
	UpstreamID string `json:"upstream_id"`
	AccessedAt int64  `json:"accessed_at"`
	RawUserID  string `json:"raw_user_id,omitempty"`
	ReqCount   int    `json:"req_count"`
}

// SessionInfo 返回当前所有会话绑定
func (b *Balancer) SessionInfo() []any {
	b.sessionsMu.RLock()
	defer b.sessionsMu.RUnlock()
	result := make([]any, 0, len(b.sessions))
	for k, v := range b.sessions {
		result = append(result, SessionEntry{
			SessionKey: k,
			UpstreamID: v.UpstreamID,
			AccessedAt: v.AccessedAt.Unix(),
			RawUserID:  v.RawUserID,
			ReqCount:   v.ReqCount,
		})
	}
	return result
}

// CooldownInfo 返回所有冷却中上游的到期时间
func (b *Balancer) CooldownInfo() map[string]time.Time {
	result := make(map[string]time.Time)
	now := time.Now()
	b.cooldowns.Range(func(key, value any) bool {
		id := key.(string)
		until := value.(*time.Time)
		if now.Before(*until) {
			result[id] = *until
		} else {
			b.cooldowns.Delete(key)
		}
		return true
	})
	return result
}

// ActiveCount 返回指定分组中 active 且非冷却的上游数量
func (b *Balancer) ActiveCount(groupID string) int {
	cfg := b.store.Get()
	count := 0
	for _, u := range cfg.Upstreams {
		if u.GroupID == groupID && (u.Status == "active" || u.Status == "half_open") {
			count++
		}
	}
	return count
}

func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
