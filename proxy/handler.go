package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-gateway/health"
	"ai-gateway/model"
	"ai-gateway/store"

	"github.com/gin-gonic/gin"
)

// RequestLog 请求日志
type RequestLog struct {
	Time       int64  `json:"time"`
	GroupID    string `json:"group_id"`
	GroupName  string `json:"group_name"`
	SessionKey string `json:"session_key"`
	ClientIP   string `json:"client_ip"`
	RequestID  string `json:"request_id,omitempty"`
	UpstreamID string `json:"upstream_id"`
	Remark     string `json:"remark"`
	Status     int    `json:"status"`
	Duration   int64  `json:"duration_ms"`
	TTFBMs     int64  `json:"ttfb_ms,omitempty"`
	Action     string `json:"action"` // success / failover / error
	Detail     string `json:"detail,omitempty"`
	Model      string `json:"model,omitempty"`
	RetryCount int    `json:"retry_count,omitempty"` // 重试计数
	// Token 用量
	InputTokens      int `json:"input_tokens,omitempty"`
	OutputTokens     int `json:"output_tokens,omitempty"`
	CacheReadTokens  int `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
	// 失败时记录请求体和响应体
	RequestBody  string `json:"request_body,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
}

// Usage 从响应中提取的 token 用量
type Usage struct {
	InputTokens             int `json:"input_tokens"`
	OutputTokens            int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens    int `json:"cache_read_input_tokens"`
}

const maxRequestLogs = 500

// cst 东八区时区，用于日统计的日期边界
var cst = time.FixedZone("CST", 8*3600)

var scanBufPool = sync.Pool{
	New: func() any { return make([]byte, 256*1024) },
}

// costEntry 异步费用更新
type costEntry struct {
	upstreamID string
	cost       float64
}

const maxLinesPerLogFile = 10000

// groupLogState 管理单个分组的日志文件状态
type groupLogState struct {
	dir       string
	file      *os.File
	lineCount int
	fileIdx   int
}

type logSessionEntry struct {
	decision bool
	at       time.Time
}

type Handler struct {
	store    *store.Store
	balancer *Balancer
	client   *http.Client

	logBaseDir string             // 日志根目录：{dataDir}/logs/
	logWriters map[string]*groupLogState // groupID → writer state
	logCh      chan RequestLog    // 异步写入通道
	costCh     chan costEntry     // 异步费用更新

	// 最近日志环形缓冲（供 API 查询，不读磁盘）
	recentLogs   []RequestLog
	recentLogsMu sync.RWMutex

	logSessions   map[string]logSessionEntry // session key → 采样决策
	logSessionsMu sync.RWMutex

	siteStats     SiteStats
	siteStatsMu   sync.Mutex
	siteStatsPath string

	upstreamStats     map[string]*UpstreamAccum // 上游独立累计
	upstreamStatsMu   sync.Mutex
	upstreamStatsPath string
	latencyStatsPath  string

	wg sync.WaitGroup // 等待 logWriter/costWriter 退出
}

// UpstreamAccum 单个上游的持久化累计数据
type UpstreamAccum struct {
	TodayDate     string  `json:"today_date"`
	TodayCost     float64 `json:"today_cost"`
	TodayRequests int     `json:"today_requests"`
	TotalCost     float64 `json:"total_cost"`
	TotalRequests int     `json:"total_requests"`
	GroupID       string  `json:"group_id,omitempty"`

	// 延迟统计（不持久化到 upstream_stats.json，单独文件）
	LatencyCount int   `json:"-"`
	LatencySum   int64 `json:"-"`
	TTFBSum      int64 `json:"-"`
}

// upstreamLatency 延迟持久化结构
type upstreamLatency struct {
	Count int   `json:"count"`
	Sum   int64 `json:"sum"`
	TTFB  int64 `json:"ttfb"`
}

func (h *Handler) addLog(entry RequestLog) {
	// 非阻塞发送到异步写入 goroutine
	select {
	case h.logCh <- entry:
	default:
	}
}

// AddHealthLog 将健康检查结果写入请求日志（实现 health.LogWriter 接口）
func (h *Handler) AddHealthLog(entry health.HealthLogEntry) {
	h.addLog(RequestLog{
		Time:       entry.Time,
		GroupID:    entry.GroupID,
		GroupName:  entry.GroupName,
		UpstreamID: entry.UpstreamID,
		Remark:     entry.Remark,
		Status:     entry.Status,
		Duration:   entry.DurationMs,
		Action:     entry.Action,
		Detail:     entry.Detail,
		Model:      entry.Model,
	})
}

// shouldLog 根据分组的日志记录模式判断该请求是否应记录日志
func (h *Handler) shouldLog(group *model.Group, sessionKey string) bool {
	mode := group.LogMode
	if mode == "" || mode == "all" || mode == "error_only" {
		return true
	}
	if mode == "off" {
		return false
	}
	rate := group.LogSampleRate
	if rate <= 0 {
		rate = 10
	}
	if rate >= 100 {
		return true
	}
	if mode == "random" {
		return rand.Intn(100) < rate
	}
	// random_session: 按会话维度决策
	if sessionKey == "" {
		return rand.Intn(100) < rate
	}
	key := group.ID + ":" + sessionKey
	h.logSessionsMu.RLock()
	if entry, ok := h.logSessions[key]; ok {
		h.logSessionsMu.RUnlock()
		return entry.decision
	}
	h.logSessionsMu.RUnlock()
	decision := rand.Intn(100) < rate
	h.logSessionsMu.Lock()
	h.logSessions[key] = logSessionEntry{decision: decision, at: time.Now()}
	h.logSessionsMu.Unlock()
	return decision
}

// RequestLogs 返回请求日志（倒序），剥离 body 字段减少传输量
func (h *Handler) RequestLogs(groupID string) []any {
	h.recentLogsMu.RLock()
	src := h.recentLogs
	h.recentLogsMu.RUnlock()

	result := make([]any, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		if groupID != "" && src[i].GroupID != groupID {
			continue
		}
		entry := src[i]
		entry.RequestBody = ""
		entry.ResponseBody = ""
		result = append(result, entry)
	}
	return result
}

// RequestLogDetail 返回单条日志完整内容（含 body），idx 为倒序索引（0=最新）
func (h *Handler) RequestLogDetail(idx int, groupID string) *RequestLog {
	h.recentLogsMu.RLock()
	src := h.recentLogs
	h.recentLogsMu.RUnlock()

	// 按条件倒序遍历，找到第 idx 条
	count := 0
	for i := len(src) - 1; i >= 0; i-- {
		if groupID != "" && src[i].GroupID != groupID {
			continue
		}
		if count == idx {
			entry := src[i]
			return &entry
		}
		count++
	}
	return nil
}

// UpstreamStats 单个上游的统计数据
type UpstreamStats struct {
	UpstreamID        string  `json:"upstream_id"`
	RequestCount      int     `json:"request_count"`
	TodayRequestCount int     `json:"today_request_count"`
	AvgLatency        int64   `json:"avg_latency_ms"`
	AvgTTFB           int64   `json:"avg_ttfb_ms"`
	TodayCost         float64 `json:"today_cost"`
	TotalCost         float64 `json:"total_cost"`
}

// GroupStats 单个分组的统计数据
type GroupStats struct {
	GroupID   string  `json:"group_id"`
	TodayCost float64 `json:"today_cost"`
	TotalCost float64 `json:"total_cost"`
}

// SiteStats 全站独立累计统计（不依赖上游列表）
type SiteStats struct {
	TodayDate     string  `json:"today_date"`
	TodayCost     float64 `json:"today_cost"`
	TodayRequests int     `json:"today_requests"`
	TotalCost     float64 `json:"total_cost"`
	TotalRequests int     `json:"total_requests"`
}

// StatsResponse 统计响应
type StatsResponse struct {
	Upstreams map[string]UpstreamStats `json:"upstreams"`
	Groups    map[string]GroupStats    `json:"groups"`
	Site      SiteStats               `json:"site"`
}

func calcCost(log RequestLog, pricingMap map[string]model.ModelPricing) float64 {
	p, ok := pricingMap[log.Model]
	if !ok {
		return 0
	}
	return (float64(log.InputTokens)*p.Input +
		float64(log.OutputTokens)*p.Output +
		float64(log.CacheReadTokens)*p.CacheRead +
		float64(log.CacheWriteTokens)*p.CacheWrite) / 1_000_000
}

// DailyStats 统计数据：全部从内存读取，无磁盘 I/O
func (h *Handler) DailyStats(pricing []model.ModelPricing) any {
	now := time.Now().In(cst)
	today := now.Format("2006-01-02")

	// 从持久化文件读花费/请求数
	h.upstreamStatsMu.Lock()
	upSnap := make(map[string]UpstreamAccum, len(h.upstreamStats))
	for id, acc := range h.upstreamStats {
		upSnap[id] = *acc
	}
	h.upstreamStatsMu.Unlock()

	resp := StatsResponse{
		Upstreams: make(map[string]UpstreamStats),
		Groups:    make(map[string]GroupStats),
	}

	for id, acc := range upSnap {
		avg := int64(0)
		avgTTFB := int64(0)
		if acc.LatencyCount > 0 {
			avg = acc.LatencySum / int64(acc.LatencyCount)
			avgTTFB = acc.TTFBSum / int64(acc.LatencyCount)
		}
		todayCost := acc.TodayCost
		todayReqs := acc.TodayRequests
		if acc.TodayDate != today {
			todayCost = 0
			todayReqs = 0
		}
		resp.Upstreams[id] = UpstreamStats{
			UpstreamID:        id,
			RequestCount:      acc.TotalRequests,
			TodayRequestCount: todayReqs,
			AvgLatency:        avg,
			AvgTTFB:           avgTTFB,
			TodayCost:         todayCost,
			TotalCost:         acc.TotalCost,
		}
	}

	// 分组统计：按上游所属分组聚合（优先用持久化的 group_id，上游删除后仍可聚合）
	cfg := h.store.Get()
	upGroupMap := make(map[string]string)
	for _, u := range cfg.Upstreams {
		upGroupMap[u.ID] = u.GroupID
	}
	for id, st := range resp.Upstreams {
		gid := upGroupMap[id]
		if gid == "" {
			// 上游已被删除，从持久化记录中取 group_id
			if acc, ok := upSnap[id]; ok {
				gid = acc.GroupID
			}
		}
		if gid == "" {
			continue
		}
		ga := resp.Groups[gid]
		ga.GroupID = gid
		ga.TodayCost += st.TodayCost
		ga.TotalCost += st.TotalCost
		resp.Groups[gid] = ga
	}

	// 全站独立统计
	h.siteStatsMu.Lock()
	site := h.siteStats
	if site.TodayDate != today {
		site.TodayCost = 0
		site.TodayRequests = 0
	}
	h.siteStatsMu.Unlock()
	resp.Site = site

	return resp
}

func NewHandler(s *store.Store, dataPath string) *Handler {
	h := &Handler{
		store:    s,
		balancer: NewBalancer(s),
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 256,
				MaxConnsPerHost:     512,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		logCh:       make(chan RequestLog, 1024),
		costCh:      make(chan costEntry, 256),
		logWriters:  make(map[string]*groupLogState),
		logSessions: make(map[string]logSessionEntry),
	}
	h.balancer.SetSessionsPath(dataPath)
	// 日志根目录：数据文件同级的 logs/
	h.logBaseDir = filepath.Join(filepath.Dir(dataPath), "logs")
	os.MkdirAll(h.logBaseDir, 0755)
	// 全站统计持久化
	h.siteStatsPath = filepath.Join(filepath.Dir(dataPath), "site_stats.json")
	h.loadSiteStats()
	// 上游统计持久化
	h.upstreamStatsPath = filepath.Join(filepath.Dir(dataPath), "upstream_stats.json")
	h.loadUpstreamStats()
	// 延迟统计持久化
	h.latencyStatsPath = filepath.Join(filepath.Dir(dataPath), "latency_stats.json")
	h.loadLatencyStats()
	// 预热日志环形缓冲
	h.recentLogs = h.readLogs("", maxRequestLogs)
	// 启动异步写入 goroutine
	h.wg.Add(2)
	model.SafeGo("logWriter", h.logWriter)
	model.SafeGo("costWriter", h.costWriter)
	return h
}

// Close 关闭异步通道，等待 logWriter/costWriter 刷盘后退出
func (h *Handler) Close() {
	close(h.logCh)
	close(h.costCh)
	h.wg.Wait()
	h.balancer.Close()
}

// logWriter 后台 goroutine，按分组写入日志文件，每 1w 条一个文件
func (h *Handler) logWriter() {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	dirty := make(map[string]bool)
	for {
		select {
		case entry, ok := <-h.logCh:
			if !ok {
				// channel 关闭，刷盘后退出
				for gid := range dirty {
					if w, exists := h.logWriters[gid]; exists {
						w.file.Sync()
					}
				}
				return
			}
			groupID := entry.GroupID
			if groupID == "" {
				groupID = "_unknown"
			}
			w := h.getOrCreateWriter(groupID)
			if w == nil {
				continue
			}
			data, _ := json.Marshal(entry)
			w.file.Write(append(data, '\n'))
			dirty[groupID] = true
			// 追加到内存环形缓冲
			h.recentLogsMu.Lock()
			h.recentLogs = append(h.recentLogs, entry)
			if len(h.recentLogs) > maxRequestLogs {
				h.recentLogs = h.recentLogs[len(h.recentLogs)-maxRequestLogs:]
			}
			h.recentLogsMu.Unlock()
			w.lineCount++
			if w.lineCount >= maxLinesPerLogFile {
				w.file.Sync()
				delete(dirty, groupID)
				h.rotateWriter(groupID, w)
			}
		case <-ticker.C:
			for gid := range dirty {
				if w, exists := h.logWriters[gid]; exists {
					w.file.Sync()
				}
			}
			dirty = make(map[string]bool)
			// 清理过期的 logSessions（TTL 10 分钟）
			h.logSessionsMu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for k, v := range h.logSessions {
				if v.at.Before(cutoff) {
					delete(h.logSessions, k)
				}
			}
			h.logSessionsMu.Unlock()
		}
	}
}

// getOrCreateWriter 获取或创建分组日志写入器
func (h *Handler) getOrCreateWriter(groupID string) *groupLogState {
	if w, ok := h.logWriters[groupID]; ok {
		return w
	}

	dir := filepath.Join(h.logBaseDir, groupID)
	os.MkdirAll(dir, 0755)

	// 查找已有文件确定起始状态
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	sort.Strings(files)

	fileIdx := 1
	lineCount := 0

	if len(files) > 0 {
		latest := files[len(files)-1]
		base := filepath.Base(latest)
		name := strings.TrimSuffix(base, ".jsonl")
		if n, err := strconv.Atoi(name); err == nil {
			fileIdx = n
		}
		lineCount = countFileLines(latest)
		if lineCount >= maxLinesPerLogFile {
			fileIdx++
			lineCount = 0
		}
	}

	path := filepath.Join(dir, fmt.Sprintf("%05d.jsonl", fileIdx))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[proxy] failed to open log file %s: %v", path, err)
		return nil
	}

	w := &groupLogState{
		dir:       dir,
		file:      f,
		lineCount: lineCount,
		fileIdx:   fileIdx,
	}
	h.logWriters[groupID] = w
	return w
}

// rotateWriter 轮转日志文件
func (h *Handler) rotateWriter(groupID string, w *groupLogState) {
	w.file.Close()
	w.fileIdx++
	w.lineCount = 0
	path := filepath.Join(w.dir, fmt.Sprintf("%05d.jsonl", w.fileIdx))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[proxy] failed to rotate log file %s: %v", path, err)
		delete(h.logWriters, groupID)
		return
	}
	w.file = f
}

// costWriter 后台 goroutine，异步持久化费用统计（批量刷盘）
func (h *Handler) costWriter() {
	defer h.wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	dirty := false
	for {
		select {
		case _, ok := <-h.costCh:
			if !ok {
				if dirty {
					h.saveSiteStats()
					h.saveUpstreamStats()
					h.saveLatencyStats()
				}
				return
			}
			dirty = true
		case <-ticker.C:
			if dirty {
				h.saveSiteStats()
				h.saveUpstreamStats()
				h.saveLatencyStats()
				dirty = false
			}
		}
	}
}

// countFileLines 快速统计文件行数
func countFileLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	count := 0
	buf := make([]byte, 32*1024)
	for {
		n, err := f.Read(buf)
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				count++
			}
		}
		if err != nil {
			break
		}
	}
	return count
}

// readLogs 读取最近 n 条日志，groupID 为空则读取所有分组
func (h *Handler) readLogs(groupID string, n int) []RequestLog {
	if groupID != "" {
		return h.readGroupLogs(groupID, n)
	}
	return h.readAllGroupLogs(n)
}

// readGroupLogs 从指定分组的日志文件中读取最近 n 条（tail 方式，从最新文件倒序读取）
func (h *Handler) readGroupLogs(groupID string, n int) []RequestLog {
	dir := filepath.Join(h.logBaseDir, groupID)
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil || len(files) == 0 {
		return nil
	}
	sort.Strings(files)

	var result []RequestLog
	for i := len(files) - 1; i >= 0 && len(result) < n; i-- {
		remaining := n - len(result)
		entries := tailReadFile(files[i], remaining)
		result = append(entries, result...)
	}
	if len(result) > n {
		result = result[len(result)-n:]
	}
	return result
}

// readAllGroupLogs 从所有分组中读取最近 n 条日志并按时间合并
func (h *Handler) readAllGroupLogs(n int) []RequestLog {
	entries, err := os.ReadDir(h.logBaseDir)
	if err != nil {
		return nil
	}

	var all []RequestLog
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		groupLogs := h.readGroupLogs(e.Name(), n)
		all = append(all, groupLogs...)
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Time < all[j].Time
	})
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// tailReadFile 从 JSONL 文件尾部读取最后 n 条记录
func tailReadFile(path string, n int) []RequestLog {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return nil
	}
	size := info.Size()

	readFrom := int64(0)
	buf := make([]byte, 32*1024)
	nlCount := 0
	pos := size
	for pos > 0 {
		readSize := int64(len(buf))
		if readSize > pos {
			readSize = pos
		}
		pos -= readSize
		nr, err := f.ReadAt(buf[:readSize], pos)
		if err != nil && err != io.EOF {
			break
		}
		for i := nr - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				nlCount++
				if nlCount > n {
					readFrom = pos + int64(i) + 1
					goto scan
				}
			}
		}
	}

scan:
	f.Seek(readFrom, io.SeekStart)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	var logs []RequestLog
	for scanner.Scan() {
		var entry RequestLog
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			logs = append(logs, entry)
		}
	}
	if len(logs) > n {
		logs = logs[len(logs)-n:]
	}
	return logs
}

func (h *Handler) loadSiteStats() {
	data, err := os.ReadFile(h.siteStatsPath)
	if err == nil {
		h.siteStatsMu.Lock()
		json.Unmarshal(data, &h.siteStats)
		h.siteStatsMu.Unlock()
		return
	}
	// 文件不存在，从日志文件回填
	logs := h.readLogs("", maxRequestLogs)
	pm := h.pricingMap()
	today := time.Now().In(cst).Format("2006-01-02")
	h.siteStatsMu.Lock()
	defer h.siteStatsMu.Unlock()
	h.siteStats.TodayDate = today
	for _, l := range logs {
		if l.Action != "success" || l.UpstreamID == "" {
			continue
		}
		cost := calcCost(l, pm)
		h.siteStats.TotalCost += cost
		h.siteStats.TotalRequests++
		if time.Unix(l.Time, 0).In(cst).Format("2006-01-02") == today {
			h.siteStats.TodayCost += cost
			h.siteStats.TodayRequests++
		}
	}
	h.saveSiteStats()
	log.Printf("[proxy] initialized site stats from logs: total=$%.4f (%d reqs), today=$%.4f (%d reqs)",
		h.siteStats.TotalCost, h.siteStats.TotalRequests, h.siteStats.TodayCost, h.siteStats.TodayRequests)
}

func (h *Handler) saveSiteStats() {
	data, _ := json.Marshal(h.siteStats)
	os.WriteFile(h.siteStatsPath, data, 0644)
}

func (h *Handler) addSiteCost(cost float64) {
	h.siteStatsMu.Lock()
	defer h.siteStatsMu.Unlock()
	today := time.Now().In(cst).Format("2006-01-02")
	if h.siteStats.TodayDate != today {
		h.siteStats.TodayDate = today
		h.siteStats.TodayCost = 0
		h.siteStats.TodayRequests = 0
	}
	h.siteStats.TodayCost += cost
	h.siteStats.TodayRequests++
	h.siteStats.TotalCost += cost
	h.siteStats.TotalRequests++
}

func (h *Handler) pricingMapFrom(cfg model.Config) map[string]model.ModelPricing {
	m := make(map[string]model.ModelPricing, len(cfg.ModelPricing))
	for _, p := range cfg.ModelPricing {
		m[p.Model] = p
	}
	return m
}

// pricingMap 供非热路径调用（如启动时加载统计）
func (h *Handler) pricingMap() map[string]model.ModelPricing {
	return h.pricingMapFrom(h.store.Get())
}

func (h *Handler) loadUpstreamStats() {
	data, err := os.ReadFile(h.upstreamStatsPath)
	if err == nil {
		h.upstreamStatsMu.Lock()
		h.upstreamStats = make(map[string]*UpstreamAccum)
		json.Unmarshal(data, &h.upstreamStats)
		h.upstreamStatsMu.Unlock()
		return
	}
	// 文件不存在，从日志文件回填
	logs := h.readLogs("", maxRequestLogs)
	pm := h.pricingMap()
	today := time.Now().In(cst).Format("2006-01-02")
	h.upstreamStatsMu.Lock()
	defer h.upstreamStatsMu.Unlock()
	h.upstreamStats = make(map[string]*UpstreamAccum)
	for _, l := range logs {
		if l.Action != "success" || l.UpstreamID == "" {
			continue
		}
		acc := h.upstreamStats[l.UpstreamID]
		if acc == nil {
			acc = &UpstreamAccum{TodayDate: today, GroupID: l.GroupID}
			h.upstreamStats[l.UpstreamID] = acc
		}
		cost := calcCost(l, pm)
		acc.TotalCost += cost
		acc.TotalRequests++
		if time.Unix(l.Time, 0).In(cst).Format("2006-01-02") == today {
			acc.TodayCost += cost
			acc.TodayRequests++
		}
	}
	h.saveUpstreamStats()
	log.Printf("[proxy] initialized upstream stats from logs: %d upstreams", len(h.upstreamStats))
}

func (h *Handler) saveUpstreamStats() {
	data, _ := json.Marshal(h.upstreamStats)
	os.WriteFile(h.upstreamStatsPath, data, 0644)
}

func (h *Handler) loadLatencyStats() {
	data, err := os.ReadFile(h.latencyStatsPath)
	if err != nil {
		return
	}
	var m map[string]upstreamLatency
	if json.Unmarshal(data, &m) != nil {
		return
	}
	h.upstreamStatsMu.Lock()
	defer h.upstreamStatsMu.Unlock()
	for id, lat := range m {
		acc := h.upstreamStats[id]
		if acc == nil {
			acc = &UpstreamAccum{}
			h.upstreamStats[id] = acc
		}
		acc.LatencyCount = lat.Count
		acc.LatencySum = lat.Sum
		acc.TTFBSum = lat.TTFB
	}
}

func (h *Handler) saveLatencyStats() {
	h.upstreamStatsMu.Lock()
	m := make(map[string]upstreamLatency, len(h.upstreamStats))
	for id, acc := range h.upstreamStats {
		if acc.LatencyCount > 0 {
			m[id] = upstreamLatency{Count: acc.LatencyCount, Sum: acc.LatencySum, TTFB: acc.TTFBSum}
		}
	}
	h.upstreamStatsMu.Unlock()
	data, _ := json.Marshal(m)
	os.WriteFile(h.latencyStatsPath, data, 0644)
}

func (h *Handler) addUpstreamCost(upstreamID string, groupID string, cost float64, durationMs int64, ttfbMs int64) {
	h.upstreamStatsMu.Lock()
	defer h.upstreamStatsMu.Unlock()
	acc := h.upstreamStats[upstreamID]
	if acc == nil {
		acc = &UpstreamAccum{}
		h.upstreamStats[upstreamID] = acc
	}
	today := time.Now().In(cst).Format("2006-01-02")
	if acc.TodayDate != today {
		acc.TodayDate = today
		acc.TodayCost = 0
		acc.TodayRequests = 0
	}
	acc.GroupID = groupID
	acc.TodayCost += cost
	acc.TodayRequests++
	acc.TotalCost += cost
	acc.TotalRequests++
	acc.LatencyCount++
	acc.LatencySum += durationMs
	acc.TTFBSum += ttfbMs
}

func (h *Handler) RegisterRoutes(r *gin.Engine) {
	r.POST("/v1/messages", h.proxyMessages)
}

// Balancer 返回负载均衡器实例
func (h *Handler) Balancer() *Balancer {
	return h.balancer
}

// CooldownInfo 转发到 Balancer
func (h *Handler) CooldownInfo() map[string]time.Time {
	return h.balancer.CooldownInfo()
}

// SessionInfo 转发到 Balancer
func (h *Handler) SessionInfo() []any {
	return h.balancer.SessionInfo()
}

// LoadInfo 返回所有上游的当前并发数
func (h *Handler) LoadInfo() map[string]int64 {
	cfg := h.store.Get()
	result := make(map[string]int64)
	for _, u := range cfg.Upstreams {
		result[u.ID] = h.balancer.GetLoad(u.ID)
	}
	return result
}

func (h *Handler) proxyMessages(c *gin.Context) {
	startTime := time.Now()

	// 通过 API Key 匹配分组
	apiKey := c.GetHeader("X-Api-Key")
	if apiKey == "" {
		auth := c.GetHeader("Authorization")
		if strings.HasPrefix(auth, "Bearer ") {
			apiKey = strings.TrimPrefix(auth, "Bearer ")
		}
	}
	if apiKey == "" {
		anthropicError(c, http.StatusUnauthorized, "authentication_error", "missing api key")
		return
	}

	cfg, group := h.store.FindGroupByKey(apiKey)
	if group == nil {
		anthropicError(c, http.StatusUnauthorized, "authentication_error", "invalid api key")
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	// 单次解析请求体，后续所有提取操作从 parsed 读取
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	// 提取 model 名称
	reqModel, _ := parsed["model"].(string)

	// 分组模型校验：如果分组配置了支持模型列表，则只允许列表内的模型
	// 模型映射的源模型也视为允许的模型
	if len(group.Models) > 0 && reqModel != "" {
		allowed := false
		for _, m := range group.Models {
			if m == reqModel {
				allowed = true
				break
			}
		}
		if !allowed {
			if _, ok := group.ModelMapping[reqModel]; ok {
				allowed = true
			}
		}
		if !allowed {
			anthropicError(c, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("model %q is not allowed in this group, allowed models: %v", reqModel, group.Models))
			return
		}
	}

	// 模型映射：替换请求中的模型
	var originalModel string
	if len(group.ModelMapping) > 0 && reqModel != "" {
		if mapped, ok := group.ModelMapping[reqModel]; ok {
			originalModel = reqModel
			parsed["model"] = mapped
			reqModel = mapped
		}
	}

	// 请求模式校验
	reqStream, _ := parsed["stream"].(bool)
	if reqStream {
		if group.AllowStream != nil && !*group.AllowStream {
			anthropicError(c, http.StatusBadRequest, "invalid_request_error", "streaming is not allowed in this group")
			return
		}
	} else {
		allowNonStream := group.AllowNonStream != nil && *group.AllowNonStream
		if !allowNonStream {
			anthropicError(c, http.StatusBadRequest, "invalid_request_error", "non-streaming is not allowed in this group")
			return
		}
	}

	// 字段剔除（分组优先，全局兜底）
	stripFieldsCfg := group.StripFields
	if len(stripFieldsCfg) == 0 {
		stripFieldsCfg = cfg.StripFields
	}
	if len(stripFieldsCfg) > 0 {
		for _, path := range stripFieldsCfg {
			parts := strings.Split(path, ".")
			stripPath(parsed, parts)
		}
	}

	// 单次序列化：模型替换和字段剔除后重新生成 body
	body, err = json.Marshal(parsed)
	if err != nil {
		anthropicError(c, http.StatusBadRequest, "invalid_request_error", "failed to marshal request body")
		return
	}

	// 会话亲和 session key: 仅从 metadata.user_id 提取，无 user_id 时不做亲和
	sessionKey, rawUserID := extractUserIDFromMap(parsed)

	// 根据分组日志模式决定是否记录本次请求
	logEnabled := h.shouldLog(group, sessionKey)
	logErrorOnly := group.LogMode == "error_only"

	// 提取上游请求 ID（用于关联 new-api 日志）
	requestID := c.GetHeader("X-Request-Log-Id")

	maxRetries := h.balancer.ActiveCount(cfg, group.ID)
	if maxRetries == 0 {
		if logEnabled {
			h.addLog(RequestLog{
				Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
				SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
				Status: 502, Duration: time.Since(startTime).Milliseconds(),
				Action: "error", Detail: "no active upstream",
				RequestBody: string(body),
			})
		}
		anthropicError(c, http.StatusBadGateway, "api_error", "no active upstream")
		return
	}

	// 构建故障转移规则映射
	failoverRules := group.FailoverRules
	if len(failoverRules) == 0 {
		failoverRules = model.DefaultFailoverRules()
	}
	ruleMap := make(map[int]model.FailoverRule, len(failoverRules))
	for _, r := range failoverRules {
		ruleMap[r.Code] = r
	}

	// retry 动作按状态码记录已重试次数
	retryCount := make(map[int]int)

	excluded := make(map[string]bool)
	for attempt := 0; attempt < maxRetries; attempt++ {
		upstream := h.balancer.Pick(cfg, group.ID, sessionKey, rawUserID, reqModel, excluded)
		if upstream == nil {
			break
		}
		excluded[upstream.ID] = true

		h.balancer.IncLoad(upstream.ID)
		attemptStart := time.Now()
		resp, err := h.doRequest(c, upstream, body)
		ttfb := time.Since(attemptStart).Milliseconds()
		h.balancer.DecLoad(upstream.ID)

		if err != nil {
			log.Printf("[proxy] upstream %s (%s) request error: %v", upstream.ID, upstream.Remark, err)
			if logEnabled {
				h.addLog(RequestLog{
					Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
					SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
					UpstreamID: upstream.ID, Remark: upstream.Remark,
					Status: 0, Duration: time.Since(startTime).Milliseconds(),
					Action: "failover", Detail: fmt.Sprintf("连接错误: %v", err), RetryCount: attempt,
					RequestBody: string(body),
				})
			}
			// 客户端主动断开（context canceled）不标记上游故障
			if c.Request.Context().Err() != nil {
				return
			}
			// 连接错误仅 failover，不标记故障（可能是临时网络抖动），由健康检查判断是否真正离线
			continue
		}

		statusCode := resp.StatusCode

		// 检查是否命中故障转移规则
		if rule, hit := ruleMap[statusCode]; hit {
			// 读取错误响应体用于日志
			errBody, _ := io.ReadAll(resp.Body)
			errBodyStr := string(errBody)
			log.Printf("[proxy] upstream %s (%s) returned %d, action=%s, body: %s", upstream.ID, upstream.Remark, statusCode, rule.Action, errBodyStr)
			detail := fmt.Sprintf("HTTP %d → %s", statusCode, rule.Action)
			switch rule.Action {
			case "retry", "retry_other":
				maxRetry := rule.Retries
				if maxRetry <= 0 {
					maxRetry = 1
				}
				retryCount[statusCode]++
				if retryCount[statusCode] > maxRetry {
					// 超过重试次数，不再重试，直接返回错误给客户端
					detail = fmt.Sprintf("HTTP %d → retry exhausted (%d/%d)", statusCode, retryCount[statusCode]-1, maxRetry)
					if logEnabled {
						h.addLog(RequestLog{
							Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
							SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
							UpstreamID: upstream.ID, Remark: upstream.Remark,
							Status: statusCode, Duration: time.Since(startTime).Milliseconds(),
							Action: "error", Detail: detail, RetryCount: attempt,
							RequestBody: string(body), ResponseBody: errBodyStr,
						})
					}
					resp.Body.Close()
					// 原样返回上游响应
					c.Data(statusCode, "application/json", errBody)
					return
				}
				actionLabel := "retry"
				if rule.Action == "retry" {
					// 重试：允许同一上游再次被选中
					delete(excluded, upstream.ID)
					actionLabel = "retry(same)"
				} else {
					actionLabel = "retry(other)"
				}
				detail = fmt.Sprintf("HTTP %d → %s %d/%d", statusCode, actionLabel, retryCount[statusCode], maxRetry)
				log.Printf("[proxy] upstream %s %s %d/%d for HTTP %d", upstream.ID, actionLabel, retryCount[statusCode], maxRetry, statusCode)
			case "cooldown":
				dur := time.Duration(rule.CooldownS) * time.Second
				if dur == 0 {
					dur = 60 * time.Second
				}
				if rule.UseHeader != "" {
					if hv := resp.Header.Get(rule.UseHeader); hv != "" {
						if d := parseRetryAfter(hv); d > 0 {
							dur = d
						}
					}
				}
				h.balancer.SetCooldown(upstream.ID, dur)
				detail = fmt.Sprintf("HTTP %d → cooldown %v, retry-after: %s", statusCode, dur, resp.Header.Get("retry-after"))
				log.Printf("[proxy] upstream %s cooldown %v, retry-after: %s", upstream.ID, dur, resp.Header.Get("retry-after"))
			default: // "offline"
				h.markFault(upstream.ID, fmt.Sprintf("HTTP %d: %s", statusCode, errBodyStr))
			}
			if logEnabled {
				h.addLog(RequestLog{
					Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
					SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
					UpstreamID: upstream.ID, Remark: upstream.Remark,
					Status: statusCode, Duration: time.Since(startTime).Milliseconds(),
					Action: "failover", Detail: detail, RetryCount: attempt,
					RequestBody: string(body), ResponseBody: errBodyStr,
				})
			}
			resp.Body.Close()
			continue
		}

		// 错误码映射：分组级优先，全局兜底
		mappings := group.ErrorMappings
		if len(mappings) == 0 {
			mappings = cfg.ErrorMappings
		}
		statusCode, respBody := h.applyErrorMapping(mappings, statusCode, resp)

		// 写响应并捕获 usage
		usage, respBodyBytes := h.writeResponseWithUsage(c, statusCode, resp.Header, respBody, originalModel)

		// 日志中记录用户请求的原始模型
		logModel := reqModel
		if originalModel != "" {
			logModel = originalModel
		}
		logEntry := RequestLog{
			Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
			SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: logModel,
			UpstreamID: upstream.ID, Remark: upstream.Remark,
			Status: statusCode, Duration: time.Since(startTime).Milliseconds(), TTFBMs: ttfb,
			Action: "success", RetryCount: attempt,
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CacheReadTokens: usage.CacheReadInputTokens, CacheWriteTokens: usage.CacheCreationInputTokens,
			RequestBody: string(body),
			ResponseBody: string(respBodyBytes),
		}
		if statusCode >= 400 {
			logEntry.Action = "error"
			logEntry.Detail = fmt.Sprintf("HTTP %d", statusCode)
			log.Printf("[proxy] upstream %s (%s) returned %d (not in failover rules), body: %s", upstream.ID, upstream.Remark, statusCode, string(respBodyBytes))
		}
		if logEnabled && !(logErrorOnly && logEntry.Action == "success") {
			h.addLog(logEntry)
		}
		if logEntry.Action == "success" {
			costLog := logEntry
			costLog.Model = reqModel // 使用实际转发的模型计算费用
			cost := calcCost(costLog, h.pricingMapFrom(cfg))
			h.addSiteCost(cost)
			h.addUpstreamCost(upstream.ID, group.ID, cost, logEntry.Duration, logEntry.TTFBMs)
			// 非阻塞发送费用到异步持久化 goroutine
			select {
			case h.costCh <- costEntry{upstreamID: upstream.ID, cost: cost}:
			default:
			}
		}
		return
	}

	if logEnabled {
		h.addLog(RequestLog{
			Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
			SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
			Status: 502, Duration: time.Since(startTime).Milliseconds(),
			Action: "error", Detail: "all upstreams failed",
			RequestBody: string(body),
		})
	}
	anthropicError(c, http.StatusBadGateway, "api_error", "all upstreams failed")
}

func (h *Handler) doRequest(c *gin.Context, upstream *model.Upstream, body []byte) (*http.Response, error) {
	base := strings.TrimRight(upstream.API, "/")
	url := base + "/v1/messages"
	if strings.HasSuffix(base, "/v1/messages") {
		url = base
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// 复制原始请求头
	for key, vals := range c.Request.Header {
		k := strings.ToLower(key)
		if k == "host" || k == "content-length" {
			continue
		}
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}
	// 替换认证头
	req.Header.Set("X-Api-Key", upstream.APIKey)
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	// 确保 Content-Type 为 JSON
	req.Header.Set("Content-Type", "application/json")

	return h.client.Do(req)
}

func (h *Handler) applyErrorMapping(errorMappings []model.ErrorMapping, statusCode int, resp *http.Response) (int, io.ReadCloser) {
	for _, m := range errorMappings {
		if m.SourceCode == statusCode {
			if m.Message != "" {
				resp.Body.Close()
				errBody := map[string]any{
					"type": "error",
					"error": map[string]any{
						"type":    httpStatusToAnthropicError(m.TargetCode),
						"message": m.Message,
					},
				}
				data, _ := json.Marshal(errBody)
				return m.TargetCode, io.NopCloser(strings.NewReader(string(data)))
			}
			return m.TargetCode, resp.Body
		}
	}
	return statusCode, resp.Body
}

// httpStatusToAnthropicError 将 HTTP 状态码映射为 Anthropic 错误类型
func httpStatusToAnthropicError(code int) string {
	switch code {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 408:
		return "request_timeout"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func (h *Handler) writeResponseWithUsage(c *gin.Context, statusCode int, header http.Header, body io.ReadCloser, originalModel string) (Usage, []byte) {
	defer body.Close()

	// 是否需要做模型名替换
	needModelReplace := originalModel != ""

	for key, vals := range header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Writer.WriteHeader(statusCode)

	var usage Usage
	var captured []byte

	if isSSE(header) {
		// 流式：边写边扫描 usage
		flusher, ok := c.Writer.(http.Flusher)
		scanner := bufio.NewScanner(body)
		scanBuf := scanBufPool.Get().([]byte)
		defer scanBufPool.Put(scanBuf)
		scanner.Buffer(scanBuf, 256*1024)
		var sseBuf strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			// 模型映射：替换 SSE 中的模型名
			if needModelReplace {
				line = replaceModelInSSELine(line, originalModel)
			}
			c.Writer.Write([]byte(line + "\n"))
			if ok {
				flusher.Flush()
			}
			// 捕获响应内容用于日志
			sseBuf.WriteString(line)
			sseBuf.WriteByte('\n')
			// 从 SSE data 行提取 usage
			if strings.HasPrefix(line, "data: ") {
				data := line[6:]
				extractSSEUsage(data, &usage)
			}
		}
		if sseBuf.Len() > 0 {
			captured = []byte(sseBuf.String())
		}
	} else {
		// 非流式：读取全部，解析 usage，再写出
		data, _ := io.ReadAll(body)
		// 模型映射：替换响应中的模型名
		if needModelReplace {
			data = replaceModelInResponse(data, originalModel)
		}
		c.Writer.Write(data)
		captured = data
		var resp struct {
			Usage Usage `json:"usage"`
		}
		if json.Unmarshal(data, &resp) == nil {
			usage = resp.Usage
		}
	}
	return usage, captured
}

// extractSSEUsage 从 SSE data 中提取 usage 信息
func extractSSEUsage(data string, usage *Usage) {
	// 快速跳过：只有 message_start 和 message_delta 包含 usage
	if !strings.Contains(data, `"message_start"`) && !strings.Contains(data, `"message_delta"`) {
		return
	}
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage Usage `json:"usage"`
		} `json:"message"`
		Usage *Usage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &event) != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			usage.InputTokens = event.Message.Usage.InputTokens
			usage.OutputTokens = event.Message.Usage.OutputTokens
			usage.CacheCreationInputTokens = event.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = event.Message.Usage.CacheReadInputTokens
		}
	case "message_delta":
		if event.Usage != nil {
			if event.Usage.OutputTokens > 0 {
				usage.OutputTokens = event.Usage.OutputTokens
			}
			if event.Usage.InputTokens > 0 {
				usage.InputTokens = event.Usage.InputTokens
			}
			if event.Usage.CacheCreationInputTokens > 0 {
				usage.CacheCreationInputTokens = event.Usage.CacheCreationInputTokens
			}
			if event.Usage.CacheReadInputTokens > 0 {
				usage.CacheReadInputTokens = event.Usage.CacheReadInputTokens
			}
		}
	}
}

func isSSE(header http.Header) bool {
	ct := header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}

// anthropicError 返回 Anthropic 规范格式的错误响应
func anthropicError(c *gin.Context, statusCode int, errType string, message string) {
	c.JSON(statusCode, gin.H{
		"type": "error",
		"error": gin.H{
			"type":    errType,
			"message": message,
		},
	})
}

func (h *Handler) markFault(upstreamID string, reason string) {
	now := time.Now().Unix()
	if err := h.store.Update(func(cfg *model.Config) {
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].ID == upstreamID {
				cfg.Upstreams[i].Status = "faulted"
				cfg.Upstreams[i].FaultedAt = &now
				cfg.Upstreams[i].FaultType = "auto"
				cfg.Upstreams[i].FaultReason = reason
				return
			}
		}
	}); err != nil {
		fmt.Printf("[proxy] failed to mark upstream %s as faulted: %v\n", upstreamID, err)
	}
}

// extractUserIDFromMap 从已解析的 map 中提取会话标识和原始 user_id JSON
func extractUserIDFromMap(parsed map[string]any) (sessionKey string, rawUserID string) {
	meta, _ := parsed["metadata"].(map[string]any)
	if meta == nil {
		return "", ""
	}
	uid := meta["user_id"]
	if uid == nil {
		return "", ""
	}
	// 序列化原始 user_id 用于展示
	if raw, err := json.Marshal(uid); err == nil {
		rawUserID = string(raw)
	}
	// 字符串类型：可能是纯字符串或双重编码的 JSON
	if s, ok := uid.(string); ok {
		var inner map[string]any
		if json.Unmarshal([]byte(s), &inner) == nil {
			rawUserID = s
			if sid, ok := inner["session_id"].(string); ok && sid != "" {
				return sid, rawUserID
			}
			if did, ok := inner["device_id"].(string); ok && did != "" {
				return did, rawUserID
			}
		}
		return s, rawUserID
	}
	if m, ok := uid.(map[string]any); ok {
		if sid, ok := m["session_id"].(string); ok && sid != "" {
			return sid, rawUserID
		}
		if did, ok := m["device_id"].(string); ok && did != "" {
			return did, rawUserID
		}
	}
	return "", rawUserID
}

// parseRetryAfter 解析 retry-after header，支持秒数和 HTTP 日期格式
func parseRetryAfter(val string) time.Duration {
	// 尝试纯数字秒数
	if secs, err := strconv.Atoi(val); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// 尝试 HTTP 日期格式: "Fri, 10 Apr 2026 04:23:00 GMT"
	if t, err := time.Parse(time.RFC1123, val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	if t, err := time.Parse(time.RFC1123Z, val); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// replaceModelInResponse 替换非流式响应中的 model 字段
func replaceModelInResponse(data []byte, originalModel string) []byte {
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		return data
	}
	if _, ok := resp["model"]; ok {
		resp["model"] = originalModel
		out, err := json.Marshal(resp)
		if err != nil {
			return data
		}
		return out
	}
	return data
}

// replaceModelInSSELine 替换 SSE data 行中的 model 字段
func replaceModelInSSELine(line string, originalModel string) string {
	if !strings.HasPrefix(line, "data: ") {
		return line
	}
	data := line[6:]
	// 快速跳过：不含 "model" 的行无需解析
	if !strings.Contains(data, `"model"`) {
		return line
	}
	var event map[string]any
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return line
	}
	changed := false
	if _, ok := event["model"]; ok {
		event["model"] = originalModel
		changed = true
	}
	// message_start 事件中 message.model 也需要替换
	if msg, ok := event["message"].(map[string]any); ok {
		if _, ok := msg["model"]; ok {
			msg["model"] = originalModel
			changed = true
		}
	}
	if !changed {
		return line
	}
	out, err := json.Marshal(event)
	if err != nil {
		return line
	}
	return "data: " + string(out)
}

func stripPath(node any, parts []string) {
	if len(parts) == 0 {
		return
	}
	key := parts[0]
	rest := parts[1:]

	switch v := node.(type) {
	case map[string]any:
		if key == "*" {
			// 通配：对所有 value 递归
			if len(rest) == 0 {
				return // 不能删除 map 的所有 key
			}
			for _, child := range v {
				stripPath(child, rest)
			}
		} else if len(rest) == 0 {
			// 最后一段，删除该 key
			delete(v, key)
		} else {
			// 继续递归
			if child, ok := v[key]; ok {
				stripPath(child, rest)
			}
		}
	case []any:
		if key == "*" {
			// 通配：对数组每个元素递归
			if len(rest) == 0 {
				return
			}
			for _, elem := range v {
				stripPath(elem, rest)
			}
		} else {
			// key 是索引
			idx := 0
			if _, err := fmt.Sscanf(key, "%d", &idx); err == nil && idx >= 0 && idx < len(v) {
				if len(rest) == 0 {
					return // 不删除数组元素
				}
				stripPath(v[idx], rest)
			}
		}
	}
}
