# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目简介

Anthropic Claude API 聚合网关，支持 OpenAI ↔ Anthropic 跨协议自动转换。将多个上游 API 聚合到一起，对外暴露 `/v1/messages`（Anthropic）和 `/v1/chat/completions`（OpenAI）两个端点。

技术栈：Go + Gin，JSON 文件存储（`sync.RWMutex` + 原子写入），内嵌 Web UI（vanilla HTML/JS，go:embed）。

## 常用命令

```bash
go run main.go                          # 默认读 data/config.json
go run main.go -data path/to/config.json # 指定配置文件
go build -o ai-gateway .
go mod tidy
```

## 架构

```
客户端请求
  POST /v1/messages (Anthropic) 或 POST /v1/chat/completions (OpenAI)
  → API Key 匹配分组（O(1) 索引查找）
  → 协议校验 / 模型校验 / 流式/非流式校验
  → 模型映射 + 字段剔除 + 字段注入
  → Balancer 选上游（会话亲和 → 最少负载 + 加权随机）
  → 跨协议转换（上游协议与请求协议不同时自动转换请求体/响应体）
  → 转发（替换认证头，其余 header 透传）
  → 失败 → 故障转移规则（offline/cooldown/retry/retry_other）→ 下一个上游
  → 成功 → 错误码映射 → 签名替换 → noCache 改写 → 流式(SSE)/非流式透传
  → 异步记录日志 + 异步更新费用统计
```

管理 API：`/admin/*`（Basic Auth 或 Bearer Token），Web UI：`/ui`。
注册中心 API：`/registry/*`（独立 Token 鉴权），支持服务注册/心跳/注销。

## 关键路径

| 模块 | 路径 | 职责 |
|------|------|------|
| 入口 | `main.go` | 启动服务，组装各模块，优雅退出 |
| 数据模型 | `model/model.go` | 所有结构体定义（Group/Upstream/Config/FailoverRule 等） |
| 持久化 | `store/store.go` | JSON 文件读写（原子写入），API Key → Group O(1) 索引 |
| Anthropic 转发 | `proxy/handler.go` | `/v1/messages` 完整处理流程，日志/统计/费用持久化 |
| OpenAI 转发 | `proxy/openai.go` | `/v1/chat/completions` 完整处理流程 |
| 跨协议转换 | `proxy/convert.go` | OpenAI ↔ Anthropic 双向请求/响应/SSE 流转换 |
| 负载均衡 | `proxy/balancer.go` | 会话亲和 + 最少负载 + 加权随机，冷却管理，并发追踪 |
| 健康检查 | `health/checker.go` | 定时探测 + registry 心跳过期检测 + 状态机（active ↔ faulted ↔ half_open） |
| 注册中心 | `registry/handler.go` | 上游自注册/心跳/注销（按 ip+port+group 去重） |
| 管理 API | `admin/*.go` | 分组/上游/健康检查/错误码映射 CRUD |
| Web UI | `web/dist/index.html` | 内嵌管理界面 |

## 核心机制

**负载均衡**：非轮询。先查会话亲和（`metadata.user_id` 或 OpenAI `user` 字段提取 session key），无亲和则在最小负载的上游中按权重加权随机。Weight=0 暂停流量，nil 默认为 1。

**故障转移规则**（`FailoverRule`）：按 HTTP 状态码触发，支持四种动作：
- `offline`：标记故障，等待健康检查恢复
- `cooldown`：临时冷却 N 秒（支持 `retry-after` header），不标记故障
- `retry`：允许同一上游重试（删除 exclude）
- `retry_other`：换其他上游重试

**跨协议转换**：上游的 `protocol` 字段决定协议（`anthropic`/`openai`），与请求端点不同时自动双向转换，包括：请求体、非流式响应、SSE 流式响应、错误响应。

**日志系统**：异步写入，按分组分目录，每 10000 行轮转一个 `.jsonl` 文件。内存环形缓冲保留最近 500 条供 API 查询。支持 `off/random/random_session/all/error_only` 五种日志模式。

**费用统计**：异步持久化到 `site_stats.json`/`upstream_stats.json`/`latency_stats.json`，每 3 秒批量刷盘。

## 数据目录结构

配置文件同级目录下自动生成：
```
data/
  config.json          # 主配置（分组、上游、健康检查等）
  server.json          # 服务启动参数（listen_addr）
  sessions.json        # 会话亲和持久化
  site_stats.json      # 全站统计
  upstream_stats.json  # 上游统计
  latency_stats.json   # 延迟统计
  logs/                # 请求日志
    {group_id}/
      00001.jsonl
      00002.jsonl
```

## 代码风格

- 尽早返回，不用 else
- 所有错误必须处理，禁止 panic
- goroutine 必须用 `model.SafeGo()` 启动（带 panic recovery）
- Admin API 统一 `gin.H{"data": ...}` 或 `gin.H{"error": ...}` 响应格式
- Anthropic 错误响应用 `anthropicError()`，OpenAI 错误响应用 `openaiError()`
