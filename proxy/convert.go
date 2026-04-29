package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ai-gateway/model"

	"github.com/gin-gonic/gin"
)

// ============================
// 请求转换：OpenAI → Anthropic
// ============================

// convertOpenAIRequestToAnthropic 将 OpenAI Chat Completions 请求体转换为 Anthropic Messages 格式
func convertOpenAIRequestToAnthropic(parsed map[string]any) map[string]any {
	result := make(map[string]any)

	// model
	if m, ok := parsed["model"]; ok {
		result["model"] = m
	}

	// messages → system + messages
	if msgs, ok := parsed["messages"].([]any); ok {
		system, anthropicMsgs := convertOpenAIMessagesToAnthropic(msgs)
		if system != "" {
			result["system"] = system
		}
		result["messages"] = anthropicMsgs
	}

	// max_tokens / max_completion_tokens
	if mt, ok := parsed["max_completion_tokens"]; ok {
		result["max_tokens"] = mt
	} else if mt, ok := parsed["max_tokens"]; ok {
		result["max_tokens"] = mt
	} else {
		result["max_tokens"] = 4096
	}

	// stream
	if s, ok := parsed["stream"]; ok {
		result["stream"] = s
	}

	// temperature
	if t, ok := parsed["temperature"]; ok {
		result["temperature"] = t
	}

	// top_p
	if tp, ok := parsed["top_p"]; ok {
		result["top_p"] = tp
	}

	// stop → stop_sequences
	if stop, ok := parsed["stop"]; ok {
		switch v := stop.(type) {
		case string:
			result["stop_sequences"] = []string{v}
		case []any:
			result["stop_sequences"] = v
		}
	}

	// tools
	if tools, ok := parsed["tools"].([]any); ok {
		result["tools"] = convertOpenAIToolsToAnthropic(tools)
	}

	// tool_choice
	if tc, ok := parsed["tool_choice"]; ok {
		result["tool_choice"] = convertOpenAIToolChoiceToAnthropic(tc)
	}

	// metadata (user → metadata.user_id)
	if user, ok := parsed["user"].(string); ok {
		result["metadata"] = map[string]any{"user_id": user}
	}

	return result
}

// convertOpenAIMessagesToAnthropic 转换消息数组，提取 system 消息
func convertOpenAIMessagesToAnthropic(msgs []any) (system string, result []any) {
	var systemParts []string
	var anthropicMsgs []any

	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)

		switch role {
		case "system", "developer":
			// system / developer 消息提取为顶层 system 参数
			if content, ok := m["content"].(string); ok {
				systemParts = append(systemParts, content)
			} else if parts, ok := m["content"].([]any); ok {
				for _, part := range parts {
					if p, ok := part.(map[string]any); ok {
						if text, ok := p["text"].(string); ok {
							systemParts = append(systemParts, text)
						}
					}
				}
			}

		case "user":
			anthropicMsgs = append(anthropicMsgs, map[string]any{
				"role":    "user",
				"content": convertOpenAIContentToAnthropic(m["content"]),
			})

		case "assistant":
			content := convertAssistantContentToAnthropic(m)
			anthropicMsgs = append(anthropicMsgs, map[string]any{
				"role":    "assistant",
				"content": content,
			})

		case "tool":
			// OpenAI tool result → Anthropic tool_result
			toolCallID, _ := m["tool_call_id"].(string)
			contentStr, _ := m["content"].(string)
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolCallID,
				"content":     contentStr,
			}
			// 合并到前一个 user 消息或创建新的
			if len(anthropicMsgs) > 0 {
				last := anthropicMsgs[len(anthropicMsgs)-1].(map[string]any)
				if last["role"] == "user" {
					// 追加到已有 user 消息的 content 数组
					if arr, ok := last["content"].([]any); ok {
						last["content"] = append(arr, block)
					} else {
						last["content"] = []any{
							map[string]any{"type": "text", "text": last["content"]},
							block,
						}
					}
					continue
				}
			}
			anthropicMsgs = append(anthropicMsgs, map[string]any{
				"role":    "user",
				"content": []any{block},
			})
		}
	}

	return strings.Join(systemParts, "\n"), anthropicMsgs
}

// convertOpenAIContentToAnthropic 转换 user 消息的 content
func convertOpenAIContentToAnthropic(content any) any {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var blocks []any
		for _, part := range v {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			partType, _ := p["type"].(string)
			switch partType {
			case "text":
				blocks = append(blocks, map[string]any{
					"type": "text",
					"text": p["text"],
				})
			case "image_url":
				if imgURL, ok := p["image_url"].(map[string]any); ok {
					url, _ := imgURL["url"].(string)
					if strings.HasPrefix(url, "data:") {
						// data URI → base64 source
						mediaType, b64Data := parseDataURI(url)
						blocks = append(blocks, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type":         "base64",
								"media_type":   mediaType,
								"data":         b64Data,
							},
						})
					} else {
						blocks = append(blocks, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "url",
								"url":  url,
							},
						})
					}
				}
			}
		}
		if len(blocks) == 0 {
			return ""
		}
		return blocks
	default:
		return ""
	}
}

// convertAssistantContentToAnthropic 转换 assistant 消息（含 tool_calls）
func convertAssistantContentToAnthropic(m map[string]any) []any {
	var blocks []any

	// 文本内容
	if content, ok := m["content"].(string); ok && content != "" {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": content,
		})
	}

	// tool_calls → tool_use
	if toolCalls, ok := m["tool_calls"].([]any); ok {
		for _, tc := range toolCalls {
			call, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			argsStr, _ := fn["arguments"].(string)
			var args any
			if json.Unmarshal([]byte(argsStr), &args) != nil {
				args = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    call["id"],
				"name":  name,
				"input": args,
			})
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{
			"type": "text",
			"text": "",
		})
	}
	return blocks
}

// convertOpenAIToolsToAnthropic 转换 tools 定义
func convertOpenAIToolsToAnthropic(tools []any) []any {
	var result []any
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] != "function" {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		anthropicTool := map[string]any{
			"name": fn["name"],
		}
		if desc, ok := fn["description"]; ok {
			anthropicTool["description"] = desc
		}
		if params, ok := fn["parameters"]; ok {
			anthropicTool["input_schema"] = params
		} else {
			anthropicTool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		result = append(result, anthropicTool)
	}
	return result
}

// convertOpenAIToolChoiceToAnthropic 转换 tool_choice
func convertOpenAIToolChoiceToAnthropic(tc any) any {
	switch v := tc.(type) {
	case string:
		switch v {
		case "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "auto"} // Anthropic 没有 none，用 auto 避免强制调用
		case "required":
			return map[string]any{"type": "any"}
		default:
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		if fn, ok := v["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		return map[string]any{"type": "auto"}
	default:
		return map[string]any{"type": "auto"}
	}
}

// ============================
// 请求转换：Anthropic → OpenAI
// ============================

// convertAnthropicRequestToOpenAI 将 Anthropic Messages 请求体转换为 OpenAI Chat Completions 格式
func convertAnthropicRequestToOpenAI(parsed map[string]any) map[string]any {
	result := make(map[string]any)

	// model
	if m, ok := parsed["model"]; ok {
		result["model"] = m
	}

	var openAIMsgs []any

	// system → system message
	if sys, ok := parsed["system"].(string); ok && sys != "" {
		openAIMsgs = append(openAIMsgs, map[string]any{
			"role":    "system",
			"content": sys,
		})
	} else if sysArr, ok := parsed["system"].([]any); ok {
		// Anthropic 支持 system 为数组格式
		var text string
		for _, s := range sysArr {
			if block, ok := s.(map[string]any); ok {
				if t, ok := block["text"].(string); ok {
					if text != "" {
						text += "\n"
					}
					text += t
				}
			}
		}
		if text != "" {
			openAIMsgs = append(openAIMsgs, map[string]any{
				"role":    "system",
				"content": text,
			})
		}
	}

	// messages
	if msgs, ok := parsed["messages"].([]any); ok {
		openAIMsgs = append(openAIMsgs, convertAnthropicMessagesToOpenAI(msgs)...)
	}
	result["messages"] = openAIMsgs

	// max_tokens → max_completion_tokens
	if mt, ok := parsed["max_tokens"]; ok {
		result["max_completion_tokens"] = mt
	}

	// stream
	if s, ok := parsed["stream"]; ok {
		result["stream"] = s
		// OpenAI 流式默认不返回 usage，需注入 stream_options 确保拿到 usage
		if sb, ok := s.(bool); ok && sb {
			result["stream_options"] = map[string]any{"include_usage": true}
		}
	}

	// temperature
	if t, ok := parsed["temperature"]; ok {
		result["temperature"] = t
	}

	// top_p
	if tp, ok := parsed["top_p"]; ok {
		result["top_p"] = tp
	}

	// stop_sequences → stop
	if ss, ok := parsed["stop_sequences"]; ok {
		result["stop"] = ss
	}

	// tools
	if tools, ok := parsed["tools"].([]any); ok {
		result["tools"] = convertAnthropicToolsToOpenAI(tools)
	}

	// tool_choice
	if tc, ok := parsed["tool_choice"]; ok {
		result["tool_choice"] = convertAnthropicToolChoiceToOpenAI(tc)
	}

	// metadata.user_id → user
	if meta, ok := parsed["metadata"].(map[string]any); ok {
		if uid, ok := meta["user_id"].(string); ok {
			result["user"] = uid
		}
	}

	return result
}

// convertAnthropicMessagesToOpenAI 转换 Anthropic 消息数组
func convertAnthropicMessagesToOpenAI(msgs []any) []any {
	var result []any
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)

		switch role {
		case "user":
			result = append(result, convertAnthropicUserMsgToOpenAI(m)...)
		case "assistant":
			result = append(result, convertAnthropicAssistantMsgToOpenAI(m)...)
		}
	}
	return result
}

// convertAnthropicUserMsgToOpenAI 转换 user 消息（可能含 tool_result）
func convertAnthropicUserMsgToOpenAI(m map[string]any) []any {
	content := m["content"]
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": v}}
	case []any:
		var userParts []any
		var toolResults []any
		for _, block := range v {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := b["type"].(string)
			switch blockType {
			case "tool_result":
				toolID, _ := b["tool_use_id"].(string)
				contentStr := ""
				if c, ok := b["content"].(string); ok {
					contentStr = c
				} else if cArr, ok := b["content"].([]any); ok {
					// 数组内容拼接文本
					var texts []string
					for _, ci := range cArr {
						if cb, ok := ci.(map[string]any); ok {
							if t, ok := cb["text"].(string); ok {
								texts = append(texts, t)
							}
						}
					}
					contentStr = strings.Join(texts, "\n")
				}
				toolResults = append(toolResults, map[string]any{
					"role":         "tool",
					"tool_call_id": toolID,
					"content":      contentStr,
				})
			case "text":
				userParts = append(userParts, map[string]any{
					"type": "text",
					"text": b["text"],
				})
			case "image":
				if src, ok := b["source"].(map[string]any); ok {
					srcType, _ := src["type"].(string)
					if srcType == "base64" {
						mediaType, _ := src["media_type"].(string)
						data, _ := src["data"].(string)
						userParts = append(userParts, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
							},
						})
					} else if srcType == "url" {
						url, _ := src["url"].(string)
						userParts = append(userParts, map[string]any{
							"type": "image_url",
							"image_url": map[string]any{
								"url": url,
							},
						})
					}
				}
			}
		}

		var result []any
		if len(userParts) > 0 {
			if len(userParts) == 1 {
				if p, ok := userParts[0].(map[string]any); ok && p["type"] == "text" {
					result = append(result, map[string]any{"role": "user", "content": p["text"]})
				} else {
					result = append(result, map[string]any{"role": "user", "content": userParts})
				}
			} else {
				result = append(result, map[string]any{"role": "user", "content": userParts})
			}
		}
		result = append(result, toolResults...)
		return result
	default:
		return []any{map[string]any{"role": "user", "content": ""}}
	}
}

// convertAnthropicAssistantMsgToOpenAI 转换 assistant 消息（含 tool_use）
func convertAnthropicAssistantMsgToOpenAI(m map[string]any) []any {
	content := m["content"]
	switch v := content.(type) {
	case string:
		return []any{map[string]any{"role": "assistant", "content": v}}
	case []any:
		msg := map[string]any{"role": "assistant"}
		var textParts []string
		var toolCalls []any
		for _, block := range v {
			b, ok := block.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := b["type"].(string)
			switch blockType {
			case "text":
				if t, ok := b["text"].(string); ok {
					textParts = append(textParts, t)
				}
			case "tool_use":
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				input := b["input"]
				argsBytes, _ := json.Marshal(input)
				toolCalls = append(toolCalls, map[string]any{
					"id":   id,
					"type": "function",
					"function": map[string]any{
						"name":      name,
						"arguments": string(argsBytes),
					},
				})
			case "thinking":
				// 保留为文本内容（带标记）
				if t, ok := b["thinking"].(string); ok && t != "" {
					textParts = append(textParts, t)
				}
			}
		}
		if len(textParts) > 0 {
			msg["content"] = strings.Join(textParts, "\n")
		} else {
			msg["content"] = nil
		}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		return []any{msg}
	default:
		return []any{map[string]any{"role": "assistant", "content": ""}}
	}
}

// convertAnthropicToolsToOpenAI 转换 tools 定义
func convertAnthropicToolsToOpenAI(tools []any) []any {
	var result []any
	for _, t := range tools {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn := map[string]any{
			"name": tool["name"],
		}
		if desc, ok := tool["description"]; ok {
			fn["description"] = desc
		}
		if schema, ok := tool["input_schema"]; ok {
			fn["parameters"] = schema
		}
		result = append(result, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return result
}

// convertAnthropicToolChoiceToOpenAI 转换 tool_choice
func convertAnthropicToolChoiceToOpenAI(tc any) any {
	switch v := tc.(type) {
	case map[string]any:
		tcType, _ := v["type"].(string)
		switch tcType {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "tool":
			name, _ := v["name"].(string)
			return map[string]any{
				"type":     "function",
				"function": map[string]any{"name": name},
			}
		}
	}
	return "auto"
}

// ============================
// 响应转换：Anthropic → OpenAI (非流式)
// ============================

// convertAnthropicResponseToOpenAI 将 Anthropic 非流式响应转换为 OpenAI 格式
func convertAnthropicResponseToOpenAI(data []byte) ([]byte, OpenAIUsage) {
	var resp map[string]any
	if json.Unmarshal(data, &resp) != nil {
		return data, OpenAIUsage{}
	}

	mdl, _ := resp["model"].(string)
	id, _ := resp["id"].(string)
	stopReason, _ := resp["stop_reason"].(string)

	// 转换 content
	content, _ := resp["content"].([]any)
	var textParts []string
	var toolCalls []any
	for _, block := range content {
		b, ok := block.(map[string]any)
		if !ok {
			continue
		}
		blockType, _ := b["type"].(string)
		switch blockType {
		case "text":
			if t, ok := b["text"].(string); ok {
				textParts = append(textParts, t)
			}
		case "thinking":
			if t, ok := b["thinking"].(string); ok && t != "" {
				textParts = append(textParts, t)
			}
		case "tool_use":
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			input := b["input"]
			argsBytes, _ := json.Marshal(input)
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(argsBytes),
				},
			})
		}
	}

	// 转换 usage：Anthropic input_tokens 不含缓存，OpenAI prompt_tokens 含缓存
	var usage OpenAIUsage
	if u, ok := resp["usage"].(map[string]any); ok {
		inputTokens := 0
		cacheRead := 0
		cacheWrite := 0
		if v, ok := u["input_tokens"].(float64); ok {
			inputTokens = int(v)
		}
		if v, ok := u["cache_read_input_tokens"].(float64); ok {
			cacheRead = int(v)
		}
		if v, ok := u["cache_creation_input_tokens"].(float64); ok {
			cacheWrite = int(v)
		}
		usage.PromptTokens = inputTokens + cacheRead + cacheWrite // OpenAI prompt_tokens = 总输入
		usage.CachedTokens = cacheRead
		usage.CacheWriteTokens = cacheWrite
		if v, ok := u["output_tokens"].(float64); ok {
			usage.CompletionTokens = int(v)
		}
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}

	textContent := strings.Join(textParts, "\n")
	choice := map[string]any{
		"index": 0,
		"message": map[string]any{
			"role":    "assistant",
			"content": textContent,
		},
		"finish_reason": convertAnthropicStopReason(stopReason),
	}
	if len(toolCalls) > 0 {
		choice["message"].(map[string]any)["tool_calls"] = toolCalls
	}

	usageMap := map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      usage.CachedTokens,
			"cache_write_tokens": usage.CacheWriteTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
	openAIResp := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   mdl,
		"choices": []any{choice},
		"usage":   usageMap,
	}

	out, err := json.Marshal(openAIResp)
	if err != nil {
		return data, usage
	}
	return out, usage
}

// ============================
// 响应转换：OpenAI → Anthropic (非流式)
// ============================

// convertOpenAIResponseToAnthropic 将 OpenAI 非流式响应转换为 Anthropic 格式
func convertOpenAIResponseToAnthropic(data []byte) ([]byte, Usage) {
	var resp map[string]any
	if json.Unmarshal(data, &resp) != nil {
		return data, Usage{}
	}

	mdl, _ := resp["model"].(string)
	id, _ := resp["id"].(string)

	// OpenAI prompt_tokens 含缓存，需拆分
	var usage Usage
	if u, ok := resp["usage"].(map[string]any); ok {
		promptTokens := 0
		cachedTokens := 0
		cacheWriteTokens := 0
		if v, ok := u["prompt_tokens"].(float64); ok {
			promptTokens = int(v)
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			usage.OutputTokens = int(v)
		}
		// 提取 cached_tokens 和 cache_write_tokens（OpenRouter）
		if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
			if v, ok := details["cached_tokens"].(float64); ok {
				cachedTokens = int(v)
			}
			if v, ok := details["cache_write_tokens"].(float64); ok {
				cacheWriteTokens = int(v)
			}
		}
		usage.InputTokens = promptTokens - cachedTokens - cacheWriteTokens
		usage.CacheReadInputTokens = cachedTokens
		usage.CacheCreationInputTokens = cacheWriteTokens
	}

	// 提取 choice
	var contentBlocks []any
	var stopReason string
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)
		stopReason = convertOpenAIFinishReason(finishReason)

		if msg, ok := choice["message"].(map[string]any); ok {
			// 文本内容
			if content, ok := msg["content"].(string); ok && content != "" {
				contentBlocks = append(contentBlocks, map[string]any{
					"type": "text",
					"text": content,
				})
			}
			// tool_calls → tool_use
			if toolCalls, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range toolCalls {
					call, ok := tc.(map[string]any)
					if !ok {
						continue
					}
					callID, _ := call["id"].(string)
					fn, _ := call["function"].(map[string]any)
					if fn == nil {
						continue
					}
					name, _ := fn["name"].(string)
					argsStr, _ := fn["arguments"].(string)
					var input any
					if json.Unmarshal([]byte(argsStr), &input) != nil {
						input = map[string]any{}
					}
					contentBlocks = append(contentBlocks, map[string]any{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": input,
					})
				}
			}
		}
	}

	if len(contentBlocks) == 0 {
		contentBlocks = []any{map[string]any{"type": "text", "text": ""}}
	}

	usageMap := map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	}
	if usage.CacheReadInputTokens > 0 {
		usageMap["cache_read_input_tokens"] = usage.CacheReadInputTokens
	}
	if usage.CacheCreationInputTokens > 0 {
		usageMap["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
	}

	anthropicResp := map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       mdl,
		"content":     contentBlocks,
		"stop_reason": stopReason,
		"usage":       usageMap,
	}

	out, err := json.Marshal(anthropicResp)
	if err != nil {
		return data, usage
	}
	return out, usage
}

// ============================
// SSE 流式转换
// ============================

// convertAnthropicSSEToOpenAI 将 Anthropic SSE 流转换为 OpenAI SSE 流
// 返回转换后的 response body、提取的 usage
func convertAnthropicSSEToOpenAI(c *gin.Context, statusCode int, header http.Header, body io.ReadCloser, originalModel string, noCache bool) (OpenAIUsage, []byte) {
	defer body.Close()

	// 解压
	contentEncoding := strings.ToLower(header.Get("Content-Encoding"))
	decodedBody := io.Reader(body)
	if contentEncoding != "" {
		if r, err := newDecompressReader(contentEncoding, body); err == nil {
			decodedBody = r
			header.Del("Content-Encoding")
		}
	}

	// 写响应头
	for key, vals := range header {
		k := strings.ToLower(key)
		if k == "content-length" || k == "content-type" {
			continue
		}
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(statusCode)

	flusher, hasFlusher := c.Writer.(http.Flusher)
	scanner := bufio.NewScanner(decodedBody)
	scanBuf := scanBufPool.Get().([]byte)
	defer scanBufPool.Put(scanBuf)
	scanner.Buffer(scanBuf, 256*1024)

	var usage OpenAIUsage
	var captured strings.Builder
	var msgID string
	var mdl string
	contentBlockIdx := 0
	toolCallIdx := -1 // OpenAI tool_calls index，独立于 Anthropic content_block index
	// Anthropic content_block index → OpenAI tool_calls index 映射
	blockToToolIdx := make(map[int]int)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			// 透传 event: 行和空行
			if strings.HasPrefix(line, "event: ") || line == "" {
				// 不写 Anthropic 的 event: 行到 OpenAI 流，只传 data:
			}
			continue
		}

		data := line[6:]
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		eventType, _ := event["type"].(string)

		switch eventType {
		case "message_start":
			// 提取 message id 和 model
			if msg, ok := event["message"].(map[string]any); ok {
				msgID, _ = msg["id"].(string)
				mdl, _ = msg["model"].(string)
				if originalModel != "" {
					mdl = originalModel
				}
				// 提取 input usage：Anthropic input_tokens 不含缓存
				if u, ok := msg["usage"].(map[string]any); ok {
					inputTokens := 0
					cacheRead := 0
					cacheWrite := 0
					if v, ok := u["input_tokens"].(float64); ok {
						inputTokens = int(v)
					}
					if v, ok := u["cache_read_input_tokens"].(float64); ok {
						cacheRead = int(v)
					}
					if v, ok := u["cache_creation_input_tokens"].(float64); ok {
						cacheWrite = int(v)
					}
					usage.PromptTokens = inputTokens + cacheRead + cacheWrite
					usage.CachedTokens = cacheRead
					usage.CacheWriteTokens = cacheWrite
				}
			}
			// 发送初始 chunk
			chunk := buildOpenAIChunk(msgID, mdl, 0, map[string]any{"role": "assistant", "content": ""}, nil)
			writeSSEChunk(c.Writer, chunk)
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("data: " + chunk + "\n")

		case "content_block_start":
			if cb, ok := event["content_block"].(map[string]any); ok {
				cbType, _ := cb["type"].(string)
				idx := int(0)
				if i, ok := event["index"].(float64); ok {
					idx = int(i)
				}
				contentBlockIdx = idx
				if cbType == "tool_use" {
					// 开始一个 tool call，OpenAI tool_calls index 从 0 递增
					toolCallIdx++
					blockToToolIdx[idx] = toolCallIdx
					id, _ := cb["id"].(string)
					name, _ := cb["name"].(string)
					tc := map[string]any{
						"index": toolCallIdx,
						"id":    id,
						"type":  "function",
						"function": map[string]any{
							"name":      name,
							"arguments": "",
						},
					}
					chunk := buildOpenAIChunk(msgID, mdl, 0, map[string]any{"tool_calls": []any{tc}}, nil)
					writeSSEChunk(c.Writer, chunk)
					if hasFlusher {
						flusher.Flush()
					}
					captured.WriteString("data: " + chunk + "\n")
				}
			}

		case "content_block_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				deltaType, _ := delta["type"].(string)
				switch deltaType {
				case "text_delta":
					text, _ := delta["text"].(string)
					chunk := buildOpenAIChunk(msgID, mdl, 0, map[string]any{"content": text}, nil)
					writeSSEChunk(c.Writer, chunk)
					if hasFlusher {
						flusher.Flush()
					}
					captured.WriteString("data: " + chunk + "\n")
				case "thinking_delta":
					// thinking 内容作为普通文本透传给 OpenAI 客户端
					text, _ := delta["thinking"].(string)
					if text != "" {
						chunk := buildOpenAIChunk(msgID, mdl, 0, map[string]any{"content": text}, nil)
						writeSSEChunk(c.Writer, chunk)
						if hasFlusher {
							flusher.Flush()
						}
						captured.WriteString("data: " + chunk + "\n")
					}
				case "input_json_delta":
					partial, _ := delta["partial_json"].(string)
					tcIdx := blockToToolIdx[contentBlockIdx] // 映射到 OpenAI tool_calls index
					tc := map[string]any{
						"index": tcIdx,
						"function": map[string]any{
							"arguments": partial,
						},
					}
					chunk := buildOpenAIChunk(msgID, mdl, 0, map[string]any{"tool_calls": []any{tc}}, nil)
					writeSSEChunk(c.Writer, chunk)
					if hasFlusher {
						flusher.Flush()
					}
					captured.WriteString("data: " + chunk + "\n")
				}
			}

		case "message_delta":
			if u, ok := event["usage"].(map[string]any); ok {
				if v, ok := u["output_tokens"].(float64); ok {
					usage.CompletionTokens = int(v)
				}
			}
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
			stopReason := ""
			if d, ok := event["delta"].(map[string]any); ok {
				if sr, ok := d["stop_reason"].(string); ok {
					stopReason = convertAnthropicStopReason(sr)
				}
			}
			// NoCache: 清零缓存详情字段，隐藏缓存信息
			respUsage := usage
			if noCache {
				respUsage.CachedTokens = 0
				respUsage.CacheWriteTokens = 0
			}
			chunk := buildOpenAIChunkWithFinish(msgID, mdl, 0, stopReason, &respUsage)
			writeSSEChunk(c.Writer, chunk)
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("data: " + chunk + "\n")

		case "message_stop":
			c.Writer.Write([]byte("data: [DONE]\n\n"))
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("data: [DONE]\n")

		case "error":
			// Anthropic 流式错误 → 作为 OpenAI error chunk 转发
			errMsg := ""
			if errObj, ok := event["error"].(map[string]any); ok {
				errMsg, _ = errObj["message"].(string)
			}
			errChunk := map[string]any{
				"error": map[string]any{
					"message": errMsg,
					"type":    "server_error",
				},
			}
			errJSON, _ := json.Marshal(errChunk)
			writeSSEChunk(c.Writer, string(errJSON))
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("data: " + string(errJSON) + "\n")
		}
	}

	return usage, []byte(captured.String())
}

// convertOpenAISSEToAnthropic 将 OpenAI SSE 流转换为 Anthropic SSE 流
func convertOpenAISSEToAnthropic(c *gin.Context, statusCode int, header http.Header, body io.ReadCloser, originalModel string, noCache bool) (Usage, []byte) {
	defer body.Close()

	// 解压
	contentEncoding := strings.ToLower(header.Get("Content-Encoding"))
	decodedBody := io.Reader(body)
	if contentEncoding != "" {
		if r, err := newDecompressReader(contentEncoding, body); err == nil {
			decodedBody = r
			header.Del("Content-Encoding")
		}
	}

	// 写响应头
	for key, vals := range header {
		k := strings.ToLower(key)
		if k == "content-length" || k == "content-type" {
			continue
		}
		for _, v := range vals {
			c.Writer.Header().Add(key, v)
		}
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.WriteHeader(statusCode)

	flusher, hasFlusher := c.Writer.(http.Flusher)
	scanner := bufio.NewScanner(decodedBody)
	scanBuf := scanBufPool.Get().([]byte)
	defer scanBufPool.Put(scanBuf)
	scanner.Buffer(scanBuf, 256*1024)

	var usage Usage
	var captured strings.Builder
	msgStartSent := false
	contentBlockStarted := false
	var msgID string
	mdl := originalModel
	var pendingStopReason string // 缓存 stop_reason，延迟到 [DONE] 时发送 message_delta

	// 收集 tool call 状态
	type toolCallState struct {
		id   string
		name string
		args strings.Builder
	}
	toolCalls := make(map[int]*toolCallState)
	currentToolIdx := -1

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			// 发送 message_delta（包含 stop_reason 和 accumulated usage）
			if pendingStopReason != "" {
				deltaUsage := map[string]any{
					"output_tokens": usage.OutputTokens,
				}
				if usage.InputTokens > 0 || usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
					deltaUsage["input_tokens"] = usage.InputTokens
				}
				if noCache {
					if usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
						deltaUsage["input_tokens"] = usage.InputTokens + usage.CacheReadInputTokens
						if usage.CacheCreationInputTokens > 0 {
							deltaUsage["output_tokens"] = usage.OutputTokens + usage.CacheCreationInputTokens
						}
					}
				} else {
					if usage.CacheReadInputTokens > 0 {
						deltaUsage["cache_read_input_tokens"] = usage.CacheReadInputTokens
					}
					if usage.CacheCreationInputTokens > 0 {
						deltaUsage["cache_creation_input_tokens"] = usage.CacheCreationInputTokens
					}
				}
				msgDelta := map[string]any{
					"type": "message_delta",
					"delta": map[string]any{
						"stop_reason": pendingStopReason,
					},
					"usage": deltaUsage,
				}
				mdJSON, _ := json.Marshal(msgDelta)
				writeAnthropicSSE(c.Writer, "message_delta", string(mdJSON))
				if hasFlusher {
					flusher.Flush()
				}
				captured.WriteString("event: message_delta\ndata: " + string(mdJSON) + "\n\n")
			}
			// 发送 message_stop
			writeAnthropicSSE(c.Writer, "message_stop", `{}`)
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("event: message_stop\ndata: {}\n\n")
			continue
		}

		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}

		if msgID == "" {
			if id, ok := chunk["id"].(string); ok {
				msgID = id
			}
		}
		if m, ok := chunk["model"].(string); ok && mdl == "" {
			mdl = m
		}

		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			// usage-only chunk（OpenAI stream_options 返回的独立 usage chunk）
			extractOpenAIChunkUsage(chunk, &usage)
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}

		// 发送 message_start（仅一次）
		if !msgStartSent {
			msgStart := map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":      msgID,
					"type":    "message",
					"role":    "assistant",
					"model":   mdl,
					"content": []any{},
					"usage": map[string]any{
						"input_tokens":  0,
						"output_tokens": 0,
					},
				},
			}
			msgStartJSON, _ := json.Marshal(msgStart)
			writeAnthropicSSE(c.Writer, "message_start", string(msgStartJSON))
			if hasFlusher {
				flusher.Flush()
			}
			captured.WriteString("event: message_start\ndata: " + string(msgStartJSON) + "\n\n")
			msgStartSent = true
		}

		delta, _ := choice["delta"].(map[string]any)
		finishReason, _ := choice["finish_reason"].(string)

		if delta != nil {
			// 文本内容
			if content, ok := delta["content"].(string); ok && content != "" {
				if !contentBlockStarted {
					// content_block_start
					blockStart := map[string]any{
						"type":  "content_block_start",
						"index": 0,
						"content_block": map[string]any{
							"type": "text",
							"text": "",
						},
					}
					bsJSON, _ := json.Marshal(blockStart)
					writeAnthropicSSE(c.Writer, "content_block_start", string(bsJSON))
					if hasFlusher {
						flusher.Flush()
					}
					captured.WriteString("event: content_block_start\ndata: " + string(bsJSON) + "\n\n")
					contentBlockStarted = true
				}
				// content_block_delta
				blockDelta := map[string]any{
					"type":  "content_block_delta",
					"index": 0,
					"delta": map[string]any{
						"type": "text_delta",
						"text": content,
					},
				}
				bdJSON, _ := json.Marshal(blockDelta)
				writeAnthropicSSE(c.Writer, "content_block_delta", string(bdJSON))
				if hasFlusher {
					flusher.Flush()
				}
				captured.WriteString("event: content_block_delta\ndata: " + string(bdJSON) + "\n\n")
			}

			// tool_calls
			if tcs, ok := delta["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					tcMap, _ := tc.(map[string]any)
					if tcMap == nil {
						continue
					}
					idx := 0
					if i, ok := tcMap["index"].(float64); ok {
						idx = int(i)
					}

					// 关闭前一个文本 block
					if contentBlockStarted && currentToolIdx < 0 {
						blockStop := map[string]any{"type": "content_block_stop", "index": 0}
						bsJSON, _ := json.Marshal(blockStop)
						writeAnthropicSSE(c.Writer, "content_block_stop", string(bsJSON))
						if hasFlusher {
							flusher.Flush()
						}
						captured.WriteString("event: content_block_stop\ndata: " + string(bsJSON) + "\n\n")
					}

					state := toolCalls[idx]
					if state == nil {
						state = &toolCallState{}
						toolCalls[idx] = state
					}

					if id, ok := tcMap["id"].(string); ok && id != "" {
						state.id = id
					}
					if fn, ok := tcMap["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok && name != "" {
							state.name = name
						}
						if args, ok := fn["arguments"].(string); ok {
							state.args.WriteString(args)
						}
					}

					// 第一次看到这个 tool call，先关闭前一个 tool block，再发送 content_block_start
					if currentToolIdx < idx {
						// 关闭前一个 tool block（如果有）
						if currentToolIdx >= 0 {
							prevAnthropicIdx := currentToolIdx + 1
							if !contentBlockStarted {
								prevAnthropicIdx = currentToolIdx
							}
							blockStop := map[string]any{"type": "content_block_stop", "index": prevAnthropicIdx}
							bsJSON, _ := json.Marshal(blockStop)
							writeAnthropicSSE(c.Writer, "content_block_stop", string(bsJSON))
							if hasFlusher {
								flusher.Flush()
							}
							captured.WriteString("event: content_block_stop\ndata: " + string(bsJSON) + "\n\n")
						}
						anthropicIdx := idx + 1 // 文本 block 占了 index 0
						if !contentBlockStarted {
							anthropicIdx = idx
						}
						blockStart := map[string]any{
							"type":  "content_block_start",
							"index": anthropicIdx,
							"content_block": map[string]any{
								"type":  "tool_use",
								"id":    state.id,
								"name":  state.name,
								"input": map[string]any{},
							},
						}
						bsJSON, _ := json.Marshal(blockStart)
						writeAnthropicSSE(c.Writer, "content_block_start", string(bsJSON))
						if hasFlusher {
							flusher.Flush()
						}
						captured.WriteString("event: content_block_start\ndata: " + string(bsJSON) + "\n\n")
						currentToolIdx = idx
					}

					// 发送参数增量
					if fn, ok := tcMap["function"].(map[string]any); ok {
						if args, ok := fn["arguments"].(string); ok && args != "" {
							anthropicIdx := idx + 1
							if !contentBlockStarted {
								anthropicIdx = idx
							}
							blockDelta := map[string]any{
								"type":  "content_block_delta",
								"index": anthropicIdx,
								"delta": map[string]any{
									"type":         "input_json_delta",
									"partial_json": args,
								},
							}
							bdJSON, _ := json.Marshal(blockDelta)
							writeAnthropicSSE(c.Writer, "content_block_delta", string(bdJSON))
							if hasFlusher {
								flusher.Flush()
							}
							captured.WriteString("event: content_block_delta\ndata: " + string(bdJSON) + "\n\n")
						}
					}
				}
			}
		}

		// finish_reason → 关闭 content blocks，缓存 stop_reason（message_delta 延迟到 [DONE] 时发送）
		if finishReason != "" {
			// 先关闭当前 content block
			if contentBlockStarted || currentToolIdx >= 0 {
				closeIdx := 0
				if currentToolIdx >= 0 {
					closeIdx = currentToolIdx + 1
					if !contentBlockStarted {
						closeIdx = currentToolIdx
					}
				}
				blockStop := map[string]any{"type": "content_block_stop", "index": closeIdx}
				bsJSON, _ := json.Marshal(blockStop)
				writeAnthropicSSE(c.Writer, "content_block_stop", string(bsJSON))
				if hasFlusher {
					flusher.Flush()
				}
				captured.WriteString("event: content_block_stop\ndata: " + string(bsJSON) + "\n\n")
			}

			// 提取 usage（如果和 finish_reason 在同一个 chunk 里）
			extractOpenAIChunkUsage(chunk, &usage)

			pendingStopReason = convertOpenAIFinishReason(finishReason)
		}
	}

	return usage, []byte(captured.String())
}

// ============================
// 辅助函数
// ============================

// convertAnthropicStopReason Anthropic stop_reason → OpenAI finish_reason
func convertAnthropicStopReason(sr string) string {
	switch sr {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence":
		return "stop"
	default:
		return "stop"
	}
}

// convertOpenAIFinishReason OpenAI finish_reason → Anthropic stop_reason
func convertOpenAIFinishReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// parseDataURI 解析 data:image/png;base64,... 格式
func parseDataURI(uri string) (mediaType string, data string) {
	// data:image/png;base64,xxxxx
	if !strings.HasPrefix(uri, "data:") {
		return "", ""
	}
	rest := uri[5:]
	idx := strings.Index(rest, ",")
	if idx < 0 {
		return "", ""
	}
	meta := rest[:idx]
	data = rest[idx+1:]
	// meta = "image/png;base64"
	parts := strings.Split(meta, ";")
	if len(parts) > 0 {
		mediaType = parts[0]
	}
	return mediaType, data
}

// buildOpenAIChunk 构建 OpenAI SSE chunk JSON
func buildOpenAIChunk(id string, mdl string, index int, delta map[string]any, usage *OpenAIUsage) string {
	chunk := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   mdl,
		"choices": []any{map[string]any{"index": index, "delta": delta, "finish_reason": nil}},
	}
	if usage != nil {
		chunk["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	out, _ := json.Marshal(chunk)
	return string(out)
}

// buildOpenAIChunkWithFinish 构建带 finish_reason 的 chunk
func buildOpenAIChunkWithFinish(id string, mdl string, index int, finishReason string, usage *OpenAIUsage) string {
	chunk := map[string]any{
		"id":      "chatcmpl-" + id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   mdl,
		"choices": []any{map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": finishReason}},
	}
	if usage != nil {
		usageMap := map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
		if usage.CachedTokens > 0 || usage.CacheWriteTokens > 0 {
			usageMap["prompt_tokens_details"] = map[string]any{
				"cached_tokens":      usage.CachedTokens,
				"cache_write_tokens": usage.CacheWriteTokens,
			}
		}
		chunk["usage"] = usageMap
	}
	out, _ := json.Marshal(chunk)
	return string(out)
}

func writeSSEChunk(w io.Writer, chunk string) {
	w.Write([]byte("data: " + chunk + "\n\n"))
}

func writeAnthropicSSE(w io.Writer, eventType string, data string) {
	w.Write([]byte("event: " + eventType + "\ndata: " + data + "\n\n"))
}

// extractOpenAIChunkUsage 从 OpenAI SSE chunk 中提取 usage（适用于 Anthropic 格式的 Usage）
// OpenAI prompt_tokens 含缓存，需拆分；支持 OpenRouter 的 cache_write_tokens
func extractOpenAIChunkUsage(chunk map[string]any, usage *Usage) {
	u, ok := chunk["usage"].(map[string]any)
	if !ok {
		return
	}
	promptTokens := 0
	cachedTokens := 0
	cacheWriteTokens := 0
	if v, ok := u["prompt_tokens"].(float64); ok {
		promptTokens = int(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		usage.OutputTokens = int(v)
	}
	if details, ok := u["prompt_tokens_details"].(map[string]any); ok {
		if v, ok := details["cached_tokens"].(float64); ok {
			cachedTokens = int(v)
		}
		if v, ok := details["cache_write_tokens"].(float64); ok {
			cacheWriteTokens = int(v)
		}
	}
	usage.InputTokens = promptTokens - cachedTokens - cacheWriteTokens
	usage.CacheReadInputTokens = cachedTokens
	usage.CacheCreationInputTokens = cacheWriteTokens
}

// ============================
// 上游请求构建（跨协议）
// ============================

// doRequestWithProtocol 根据上游协议选择正确的 URL 和认证方式发送请求
func (h *Handler) doRequestWithProtocol(c *gin.Context, upstream *model.Upstream, body []byte, protocol string) (*http.Response, error) {
	base := strings.TrimRight(upstream.API, "/")
	var url string
	switch protocol {
	case "openai":
		url = base + "/v1/chat/completions"
		if strings.HasSuffix(base, "/v1/chat/completions") {
			url = base
		}
	default: // anthropic
		url = base + "/v1/messages"
		if strings.HasSuffix(base, "/v1/messages") {
			url = base
		}
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

	// 根据协议设置认证头
	switch protocol {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
		req.Header.Del("X-Api-Key")
	default: // anthropic
		req.Header.Set("X-Api-Key", upstream.APIKey)
		req.Header.Set("Authorization", "Bearer "+upstream.APIKey)
	}
	req.Header.Set("Content-Type", "application/json")

	return h.client.Do(req)
}

// ============================
// 错误响应格式转换
// ============================

// convertAnthropicErrorToOpenAI 将 Anthropic 错误响应体转换为 OpenAI 格式
func convertAnthropicErrorToOpenAI(data []byte, statusCode int) []byte {
	var resp map[string]any
	if json.Unmarshal(data, &resp) != nil {
		return data
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		return data
	}
	msg, _ := errObj["message"].(string)
	openAIErr := map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    httpStatusToOpenAIError(statusCode),
			"param":   nil,
			"code":    nil,
		},
	}
	out, err := json.Marshal(openAIErr)
	if err != nil {
		return data
	}
	return out
}

// convertOpenAIErrorToAnthropic 将 OpenAI 错误响应体转换为 Anthropic 格式
func convertOpenAIErrorToAnthropic(data []byte, statusCode int) []byte {
	var resp map[string]any
	if json.Unmarshal(data, &resp) != nil {
		return data
	}
	errObj, _ := resp["error"].(map[string]any)
	if errObj == nil {
		return data
	}
	msg, _ := errObj["message"].(string)
	anthropicErr := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    httpStatusToAnthropicError(statusCode),
			"message": msg,
		},
	}
	out, err := json.Marshal(anthropicErr)
	if err != nil {
		return data
	}
	return out
}
