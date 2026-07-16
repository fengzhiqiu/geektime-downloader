# 可观测性与 ctx 响应 (P2) 设计规范

**日期：** 2026-07-16
**状态：** 已评审，待实现
**阶段：** P2（P0+P1 已合并进 main）。本 spec 是 `2026-07-16-download-stability-design.md` §11 列出的 P2 项的展开。

---

## 1. 背景与目标

P0+P1 落地了下载链路硬化、看门狗、限流自动恢复与细粒度进度。但运行期可观测性仍弱：Agent 轮询只能看到 job 级状态，看不到服务级健康度（运行时长、最后活跃时间、卡死嫌疑 job 数、错误码分布、限流冷却状态）；且 `tsparser`/`mergeTSFiles` 是纯 CPU 循环不响应 ctx，被硬超时取消时无法及时退出（只能等循环跑完）。

P2 目标：
1. `/health` 与 `/downloads/{id}` 暴露服务级与 job 级运行时健康度。
2. 终态错误按 `apperr` code 分级日志 + 内存计数。
3. `tsparser`/`mergeTSFiles` 响应 ctx，配合 P0 的硬超时及时退出。

### 1.1 现状（代码核实）

- `internal/api/handlers.go:handleHealth` 返回 `status/version/chrome_available/worker_status/active_job_id`。
- `handleGetDownload` 直接返回 `*Job`（含 `updated_at`/`started_at`/`status_reason`）。
- `internal/pkg/logger`：logrus，文件输出，`Infof/Warnf/Errorf`，无结构化计数。
- `internal/job/worker.go`：已暴露 `ActiveJobID()`、`OnCookiesUpdated`、`CancelActive`；`rateLimitUntil atomic.Int64`（P1）已在 worker 上；终态错误在 `runJob` 的 switch 内 `UpdateJobStatus` 后 return，未分级记日志、未计数。
- `internal/m3u8/tsparser.go`：`NewTSParser(data, key)` → `Decrypt()`，`decryptPES` 遍历 `pesFragments`，`parseTS` 遍历 packet，无 ctx。
- `internal/video/video.go:mergeTSFiles(tempVideoDir, filenamifyTitle, projectDir, key, isVodEncryptVideo)`：遍历 ts 文件，无 ctx；被 `download` 调用（`download` 已有 ctx）。
- `apperr` code 常量：`CodeAuthExpired/CodeRateLimited/CodeTimeout/CodeInternal/CodeNotPurchased/CodeInvalidProduct/CodeCancelled/CodeNotFound/CodeBadRequest/CodeUnauthorized`。

---

## 2. 非目标（YAGNI）

- 错误码计数**不持久化**（内存，重启清零）。
- `parseTS` 的 packet 循环**不加** ctx 检查（PES 边界粒度已足够降延迟）。
- 不引入新配置 flag（P2 无阈值可调）。
- 不改 `service`/`config`/`geektime` 包。
- 不做滑动窗口/百分位统计（仅累计计数）。

---

## 3. 组件设计

### 3.1 `internal/job/stats.go`（新文件）

内存错误码计数器，独立可测。

```go
package job

import "sync/atomic"

// Stats counts terminal job errors by apperr code (in-memory, reset on restart).
type Stats struct {
	authExpired, rateLimited, timeout, internalErr atomic.Int64
}

func (s *Stats) Inc(code string) {
	switch code {
	case "AUTH_EXPIRED":
		s.authExpired.Add(1)
	case "RATE_LIMITED":
		s.rateLimited.Add(1)
	case "TIMEOUT":
		s.timeout.Add(1)
	default:
		s.internalErr.Add(1) // INTERNAL_ERROR / 未识别码统一计入
	}
}

// Snapshot returns a copy of current counts keyed by apperr code.
func (s *Stats) Snapshot() map[string]int64 {
	return map[string]int64{
		"AUTH_EXPIRED":   s.authExpired.Load(),
		"RATE_LIMITED":   s.rateLimited.Load(),
		"TIMEOUT":        s.timeout.Load(),
		"INTERNAL_ERROR": s.internalErr.Load(),
	}
}
```

> 4 个固定桶，避免 map+mutex 的并发开销；未识别 code 归入 INTERNAL_ERROR。Agent 拿到的就是这 4 个 key。

### 3.2 `internal/job/worker.go` 改动

- `Worker` 新增字段 `stats *Stats`、`lastActiveAt atomic.Int64`、`startedAt time.Time`（worker 启动时刻，用于 uptime）。
- `NewWorker(store, exec, clientProvider, st Stability, stats *Stats)`：`stats` 由 serve 注入（可 nil，nil 时跳过计数）。`startedAt = time.Now()`。
- `runJob` 终态错误分支（含硬超时分支）：
  - 取 `apiErr := apperr.MapError(err)` 后，按 code 选级别记日志并 `stats.Inc(apiErr.Code)`：
    - `AUTH_EXPIRED`/`RATE_LIMITED`/`TIMEOUT` → `logger.Warnf("job %s terminal: %s — %s", jobID, apiErr.Code, apiErr.Message)`
    - 其他 → `logger.Errorf(err, "job %s terminal: %s", jobID, apiErr.Code)`（含 underlying err）
  - `stats` 为 nil 时跳过。
- `lastActiveAt`：在 `loop` 每轮取到任务时、`runJob` 结束时、`stabilityLoop` 每 tick 时 `Store(time.Now().UnixNano())`。
- 新增暴露方法：
  - `Stats() map[string]int64`（nil-safe：`w.stats==nil` 时返回 nil，JSON 输出 null；serve 总是注入非 nil `*Stats`，故生产路径始终有计数）
  - `LastActiveAt() time.Time`（`lastActiveAt` 为 0 时返回 zero time）
  - `Uptime() time.Duration`
  - `RateLimitUntil() time.Time`（读 `rateLimitUntil`，0 返回 zero time）
- `stabilityLoop` 与 `loop` 中原有逻辑不变，仅补 `lastActiveAt` 更新。

### 3.3 `internal/job/store.go`

新增：
```go
func (s *Store) CountStaleJobs(ctx context.Context) (int64, error)
```
`SELECT count(*) FROM jobs WHERE status='running' AND status_reason='stale_progress'`。

### 3.4 `internal/api/handlers.go`

`handleHealth` 在现有字段上追加：
```go
writeOK(w, map[string]any{
    "status": "ok", "version": s.version, "chrome_available": chromeAvailable(),
    "worker_status": workerStatus, "active_job_id": nilString(activeJob),
    "uptime_seconds":          int64(s.worker.Uptime().Seconds()),
    "last_active_at":          isoTime(s.worker.LastActiveAt()),
    "stale_jobs":              staleCount,
    "error_counts":            s.worker.Stats(),
    "rate_limit_cooldown_until": isoTimePtr(s.worker.RateLimitUntil()),
})
```
- `staleCount` 由 `s.store.CountStaleJobs(r.Context())` 取（错误时 -1 或 0，记日志不阻断 health）。
- `rate_limit_cooldown_until`：`RateLimitUntil()` 为 zero time 时输出 null。

`handleGetDownload`：在 `writeOK(w, j)` 前给 `j.RuntimeSeconds` 赋值（仅 running 时 = `int(now - started_at)`），其余状态 0/省略。

`internal/job/model.go`：`Job` 加 `RuntimeSeconds int64 json:"runtime_seconds,omitempty"`（仅运行时上报）。

### 3.5 `internal/m3u8/tsparser.go`

签名加 ctx：
```go
func NewTSParser(ctx context.Context, data []byte, key string) (*TSParser, error)
func (p *TSParser) Decrypt(ctx context.Context) ([]byte, error)
func (p *TSParser) decryptPES(ctx context.Context, ...) error
```
- `decryptPES` 的 `for _, pes := range pesFragments` 循环顶部：`if err := ctx.Err(); err != nil { return err }`。
- `NewTSParser` 内 `stream.parseTS()` 不改（不加 ctx）。
- `parseTS` 不改。

### 3.6 `internal/video/video.go`

- `mergeTSFiles(ctx context.Context, tempVideoDir, filenamifyTitle, projectDir string, key []byte, isVodEncryptVideo bool) error`：加 ctx；ts 文件循环每处理完一个后 `if err := ctx.Err(); err != nil { removeOnError = true; return err }`。
- 调用处 `download` 已有 ctx（透传）。
- `NewTSParser`/`Decrypt` 调用补 ctx。

### 3.7 `cmd/serve.go`

- 构造 `stats := &job.Stats{}`，传入 `job.NewWorker(store, dlSvc, authMgr.GetClient, stability, stats)`。
- 无新 flag。

---

## 4. 数据流

```
runJob 终态错误 → logger.{Warn|Error}f(code) + stats.Inc(code)
                                       │
stabilityLoop tick → lastActiveAt = now │
loop 取任务/runJob 结束 → lastActiveAt  │
                                       ▼
GET /health ← worker.{Uptime,LastActiveAt,Stats,RateLimitUntil} + store.CountStaleJobs
GET /downloads/{id} ← Job.RuntimeSeconds = now - started_at (running)
```

`tsparser` ctx：`download`(ctx) → `mergeTSFiles`(ctx) → `NewTSParser`(ctx)/`Decrypt`(ctx) → `decryptPES` 边界 `ctx.Err()`。

---

## 5. 测试策略

| 层级 | 内容 |
|------|------|
| `job/stats_test.go` | 并发 `Inc` 各 code，`-race` 下 `Snapshot` 计数正确；未识别 code 归入 INTERNAL_ERROR |
| `job/store_test.go` | `CountStaleJobs`：1 个 stale + 1 个正常 running → 返回 1 |
| `m3u8/tsparser_test.go` | 构造较大 fake ts 流（多 PES fragment），`Decrypt(ctx)` 中途 cancel → 返回 `ctx.Err()`（不 panic、不 hang）；正常 ctx 返回 nil |
| `video/video_test.go` | `mergeTSFiles` ctx 取消提前返回（复用 tsparser fake） |
| `api/handlers_test.go` | `handleHealth` 返回 6 个新字段；`handleGetDownload` running job `runtime_seconds>0`（httptest + 内存 store + 真 worker） |
| 回归 | `go test ./internal/...`、`go vet`、`-race ./internal/job/` |

---

## 6. 文件清单

```
internal/job/stats.go             新增 Stats
internal/job/stats_test.go        新增测试
internal/job/worker.go            持有 stats/lastActiveAt/startedAt；分级日志；暴露 Stats/LastActiveAt/Uptime/RateLimitUntil
internal/job/store.go             CountStaleJobs
internal/job/store_test.go        CountStaleJobs 测试
internal/job/model.go             Job.RuntimeSeconds
internal/m3u8/tsparser.go         NewTSParser/Decrypt/decryptPES 加 ctx
internal/m3u8/tsparser_test.go    新增 ctx 取消测试
internal/video/video.go           mergeTSFiles 加 ctx；调用补 ctx
internal/video/video_test.go      mergeTSFiles ctx 测试
internal/api/handlers.go          handleHealth/handleGetDownload 新字段
internal/api/handlers_test.go      新增测试
cmd/serve.go                      注入 stats
docs/superpowers/specs/2026-07-16-observability-ctx-design.md  本 spec
```

---

## 7. 兼容性

- `/health` 与 `/downloads/{id}` 仅**新增字段**，老客户端忽略多余字段，向后兼容。
- `NewTSParser`/`Decrypt`/`mergeTSFiles` 签名变更：调用点全在本仓 `internal/` 内，随本 spec 一并改。
- `NewWorker` 增第 5 参 `stats *Stats`：调用点 `cmd/serve.go` 传 `&job.Stats{}`；`internal/job/worker_test.go` 现有 `NewWorker(...)` 调用补 `nil`（T8 测试不验证 stats，nil 即可）。
- 无 DB schema 变更、无 config 变更、无 CLI 行为变更。
