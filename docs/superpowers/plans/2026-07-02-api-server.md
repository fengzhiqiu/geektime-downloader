# 本地 API 服务实现计划

> **Goal:** 为 geektime-downloader 增加 `serve` 子命令，暴露 Agent 友好的异步下载 REST API。

**Architecture:** Service 层抽取 FSM 业务逻辑；SQLite 持久化任务；单 Worker 串行执行；HTTP API + Bearer 鉴权。

**Tech Stack:** Go, modernc.org/sqlite, net/http, 现有 course/geektime 核心包

---

## Phase 1 — Service 层 ✅

- `internal/progress/reporter.go` — 进度回调接口
- `internal/service/product_types.go` — 产品类型定义
- `internal/service/download.go` — 课程查询与下载编排
- `internal/course/downloader.go` — 接入 ProgressReporter

## Phase 2 — Job 存储与 Worker ✅

- `internal/job/model.go` / `store.go` / `worker.go`
- `internal/auth/manager.go` / `session_store.go`
- `internal/apperr/errors.go` — 错误码映射

## Phase 3 — HTTP API ✅

- `internal/api/handlers.go` / `response.go`
- `cmd/serve.go`
- `docs/superpowers/specs/2026-07-02-api-server-design.md`

## 验证

```bash
go test ./internal/apperr/... ./internal/job/...
go build -o geektime-downloader .
./geektime-downloader serve --api-key test --gcid ... --gcess ...
```
