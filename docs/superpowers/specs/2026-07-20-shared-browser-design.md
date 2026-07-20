# 进程级长驻浏览器修复 PDF 反爬拦截设计

**日期：** 2026-07-20
**状态：** 已实现（commit 3120ebf, 55f8a23, 8f04458, cef48a4）
**适用范围：** `geektime-downloader serve` 本地 HTTP API 服务（PDF/chromedp 层）

---

## 1. 背景与问题

v0.14.2 修复了 452 误判（API 层），但日志暴露了一个更深的、浏览器层的根因。

### 1.1 关键日志序列（2026-07-20 `/tmp/geektime-serve.log`）

```
11:25:40  Auth request start                       ← 手动刷新 cookie
11:25:40  /serv/v3/column/info        → 200
11:25:41  /serv/v1/column/articles    → 200
11:25:41  /serv/v1/article            → 200         ← 同一份 cookie，API 全 200
11:25:42  Begin download article pdf
11:25:43  Article page load failed: net::ERR_INVALID_AUTH_CREDENTIALS   ← chromedp 打开页面瞬间失败
```

### 1.2 根因（坐实）

**不是 cookie 失效，也不是请求频率——是 chromedp 浏览器实例被极客时间前端反爬识别为非法客户端。**

证据闭环：
1. **同一时刻、同一份 cookie**：resty API 调用 200，chromedp 打开 `/column/article/{aid}` 页面瞬间 401（`ERR_INVALID_AUTH_CREDENTIALS`）。差别只在 resty 是纯 HTTP 带 cookie，chromedp 是完整 Chrome 进程。
2. `pdf.go` 现在**每篇文章 `chromedp.NewContext` 新建一个 Chrome**（每次新 `ExecAllocator` + 新 temp `--user-data-dir`，已核实 chromedp 源码 `chromedp.go:122` + `allocate.go:149`）。极客时间前端反爬检测这种"无历史、无指纹、反复新建"的浏览器实例 → 直接 401 拒绝。
3. 只有 PDF 环节开浏览器（MD/audio/video 走 API 不开浏览器），所以只有 PDF 挂——解释了"带 PDF 输出的课程才失败"。
4. 批量下载短时间内开几十个新 Chrome → 反爬 flag 累积加深 → 越撞越难恢复。

### 1.3 v0.14.2 已做的（保留，仍然正确且必要）

- 452 → `ErrGeekTimeRateLimit`（API 层反爬，复用 worker 冷却）。
- resty 451/452/5xx 重试。
- PDF `EventLoadingFailed` 监听 → fast-fail 成限流（日志 `11:25:43` 已生效，秒级失败而非 60s 超时）。
- `--interval` 默认 3s。

这些处理的是 **API 层**问题；本 spec 处理的是 **浏览器层**问题，与 v0.14.2 互补。

### 1.4 为什么原版 CLI 不易遇到

交互式一门一门慢慢下，新浏览器开得少、间隔大，反爬 flag 不累积。serve 批量模式短时间开几十个新 Chrome 才持续触发。

---

## 2. 目标与非目标

### 2.1 目标

- serve 进程级别维护**一个长驻 Chrome**，所有 job/文章共用，每篇文章开新 tab、用完关 tab。
- 设备指纹稳定（同一 user-data-dir、连续浏览器历史、持久 cookie jar），极客时间不再判定"可疑新设备"。
- 浏览器崩溃/退出时能自愈重建，不卡死整个 serve。
- 保留 v0.14.2 的所有修复。

### 2.2 非目标（YAGNI）

- 不做多浏览器池（worker 单线程，无需并发 tab）。
- 不改 cookie 构造（仍 GCID+GCESS，浏览器 jar 自然接收 SERVERID 等）。
- 不改 DB schema、不改状态机、不改 CLI FSM 行为。
- 不改 worker 调度（仍单 job 串行）。

---

## 3. 已决议的设计分叉

| 分叉 | 决议 | 理由 |
|------|------|------|
| 浏览器生命周期范围 | **进程级单浏览器** | serve 启动时起一个 Chrome，所有 job/文章共用；进程退出时关。复用率最高，指纹最稳。worker 单线程无并发 tab 冲突。 |
| tab 生命周期 | **每篇文章开 tab、用完关** | 复用 chromedp 原生模型：child `NewContext(browserCtx)` 开 tab，`cancel()` 只关 tab 不关浏览器（已核实 `chromedp.go:176` `c.first==false` 分支）。 |
| 浏览器崩溃恢复 | **惰性重建** | 下次取浏览器时发现已死，重建一个。不主动健康检查（YAGNI）。 |
| user-data-dir | **固定目录** | `~/.config/geektime-downloader/chrome-profile/`。持久化指纹/历史/cookie jar，重启 serve 也延续。 |

---

## 4. 架构概览

```
serve 启动
  └─ BrowserPool.New()  ← 持有 allocatorCtx，不立即起浏览器（惰性）
                          暴露 WithBrowser(ctx, fn) 接口

worker.runJob (单线程)
  └─ DownloadAll → 每篇文章
       └─ pdf.PrintArticlePageToPDF(ctx, ...)
            └─ BrowserPool.WithBrowser(parentCtx, func(browserCtx) error {
                   // browserCtx 已绑定长驻 Chrome；首调惰性 allocate
                   tabCtx, cancel := chromedp.NewContext(browserCtx)  // 新 tab
                   defer cancel()  // 只关 tab
                   ... Navigate / waitArticleReady / printToPDF ...
               })
            └─ EventLoadingFailed 监听（v0.14.2 保留）→ fast-fail 限流

serve 退出 / ctx cancel
  └─ BrowserPool.Close() → chromedp.Cancel → 优雅关 Chrome
```

---

## 5. 组件设计

### 5.1 `internal/pdf/browser.go`（新建）—— BrowserPool

进程级长驻浏览器管理器。职责单一：持有一个 chromedp browser context，惰性分配，提供 `WithBrowser` 在长驻浏览器上开 tab 执行操作，崩溃时重建。

```go
package pdf

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/chromedp/chromedp"
)

// BrowserPool maintains a single long-lived Chrome browser for the serve
// process. All PDF jobs share it via per-article tabs, so the browser's
// device fingerprint, history and cookie jar stay stable — avoiding the
// per-article new-browser anti-bot blocks (ERR_INVALID_AUTH_CREDENTIALS)
// that came from spawning a fresh Chrome per article.
type BrowserPool struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc

	mu         sync.Mutex
	browserCtx context.Context // current live browser; nil when none/crashed
	browserCxl context.CancelFunc
}

// NewBrowserPool creates a pool. The browser is allocated lazily on first use.
// allocCtx should be the serve process context so the browser dies with serve.
func NewBrowserPool(allocCtx context.Context) *BrowserPool {
	return &BrowserPool{allocCtx: allocCtx}
}

// WithBrowser runs fn against the long-lived browser. fn receives a browser
// context; calling chromedp.NewContext on it opens a new tab (not a new
// browser). If the live browser has died, a new one is allocated first.
func (p *BrowserPool) WithBrowser(ctx context.Context, fn func(browserCtx context.Context) error) error {
	bc, err := p.getOrCreate(ctx)
	if err != nil {
		return err
	}
	// Run fn on the live browser context. A child NewContext inside fn
	// creates a tab; cancelling it closes only the tab.
	return fn(bc)
}

func (p *BrowserPool) getOrCreate(ctx context.Context) (context.Context, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.browserCtx != nil {
		// cheap liveness check: if the browser context is done, it died.
		select {
		case <-p.browserCtx.Done():
			p.browserCtx = nil
		default:
			return p.browserCtx, nil
		}
	}
	// allocate lazily: NewContext on the allocator parent launches Chrome
	// on first Run. We wrap in a child context bound to the caller's ctx
	// lifetime only for the allocate step; the browser itself lives on
	// allocCtx.
	browserCtx, cancel := chromedp.NewContext(p.allocCtx)
	// force allocate now so failures surface here, not in the first tab.
	allocCtx, allocCancel := context.WithTimeout(ctx, 60*time.Second)
	defer allocCancel()
	if err := chromedp.Run(allocCtx); err != nil {
		cancel()
		return nil, err
	}
	p.browserCtx = browserCtx
	p.browserCxl = cancel
	return browserCtx, nil
}

// Close shuts down the browser. Called on serve exit.
func (p *BrowserPool) Close() {
	p.mu.Lock()
	bc := p.browserCtx
	p.browserCtx, p.browserCxl = nil, nil
	p.mu.Unlock()
	if bc != nil {
		// chromedp.Cancel reads the browser from bc via FromContext and
		// gracefully closes it (first==true path). Attach a timeout so a
		// hung browser doesn't block serve shutdown.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		// bc already carries the browser; chromedp.Cancel uses FromContext
		// on the passed ctx, so pass a context whose value is bc.
		_ = chromedp.Cancel(bc)
		_ = closeCtx
	}
}
```

> **实现注意**：`chromedp.Cancel(ctx)` 取 `FromContext(ctx)` 拿 browser，所以必须传 `bc`（browserCtx，内含 browser）而非任意 ctx。`closeCtx` 仅用于超时控制——但 `chromedp.Cancel` 不读传入 ctx 的 deadline，它读的是 `FromContext`。若需超时，用 `chromedp.Cancel` 后 `select { <-done; <-time.After }`。实现时先验证 `chromedp.Cancel(bc)` 的行为，若 Cancel 不尊重超时，改用 `cancel()`（browserCxl）+ 等待 `allocated` channel。

**Allocator options**：默认 allocator（`--no-sandbox` 在 root 下自动加，已核实 `allocate.go:159`）。但需要**固定 user-data-dir**，所以在 `NewBrowserPool` 里用 `chromedp.NewExecAllocator(allocCtx, opts...)` 显式建 allocator，opts 含 `chromedp.UserDataDir(profileDir)` + 默认选项。profileDir = `filepath.Join(os.UserConfigDir(), "geektime-downloader", "chrome-profile")`。

### 5.2 `internal/pdf/pdf.go` —— 改 PrintArticlePageToPDF 用 pool

当前每篇 `chromedp.NewContext(parentCtx)` 新建浏览器。改为通过注入的 `*BrowserPool` 开 tab：

```go
func PrintArticlePageToPDF(parentCtx context.Context,
	article geektime.Article,
	dir string,
	cookies []*http.Cookie,
	cfg *config.AppConfig,
	pool *BrowserPool,   // 新增
) error {
	// ... rateLimit, aid, pdfFileName, commentsDone 不变 ...

	// listener 注册需要在 tab 上下文上做，所以整体放进 WithBrowser 回调。
	err := pool.WithBrowser(parentCtx, func(browserCtx context.Context) error {
		chromeCtx, chromeCancel := chromedp.NewContext(browserCtx) // 新 tab
		defer chromeCancel()

		timeoutCtx, timeoutCancel := context.WithTimeout(chromeCtx, time.Duration(cfg.PrintPDFTimeoutSeconds)*time.Second)
		defer timeoutCancel()

		listenerCtx, listenerCtxCancel := context.WithCancel(timeoutCtx)
		defer listenerCtxCancel()

		// ... listener 不变（含 v0.14.2 的 EventLoadingFailed）...
		chromedp.ListenTarget(listenerCtx, listener)

		tasks := chromedp.Tasks{ /* 不变：network.Enable, Emulate, setCookies, Navigate, waitArticleReady */ }
		// ... comments/printToPDF 任务拼接不变 ...

		return chromedp.Run(timeoutCtx, tasks)
	})

	// 错误映射不变（rateLimit → ErrGeekTimeRateLimit）
	if err != nil { ... }
	return nil
}
```

**关键**：`rateLimit` 闭包变量现在在 `WithBrowser` 回调内，fast-fail 的 `timeoutCancel()`/`listenerCtxCancel()` 仍然作用于 tab 的 `timeoutCtx`，行为与 v0.14.2 一致。回调返回后外层做 `rateLimit` 判断——为此 `rateLimit` 需提升为回调可写、外层可读（用 `*bool` 或闭包返回值）。实现时让回调返回 `(error, bool rateLimit)` 或用外层变量指针。

### 5.3 `internal/course/downloader.go` —— 透传 pool

`CourseDownloader` 持有 `pool *pdf.BrowserPool`（由 serve 注入，可为 nil）。`downloadTextArticle` 调 `pdf.PrintArticlePageToPDF` 时传入。

- `pool == nil`（CLI FSM 模式）时：`PrintArticlePageToPDF` 内部 fallback 为旧的"每篇新浏览器"行为，保持 CLI 不变。即 `if pool == nil { pool = nil; 内部 NewContext(parentCtx) }`——实现时在 pdf.go 里 `if pool != nil { 走 WithBrowser } else { 走旧路径 }`。

### 5.4 `cmd/serve.go` —— 创建 pool 并注入

```go
// serve RunE 内，worker 启动前：
browserPool := pdf.NewBrowserPool(cmd.Context())
defer browserPool.Close()
// 透传给 DownloadService → CourseDownloader
dlSvc := service.NewDownloadService(&cfg, authMgr.GetClient(), browserPool)
```

`cmd.Context()` 是 cobra 的根 context，serve 进程退出时 cancel → `allocCtx` 取消 → `Close` 关浏览器。`defer browserPool.Close()` 双保险。

### 5.5 透传链路

`service.DownloadService` 加 `pool *pdf.BrowserPool` 字段；`NewDownloadService` 加参数；`ExecuteDownload` 构造 `CourseDownloader` 时传入。`course.NewCourseDownloader` 加 `pool` 参数。

CLI 路径（`root.go RunE`）传 `nil`。

---

## 6. 错误处理

- **浏览器分配失败**（Chrome 没装/起不来）：`WithBrowser` 返回错误 → `PrintArticlePageToPDF` 返回该错误 → `apperr.MapError` default → `INTERNAL_ERROR, retryable`。agent 可 retry。CLI 模式不受影响（pool=nil 走旧路径，行为同 v0.14.2）。
- **tab 内页面加载失败**（`ERR_INVALID_AUTH_CREDENTIALS`）：v0.14.2 的 `EventLoadingFailed` 监听不变 → fast-fail → `ErrGeekTimeRateLimit` → worker 冷却。复用浏览器后此类失败应大幅减少。
- **浏览器崩溃**：`getOrCreate` 检测 `browserCtx.Done()` → 重建。下次 `WithBrowser` 自动恢复。
- **serve 退出**：`Close` → `chromedp.Cancel(bc)` 优雅关 Chrome。

---

## 7. 测试策略

| 层级 | 内容 |
|------|------|
| `pdf/browser_test.go` | `WithBrowser` 首调惰性分配、再调复用同一 browser（断言只 allocate 一次）、`Close` 后 browserCtx.Done；浏览器不可用时返回错误（`chromeAvailable` skip 同 v0.14.2） |
| `pdf` 回归 | `PrintArticlePageToPDF` 在 pool!=nil 时走 tab、pool==nil 时走旧路径，两条路径都跑通（smoke）；`EventLoadingFailed` fast-fail 用例（v0.14.2 已有）仍通过 |
| 集成（手动，Ubuntu） | 部署后批量下多门带 PDF 课程，观察 `ERR_INVALID_AUTH_CREDENTIALS` 是否消失、PDF 是否成功生成 |
| CLI 回归 | pool=nil 路径行为不变 |

---

## 8. 配置

无新 flag。user-data-dir 路径固定为 `~/.config/geektime-downloader/chrome-profile/`（代码常量）。如需可配，后续再加（YAGNI）。

---

## 9. 文件清单

```
internal/pdf/browser.go         新建：BrowserPool
internal/pdf/browser_test.go    新建：BrowserPool 测试
internal/pdf/pdf.go             改 PrintArticlePageToPDF 用 pool；pool==nil fallback 旧路径
internal/course/downloader.go   持有 pool，透传给 PrintArticlePageToPDF
internal/service/download.go    持有 pool，透传给 CourseDownloader
cmd/serve.go                    创建 BrowserPool，注入，defer Close
cmd/root.go                     CLI 路径传 nil
docs/superpowers/specs/2026-07-20-shared-browser-design.md  本 spec
```

---

## 10. 验证标准

- 部署后 Ubuntu 上批量下多门带 PDF 课程：`ERR_INVALID_AUTH_CREDENTIALS` 不再出现，PDF 正常生成。
- serve 运行期间只起一个 Chrome 进程（`ps` 确认），每篇文章是 tab 不是新进程。
- serve 退出后 Chrome 进程消失。
- 浏览器崩溃后下一篇文章自动恢复（手动 kill Chrome 验证）。
- CLI 模式（pool=nil）行为与 v0.14.2 一致，无回归。
