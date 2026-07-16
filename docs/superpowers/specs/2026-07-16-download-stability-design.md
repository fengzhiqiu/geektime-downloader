# 下载稳定性改造设计规范

**日期：** 2026-07-16
**状态：** 已评审，待实现
**适用范围：** `geektime-downloader serve` 本地 HTTP API 服务
**阶段：** P0（止血）+ P1（稳定性）。P2（health 增强 / 错误码日志 / tsparser ctx）作为后续小迭代，不在本 spec 范围内。

---

## 1. 背景与问题

`geektime-downloader` 已由 CLI 改造为本地单用户 HTTP API 服务（详见 `2026-07-02-api-server-design.md`）。状态机与错误码设计完备，但**实际下载链路与 Worker 调度的工程实现**在共享账号场景下暴露出三类稳定性问题：

1. **下载卡死且不感知**：单 Worker 串行同步阻塞执行，文件下载无超时、无状态码检查，单分段 hang 会卡死整队；Worker 无看门狗，job 长期 `running` 而 progress 不更新，Agent 无法区分"在慢慢下"与"已卡死"。
2. **452/被踢登不感知**：极客时间 API 请求（走 `do()`）已检测 451/452；但视频 `getPlayInfo`（阿里云 VOD URL）绕过 `do()` 不检查状态码；文件下载（ts/mp4）完全不检查状态码，被拦截返回的错误页被 `io.ReadAll` 当视频字节写盘，静默产出损坏文件。
3. **限流时继续撞墙**：`CodeAuthExpired` 会暂停整个 Worker，但 `CodeRateLimited` 只把当前任务置 `waiting_rate_limit` 后 `return`，loop 立即取下一个 pending 任务继续打 API，连续触发限流。且无自动恢复，全靠 Agent 手动 retry。

### 1.1 关键事实（代码核实）

- 所有极客时间 API 方法（`V1ArticleInfo`/`VideoPlayAuth`/`CourseInfo` 等）均走 `client.newRequest` + `do()`，**451/452 检测已覆盖**。唯一绕过 `do()` 的是 `video.go:getPlayInfo`（直连阿里云 VOD URL）。
- `do()` 的状态码映射是内联 switch，可抽取复用。
- `http.DefaultClient`（`pkg/downloader` 使用）**无 Timeout**；Go 默认 client 不设整体超时。
- `DownloadMP4`（`video.go:213`）下载失败时 `return nil`，**静默吞错**。
- `apperr.MapError` 只识别 `os.IsTimeout`，**漏了 `context.DeadlineExceeded`**。
- Worker `runJob` 同步阻塞，无最长执行时间；`RecoverRunningJobs` 仅在服务重启时把 `running→pending`，运行期无卡死检测。
- progress `updated_at` 仅在文章级事件（start/phase/complete/skip/failed）刷新；视频单篇下载期间无细分事件 → 心跳停滞。

---

## 2. 目标与非目标

### 2.1 目标

- 文件下载不再静默产出坏文件、不再无限 hang。
- 共享账号被踢登/限流时，服务端能自感知并自恢复，减少对 Agent 手动介入的依赖。
- Agent 轮询能从进度字段区分"正在下"与"卡死"。
- 看门狗在真卡死时能释放 Worker，恢复队列吞吐。

### 2.2 非目标（YAGNI）

- 多账号 / 多租户。
- 分布式队列。
- 改动 CLI FSM 行为（reporter=nil 时行为不变）。
- DB schema 变更或 migration（复用 `updated_at`，progress JSON 加字段向前兼容）。
- P2 项（health 字段增强、按错误码计数日志、tsparser/merge 响应 ctx）。

---

## 3. 已决议的设计分叉

| 分叉 | 决议 | 理由 |
|------|------|------|
| 看门狗策略 | **硬超时 + 心跳双触发** | 硬超时（60min 较大阈值）能解真卡死并释放 Worker，误杀慢视频风险低；心跳停滞（10min）只告警不强制结束，双保险 |
| 限流处理 | **服务端自动恢复 + 全局冷却** | 共享单账号语义下全局冷却天然覆盖所有任务；自动恢复免去 Agent 介入链路 |
| 交付节奏 | **分阶段：P0+P1 先落地，P2 后续** | 跨 7 个包，单次改动大回归风险高；分阶段可逐项验证 |

---

## 4. 架构概览

```
HTTP API ── Enqueue ─► Worker.loop
                       │  ① 限流冷却闸门 (rateLimitUntil)
                       │  ② 取 NextPendingJob
                       ▼
                    runJob(ctx)
                       │  ③ context.WithTimeout(jobCtx, JobTimeout) ← 硬超时
                       │  ④ storeReporter (per-ts-segment 回调 → tick updated_at) ← 心跳源
                       ▼
              DownloadService.ExecuteDownload
                       │
        ┌──────────────┼────────────────┐
        ▼              ▼                ▼
   geektime API    video.download    pkg/downloader
   (do(): 451/452 ✓) (per-seg progress) (✱ 超时+状态码检查 ← P0核心)
                       │
            后台 ticker (60s) ── 心跳停滞扫描(stale→status_reason)
                             ── 限流冷却到期 → ResumeRateLimitJobs
```

---

## 5. 组件设计

### 5.1 P0-1 文件下载硬化（`internal/pkg/downloader/downloader.go`）

**问题**：`http.DefaultClient` 无超时；GET/HEAD 不检查 `resp.StatusCode`；452/403 错误页被 `io.ReadAll` 当视频字节写盘；`retry` 不分错误类型对 4xx 也无意义重试。

**改动**：

1. 新增包级 HTTP client，替换 `http.DefaultClient`：
   ```go
   var httpClient = &http.Client{
       Transport: &http.Transport{
           TLSHandshakeTimeout:   10 * time.Second,
           ResponseHeaderTimeout: 30 * time.Second,
           IdleConnTimeout:      90 * time.Second,
       },
   }
   ```
2. 新增错误类型：
   ```go
   type StatusError struct {
       StatusCode int
       Body       string
   }
   func (e *StatusError) Error() string
   ```
3. HEAD 请求（`DownloadFileConcurrently` 内）：执行后检查状态码，非 2xx 返回 `StatusError`，不再走"空文件即成功"分支。
4. GET 分段（`download` 内）：执行后检查状态码，非 2xx 返回 `StatusError`，不把错误页 body 写入 `Part`。
5. 每个请求用 `context.WithTimeout(ctx, SegmentTimeout)` 包一层（默认 60s，可配），覆盖 header+body 读。
6. `retry` 区分错误类型：
   - `StatusError`（服务端业务拒绝，如 403/452）：**不重试**，直接返回。
   - transport/timeout 类：重试，退避改为指数+抖动（`700ms → 1.4s → 2.8s`，±20% jitter）。
   - 仍尊重 ctx 取消（`context.Canceled` 立即返回）。

**调用方翻译**：video 层把 `StatusError` 翻译为 `geektime.ErrAuthFailed`(452)/`ErrGeekTimeRateLimit`(451)；其余原样冒泡到 `apperr.MapError` 的 default 分支（INTERNAL_ERROR, retryable）。

### 5.2 P0-2 修 `DownloadMP4` 吞错（`internal/video/video.go:216`）

`return nil` → `return err`。单篇内嵌 mp4 下载失败能冒泡到文章级 → job failed。

### 5.3 P0-3 `getPlayInfo` 加状态码检查（`internal/video/video.go:364` + `internal/geektime/client.go`）

1. 在 `client.go` 抽出 `func checkStatus(resp *resty.Response) error`：封装现有 `do()` 内 `statusCode != 200` 的 switch（451→`ErrGeekTimeRateLimit`，452→`ErrAuthFailed`，其他非 200→`ErrGeekTimeAPIBadCode`），200 返回 nil。`do()` 复用之消除重复。
2. `getPlayInfo` 执行 `Get` 后调用 `checkStatus(resp)`，非 200 直接返回映射错误，避免静默解析错误响应成空 PlayInfo 再在 `m3u8.Parse` 莫名失败。

> 注：该 URL 为阿里云 VOD，不会返回 452；主要挡住 403（playAuth 失效）等并统一错误信号。

### 5.4 P1-1 看门狗：硬超时 + 心跳双触发（`internal/job/worker.go` + `internal/job/store.go`）

**硬超时**：
- `runJob` 用 `jobCtx, cancel := context.WithTimeout(parent, cfg.JobTimeout)`（默认 60min）包裹。
- 超时触发 ctx 取消 → 下载返回 `context.DeadlineExceeded`。
- `runJob` 在 `MapError` 前先判 `errors.Is(err, context.DeadlineExceeded)` → 置 `StatusFailed`，`status_reason="watchdog_timeout"`，APIError `Code=TIMEOUT, Action=RETRY`。
- 注意保留现有 `defer` 清理 `activeJobID`/`cancel`，且硬超时 cancel 与用户 `CancelActive` 共用同一 `cancel`（`WithTimeout` 返回的 cancel 即可被外部调用）。

**心跳**：
- Worker 启动后台 ticker（周期 60s）。
- 每 tick 调 `store.MarkStaleJobs(ctx, HeartbeatTimeout)`（默认 10min）：把 `status=running AND updated_at < now-HeartbeatTimeout` 的 job 的 `status_reason` 置为 `"stale_progress"`（**保持 running**，不强制结束，符合"心跳只告警"决议）。
- `status_reason` 非空即对外可见，Agent 据此决定是否 cancel。
- 心跳依赖 `updated_at` 持续刷新，刷新源是 5.6 的 per-segment 回调。

### 5.5 P1-2 限流全局冷却 + 自动恢复（`internal/job/worker.go` + `internal/job/store.go`）

- Worker 新增 `rateLimitUntil atomic.Int64`（unix nano 恢复时刻）与 `cooldownStep`（指数退避计数，受 `mu` 保护或单独 `atomic`）。
- `runJob` 遇 `CodeRateLimited`：不动 `pausedAuth`；设 `rateLimitUntil = now + cooldown`，`cooldown` 按 `RateLimitCooldown` 基数指数增长（`120s → 240s → 480s`，封顶 30min）；`Enqueue` 唤醒。
- `loop` 顶部：若 `now < rateLimitUntil` → `select { case <-ctx.Done(): return; case <-time.After(residue): }` 等到冷却结束，期间**不取新任务**。
- 同一后台 ticker（与心跳共用，60s）：冷却到期（`now >= rateLimitUntil`）→ `store.ResumeRateLimitJobs(ctx)`（`waiting_rate_limit` 全部 → `pending`）→ `cooldownStep` 减半（渐进恢复，下次再限流从更小冷却起步）→ `Enqueue`。
- 共享单账号语义下，全局冷却天然覆盖所有 `waiting_rate_limit` 任务，无需 per-job 定时器。

**新增 Store 方法**：
- `MarkStaleJobs(ctx, heartbeatTimeout time.Duration) error`：UPDATE `jobs` SET `status_reason='stale_progress'` WHERE `status='running'` AND `updated_at < ?cutoff` AND `status_reason=''`（只标记尚未带 reason 的 running job，避免覆盖已有更具体 reason；`status_reason` 非空即对外可见）。
- `ResumeRateLimitJobs(ctx) error`：UPDATE waiting_rate_limit → pending，清空 status_reason/error_json。

### 5.6 P1-3 细化视频下载进度（`internal/progress/reporter.go` + `internal/video/video.go` + `internal/job/worker.go` + `internal/job/model.go`）

1. `progress.Reporter` 接口新增方法（`Nop` 与 `storeReporter` 都实现）：
   ```go
   OnArticleProgress(aid, done, total int)
   ```
2. `CurrentArticle` 结构（`job/model.go`）新增 `Done int`、`Total int` 字段（JSON 向后兼容）。
3. `storeReporter.OnArticleProgress`：若 `progress.CurrentArticle` 为 nil 或 aid 不匹配则初始化（`{AID:aid, Phase:"downloading_video"}`），再置 `Done/Total`，调 `UpdateJobProgress` 刷新 `updated_at`（即心跳 tick）。
4. video 下载链路透传 `reporter progress.Reporter`：
   - `DownloadArticleVideo` / `DownloadEnterpriseArticleVideo` / `DownloadUniversityVideo` / `DownloadSingleVideoProduct` 各加 `reporter progress.Reporter` 参数。
   - `downloadAliyunVodEncryptVideo` → `download` 透传 `reporter` 与 `articleID`（articleID 各函数已有，无需新增）。
   - `download` 的 ts 循环里每完成一段调 `reporter.OnArticleProgress(articleID, i+1, len(tsFileNames))`。
   - `reporter == nil` 时跳过调用（CLI FSM 模式不变）。
5. `course/downloader.go` 调用 video 函数处传 `d.progressReporter`。

### 5.7 P1-4 下载间隔 jitter 加大（`internal/course/downloader.go:372`）

`waitRandomTime` 改为 `interval*1000 + rand(interval*1000, interval*1000*2)`（即 1×~2× 区间，比当前固定 0~2s 更分散）。默认 `interval` 不动（避免影响 CLI 行为）。

---

## 6. 配置项（serve flags + `config.AppConfig` + `config/validator.go`）

| flag | 默认 | 校验 | 用途 |
|------|------|------|------|
| `--job-timeout` | `60m` | >0 | 单 job 硬超时 |
| `--heartbeat-timeout` | `10m` | >0 | 心跳停滞阈值 |
| `--rate-limit-cooldown` | `120s` | >0 | 限流全局冷却基数 |
| `--segment-timeout` | `60s` | >0 | 单文件分段下载超时 |

- `AppConfig` 新增对应字段（`time.Duration`）。
- `ValidateServeConfig` 增加范围校验（均需 >0）。
- `serve.go` 注册 flag，构造 `DownloadService` 与 `Worker` 时传入（`Worker` 需持有 JobTimeout/HeartbeatTimeout/RateLimitCooldown；`DownloadService`/`CourseDownloader` 需持有 SegmentTimeout 透传给 `pkg/downloader`）。
- `SegmentTimeout` 透传路径：`serve` → `config.AppConfig` → `service.mergeConfig` → `CourseDownloader.cfg` → `video.download` → `pkg/downloader.DownloadFileConcurrently`（新增 `segmentTimeout time.Duration` 参数）。

---

## 7. 错误映射（`internal/apperr/errors.go`）

`MapError` 在 `os.IsTimeout` 分支前增加：
```go
case errors.Is(err, context.DeadlineExceeded):
    return &APIError{
        Code: CodeTimeout, Message: "任务执行超时（看门狗触发）",
        Action: "RETRY", Retryable: true, HTTPStatus: 504, Underlying: err,
    }
```
downloader 的 `StatusError` 由 video 层翻译为 `geektime.ErrAuthFailed`(452)/`ErrGeekTimeRateLimit`(451)，其余原样冒泡到 default 分支。

---

## 8. 数据模型

**不改 schema**。

- 心跳复用 `jobs.updated_at`（progress 写库时刷新）。
- `CurrentArticle.Done/Total` 是 `progress_json` 内字段，加字段向前兼容，老客户端忽略。
- 无 migration，无 DB 变更。

---

## 9. 测试策略

| 层级 | 内容 |
|------|------|
| `pkg/downloader` | httptest server 返回 452/403/慢响应；断言 `StatusError`、超时、4xx 不重试、网络错重试、坏 body 不写盘 |
| `apperr` | `context.DeadlineExceeded`→TIMEOUT；StatusError 经 video 翻译→AUTH_EXPIRED/RATE_LIMITED |
| `job/worker` | 假 service 注入超时/限流/正常；断言硬超时→failed(watchdog_timeout)、限流→rateLimitUntil 设置+冷却期不取任务、冷却到期→ResumeRateLimitJobs |
| `job/store` | `MarkStaleJobs`/`ResumeRateLimitJobs` SQL 行为单测 |
| `video` | `getPlayInfo` 非 200 返回映射错误；`DownloadMP4` 失败冒泡；per-segment 回调被调用 |
| CLI 回归 | FSM 模式（reporter=nil）行为不变 |

---

## 10. 文件清单

```
internal/pkg/downloader/downloader.go   P0-1 超时+状态码+StatusError+retry 区分
internal/video/video.go                 P0-2 吞错; P0-3 getPlayInfo; P1-3 透传 reporter
internal/geektime/client.go             P0-3 checkStatus 抽取
internal/apperr/errors.go                DeadlineExceeded→TIMEOUT
internal/job/worker.go                   P1-1 硬超时+心跳; P1-2 冷却+自动恢复; ticker
internal/job/store.go                    MarkStaleJobs/ResumeRateLimitJobs
internal/job/model.go                    CurrentArticle.Done/Total
internal/progress/reporter.go            OnArticleProgress
internal/course/downloader.go            P1-4 jitter; 传 reporter
internal/config/config.go                新字段（4 个 Duration）
internal/config/validator.go             新校验
cmd/serve.go                             新 flags（4 个）+ 透传
docs/superpowers/specs/2026-07-16-download-stability-design.md  本 spec
```

---

## 11. P2（后续，不在本 spec）

- `GET /health` 与 `GET /downloads/{id}` 暴露 `last_active_at`、当前 job 运行时长、卡死嫌疑 job 列表、最近 N 条 API 错误码统计。
- 按 451/452/超时分别打不同级别日志并计数。
- `mergeTSFiles` / `tsparser` 响应 ctx，可被 cancel。
