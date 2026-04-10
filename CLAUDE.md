# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目简介

Anthropic Claude API 聚合网关。将多个 Claude API 上游聚合到一起，对外暴露统一的 `/v1/messages` 接口。纯转发模式，不做协议转换。

技术栈：Go + Gin，JSON 文件存储，内嵌 Web UI（vanilla HTML/JS，go:embed）。

## 核心功能

- **分组管理**：先创建分组，再往分组内添加上游 API
- **负载均衡**：按分组轮询（Round Robin），仅选 active 状态的上游
- **故障转移**：请求失败或 5xx 自动切换同组下一个上游重试，重试次数 = 同组 active 上游数
- **故障标记**：自动（转发失败时）+ 手动（Admin API），健康检查恢复后自动置为 active
- **健康检查**：定时探测所有上游，支持自定义 headers/body/重试次数
- **错误码映射**：上游返回的状态码可映射为其他状态码，支持覆盖错误信息

## 常用命令

```bash
# 运行
go run main.go

# 指定配置文件路径
go run main.go -data path/to/config.json

# 构建
go build -o ai-gateway .

# 下载依赖
go mod tidy
```

## 架构

```
请求 POST /v1/messages
  → 确定分组（X-Group-ID header 或 group_id query，否则用第一个分组）
  → Balancer 轮询选择 active 上游
  → 转发请求（替换 x-api-key/authorization 为上游 key，其余 header 原样透传）
  → 失败/5xx → 标记故障 → 切换下一个上游重试
  → 成功 → 错误码映射 → 流式(SSE)/非流式响应透传给客户端
```

管理 API 挂在 `/admin/*`，Web UI 在 `/admin/ui`。

配置持久化为单个 JSON 文件（默认 `data/config.json`），`store.Store` 用 `sync.RWMutex` 保护并发读写。

## 关键路径

| 模块 | 路径 | 职责 |
|------|------|------|
| 数据模型 | `model/model.go` | 所有结构体定义 |
| 持久化 | `store/store.go` | JSON 文件读写，读写锁 |
| 转发 | `proxy/handler.go` | /v1/messages 转发 + 错误码映射 |
| 负载均衡 | `proxy/balancer.go` | 按分组轮询 |
| 健康检查 | `health/checker.go` | 定时探测 + 自动故障标记/恢复 |
| 管理 API | `admin/*.go` | 分组/上游/健康检查/错误码映射 CRUD |
| Web UI | `web/dist/index.html` | 内嵌管理界面（vanilla HTML/JS）|
| 嵌入 | `web/embed.go` | go:embed 静态文件 |

## 代码风格

- 尽早返回，不用 else
- 所有错误必须处理，禁止 panic
- Admin API 统一 `gin.H{"data": ...}` 或 `gin.H{"error": ...}` 响应格式
