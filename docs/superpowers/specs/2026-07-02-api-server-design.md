# geektime-downloader 本地 API 服务设计规范

**日期：** 2026-07-02  
**状态：** 待评审  
**部署模式：** A — 单用户本地（一套 Cookie，本机 Agent 调用）

---

## 1. 背景与目标

### 1.1 现状

`geektime-downloader` 是 Go CLI 工具，通过 Cobra flags + FSM 交互式状态机驱动课程下载。核心能力包括：

- Cookie 认证（`--gcid` / `--gcess`）
- 多种产品类型：专栏、视频课、每日一课、大厂案例、训练营等
- 多种输出格式：PDF（Chromedp）、Markdown、音频、视频（阿里云 VOD m3u8）
- 文章串行下载，带间隔限流
- 断点续传依赖目标文件是否已存在
- 错误类型：`ErrAuthFailed`、`ErrGeekTimeRateLimit`、`ErrGeekTimeAPIBadCode` 等

当前无持久化任务状态，Cookie 仅在启动时注入，认证失败时进程直接退出。

### 1.2 目标

在保留现有 CLI 的前提下，新增 **本地 HTTP API 服务**，使 Agent 能够：

1. 以标准化 REST 接口创建异步下载任务
2. 定期轮询任务状态与进度
3. 在 Cookie 过期、限流等可恢复错误时，获取结构化处置指引并执行恢复操作
4. 无需交互式终端 prompt

### 1.3 非目标（YAGNI）

- 多用户 / 多租户隔离
- 分布式任务队列（Redis 等）
- 云端部署与高可用
- 用户账号密码登录 UI（可复用已有 `Login()` API 作为后续扩展，首版不做）
- Webhook 通知（P2 可选）
- MCP Server 适配（P2 可选）

---

## 2. 架构概览

```
┌─────────────────────────────────────────────────────────┐
│  调用方：Cursor Agent / 脚本 / curl                      │
└────────────────────────┬────────────────────────────────┘
                         │ HTTP (127.0.0.1)
┌────────────────────────▼────────────────────────────────┐
│  cmd/serve.go          geektime-downloader serve         │
│  internal/api/         HTTP 路由、鉴权、请求校验          │
│  internal/job/         任务存储、状态机、Worker           │
│  internal/service/     业务编排（从 FSM 抽取）            │
├─────────────────────────────────────────────────────────┤
│  internal/geektime/    API 客户端（现有）                 │
│  internal/course/      下载编排（现有，加进度回调）        │
│  internal/video|pdf|md  各格式下载器（现有）              │
└─────────────────────────────────────────────────────────┘
                         │
              ┌──────────▼──────────┐
              │  SQLite jobs.db     │
              │  ~/.config/...      │
              └─────────────────────┘
```

### 2.1 设计原则

| 原则 | 说明 |
|------|------|
| 单用户 | 全局唯一 Session，无 `user_id` 概念 |
| 本地优先 | 默认绑定 `127.0.0.1`，API Key 防误暴露 |
| 复用核心 | `geektime` / `course` / `video` 等包尽量不改接口语义 |
| Agent 友好 | 稳定错误码 + `action` / `action_hint` 字段 |
| 可恢复 | 任务持久化，服务重启后可恢复 `pending` 任务 |

---

## 3. 进程与命令

### 3.1 新增子命令

```
geektime-downloader serve [flags]
```

| Flag | 默认值 | 说明 |
|------|--------|------|
| `--addr` | `127.0.0.1:8080` | 监听地址 |
| `--api-key` | （必填或环境变量） | Bearer Token 鉴权 |
| `--db` | `{UserConfigDir}/geektime-downloader/jobs.db` | SQLite 路径 |
| `--gcid` / `--gcess` | 可选 | 启动时注入 Cookie；也可通过 API 更新 |
| `-f, --folder` | 同 CLI | 下载根目录 |
| `-q, --quality` | `sd` | 默认视频清晰度 |
| `--output` | `1` | 默认专栏输出位掩码 |
| `--comments` | `1` | 默认评论策略 |
| `--interval` | `1` | 默认下载间隔 |
| `--enterprise` | `false` | 企业版模式 |
| `--log-level` | `info` | 日志级别 |

现有 `geektime-downloader`（无子命令）保持 FSM 交互模式不变。

### 3.2 运行时组件

启动时初始化：

1. SQLite 连接与 migration
2. `AuthManager`（加载/校验 Cookie）
3. `geektime.Client`（随 Cookie 更新而重建）
4. `DownloadService`（封装课程查询与下载）
5. `JobWorker`（单 goroutine 任务消费者）
6. HTTP Server

---

## 4. API 规范

**Base URL：** `http://127.0.0.1:8080/api/v1`  
**认证：** `Authorization: Bearer <api-key>`  
**Content-Type：** `application/json`

### 4.1 统一响应格式

成功：

```json
{
  "data": { },
  "error": null
}
```

失败：

```json
{
  "data": null,
  "error": {
    "code": "AUTH_EXPIRED",
    "message": "当前账户登录已过期",
    "action": "UPDATE_COOKIES",
    "action_hint": "从浏览器获取 gcid/gcess，调用 PUT /api/v1/session/cookies 后 retry 任务",
    "retryable": true,
    "details": {}
  }
}
```

HTTP 状态码约定：

| 场景 | 状态码 |
|------|--------|
| 成功 | 200 |
| 创建任务 | 202 Accepted |
| 请求参数错误 | 400 |
| 未授权（API Key） | 401 |
| 资源不存在 | 404 |
| 冲突（如重复 idempotency_key） | 409 |
| 服务内部错误 | 500 |

### 4.2 健康检查

#### `GET /api/v1/health`

无需 API Key（仅本机访问）。

```json
{
  "data": {
    "status": "ok",
    "version": "x.y.z",
    "chrome_available": true,
    "worker_status": "idle",
    "active_job_id": null
  },
  "error": null
}
```

### 4.3 会话管理

#### `GET /api/v1/session/status`

```json
{
  "data": {
    "status": "valid",
    "updated_at": "2026-07-02T10:00:00Z"
  },
  "error": null
}
```

`status` 枚举：`valid` | `expired` | `unknown` | `not_configured`

#### `PUT /api/v1/session/cookies`

请求：

```json
{
  "gcid": "...",
  "gcess": "..."
}
```

行为：

1. 持久化到 SQLite `session` 表
2. 调用极客时间 API 探活（轻量请求，如已有 `Auth()` 则复用）
3. 重建全局 `geektime.Client`
4. 将所有 `waiting_auth` 状态的任务自动置为 `pending`（不自动开始执行，由 Worker 按队列顺序拾取；可通过配置 `auto_resume_on_cookie_update=true` 控制）

响应：

```json
{
  "data": {
    "status": "valid",
    "updated_at": "2026-07-02T10:05:00Z",
    "resumed_jobs": ["job_01HX..."]
  },
  "error": null
}
```

Cookie 校验失败时返回 400 + `error.code = AUTH_INVALID`。

### 4.4 产品类型与课程查询

#### `GET /api/v1/product-types?enterprise=false`

返回可下载的产品类型列表（对应现有 `ui.ProductTypeSelectOption`）。

```json
{
  "data": {
    "types": [
      {
        "id": "column",
        "name": "普通课程",
        "need_select_article": true,
        "source_type": "column",
        "accept_product_types": ["c1"]
      }
    ]
  },
  "error": null
}
```

#### `POST /api/v1/courses/lookup`

同步接口，用于 Agent 在创建下载任务前确认课程信息。

请求：

```json
{
  "product_type": "column",
  "product_id": 100078001,
  "enterprise": false
}
```

响应：

```json
{
  "data": {
    "id": 100078001,
    "title": "Go 语言实战",
    "type": "c1",
    "is_video": false,
    "access": true,
    "articles": [
      { "aid": 123450, "title": "开篇词", "section_title": "" }
    ]
  },
  "error": null
}
```

未购买时返回 400 + `error.code = NOT_PURCHASED`。

### 4.5 下载任务

#### `POST /api/v1/downloads`

创建异步下载任务。返回 **202 Accepted**。

请求：

```json
{
  "product_type": "column",
  "product_id": 100078001,
  "enterprise": false,
  "mode": "all",
  "article_ids": [],
  "options": {
    "download_folder": "",
    "quality": "sd",
    "output": 7,
    "comments": 1,
    "interval": 1,
    "overwrite": false,
    "print_pdf_wait": 5,
    "print_pdf_timeout": 60
  },
  "idempotency_key": "optional-uuid"
}
```

`mode` 枚举：

| mode | 说明 |
|------|------|
| `all` | 下载课程全部文章 |
| `articles` | 下载 `article_ids` 指定文章 |
| `single_video` | 每日一课/大厂案例等无需选篇的产品 |

`options` 中空字符串字段使用 serve 启动时的默认值。

响应：

```json
{
  "data": {
    "id": "job_01JXYZ...",
    "status": "pending",
    "created_at": "2026-07-02T10:00:00Z",
    "poll_url": "/api/v1/downloads/job_01JXYZ..."
  },
  "error": null
}
```

#### `GET /api/v1/downloads`

查询参数：`status`（可选，逗号分隔）、`limit`（默认 20）、`offset`（默认 0）。

#### `GET /api/v1/downloads/{id}`

**Agent 轮询主接口。**

```json
{
  "data": {
    "id": "job_01JXYZ...",
    "status": "running",
    "status_reason": null,
    "created_at": "2026-07-02T10:00:00Z",
    "updated_at": "2026-07-02T10:05:30Z",
    "started_at": "2026-07-02T10:00:05Z",
    "finished_at": null,
    "request": { },
    "course": {
      "id": 100078001,
      "title": "Go 语言实战",
      "is_video": false
    },
    "progress": {
      "total": 45,
      "completed": 12,
      "skipped": 3,
      "failed": 0,
      "current_article": {
        "aid": 123456,
        "title": "第13讲",
        "phase": "generating_pdf"
      }
    },
    "articles": [
      {
        "aid": 123450,
        "title": "开篇词",
        "status": "completed",
        "files": ["开篇词.pdf"]
      }
    ],
    "error": null,
    "download_folder": "/Users/me/geektime-downloader/Go语言实战"
  },
  "error": null
}
```

`phase` 枚举（`current_article` 运行时）：`fetching` | `generating_pdf` | `generating_markdown` | `downloading_audio` | `downloading_video` | `downloading_images`

#### `POST /api/v1/downloads/{id}/retry`

将 `failed` / `waiting_auth` / `waiting_rate_limit` 任务重置为 `pending`。

仅当任务不处于 `running` 时允许。

#### `POST /api/v1/downloads/{id}/cancel`

取消任务。若正在运行，通过 context cancel 中断当前文章下载，状态变为 `cancelled`。

#### `DELETE /api/v1/downloads/{id}`

删除任务记录（不删除已下载文件）。`running` 任务需先 cancel。

---

## 5. 任务状态机

```
pending ──► running ──► completed
              │
              ├──► failed
              ├──► cancelled
              ├──► waiting_auth      (ErrAuthFailed)
              └──► waiting_rate_limit (ErrGeekTimeRateLimit)

waiting_auth ──► pending   (Cookie 更新 或 手动 retry)
waiting_rate_limit ──► pending (等待后 retry)
failed ──► pending (retry)
```

### 5.1 状态说明

| status | 含义 | Agent 动作 |
|--------|------|------------|
| `pending` | 排队等待 Worker | 继续轮询 |
| `running` | 正在下载 | 继续轮询，读 progress |
| `completed` | 全部完成 | 结束 |
| `failed` | 不可恢复或超过重试次数 | 查 error，决定是否 retry |
| `cancelled` | 用户/Agent 取消 | 结束 |
| `waiting_auth` | Cookie 过期 | `PUT /session/cookies` → `retry` |
| `waiting_rate_limit` | 触发限流 | 等待后 `retry`，或更新 Cookie |

### 5.2 服务重启恢复策略

启动时扫描 SQLite：

- `running` → 改为 `pending`（上次可能异常中断）
- `waiting_auth` / `waiting_rate_limit` / `failed` → 保持不变
- Worker 自动从 `pending` 队列头部取任务执行

---

## 6. 错误码映射

| API code | Go error / 场景 | retryable | action |
|----------|-----------------|-----------|--------|
| `AUTH_EXPIRED` | `geektime.ErrAuthFailed` | true | `UPDATE_COOKIES` |
| `AUTH_INVALID` | Cookie 探活失败 | false | `UPDATE_COOKIES` |
| `RATE_LIMITED` | `geektime.ErrGeekTimeRateLimit` | true | `WAIT_AND_RETRY` |
| `NOT_PURCHASED` | Access 校验失败 | false | `NONE` |
| `INVALID_PRODUCT` | 产品 ID 与类型不匹配 | false | `NONE` |
| `TIMEOUT` | 请求/PDF 超时 | true | `RETRY` |
| `CHROME_UNAVAILABLE` | Chromedp 启动失败 | false | `CHECK_CHROME` |
| `CANCELLED` | context 取消 | false | `NONE` |
| `INTERNAL_ERROR` | 其他 | 视情况 | `RETRY` |

任务级 `error` 对象结构与 API 级一致，附加 `failed_article` 字段（若适用）。

---

## 7. 数据模型（SQLite）

数据库路径：`{UserConfigDir}/geektime-downloader/jobs.db`

### 7.1 session

```sql
CREATE TABLE session (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  gcid       TEXT NOT NULL,
  gcess      TEXT NOT NULL,
  status     TEXT NOT NULL DEFAULT 'unknown',
  updated_at TEXT NOT NULL
);
```

单用户约束：始终只有 `id = 1` 一行。

### 7.2 jobs

```sql
CREATE TABLE jobs (
  id              TEXT PRIMARY KEY,
  status          TEXT NOT NULL,
  status_reason   TEXT,
  idempotency_key TEXT UNIQUE,
  request_json    TEXT NOT NULL,
  course_json     TEXT,
  progress_json   TEXT NOT NULL DEFAULT '{}',
  error_json      TEXT,
  download_folder TEXT,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  started_at      TEXT,
  finished_at     TEXT
);

CREATE INDEX idx_jobs_status ON jobs(status);
CREATE INDEX idx_jobs_created_at ON jobs(created_at DESC);
```

### 7.3 job_articles

```sql
CREATE TABLE job_articles (
  job_id      TEXT NOT NULL,
  aid         INTEGER NOT NULL,
  title       TEXT NOT NULL,
  status      TEXT NOT NULL DEFAULT 'pending',
  files_json  TEXT NOT NULL DEFAULT '[]',
  error_json  TEXT,
  updated_at  TEXT NOT NULL,
  PRIMARY KEY (job_id, aid),
  FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
```

### 7.4 任务 ID 格式

使用 `job_` + ULID（或 UUID v7），保证可排序与唯一。

---

## 8. 核心模块设计

### 8.1 internal/service/

从 `internal/fsm/runner.go` 抽取，不依赖 `promptui` / `spinner`。

```go
// ProductType 对应 ui.ProductTypeSelectOption 的数据部分
type ProductType struct { ... }

type LookupRequest struct {
    ProductType string
    ProductID   int
    Enterprise  bool
}

type DownloadRequest struct {
    ProductType string
    ProductID   int
    Enterprise  bool
    Mode        string // all | articles | single_video
    ArticleIDs  []int
    Options     DownloadOptions
}

type ArticleProgress struct {
    AID    int
    Title  string
    Phase  string
    Status string // pending|running|completed|skipped|failed
    Files  []string
    Error  *APIError
}

type ProgressReporter interface {
    OnArticleStart(aid int, title, phase string)
    OnArticleComplete(aid int, files []string)
    OnArticleSkipped(aid int, reason string)
    OnArticleFailed(aid int, err error)
}

type DownloadService struct { ... }

func (s *DownloadService) ListProductTypes(enterprise bool) []ProductType
func (s *DownloadService) LookupCourse(ctx, req LookupRequest) (geektime.Course, error)
func (s *DownloadService) ExecuteDownload(ctx, course geektime.Course, req DownloadRequest, reporter ProgressReporter) error
```

`ExecuteDownload` 内部调用现有 `CourseDownloader`，为其增加 `ProgressReporter` 回调钩子。

### 8.2 internal/job/

```go
type Store interface {
    CreateJob(ctx, req CreateJobInput) (*Job, error)
    GetJob(ctx, id string) (*Job, error)
    ListJobs(ctx, filter ListFilter) ([]*Job, error)
    UpdateJobStatus(ctx, id, status, reason string, err *APIError) error
    UpdateProgress(ctx, id string, progress Progress, articles []ArticleProgress) error
    FindByIdempotencyKey(ctx, key string) (*Job, error)
}

type Worker struct {
    store   Store
    service *service.DownloadService
    // 单 goroutine，串行执行任务
}

func (w *Worker) Start(ctx context.Context)
func (w *Worker) Enqueue(jobID string)
```

Worker 行为：

1. 从 DB 取最早 `pending` 任务
2. 标记 `running`，调用 `LookupCourse` + `ExecuteDownload`
3. 通过 `ProgressReporter` 实时写 DB
4. 遇 `ErrAuthFailed` → `waiting_auth`，停止 Worker 当前任务（不继续队列，等 Cookie 恢复）
5. 遇 `ErrGeekTimeRateLimit` → `waiting_rate_limit`
6. 其他致命错误 → `failed`
7. 正常结束 → `completed`

### 8.3 internal/api/

- 路由注册（推荐 `chi` 或标准库 `net/http` + `http.ServeMux` Go 1.22+ 路由）
- 中间件：API Key 校验、请求日志、panic recover
- Handler 薄层，委托给 `job.Store` + `service.DownloadService` + `AuthManager`
- `openapi.yaml` 放在 `api/openapi.yaml`

### 8.4 internal/auth/

```go
type Manager struct {
    store  SessionStore
    client atomic.Pointer[geektime.Client]
}

func (m *Manager) GetClient() *geektime.Client
func (m *Manager) UpdateCookies(ctx, gcid, gcess string) (*SessionStatus, error)
func (m *Manager) Status(ctx) SessionStatus
```

### 8.5 CourseDownloader 改动（最小）

在 `internal/course/downloader.go` 增加可选字段：

```go
type CourseDownloader struct {
    // 现有字段...
    progressReporter service.ProgressReporter // 可为 nil
}
```

在 `downloadTextArticle` / `downloadVideoArticle` 关键节点调用 reporter。CLI 模式下 reporter 为 nil，行为不变。

---

## 9. Agent 集成指南

### 9.1 推荐轮询策略

| 任务状态 | 轮询间隔 |
|----------|----------|
| `pending` / `running` | 5–15 秒 |
| `waiting_auth` / `waiting_rate_limit` | 30–60 秒 |
| `completed` / `failed` / `cancelled` | 停止轮询 |

### 9.2 典型 Agent 工作流

```
1. GET  /health                          → 确认服务可用
2. PUT  /session/cookies                 → 注入 Cookie（若未在启动时提供）
3. POST /courses/lookup                  → 确认课程与文章列表
4. POST /downloads                       → 创建任务，拿到 job_id
5. loop GET /downloads/{id}              → 监控进度
6. if status == waiting_auth:
     → 向用户索取 Cookie
     → PUT /session/cookies
     → POST /downloads/{id}/retry
7. if status == completed:
     → 读取 download_folder，交付文件
```

### 9.3 OpenAPI

提供 `api/openapi.yaml`，Agent 框架可据此自动生成 tool definition。首版手写 spec，与实现对齐。

---

## 10. 安全

| 项 | 措施 |
|----|------|
| 网络 | 默认 `127.0.0.1`，文档明确禁止 `0.0.0.0` 除非用户明确需要 |
| API Key | 启动参数或 `GEEKTIME_DL_API_KEY` 环境变量 |
| Cookie | 存 SQLite，日志脱敏（仅打印前 4 位） |
| 文件系统 | 下载目录由配置限定，API 不接受任意路径（防目录遍历，`download_folder` 为空则用默认，非空须为默认目录的子路径或等于默认目录） |

---

## 11. 测试策略

| 层级 | 内容 |
|------|------|
| 单元测试 | 错误码映射、状态机转换、idempotency |
| 集成测试 | SQLite Store CRUD、API handler（httptest） |
| 手工测试 | 真实 Cookie + 小课程下载全流程 |
| CLI 回归 | 确保 FSM 模式行为不变 |

---

## 12. 实现阶段

### Phase 1 — Service 层抽取（P0）

- 新建 `internal/service/`
- FSM 改为调用 Service
- `CourseDownloader` 加 `ProgressReporter`
- CLI 回归测试通过

### Phase 2 — Job 存储与 Worker（P0）

- SQLite schema + migration
- `internal/job/store.go` + `worker.go`
- 单 Worker 串行执行

### Phase 3 — HTTP API（P0）

- `geektime-downloader serve` 子命令
- 全部 P0 端点
- `api/openapi.yaml`

### Phase 4 — 增强（P1）

- 服务重启恢复
- `idempotency_key` 去重
- 更细粒度 `phase` 上报

### Phase 5 — 可选（P2）

- Webhook 回调
- MCP Server tools 包装

---

## 13. 文件结构（新增/修改）

```
cmd/
  root.go              # 注册 serve 子命令
  serve.go             # 新增

internal/
  api/
    server.go
    handlers.go
    middleware.go
    errors.go
  auth/
    manager.go
  job/
    store.go
    worker.go
    model.go
    errors.go
  service/
    download.go
    product_types.go
    progress.go
  fsm/runner.go        # 重构：调用 service
  course/downloader.go # 加 ProgressReporter

api/
  openapi.yaml

docs/superpowers/
  specs/2026-07-02-api-server-design.md   # 本文档
  plans/2026-07-02-api-server.md           # 实现计划（下一步）
```

---

## 14. 开放问题（已决议）

| 问题 | 决议 |
|------|------|
| 单用户 vs 多用户 | **单用户本地（A）** |
| 任务队列 | 单 Worker 串行，保持现有限流策略 |
| 持久化 | SQLite |
| CLI 保留 | 是，共用 Service 层 |

---

## 15. 评审清单

- [ ] API 路径与字段命名是否符合 Agent 使用习惯
- [ ] 任务状态机是否覆盖 Cookie 过期 / 限流场景
- [ ] `waiting_auth` 时是否暂停整个 Worker（推荐：是，避免后续任务连续失败）
- [ ] Phase 划分与优先级是否合理
