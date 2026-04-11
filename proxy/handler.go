package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

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
const maxLogBodySize = 4096 // 日志中记录的请求/响应体最大长度

func truncateBody(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// costEntry 异步费用更新
type costEntry struct {
	upstreamID string
	cost       float64
}

type Handler struct {
	store    *store.Store
	balancer *Balancer
	client   *http.Client

	logs    []RequestLog
	logsMu  sync.RWMutex
	logFile *os.File // 日志持久化文件
	logCh   chan RequestLog // 异步写入文件
	costCh  chan costEntry  // 异步费用更新

	siteStats     SiteStats
	siteStatsMu   sync.Mutex
	siteStatsPath string

	upstreamStats     map[string]*UpstreamAccum // 上游独立累计
	upstreamStatsMu   sync.Mutex
	upstreamStatsPath string
}

// UpstreamAccum 单个上游的持久化累计数据
type UpstreamAccum struct {
	TodayDate     string  `json:"today_date"`
	TodayCost     float64 `json:"today_cost"`
	TodayRequests int     `json:"today_requests"`
	TotalCost     float64 `json:"total_cost"`
	TotalRequests int     `json:"total_requests"`
}

func (h *Handler) addLog(entry RequestLog) {
	h.logsMu.Lock()
	h.logs = append(h.logs, entry)
	if len(h.logs) > maxRequestLogs {
		h.logs = h.logs[len(h.logs)-maxRequestLogs:]
	}
	h.logsMu.Unlock()
	// 非阻塞发送到异步写入 goroutine
	select {
	case h.logCh <- entry:
	default:
	}
}

// RequestLogs 返回请求日志（倒序）
func (h *Handler) RequestLogs() []any {
	h.logsMu.RLock()
	defer h.logsMu.RUnlock()
	result := make([]any, len(h.logs))
	for i, e := range h.logs {
		result[len(h.logs)-1-i] = e
	}
	return result
}

// RequestLogsRaw 返回原始日志用于统计
func (h *Handler) RequestLogsRaw() []any {
	h.logsMu.RLock()
	defer h.logsMu.RUnlock()
	result := make([]any, len(h.logs))
	for i, e := range h.logs {
		result[i] = e
	}
	return result
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

// DailyStats 统计数据：花费/请求数从持久化文件读，延迟从近期日志算
func (h *Handler) DailyStats(pricing []model.ModelPricing) any {
	now := time.Now()
	today := now.Format("2006-01-02")

	// 从日志算延迟（反映近期性能）
	h.logsMu.RLock()
	logs := make([]RequestLog, len(h.logs))
	copy(logs, h.logs)
	h.logsMu.RUnlock()

	type latencyAcc struct {
		count     int
		totalMs   int64
		totalTTFB int64
	}
	latMap := make(map[string]*latencyAcc)
	for _, l := range logs {
		if l.Action != "success" || l.UpstreamID == "" {
			continue
		}
		la, ok := latMap[l.UpstreamID]
		if !ok {
			la = &latencyAcc{}
			latMap[l.UpstreamID] = la
		}
		la.count++
		la.totalMs += l.Duration
		la.totalTTFB += l.TTFBMs
	}

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

	// 合并：所有有持久化记录的上游都出现
	for id, acc := range upSnap {
		avg := int64(0)
		avgTTFB := int64(0)
		if la, ok := latMap[id]; ok && la.count > 0 {
			avg = la.totalMs / int64(la.count)
			avgTTFB = la.totalTTFB / int64(la.count)
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

	// 分组统计：按上游所属分组聚合
	cfg := h.store.Get()
	upGroupMap := make(map[string]string)
	for _, u := range cfg.Upstreams {
		upGroupMap[u.ID] = u.GroupID
	}
	for id, st := range resp.Upstreams {
		gid := upGroupMap[id]
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
		},
		logCh:  make(chan RequestLog, 1024),
		costCh: make(chan costEntry, 256),
	}
	h.balancer.SetSessionsPath(dataPath)
	// 日志目录：数据文件同级的 logs/
	logDir := filepath.Join(filepath.Dir(dataPath), "logs")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "requests.jsonl")
	h.loadLogs(logPath)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("[proxy] failed to open log file: %v", err)
	} else {
		h.logFile = f
	}
	// 全站统计持久化
	h.siteStatsPath = filepath.Join(filepath.Dir(dataPath), "site_stats.json")
	h.loadSiteStats()
	// 上游统计持久化
	h.upstreamStatsPath = filepath.Join(filepath.Dir(dataPath), "upstream_stats.json")
	h.loadUpstreamStats()
	// 启动异步写入 goroutine
	go h.logWriter()
	go h.costWriter()
	return h
}

// logWriter 后台 goroutine，异步写入日志文件
func (h *Handler) logWriter() {
	for entry := range h.logCh {
		if h.logFile != nil {
			data, _ := json.Marshal(entry)
			h.logFile.Write(append(data, '\n'))
		}
	}
}

// costWriter 后台 goroutine，异步持久化费用统计
func (h *Handler) costWriter() {
	for range h.costCh {
		h.saveSiteStats()
		h.saveUpstreamStats()
	}
}

func (h *Handler) loadLogs(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	// 从文件尾部向前扫描，只读取最后 maxRequestLogs 行
	info, err := f.Stat()
	if err != nil {
		return
	}
	size := info.Size()
	if size == 0 {
		return
	}

	// 向前扫描找到倒数第 maxRequestLogs 个换行符的位置
	readFrom := int64(0)
	if size > 0 {
		buf := make([]byte, 32*1024)
		nlCount := 0
		pos := size
		for pos > 0 {
			readSize := int64(len(buf))
			if readSize > pos {
				readSize = pos
			}
			pos -= readSize
			n, err := f.ReadAt(buf[:readSize], pos)
			if err != nil && err != io.EOF {
				break
			}
			for i := n - 1; i >= 0; i-- {
				if buf[i] == '\n' {
					nlCount++
					if nlCount > maxRequestLogs {
						readFrom = pos + int64(i) + 1
						goto scan
					}
				}
			}
		}
	}

scan:
	f.Seek(readFrom, io.SeekStart)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		var entry RequestLog
		if json.Unmarshal(scanner.Bytes(), &entry) == nil {
			h.logs = append(h.logs, entry)
		}
	}
	// 保险：确保不超过上限
	if len(h.logs) > maxRequestLogs {
		h.logs = h.logs[len(h.logs)-maxRequestLogs:]
	}
	if len(h.logs) > 0 {
		log.Printf("[proxy] loaded %d request logs from %s", len(h.logs), path)
	}
}

func (h *Handler) loadSiteStats() {
	data, err := os.ReadFile(h.siteStatsPath)
	if err == nil {
		h.siteStatsMu.Lock()
		json.Unmarshal(data, &h.siteStats)
		h.siteStatsMu.Unlock()
		return
	}
	// 文件不存在，从现有日志回填
	pm := h.pricingMap()
	today := time.Now().Format("2006-01-02")
	h.siteStatsMu.Lock()
	defer h.siteStatsMu.Unlock()
	h.siteStats.TodayDate = today
	for _, l := range h.logs {
		if l.Action != "success" || l.UpstreamID == "" {
			continue
		}
		cost := calcCost(l, pm)
		h.siteStats.TotalCost += cost
		h.siteStats.TotalRequests++
		if time.Unix(l.Time, 0).Format("2006-01-02") == today {
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
	today := time.Now().Format("2006-01-02")
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

func (h *Handler) pricingMap() map[string]model.ModelPricing {
	cfg := h.store.Get()
	m := make(map[string]model.ModelPricing, len(cfg.ModelPricing))
	for _, p := range cfg.ModelPricing {
		m[p.Model] = p
	}
	return m
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
	// 文件不存在，从现有日志回填
	pm := h.pricingMap()
	today := time.Now().Format("2006-01-02")
	h.upstreamStatsMu.Lock()
	defer h.upstreamStatsMu.Unlock()
	h.upstreamStats = make(map[string]*UpstreamAccum)
	for _, l := range h.logs {
		if l.Action != "success" || l.UpstreamID == "" {
			continue
		}
		acc := h.upstreamStats[l.UpstreamID]
		if acc == nil {
			acc = &UpstreamAccum{TodayDate: today}
			h.upstreamStats[l.UpstreamID] = acc
		}
		cost := calcCost(l, pm)
		acc.TotalCost += cost
		acc.TotalRequests++
		if time.Unix(l.Time, 0).Format("2006-01-02") == today {
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

func (h *Handler) addUpstreamCost(upstreamID string, cost float64) {
	h.upstreamStatsMu.Lock()
	defer h.upstreamStatsMu.Unlock()
	acc := h.upstreamStats[upstreamID]
	if acc == nil {
		acc = &UpstreamAccum{}
		h.upstreamStats[upstreamID] = acc
	}
	today := time.Now().Format("2006-01-02")
	if acc.TodayDate != today {
		acc.TodayDate = today
		acc.TodayCost = 0
		acc.TodayRequests = 0
	}
	acc.TodayCost += cost
	acc.TodayRequests++
	acc.TotalCost += cost
	acc.TotalRequests++
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing api key"})
		return
	}

	cfg := h.store.Get()
	var group *model.Group
	for _, g := range cfg.Groups {
		if g.APIKey == apiKey {
			group = &g
			break
		}
	}
	if group == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid api key"})
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	// 提取 model 名称
	reqModel := extractModel(body)

	// 字段剔除
	cfg = h.store.Get()
	if len(cfg.StripFields) > 0 {
		body = stripFields(body, cfg.StripFields)
	}

	// 会话亲和 session key: 仅从 metadata.user_id 提取，无 user_id 时不做亲和
	sessionKey := extractUserID(body)

	// 提取上游请求 ID（用于关联 new-api 日志）
	requestID := c.GetHeader("X-Request-Log-Id")

	maxRetries := h.balancer.ActiveCount(group.ID)
	if maxRetries == 0 {
		h.addLog(RequestLog{
			Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
			SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
			Status: 502, Duration: time.Since(startTime).Milliseconds(),
			Action: "error", Detail: "no active upstream",
		})
		c.JSON(http.StatusBadGateway, gin.H{"error": "no active upstream"})
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

	excluded := make(map[string]bool)
	for attempt := 0; attempt < maxRetries; attempt++ {
		upstream := h.balancer.Pick(group.ID, sessionKey, reqModel, excluded)
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
			h.addLog(RequestLog{
				Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
				SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
				UpstreamID: upstream.ID, Remark: upstream.Remark,
				Status: 0, Duration: time.Since(startTime).Milliseconds(),
				Action: "failover", Detail: fmt.Sprintf("连接错误: %v", err),
				RequestBody: truncateBody(body, maxLogBodySize),
			})
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
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, int64(maxLogBodySize)))
			errBodyStr := string(errBody)
			log.Printf("[proxy] upstream %s (%s) returned %d, action=%s, body: %s", upstream.ID, upstream.Remark, statusCode, rule.Action, errBodyStr)
			detail := fmt.Sprintf("HTTP %d → %s", statusCode, rule.Action)
			switch rule.Action {
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
			h.addLog(RequestLog{
				Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
				SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
				UpstreamID: upstream.ID, Remark: upstream.Remark,
				Status: statusCode, Duration: time.Since(startTime).Milliseconds(),
				Action: "failover", Detail: detail,
				RequestBody: truncateBody(body, maxLogBodySize), ResponseBody: errBodyStr,
			})
			resp.Body.Close()
			continue
		}

		// 错误码映射
		statusCode, respBody := h.applyErrorMapping(statusCode, resp)

		// 写响应并捕获 usage
		usage, respBodyBytes := h.writeResponseWithUsage(c, statusCode, resp.Header, respBody)

		logEntry := RequestLog{
			Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
			SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
			UpstreamID: upstream.ID, Remark: upstream.Remark,
			Status: statusCode, Duration: time.Since(startTime).Milliseconds(), TTFBMs: ttfb,
			Action: "success",
			InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CacheReadTokens: usage.CacheReadInputTokens, CacheWriteTokens: usage.CacheCreationInputTokens,
		}
		if statusCode >= 400 {
			logEntry.Action = "error"
			logEntry.Detail = fmt.Sprintf("HTTP %d", statusCode)
			logEntry.RequestBody = truncateBody(body, maxLogBodySize)
			logEntry.ResponseBody = truncateBody(respBodyBytes, maxLogBodySize)
			log.Printf("[proxy] upstream %s (%s) returned %d (not in failover rules), body: %s", upstream.ID, upstream.Remark, statusCode, truncateBody(respBodyBytes, 512))
		}
		h.addLog(logEntry)
		if logEntry.Action == "success" {
			cost := calcCost(logEntry, h.pricingMap())
			h.addSiteCost(cost)
			h.addUpstreamCost(upstream.ID, cost)
			// 非阻塞发送费用到异步持久化 goroutine
			select {
			case h.costCh <- costEntry{upstreamID: upstream.ID, cost: cost}:
			default:
			}
		}
		return
	}

	h.addLog(RequestLog{
		Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
		SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
		Status: 502, Duration: time.Since(startTime).Milliseconds(),
		Action: "error", Detail: "all upstreams failed",
	})
	c.JSON(http.StatusBadGateway, gin.H{"error": "all upstreams failed"})
}

func (h *Handler) doRequest(c *gin.Context, upstream *model.Upstream, body []byte) (*http.Response, error) {
	base := strings.TrimRight(upstream.API, "/")
	url := base + "/v1/messages"
	if strings.HasSuffix(base, "/v1/messages") {
		url = base
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}

	// 复制原始请求头
	for key, vals := range c.Request.Header {
		k := strings.ToLower(key)
		if k == "host" {
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

func (h *Handler) applyErrorMapping(statusCode int, resp *http.Response) (int, io.ReadCloser) {
	cfg := h.store.Get()
	for _, m := range cfg.ErrorMappings {
		if m.SourceCode == statusCode {
			if m.Message != "" {
				resp.Body.Close()
				errBody := map[string]any{
					"type": "error",
					"error": map[string]any{
						"type":    http.StatusText(m.TargetCode),
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

func (h *Handler) writeResponseWithUsage(c *gin.Context, statusCode int, header http.Header, body io.ReadCloser) (Usage, []byte) {
	defer body.Close()

	for key, vals := range header {
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
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		var sseBuf strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			c.Writer.Write([]byte(line + "\n"))
			if ok {
				flusher.Flush()
			}
			// 错误响应时捕获内容用于日志
			if statusCode >= 400 && sseBuf.Len() < maxLogBodySize {
				sseBuf.WriteString(line)
				sseBuf.WriteByte('\n')
			}
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
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage Usage `json:"usage"`
		} `json:"message"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &event) != nil {
		return
	}
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			usage.InputTokens = event.Message.Usage.InputTokens
			usage.CacheCreationInputTokens = event.Message.Usage.CacheCreationInputTokens
			usage.CacheReadInputTokens = event.Message.Usage.CacheReadInputTokens
		}
	case "message_delta":
		if event.Usage != nil {
			usage.OutputTokens = event.Usage.OutputTokens
		}
	}
}

func isSSE(header http.Header) bool {
	ct := header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
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

// extractUserID 从请求体中提取会话标识
// 兼容两种格式:
//   - 旧版: metadata.user_id = "some-string"
//   - 新版: metadata.user_id = {"device_id":"...","account_uuid":"...","session_id":"..."}
func extractUserID(body []byte) string {
	var req struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	if req.Metadata == nil {
		return ""
	}
	uid := req.Metadata["user_id"]
	if uid == nil {
		return ""
	}
	if s, ok := uid.(string); ok {
		return s
	}
	if m, ok := uid.(map[string]any); ok {
		if sid, ok := m["session_id"].(string); ok && sid != "" {
			return sid
		}
		if did, ok := m["device_id"].(string); ok && did != "" {
			return did
		}
	}
	return ""
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

// extractModel 从请求体中提取模型名称
func extractModel(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}
	return req.Model
}

// stripFields 从 JSON body 中剔除指定字段路径
// 支持路径格式:
//   - "betas"                         → 删除顶层 betas 字段
//   - "system.*.cache_control.scope"  → 删除 system 数组每个元素的 cache_control.scope
//   - "*" 匹配数组中的每个元素或对象中的每个 key
func stripFields(body []byte, paths []string) []byte {
	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		return body
	}
	for _, path := range paths {
		parts := strings.Split(path, ".")
		stripPath(data, parts)
	}
	out, err := json.Marshal(data)
	if err != nil {
		return body
	}
	return out
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
