package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"ai-gateway/model"

	"github.com/gin-gonic/gin"
)

// openaiError 返回 OpenAI 规范格式的错误响应
func openaiError(c *gin.Context, statusCode int, errType string, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}

// OpenAIUsage OpenAI 格式的 token 用量
type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"-"` // 从 prompt_tokens_details.cached_tokens 提取
	ReasoningTokens  int `json:"-"` // 从 completion_tokens_details.reasoning_tokens 提取
	CacheWriteTokens int `json:"-"` // 跨协议时从 Anthropic cache_creation_input_tokens 提取
}

func (h *Handler) proxyOpenAI(c *gin.Context) {
	startTime := time.Now()

	// 通过 API Key 匹配分组
	apiKey := ""
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		apiKey = strings.TrimPrefix(auth, "Bearer ")
	}
	if apiKey == "" {
		apiKey = c.GetHeader("X-Api-Key")
	}
	if apiKey == "" {
		openaiError(c, http.StatusUnauthorized, "invalid_api_key", "missing api key")
		return
	}

	cfg, group := h.store.FindGroupByKey(apiKey)
	if group == nil {
		openaiError(c, http.StatusUnauthorized, "invalid_api_key", "invalid api key")
		return
	}

	// 协议校验：如果分组配置了协议列表，需包含 "openai"
	if len(group.Protocols) > 0 {
		allowed := false
		for _, p := range group.Protocols {
			if p == "openai" {
				allowed = true
				break
			}
		}
		if !allowed {
			openaiError(c, http.StatusForbidden, "invalid_request_error", "openai protocol is not allowed in this group")
			return
		}
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}

	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}

	reqModel, _ := parsed["model"].(string)

	// 分组模型校验
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
			openaiError(c, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("model %q is not allowed in this group, allowed models: %v", reqModel, group.Models))
			return
		}
	}

	// 模型映射
	var originalModel string
	bodyModified := false
	if len(group.ModelMapping) > 0 && reqModel != "" {
		if mapped, ok := group.ModelMapping[reqModel]; ok {
			originalModel = reqModel
			parsed["model"] = mapped
			reqModel = mapped
			bodyModified = true
		}
	}

	// 请求模式校验
	reqStream, _ := parsed["stream"].(bool)
	if reqStream {
		if group.AllowStream != nil && !*group.AllowStream {
			openaiError(c, http.StatusBadRequest, "invalid_request_error", "streaming is not allowed in this group")
			return
		}
	} else {
		allowNonStream := group.AllowNonStream != nil && *group.AllowNonStream
		if !allowNonStream {
			openaiError(c, http.StatusBadRequest, "invalid_request_error", "non-streaming is not allowed in this group")
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
		bodyModified = true
	}

	// 字段注入（分组优先，全局兜底）
	injectFieldsCfg := group.InjectFields
	if len(injectFieldsCfg) == 0 {
		injectFieldsCfg = cfg.InjectFields
	}
	if len(injectFieldsCfg) > 0 {
		for _, f := range injectFieldsCfg {
			parts := strings.Split(f.Path, ".")
			injectPath(parsed, parts, f.Value)
		}
		bodyModified = true
	}

	// 仅在 body 被修改时重新序列化，否则保留原始字节
	if bodyModified {
		body, err = orderedMarshal(parsed)
		if err != nil {
			openaiError(c, http.StatusBadRequest, "invalid_request_error", "failed to marshal request body")
			return
		}
	}

	// 会话亲和：OpenAI 从顶层 user 字段提取
	sessionKey, _ := parsed["user"].(string)

	logEnabled := h.shouldLog(group, sessionKey)
	logErrorOnly := group.LogMode == "error_only"
	requestID := c.GetHeader("X-Request-Log-Id")

	// Header 剔除/注入规则（分组优先，全局兜底），存入 context 供 doOpenAIRequest 使用
	stripHeadersCfg := group.StripHeaders
	if len(stripHeadersCfg) == 0 {
		stripHeadersCfg = cfg.StripHeaders
	}
	injectHeadersCfg := group.InjectHeaders
	if len(injectHeadersCfg) == 0 {
		injectHeadersCfg = cfg.InjectHeaders
	}

	// 模拟 Claude Code 客户端特征：注入 anthropic-beta header（跨协议到 anthropic 时需要）
	if group.SimulateCC {
		c.Set("_simulate_cc", true)
	}

	if len(stripHeadersCfg) > 0 || len(injectHeadersCfg) > 0 {
		if stripHeadersCfg == nil {
			stripHeadersCfg = []string{}
		}
		if injectHeadersCfg == nil {
			injectHeadersCfg = map[string]string{}
		}
		c.Set("_strip_headers", stripHeadersCfg)
		c.Set("_inject_headers", injectHeadersCfg)
	}

	// 提取请求头用于日志记录（应用相同的剔除规则）
	reqHeaders := extractRequestHeadersWithRules(c, stripHeadersCfg, injectHeadersCfg)

	maxRetries := h.balancer.ActiveCount(cfg, group.ID)
	if maxRetries == 0 {
		if logEnabled {
			h.addLog(RequestLog{
				Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
				SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
				Status: 502, Duration: time.Since(startTime).Milliseconds(),
				Action: "error", Detail: "no active upstream",
				Stream: reqStream, RequestHeaders: reqHeaders, RequestBody: string(body),
			})
		}
		openaiError(c, http.StatusBadGateway, "server_error", "no active upstream")
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

	retryCount := make(map[int]int)
	excluded := make(map[string]bool)
	for attempt := 0; attempt < maxRetries; attempt++ {
		upstream := h.balancer.Pick(cfg, group.ID, sessionKey, sessionKey, reqModel, excluded)
		if upstream == nil {
			break
		}
		excluded[upstream.ID] = true

		h.balancer.IncLoad(upstream.ID)
		attemptStart := time.Now()

		// 跨协议转换：如果上游是 Anthropic 协议，需要转换请求体
		upstreamProtocol := upstream.EffectiveProtocol()
		crossProtocol := upstreamProtocol == "anthropic"
		var sendBody []byte
		if crossProtocol {
			converted := convertOpenAIRequestToAnthropic(parsed)
			// 跨协议转发到 Anthropic 上游时，注入 CC 特征
			if group.SimulateCC {
				injectCCBody(converted)
			}
			var marshalErr error
			sendBody, marshalErr = json.Marshal(converted)
			if marshalErr != nil {
				h.balancer.DecLoad(upstream.ID)
				log.Printf("[proxy] upstream %s (%s) protocol conversion marshal error: %v", upstream.ID, upstream.Remark, marshalErr)
				continue
			}
		} else {
			sendBody = body
		}

		var resp *http.Response
		if crossProtocol {
			resp, err = h.doRequestWithProtocol(c, upstream, sendBody, "anthropic")
		} else {
			resp, err = h.doOpenAIRequest(c, upstream, sendBody)
		}
		// 使用实际转发给上游的请求头（含 CC 模拟注入等）
		if fh, ok := c.Get("_forwarded_headers"); ok {
			reqHeaders = fh.(map[string]string)
		}
		ttfb := time.Since(attemptStart).Milliseconds()
		h.balancer.DecLoad(upstream.ID)

		if err != nil {
			log.Printf("[proxy] upstream %s (%s) openai request error: %v", upstream.ID, upstream.Remark, err)
			if logEnabled {
				h.addLog(RequestLog{
					Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
					SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
					UpstreamID: upstream.ID, Remark: upstream.Remark,
					Status: 0, Duration: time.Since(startTime).Milliseconds(),
					Action: "failover", Detail: fmt.Sprintf("连接错误: %v", err), RetryCount: attempt,
					Stream: reqStream, RequestHeaders: reqHeaders, RequestBody: string(body),
				})
			}
			if c.Request.Context().Err() != nil {
				return
			}
			continue
		}

		statusCode := resp.StatusCode

		// 检查是否命中故障转移规则
		if rule, hit := ruleMap[statusCode]; hit {
			errReader := io.ReadCloser(resp.Body)
			if enc := strings.ToLower(resp.Header.Get("Content-Encoding")); enc != "" {
				if dr, err := newDecompressReader(enc, resp.Body); err == nil {
					errReader = dr
				}
			}
			errBody, _ := io.ReadAll(errReader)
			errReader.Close()
			// 跨协议时将错误体转换为 OpenAI 格式
			if crossProtocol {
				errBody = convertAnthropicErrorToOpenAI(errBody, statusCode)
			}
			errBodyStr := string(errBody)
			log.Printf("[proxy] upstream %s (%s) openai returned %d, action=%s, body: %s", upstream.ID, upstream.Remark, statusCode, rule.Action, errBodyStr)
			detail := fmt.Sprintf("HTTP %d → %s", statusCode, rule.Action)
			switch rule.Action {
			case "retry", "retry_other":
				maxRetry := rule.Retries
				if maxRetry <= 0 {
					maxRetry = 1
				}
				retryCount[statusCode]++
				if retryCount[statusCode] > maxRetry {
					detail = fmt.Sprintf("HTTP %d → retry exhausted (%d/%d)", statusCode, retryCount[statusCode]-1, maxRetry)
					if logEnabled {
						h.addLog(RequestLog{
							Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
							SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: reqModel,
							UpstreamID: upstream.ID, Remark: upstream.Remark,
							Status: statusCode, Duration: time.Since(startTime).Milliseconds(),
							Action: "error", Detail: detail, RetryCount: attempt,
							Stream: reqStream, RequestHeaders: reqHeaders, RequestBody: string(body), ResponseBody: errBodyStr,
						})
					}
					resp.Body.Close()
					c.Data(statusCode, "application/json", errBody)
					return
				}
				actionLabel := "retry"
				if rule.Action == "retry" {
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
					Stream: reqStream, RequestHeaders: reqHeaders, RequestBody: string(body), ResponseBody: errBodyStr,
				})
			}
			resp.Body.Close()
			continue
		}

		// 错误码映射
		mappings := group.ErrorMappings
		if len(mappings) == 0 {
			mappings = cfg.ErrorMappings
		}

		var usage OpenAIUsage
		var respBodyBytes []byte

		if crossProtocol {
			// 上游是 Anthropic 协议，需要将响应转换为 OpenAI 格式
			statusCode, respBody := h.applyErrorMapping(mappings, statusCode, resp)
			if reqStream && isSSE(resp.Header) {
				// 流式：Anthropic SSE → OpenAI SSE
				usage, respBodyBytes = convertAnthropicSSEToOpenAI(c, statusCode, resp.Header, respBody, originalModel, group.NoCache)
			} else {
				// 非流式：读取 Anthropic 响应，转换为 OpenAI 格式
				defer respBody.Close()
				contentEncoding := strings.ToLower(resp.Header.Get("Content-Encoding"))
				decodedBody := respBody
				if contentEncoding != "" {
					if r, decErr := newDecompressReader(contentEncoding, respBody); decErr == nil {
						decodedBody = r
						resp.Header.Del("Content-Encoding")
					}
				}
				data, _ := io.ReadAll(decodedBody)
				var converted []byte
				if statusCode < 400 {
					converted, usage = convertAnthropicResponseToOpenAI(data)
				} else {
					converted = convertAnthropicErrorToOpenAI(data, statusCode)
				}
				if originalModel != "" {
					converted = replaceModelInResponse(converted, originalModel)
				}
				for key, vals := range resp.Header {
					if strings.ToLower(key) == "content-length" {
						continue
					}
					for _, v := range vals {
						c.Writer.Header().Add(key, v)
					}
				}
				c.Writer.Header().Set("Content-Type", "application/json")
				c.Writer.WriteHeader(statusCode)
				c.Writer.Write(converted)
				respBodyBytes = converted
			}
		} else {
			// 上游是 OpenAI 协议，直接透传
			statusCode, respBody := h.applyOpenAIErrorMapping(mappings, statusCode, resp)
			usage, respBodyBytes = h.writeOpenAIResponseWithUsage(c, statusCode, resp.Header, respBody, originalModel)
		}

		logModel := reqModel
		if originalModel != "" {
			logModel = originalModel
		}
		logEntry := RequestLog{
			Time: startTime.Unix(), GroupID: group.ID, GroupName: group.Name,
			SessionKey: sessionKey, ClientIP: c.ClientIP(), RequestID: requestID, Model: logModel,
			UpstreamID: upstream.ID, Remark: upstream.Remark,
			Status: statusCode, Duration: time.Since(startTime).Milliseconds(), TTFBMs: ttfb,
			Action: "success", RetryCount: attempt, Stream: reqStream,
			InputTokens: usage.PromptTokens - usage.CachedTokens - usage.CacheWriteTokens, OutputTokens: usage.CompletionTokens,
			CacheReadTokens: usage.CachedTokens, CacheWriteTokens: usage.CacheWriteTokens,
			ReasoningTokens: usage.ReasoningTokens,
			RequestHeaders: reqHeaders,
			RequestBody: string(body), ResponseBody: string(respBodyBytes),
		}
		if statusCode >= 400 {
			logEntry.Action = "error"
			logEntry.Detail = fmt.Sprintf("HTTP %d", statusCode)
			log.Printf("[proxy] upstream %s (%s) openai returned %d (not in failover rules), body: %s", upstream.ID, upstream.Remark, statusCode, string(respBodyBytes))
		}
		if logEnabled && !(logErrorOnly && logEntry.Action == "success") {
			h.addLog(logEntry)
		}
		if logEntry.Action == "success" {
			costLog := logEntry
			costLog.Model = reqModel
			cost := calcCost(costLog, h.pricingMapFrom(cfg))
			h.addSiteCost(cost)
			h.addUpstreamCost(upstream.ID, group.ID, cost, logEntry.Duration, logEntry.TTFBMs)
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
			Stream: reqStream, RequestHeaders: reqHeaders, RequestBody: string(body),
		})
	}
	openaiError(c, http.StatusBadGateway, "server_error", "all upstreams failed")
}

func (h *Handler) doOpenAIRequest(c *gin.Context, upstream *model.Upstream, body []byte) (*http.Response, error) {
	base := strings.TrimRight(upstream.API, "/")
	url := base + "/v1/chat/completions"
	if strings.HasSuffix(base, "/v1/chat/completions") {
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
	// OpenAI 只用 Authorization: Bearer
	req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	req.Header.Del("X-Api-Key")
	req.Header.Set("Content-Type", "application/json")

	// 应用 header 剔除/注入规则
	if strip, ok := c.Get("_strip_headers"); ok {
		if inject, ok2 := c.Get("_inject_headers"); ok2 {
			applyHeaderRules(req, strip.([]string), inject.(map[string]string))
		}
	}

	// 保存实际转发的请求头供日志使用
	saveForwardedHeaders(c, req)

	return h.client.Do(req)
}

func (h *Handler) applyOpenAIErrorMapping(errorMappings []model.ErrorMapping, statusCode int, resp *http.Response) (int, io.ReadCloser) {
	for _, m := range errorMappings {
		if m.SourceCode == statusCode {
			if m.Message != "" {
				resp.Body.Close()
				errBody := map[string]any{
					"error": map[string]any{
						"message": m.Message,
						"type":    httpStatusToOpenAIError(m.TargetCode),
						"param":   nil,
						"code":    nil,
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

func httpStatusToOpenAIError(code int) string {
	switch code {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

func (h *Handler) writeOpenAIResponseWithUsage(c *gin.Context, statusCode int, header http.Header, body io.ReadCloser, originalModel string) (OpenAIUsage, []byte) {
	defer body.Close()

	needModelReplace := originalModel != ""

	// 解压响应体
	contentEncoding := strings.ToLower(header.Get("Content-Encoding"))
	decodedBody := body
	if contentEncoding != "" {
		if r, err := newDecompressReader(contentEncoding, body); err == nil {
			decodedBody = r
			header.Del("Content-Encoding")
		}
	}

	for key, vals := range header {
		if strings.ToLower(key) == "content-length" {
			continue
		}
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Writer.WriteHeader(statusCode)

	var usage OpenAIUsage
	var captured []byte

	if isSSE(header) {
		flusher, ok := c.Writer.(http.Flusher)
		scanner := bufio.NewScanner(decodedBody)
		scanBuf := scanBufPool.Get().([]byte)
		defer scanBufPool.Put(scanBuf)
		scanner.Buffer(scanBuf, 256*1024)
		var sseBuf strings.Builder
		for scanner.Scan() {
			line := scanner.Text()
			// 模型映射：替换 SSE 中的模型名（跳过 [DONE]）
			if needModelReplace && strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
				line = replaceModelInOpenAISSELine(line, originalModel)
			}
			c.Writer.Write([]byte(line + "\n"))
			if ok {
				flusher.Flush()
			}
			sseBuf.WriteString(line)
			sseBuf.WriteByte('\n')
			// 提取 usage
			if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
				extractOpenAISSEUsage(line[6:], &usage)
			}
		}
		if sseBuf.Len() > 0 {
			captured = []byte(sseBuf.String())
		}
	} else {
		data, _ := io.ReadAll(decodedBody)
		if needModelReplace {
			data = replaceModelInResponse(data, originalModel)
		}
		c.Writer.Write(data)
		captured = data
		var resp struct {
			Usage OpenAIUsage `json:"usage"`
		}
		if json.Unmarshal(data, &resp) == nil {
			usage = resp.Usage
		}
	}
	return usage, captured
}

// extractOpenAISSEUsage 从 OpenAI SSE data 中提取 usage 信息
func extractOpenAISSEUsage(data string, usage *OpenAIUsage) {
	if !strings.Contains(data, `"usage"`) {
		return
	}
	var chunk struct {
		Usage *OpenAIUsage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return
	}
	if chunk.Usage != nil {
		*usage = *chunk.Usage
	}
}

// replaceModelInOpenAISSELine 替换 OpenAI SSE data 行中的 model 字段
func replaceModelInOpenAISSELine(line string, originalModel string) string {
	if !strings.HasPrefix(line, "data: ") {
		return line
	}
	data := line[6:]
	if !strings.Contains(data, `"model"`) {
		return line
	}
	var event map[string]any
	if json.Unmarshal([]byte(data), &event) != nil {
		return line
	}
	if _, ok := event["model"]; ok {
		event["model"] = originalModel
		out, err := json.Marshal(event)
		if err != nil {
			return line
		}
		return "data: " + string(out)
	}
	return line
}
