package health

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-gateway/model"
	"ai-gateway/store"
)

type Checker struct {
	store  *store.Store
	client *http.Client
	cancel context.CancelFunc
	mu     sync.Mutex

	// 冷却设置接口
	cooldownSetter CooldownSetter
	// 日志写入接口
	logWriter LogWriter

	// 探测历史（环形缓冲）
	history   []HistoryEntry
	historyMu sync.RWMutex
}

// CooldownSetter 设置上游冷却（避免循环导入）
type CooldownSetter interface {
	SetCooldown(upstreamID string, d time.Duration)
}

// HealthLogEntry 健康检查日志条目
type HealthLogEntry struct {
	Time       int64
	GroupID    string
	GroupName  string
	UpstreamID string
	Remark     string
	Status     int
	DurationMs int64
	Action     string // health_ok / health_fail
	Detail     string
	Model      string
}

// LogWriter 写请求日志（避免循环导入）
type LogWriter interface {
	AddHealthLog(entry HealthLogEntry)
}

// ProbeResult 探活结果
type ProbeResult struct {
	Healthy    bool              `json:"healthy"`
	StatusCode int               `json:"status_code"`
	Body       string            `json:"body"`
	Reply      string            `json:"reply"`
	Error      string            `json:"error,omitempty"`
	Headers    map[string]string `json:"-"` // 响应 header（不序列化）
}

// HistoryEntry 探测历史记录
type HistoryEntry struct {
	UpstreamID   string `json:"upstream_id"`
	Remark       string `json:"remark"`
	GroupID      string `json:"group_id"`
	Healthy      bool   `json:"healthy"`
	StatusCode   int    `json:"status_code"`
	Error        string `json:"error,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
	CheckedAt    int64  `json:"checked_at"`
}

const maxHistory = 200

func NewChecker(s *store.Store) *Checker {
	return &Checker{
		store:  s,
		client: &http.Client{},
	}
}

// SetCooldownSetter 注入冷却设置器（在 main.go 中创建 Handler 后调用）
func (ch *Checker) SetCooldownSetter(cs CooldownSetter) {
	ch.cooldownSetter = cs
}

// SetLogWriter 注入日志写入器（在 main.go 中创建 Handler 后调用）
func (ch *Checker) SetLogWriter(lw LogWriter) {
	ch.logWriter = lw
}

func (ch *Checker) Start() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.cancel != nil {
		ch.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch.cancel = cancel
	model.SafeGo("health.loop", func() { ch.loop(ctx) })
}

func (ch *Checker) Stop() {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.cancel != nil {
		ch.cancel()
		ch.cancel = nil
	}
}

func (ch *Checker) addHistory(entry HistoryEntry) {
	ch.historyMu.Lock()
	defer ch.historyMu.Unlock()
	ch.history = append(ch.history, entry)
	if len(ch.history) > maxHistory {
		ch.history = ch.history[len(ch.history)-maxHistory:]
	}
}

// History 返回探测历史，支持按 upstreamID 筛选（空 = 全部），按时间倒序
func (ch *Checker) History(upstreamID string) []HistoryEntry {
	ch.historyMu.RLock()
	defer ch.historyMu.RUnlock()
	result := make([]HistoryEntry, 0, len(ch.history))
	for i := len(ch.history) - 1; i >= 0; i-- {
		e := ch.history[i]
		if upstreamID == "" || e.UpstreamID == upstreamID {
			result = append(result, e)
		}
	}
	return result
}

func (ch *Checker) loop(ctx context.Context) {
	for {
		cfg := ch.store.Get()

		// registry 心跳过期检测始终运行，不依赖健康检查开关
		ch.checkRegistryHeartbeats()

		if !cfg.HealthCheck.Enabled {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		interval := time.Duration(cfg.HealthCheck.IntervalS) * time.Second
		if interval < 10*time.Second {
			interval = 10 * time.Second
		}

		ch.checkAll()

		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// statusChange 批量状态变更
type statusChange struct {
	upstreamID  string
	status      string
	faultedAt   *int64
	faultType   string
	faultReason string
}

// checkRegistryHeartbeats 检测 registry 上游心跳过期，独立于健康检查开关
func (ch *Checker) checkRegistryHeartbeats() {
	cfg := ch.store.Get()
	ttl := cfg.HeartbeatTTL
	if ttl <= 0 {
		ttl = 30
	}

	var changes []statusChange
	for _, u := range cfg.Upstreams {
		if u.Source != "registry" || u.Status == "disabled" {
			continue
		}
		if u.HeartbeatAt != nil {
			elapsed := time.Now().Unix() - *u.HeartbeatAt
			if elapsed > int64(ttl) && u.Status != "faulted" {
				log.Printf("[health] registry upstream %s (%s) heartbeat expired (%ds > %ds)", u.ID, u.Remark, elapsed, ttl)
				now := time.Now().Unix()
				changes = append(changes, statusChange{
					upstreamID:  u.ID,
					status:      "faulted",
					faultedAt:   &now,
					faultType:   "auto",
					faultReason: "heartbeat expired",
				})
			}
		}
	}

	if len(changes) > 0 {
		ch.applyChanges(changes)
	}
}

func (ch *Checker) checkAll() {
	cfg := ch.store.Get()
	globalHC := cfg.HealthCheck

	// 构建 groupID -> HealthCheckConfig 映射
	groupHC := make(map[string]model.HealthCheckConfig)
	// 构建 groupID -> failover rules 映射
	groupRules := make(map[string]map[int]model.FailoverRule)
	// 构建 groupID -> name 映射
	groupName := make(map[string]string)
	for _, g := range cfg.Groups {
		groupName[g.ID] = g.Name
		if g.HealthCheck != nil {
			groupHC[g.ID] = *g.HealthCheck
		}
		rules := g.FailoverRules
		if len(rules) == 0 {
			rules = model.DefaultFailoverRules()
		}
		rm := make(map[int]model.FailoverRule, len(rules))
		for _, r := range rules {
			rm[r.Code] = r
		}
		groupRules[g.ID] = rm
	}

	var changes []statusChange

	for _, u := range cfg.Upstreams {
		if u.Status == "disabled" {
			continue
		}

		// registry 上游已在 checkRegistryHeartbeats 中处理，跳过
		if u.Source == "registry" {
			continue
		}

		// 按分组查找配置，分组无配置用全局
		hc := globalHC
		if ghc, ok := groupHC[u.GroupID]; ok {
			hc = ghc
		}

		// 分组级别未启用则跳过（全局启用但分组关闭）
		if !hc.Enabled {
			continue
		}

		timeout := time.Duration(hc.TimeoutS) * time.Second
		if timeout == 0 {
			timeout = 10 * time.Second
		}
		ch.client.Timeout = timeout

		probeStart := time.Now()
		result := ch.Probe(u.API, u.APIKey, cfg.DefaultProbeModel, hc)
		durationMs := time.Since(probeStart).Milliseconds()

		// 写请求日志
		if ch.logWriter != nil {
			action := "health_ok"
			detail := ""
			if !result.Healthy {
				action = "health_fail"
				detail = result.Error
			}
			ch.logWriter.AddHealthLog(HealthLogEntry{
				Time:       time.Now().Unix(),
				GroupID:    u.GroupID,
				GroupName:  groupName[u.GroupID],
				UpstreamID: u.ID,
				Remark:     u.Remark,
				Status:     result.StatusCode,
				DurationMs: durationMs,
				Action:     action,
				Detail:     detail,
				Model:      cfg.DefaultProbeModel,
			})
		}

		// 记录历史
		entry := HistoryEntry{
			UpstreamID: u.ID,
			Remark:     u.Remark,
			GroupID:    u.GroupID,
			Healthy:    result.Healthy,
			StatusCode: result.StatusCode,
			Error:      result.Error,
			CheckedAt:  time.Now().Unix(),
		}
		if !result.Healthy && result.Body != "" {
			entry.ResponseBody = result.Body
		}
		ch.addHistory(entry)

		// 检查是否命中 cooldown 规则（如 429），不应标记为故障
		if !result.Healthy && result.StatusCode > 0 {
			if ruleMap, ok := groupRules[u.GroupID]; ok {
				if rule, hit := ruleMap[result.StatusCode]; hit && rule.Action == "cooldown" {
					if ch.cooldownSetter != nil {
						dur := time.Duration(rule.CooldownS) * time.Second
						if dur == 0 {
							dur = 60 * time.Second
						}
						// 从响应 header 读取冷却时长
						if rule.UseHeader != "" {
							if hv, ok := result.Headers[rule.UseHeader]; ok && hv != "" {
								if d := parseRetryAfter(hv); d > 0 {
									dur = d
								}
							}
						}
						ch.cooldownSetter.SetCooldown(u.ID, dur)
						log.Printf("[health] upstream %s (%s) got %d, cooldown %v, headers: %v", u.ID, u.Remark, result.StatusCode, dur, result.Headers)
					}
					continue
				}
			}
		}

		if result.Healthy && u.Status == "faulted" {
			log.Printf("[health] upstream %s (%s) probe OK, entering half_open", u.ID, u.Remark)
			changes = append(changes, statusChange{
				upstreamID: u.ID,
				status:     "half_open",
			})
		} else if result.Healthy && u.Status == "half_open" {
			log.Printf("[health] upstream %s (%s) recovered", u.ID, u.Remark)
			changes = append(changes, statusChange{
				upstreamID: u.ID,
				status:     "active",
			})
		} else if !result.Healthy && u.Status == "half_open" {
			log.Printf("[health] upstream %s (%s) half_open probe failed, back to faulted", u.ID, u.Remark)
			now := time.Now().Unix()
			changes = append(changes, statusChange{
				upstreamID:  u.ID,
				status:      "faulted",
				faultedAt:   &now,
				faultType:   "auto",
				faultReason: "半开探测失败: " + result.Error,
			})
		} else if !result.Healthy && u.Status == "active" {
			log.Printf("[health] upstream %s (%s) marked faulted: %d %s", u.ID, u.Remark, result.StatusCode, result.Error)
			now := time.Now().Unix()
			changes = append(changes, statusChange{
				upstreamID:  u.ID,
				status:      "faulted",
				faultedAt:   &now,
				faultType:   "auto",
				faultReason: result.Error,
			})
		}
	}

	if len(changes) > 0 {
		ch.applyChanges(changes)
	}
}

// applyChanges 批量应用状态变更，仅一次 store.Update + 磁盘写入
func (ch *Checker) applyChanges(changes []statusChange) {
	changeMap := make(map[string]statusChange, len(changes))
	for _, sc := range changes {
		changeMap[sc.upstreamID] = sc
	}
	ch.store.Update(func(c *model.Config) {
		for i := range c.Upstreams {
			sc, ok := changeMap[c.Upstreams[i].ID]
			if !ok {
				continue
			}
			c.Upstreams[i].Status = sc.status
			if sc.status == "active" {
				c.Upstreams[i].FaultedAt = nil
				c.Upstreams[i].FaultType = ""
				c.Upstreams[i].FaultReason = ""
			} else if sc.faultedAt != nil {
				c.Upstreams[i].FaultedAt = sc.faultedAt
				c.Upstreams[i].FaultType = sc.faultType
				c.Upstreams[i].FaultReason = sc.faultReason
			}
		}
	})
}

// Probe 发送真实请求探活，返回详细结果。probeModel 为空则使用默认模型。
func (ch *Checker) Probe(api string, apiKey string, probeModel string, hc model.HealthCheckConfig) ProbeResult {
	retries := hc.Retries
	if retries <= 0 {
		retries = 1
	}
	var last ProbeResult
	for i := 0; i < retries; i++ {
		last = ch.doProbe(api, apiKey, probeModel, hc)
		if last.Healthy {
			return last
		}
	}
	return last
}

func (ch *Checker) doProbe(api string, apiKey string, probeModel string, hc model.HealthCheckConfig) ProbeResult {
	base := strings.TrimRight(api, "/")
	url := base + "/v1/messages"
	if strings.HasSuffix(base, "/v1/messages") {
		url = base
	}

	if probeModel == "" {
		probeModel = "claude-sonnet-4-6"
	}

	var reqBody string
	if hc.Body != "" {
		reqBody = hc.Body
	} else {
		reqBody = fmt.Sprintf(`{"model":"%s","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`, probeModel)
	}
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(reqBody))
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Anthropic-Version", "2023-06-01")
	for k, v := range hc.Headers {
		req.Header.Set(k, v)
	}
	resp, err := ch.client.Do(req)
	if err != nil {
		return ProbeResult{Error: err.Error()}
	}
	defer resp.Body.Close()

	// 读取响应体（截断到 4KB）
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	respStr := string(respBody)

	// 提取 AI 回复文本
	reply := extractReply(respStr)

	healthy := resp.StatusCode >= 200 && resp.StatusCode < 300
	// 响应体为空视为异常
	if healthy && len(respBody) == 0 {
		healthy = false
	}
	// 保存关键响应 header
	headers := make(map[string]string)
	for _, k := range []string{"retry-after", "x-ratelimit-reset"} {
		if v := resp.Header.Get(k); v != "" {
			headers[k] = v
		}
	}
	result := ProbeResult{
		Healthy:    healthy,
		StatusCode: resp.StatusCode,
		Body:       respStr,
		Reply:      reply,
		Headers:    headers,
	}
	if !healthy {
		if len(respBody) == 0 {
			result.Error = fmt.Sprintf("HTTP %d: empty response body", resp.StatusCode)
		} else {
			result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
	}
	return result
}

// parseRetryAfter 解析 retry-after header，支持秒数和 HTTP 日期格式
func parseRetryAfter(val string) time.Duration {
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	if t, err := time.Parse(time.RFC1123Z, val); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// extractReply 从 Claude Messages API 响应中提取文本回复
func extractReply(body string) string {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		return ""
	}
	for _, c := range resp.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}
