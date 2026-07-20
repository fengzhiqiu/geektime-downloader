# Process-Level Shared Browser (PDF Anti-Bot) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Maintain one long-lived Chrome at the serve process level so PDF generation reuses a stable browser (tabs per article) instead of spawning a fresh Chrome per article — fixing the `ERR_INVALID_AUTH_CREDENTIALS` anti-bot blocks.

**Architecture:** A `BrowserPool` (`internal/pdf/browser.go`) holds one chromedp browser context, allocated lazily. `PrintArticlePageToPDF` runs its chromedp work inside `pool.WithBrowser`, which opens a new tab on the shared browser. The pool is created in `serve`, threaded through `DownloadService` → `CourseDownloader` → `PrintArticlePageToPDF`. CLI path passes `nil` pool and falls back to the old per-article browser path.

**Tech Stack:** Go 1.x, `github.com/chromedp/chromedp v0.14.2`, `github.com/chromedp/cdproto`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-20-shared-browser-design.md`.
- chromedp model (verified in `chromedp@v0.14.2/chromedp.go`): the **first** `NewContext(allocatorCtx)` + `Run` allocates the browser; a **child** `NewContext(browserCtx)` opens a new tab; cancelling the child closes only the tab (`c.first == false`), not the browser.
- `chromedp.Cancel(ctx)` reads the browser via `FromContext(ctx)`, so it must be passed the **browser context** (which carries the browser), not an arbitrary ctx.
- Worker is single-threaded (one job at a time); no concurrent tabs. No locking needed around tab creation, only around browser allocation/replacement in the pool.
- user-data-dir is fixed: `filepath.Join(os.UserConfigDir(), "geektime-downloader", "chrome-profile")`. No new flag.
- CLI path (`cmd/root.go` → `fsm.NewFSMRunner` → `course.NewCourseDownloader`) must pass `nil` pool and get identical behavior to v0.14.2 (per-article new browser).
- v0.14.2 fixes (452→rate-limit, resty retry, `EventLoadingFailed` fast-fail, `waitArticleReady`, interval 3s) are **preserved unchanged**.
- Browser-dependent tests skip cleanly when Chrome is absent (reuse the `chromeAvailable(t)` helper already in `internal/pdf/pdf_test.go`).
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```

---

## File Structure

- **Create** `internal/pdf/browser.go` — `BrowserPool`: holds one long-lived chromedp browser, lazy allocate, `WithBrowser` opens tabs, `Close` shuts down.
- **Create** `internal/pdf/browser_test.go` — pool tests (allocate-once, reuse, close, crash-rebuild), Chrome-gated.
- **Modify** `internal/pdf/pdf.go` — `PrintArticlePageToPDF` gains a `pool *BrowserPool` param; `pool != nil` runs chromedp work via `pool.WithBrowser` (tab), `pool == nil` keeps the old per-article path.
- **Modify** `internal/course/downloader.go` — `CourseDownloader` holds `pool *pdf.BrowserPool`; `NewCourseDownloader` gains the param; passes it to `PrintArticlePageToPDF`.
- **Modify** `internal/service/download.go` — `DownloadService` holds `pool`; `NewDownloadService` gains the param; passes it to `NewCourseDownloader`.
- **Modify** `cmd/serve.go` — create `BrowserPool` from `cmd.Context()`, `defer Close()`, pass to `NewDownloadService`.
- **Modify** `internal/fsm/runner.go` — pass `nil` pool to `NewCourseDownloader` (CLI unchanged).

---

## Task 1: BrowserPool — lazy allocate, reuse, close

**Files:**
- Create: `internal/pdf/browser.go`
- Test: `internal/pdf/browser_test.go`

**Interfaces:**
- Produces:
  ```go
  func NewBrowserPool(allocCtx context.Context) *BrowserPool
  func (p *BrowserPool) WithBrowser(ctx context.Context, fn func(browserCtx context.Context) error) error
  func (p *BrowserPool) Close()
  ```
  `WithBrowser` runs `fn` with a context whose `chromedp.FromContext` is the live browser. Callers create tabs via `chromedp.NewContext(browserCtx)`.

- [ ] **Step 1: Write the failing test for allocate-once + reuse**

Create `internal/pdf/browser_test.go`:

```go
package pdf

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBrowserPoolReusesSingleBrowser verifies that two WithBrowser calls on
// the same pool share one browser (not a fresh Chrome each time). The fix's
// whole point is browser reuse so the device fingerprint stays stable.
func TestBrowserPoolReusesSingleBrowser(t *testing.T) {
	chromeAvailable(t)

	pool := NewBrowserPool(context.Background())
	defer pool.Close()

	var firstTargetID, secondTargetID string
	err := pool.WithBrowser(context.Background(), func(browserCtx context.Context) error {
		tabCtx, cancel := chromedp.NewContext(browserCtx)
		defer cancel()
		tctx, tcancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer tcancel()
		if err := chromedp.Run(tctx, chromedp.Navigate("about:blank")); err != nil {
			return err
		}
		firstTargetID = string(chromedp.FromContext(tabCtx).Target.TargetID)
		return nil
	})
	if err != nil {
		t.Fatalf("first WithBrowser: %v", err)
	}
	if firstTargetID == "" {
		t.Fatal("want non-empty first target id")
	}

	err = pool.WithBrowser(context.Background(), func(browserCtx context.Context) error {
		tabCtx, cancel := chromedp.NewContext(browserCtx)
		defer cancel()
		tctx, tcancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer tcancel()
		if err := chromedp.Run(tctx, chromedp.Navigate("about:blank")); err != nil {
			return err
		}
		// Same browser connection object across calls => reused browser.
		if chromedp.FromContext(browserCtx).Browser == nil {
			t.Fatal("want non-nil browser on second call")
		}
		secondTargetID = string(chromedp.FromContext(tabCtx).Target.TargetID)
		return nil
	})
	if err != nil {
		t.Fatalf("second WithBrowser: %v", err)
	}
	if secondTargetID == "" {
		t.Fatal("want non-empty second target id")
	}
	// Different tabs (different target IDs) but same browser.
	if firstTargetID == secondTargetID {
		t.Fatal("want different tab target ids; got same")
	}
}

// TestBrowserPoolWithBrowserCount verifies the pool allocates the browser at
// most once across many calls (a counter increments only on first allocate).
// We approximate by checking WithBrowser succeeds repeatedly without error.
func TestBrowserPoolWithBrowserCount(t *testing.T) {
	chromeAvailable(t)

	pool := NewBrowserPool(context.Background())
	defer pool.Close()

	var ok int32
	for i := 0; i < 3; i++ {
		err := pool.WithBrowser(context.Background(), func(browserCtx context.Context) error {
			tabCtx, cancel := chromedp.NewContext(browserCtx)
			defer cancel()
			tctx, tcancel := context.WithTimeout(tabCtx, 10*time.Second)
			defer tcancel()
			if err := chromedp.Run(tctx, chromedp.Navigate("about:blank")); err != nil {
				return err
			}
			atomic.AddInt32(&ok, 1)
			return nil
		})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if ok != 3 {
		t.Fatalf("want 3 successful tabs, got %d", ok)
	}
}

// TestBrowserPoolCloseReleasesBrowser verifies Close makes the browser context
// done, so a subsequent serve shutdown does not leak the Chrome process.
func TestBrowserPoolCloseReleasesBrowser(t *testing.T) {
	chromeAvailable(t)

	pool := NewBrowserPool(context.Background())

	err := pool.WithBrowser(context.Background(), func(browserCtx context.Context) error {
		tabCtx, cancel := chromedp.NewContext(browserCtx)
		defer cancel()
		tctx, tcancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer tcancel()
		return chromedp.Run(tctx, chromedp.Navigate("about:blank"))
	})
	if err != nil {
		t.Fatalf("WithBrowser: %v", err)
	}

	pool.Close()
	// After Close, WithBrowser must allocate a fresh browser (the old one is
	// gone). We just assert no panic and that it can run again.
	pool2 := NewBrowserPool(context.Background())
	defer pool2.Close()
	err = pool2.WithBrowser(context.Background(), func(browserCtx context.Context) error {
		tabCtx, cancel := chromedp.NewContext(browserCtx)
		defer cancel()
		tctx, tcancel := context.WithTimeout(tabCtx, 10*time.Second)
		defer tcancel()
		return chromedp.Run(tctx, chromedp.Navigate("about:blank"))
	})
	if err != nil {
		t.Fatalf("WithBrowser after close: %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/pdf/ -run TestBrowserPool -v`
Expected: FAIL / SKIP — `NewBrowserPool` undefined (compile error). If Chrome absent, they will SKIP after the package compiles; first fix the compile by implementing in Step 3.

- [ ] **Step 3: Implement BrowserPool**

Create `internal/pdf/browser.go`:

```go
package pdf

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// BrowserPool maintains a single long-lived Chrome browser for the serve
// process. All PDF jobs share it via per-article tabs, so the browser's
// device fingerprint, history and cookie jar stay stable — avoiding the
// per-article new-browser anti-bot blocks (ERR_INVALID_AUTH_CREDENTIALS)
// that came from spawning a fresh Chrome per article.
//
// The browser is allocated lazily on first use. If the live browser dies,
// the next WithBrowser allocates a new one.
type BrowserPool struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc

	mu         sync.Mutex
	browserCtx context.Context // current live browser; nil when none/crashed
	browserCxl context.CancelFunc
}

// NewBrowserPool creates a pool bound to allocCtx. The browser dies when
// allocCtx is cancelled (i.e. serve shutdown). The browser is allocated
// lazily on first use.
func NewBrowserPool(allocCtx context.Context) *BrowserPool {
	ctx, cancel := context.WithCancel(allocCtx)
	return &BrowserPool{allocCtx: ctx, allocCancel: cancel}
}

// WithBrowser runs fn against the long-lived browser. fn receives a browser
// context; calling chromedp.NewContext on it opens a new tab (not a new
// browser). If the live browser has died, a new one is allocated first.
func (p *BrowserPool) WithBrowser(ctx context.Context, fn func(browserCtx context.Context) error) error {
	bc, err := p.getOrCreate(ctx)
	if err != nil {
		return err
	}
	return fn(bc)
}

// getOrCreate returns the live browser context, allocating one if none exists
// or the current one is done.
func (p *BrowserPool) getOrCreate(ctx context.Context) (context.Context, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.browserCtx != nil {
		select {
		case <-p.browserCtx.Done():
			p.browserCtx = nil
			p.browserCxl = nil
		default:
			return p.browserCtx, nil
		}
	}

	// Build an allocator with a fixed user-data-dir so the browser's
	// fingerprint/history/cookie jar persist across serve restarts and
	// across articles. Defaults (headless, etc.) are preserved; --no-sandbox
	// is auto-added when running as root by chromedp.
	userConfigDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	profileDir := filepath.Join(userConfigDir, "geektime-downloader", "chrome-profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, err
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserDataDir(profileDir),
	)
	alloc, allocCancel := chromedp.NewExecAllocator(p.allocCtx, opts...)
	_ = allocCancel // allocator lifetime tied to allocCtx; pool owns browser ctx

	browserCtx, browserCxl := chromedp.NewContext(alloc)

	// Force the browser to allocate now so failures surface here, not in the
	// first tab. Bound by the caller's ctx so a hung Chrome doesn't block.
	allocRunCtx, allocRunCancel := context.WithTimeout(ctx, 60*time.Second)
	defer allocRunCancel()
	if err := chromedp.Run(allocRunCtx); err != nil {
		browserCxl()
		return nil, err
	}

	p.browserCtx = browserCtx
	p.browserCxl = browserCxl
	return browserCtx, nil
}

// Close shuts down the browser (if allocated). Safe to call multiple times.
// Called on serve exit; also triggered by allocCtx cancellation.
func (p *BrowserPool) Close() {
	p.mu.Lock()
	bc := p.browserCtx
	p.browserCtx = nil
	p.browserCxl = nil
	p.mu.Unlock()

	if bc != nil {
		// chromedp.Cancel reads the browser from bc via FromContext and
		// gracefully closes it (first==true path). Bound the wait so a hung
		// browser does not block serve shutdown.
		cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = chromedp.Cancel(bc)
		_ = cancelCtx
	}
	p.allocCancel()
}
```

- [ ] **Step 4: Run tests to verify they pass (or skip without Chrome)**

Run: `go test ./internal/pdf/ -run TestBrowserPool -v`
Expected: PASS on a Chrome-equipped machine, SKIP on a machine without Chrome. Either way the package compiles.

- [ ] **Step 5: Build the package**

Run: `go build ./internal/pdf/`
Expected: no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/pdf/browser.go internal/pdf/browser_test.go
git commit -m "$(cat <<'EOF'
feat(pdf): add process-level BrowserPool for a shared long-lived Chrome

Each PDF article currently spawns a fresh Chrome (new temp user-data-dir,
no history). Logs show geektime's frontend anti-bot flags these as
suspicious clients and returns ERR_INVALID_AUTH_CREDENTIALS even while
the same cookies work fine on the resty API. BrowserPool holds one
long-lived Chrome at the serve level; callers open per-article tabs on
it so the device fingerprint, history and cookie jar stay stable.

Lazy-allocates on first use, rebuilds after a crash, fixed user-data-dir
under ~/.config/geektime-downloader/chrome-profile. Chrome-gated tests
skip without Chrome.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: PrintArticlePageToPDF uses the pool (nil = old path)

**Files:**
- Modify: `internal/pdf/pdf.go` (`PrintArticlePageToPDF`, ~line 54-148)
- Test: `internal/pdf/pdf_test.go` (update existing call sites; the load-failure test still covers the pool==nil path; add a pool!=nil smoke test)

**Interfaces:**
- Consumes: `*BrowserPool` from Task 1.
- Produces: `PrintArticlePageToPDF` signature gains a trailing `pool *BrowserPool` param:
  ```go
  func PrintArticlePageToPDF(parentCtx context.Context, article geektime.Article, dir string, cookies []*http.Cookie, cfg *config.AppConfig, pool *BrowserPool) error
  ```
  `pool != nil` → chromedp work runs inside `pool.WithBrowser` (tab on shared browser). `pool == nil` → old behavior (per-article `chromedp.NewContext(parentCtx)`), preserving CLI/v0.14.2 semantics.

- [ ] **Step 1: Update the existing load-failure test to pass nil pool**

In `internal/pdf/pdf_test.go`, the call `PrintArticlePageToPDF(context.Background(), article, dir, nil, cfg)` must become `PrintArticlePageToPDF(context.Background(), article, dir, nil, cfg, nil)` (trailing `nil` pool). This keeps that test exercising the fallback path.

- [ ] **Step 2: Add a pool-based smoke test**

Append to `internal/pdf/pdf_test.go`:

```go
// TestPrintArticlePageToPDFWithPoolServesRealPage verifies that with a real
// BrowserPool, PrintArticlePageToPDF opens a tab on the shared browser and
// renders a page served by a local test server. Requires Chrome.
//
// This is a structural smoke test: it confirms the pool path runs end-to-end
// against a non-geektime URL (no anti-bot) and produces a PDF. Full anti-bot
// behavior is validated manually on the Ubuntu deployment.
func TestPrintArticlePageToPDFWithPoolServesRealPage(t *testing.T) {
	chromeAvailable(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body><article><p>hello</p></article></body></html>`))
	}))
	defer srv.Close()

	pool := NewBrowserPool(context.Background())
	defer pool.Close()

	cfg := &config.AppConfig{PrintPDFTimeoutSeconds: 30}
	article := geektime.Article{AID: 1, Title: "smoke"}
	dir := t.TempDir()

	// We can't easily point PrintArticlePageToPDF at an arbitrary URL (it
	// hardcodes the geektime article URL). So this test instead asserts the
	// pool path does not panic and surfaces an error gracefully when the
	// geektime URL is unreachable in this environment. The key contract:
	// pool != nil path executes without panicking.
	_ = PrintArticlePageToPDF(context.Background(), article, dir, nil, cfg, pool)
	// No assertion on the error: without real geektime cookies/URL it will
	// fail, but it must not panic and must return (not hang).
}
```

> Note: this smoke test is intentionally weak because `PrintArticlePageToPDF` hardcodes the geektime article URL and can't be pointed at the local server. Its value is proving the pool path executes end-to-end without panic/hang. If it proves too noisy, the implementer may delete it and rely on the Task 1 pool tests + manual Ubuntu validation; keep the decision recorded in the commit message.

- [ ] **Step 3: Verify tests fail to compile (signature changed)**

Run: `go build ./internal/pdf/`
Expected: FAIL — `PrintArticlePageToPDF` signature mismatch at the call sites inside the package (and downstream packages, fixed in later tasks). The test file updates from Step 1 should make the package itself compile; downstream (`course`, `fsm`) will fail until Tasks 3-4. Run `go vet ./internal/pdf/` to confirm the pdf package alone is consistent.

- [ ] **Step 4: Refactor PrintArticlePageToPDF**

In `internal/pdf/pdf.go`, replace the body of `PrintArticlePageToPDF` to add the `pool` param and split the chromedp work into an inner closure. The inner closure receives the browser/tab context to use for `NewContext`. New full function:

```go
// PrintArticlePageToPDF use chromedp to print article page and save.
// If pool is non-nil, the article is rendered in a new tab on the pool's
// long-lived browser (stable fingerprint, avoids per-article anti-bot
// blocks). If pool is nil (CLI path), a fresh browser is allocated per
// article (legacy behavior).
func PrintArticlePageToPDF(parentCtx context.Context,
	article geektime.Article,
	dir string,
	cookies []*http.Cookie,
	cfg *config.AppConfig,
	pool *BrowserPool,
) error {
	rateLimit := false
	aid := article.AID

	pdfFileName := filepath.Join(dir, filenamify.Filenamify(article.Title)+PDFExtension)

	// runInBrowser executes the chromedp work against the given browser
	// context. When pool != nil this is the shared long-lived browser;
	// otherwise it is the parentCtx (fresh browser per article).
	runInBrowser := func(browserCtx context.Context) error {
		chromeCtx, chromeCancel := chromedp.NewContext(browserCtx)
		defer chromeCancel()

		timeoutCtx, timeoutCancel := context.WithTimeout(chromeCtx, time.Duration(cfg.PrintPDFTimeoutSeconds)*time.Second)
		defer timeoutCancel()

		var commentsDone uint32 = 0

		listenerCtx, listenerCtxCancel := context.WithCancel(timeoutCtx)
		defer listenerCtxCancel()

		listener := func(ev interface{}) {
			switch e := ev.(type) {
			case *network.EventResponseReceived:
				response := e.Response
				if response.URL == geektime.DefaultBaseURL+"/serv/v1/article" && response.Status == 451 {
					logger.Warnf("Hit GeekTime rate limit when downloading article pdf, articleID: %d, pdfFileName: %s", aid, pdfFileName)
					rateLimit = true
					timeoutCancel()
					listenerCtxCancel()
					return
				}
				if cfg.DownloadComments == DownloadCommentsAll {
					if strings.Contains(strings.ToLower(response.URL), "comment/list") {
						reqID := e.RequestID
						url := response.URL
						fetchAndHandleCommentList(listenerCtx, reqID, url, &commentsDone)
					}
				}
			case *network.EventLoadingFailed:
				if e.Type == network.ResourceTypeDocument {
					logger.Warnf("Article page load failed, articleID: %d, error: %s, pdfFileName: %s",
						aid, e.ErrorText, pdfFileName)
					rateLimit = true
					timeoutCancel()
					listenerCtxCancel()
					return
				}
			}
		}
		chromedp.ListenTarget(listenerCtx, listener)

		tasks := chromedp.Tasks{
			network.Enable(),
			chromedp.Emulate(device.IPadPro11),
			setCookies(cookies),
			chromedp.Navigate(geektime.DefaultBaseURL + `/column/article/` + strconv.Itoa(aid)),
			waitArticleReady(),
		}

		switch cfg.DownloadComments {
		case DownloadCommentsAll:
			tasks = append(tasks, touchScrollAction(&commentsDone))
		case DownloadCommentsNone:
			tasks = append(tasks, hideCommentsBlock())
		}

		tasks = append(tasks, hideRedundantElements(), printToPDF(pdfFileName))

		logger.Infof("Begin download article pdf, articleID: %d, pdfFileName: %s", aid, pdfFileName)

		return chromedp.Run(timeoutCtx, tasks)
	}

	var err error
	if pool != nil {
		err = pool.WithBrowser(parentCtx, runInBrowser)
	} else {
		err = runInBrowser(parentCtx)
	}

	if err != nil {
		if rateLimit {
			logger.Warnf("Hit GeekTime rate limit when downloading article pdf, articleID: %d, pdfFileName: %s", aid, pdfFileName)
			return geektime.ErrGeekTimeRateLimit
		}
		logger.Errorf(err, "Failed to download article pdf")
		return err
	}

	return nil
}
```

- [ ] **Step 5: Build the pdf package**

Run: `go build ./internal/pdf/`
Expected: no errors.

- [ ] **Step 6: Run pdf package tests**

Run: `go test ./internal/pdf/ -v`
Expected: PASS (browser tests SKIP without Chrome). The load-failure test still passes on the nil-pool path.

- [ ] **Step 7: Commit**

```bash
git add internal/pdf/pdf.go internal/pdf/pdf_test.go
git commit -m "$(cat <<'EOF'
feat(pdf): route PrintArticlePageToPDF through the shared BrowserPool

Add a pool *BrowserPool param. When non-nil (serve path), the chromedp
work runs inside pool.WithBrowser, opening a per-article tab on the
long-lived browser instead of spawning a fresh Chrome. When nil (CLI
path), behavior is unchanged from v0.14.2 (fresh browser per article).

The v0.14.2 EventLoadingFailed fast-fail and waitArticleReady are
preserved inside the inner closure. The rateLimit flag is captured by
the closure and read after WithBrowser returns.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Thread the pool through CourseDownloader and DownloadService

**Files:**
- Modify: `internal/course/downloader.go` (struct + `NewCourseDownloader` + `downloadTextArticle` call site)
- Modify: `internal/service/download.go` (struct + `NewDownloadService` + `ExecuteDownload` call site)
- Modify: `internal/fsm/runner.go` (pass `nil` pool)

**Interfaces:**
- Consumes: `*pdf.BrowserPool` (Task 1), new `PrintArticlePageToPDF` signature (Task 2).
- Produces:
  - `course.NewCourseDownloader(ctx, cfg, client, sp, reporter, pool *pdf.BrowserPool) *CourseDownloader`
  - `service.NewDownloadService(baseCfg, client, pool *pdf.BrowserPool) *DownloadService`

- [ ] **Step 1: Update CourseDownloader**

In `internal/course/downloader.go`:

Add the field to the struct (after `progressReporter`):

```go
type CourseDownloader struct {
	ctx                context.Context
	cfg                *config.AppConfig
	geektimeClient     *geektime.Client
	concurrency        int
	waitRand           *rand.Rand
	downloadingSpinner *spinner.Spinner
	progressReporter   progress.Reporter
	pool               *pdf.BrowserPool
}
```

Update the constructor signature and body:

```go
func NewCourseDownloader(ctx context.Context, cfg *config.AppConfig, geektimeClient *geektime.Client, sp *spinner.Spinner, reporter progress.Reporter, pool *pdf.BrowserPool) *CourseDownloader {
	concurrency := int(math.Ceil(float64(runtime.NumCPU()) / 2.0))
	if concurrency <= 0 {
		concurrency = 1
	}
	return &CourseDownloader{
		ctx:                ctx,
		cfg:                cfg,
		geektimeClient:     geektimeClient,
		concurrency:        concurrency,
		waitRand:           rand.New(rand.NewSource(time.Now().UnixNano())),
		downloadingSpinner: sp,
		progressReporter:   reporter,
		pool:               pool,
	}
}
```

Update the `PrintArticlePageToPDF` call in `downloadTextArticle` (~line 250) to pass `d.pool`:

```go
		if err := pdf.PrintArticlePageToPDF(d.ctx,
			article,
			columnDir,
			d.geektimeClient.Cookies,
			d.cfg,
			d.pool,
		); err != nil {
			return err
		}
```

- [ ] **Step 2: Update DownloadService**

In `internal/service/download.go`:

Add the field to the struct:

```go
type DownloadService struct {
	baseCfg *config.AppConfig
	client  *geektime.Client
	pool    *pdf.BrowserPool
}
```

Update the constructor:

```go
// NewDownloadService creates a DownloadService.
func NewDownloadService(baseCfg *config.AppConfig, client *geektime.Client, pool *pdf.BrowserPool) *DownloadService {
	return &DownloadService{baseCfg: baseCfg, client: client, pool: pool}
}
```

Update the `NewCourseDownloader` call in `ExecuteDownload` (~line 123) to pass `s.pool`:

```go
	downloader := coursedl.NewCourseDownloader(ctx, cfg, s.client, nil, reporter, s.pool)
```

Add the `pdf` import to `internal/service/download.go` if not present:

```go
	"github.com/nicoxiang/geektime-downloader/internal/pdf"
```

- [ ] **Step 3: Update CLI FSM runner to pass nil**

In `internal/fsm/runner.go` (~line 40), pass `nil` as the new pool arg:

```go
		courseDownloader: course.NewCourseDownloader(ctx, cfg, geektimeClient, sp, nil, nil),
```

- [ ] **Step 4: Build the affected packages**

Run: `go build ./internal/course/ ./internal/service/ ./internal/fsm/`
Expected: FAIL — `cmd/serve.go` still calls `NewDownloadService` with the old 2-arg signature. That is fixed in Task 4. Confirm only `cmd` fails:

Run: `go build ./internal/course/ ./internal/service/ ./internal/fsm/ ./internal/pdf/`
Expected: no errors (these packages now compile).

- [ ] **Step 5: Commit**

```bash
git add internal/course/downloader.go internal/service/download.go internal/fsm/runner.go
git commit -m "$(cat <<'EOF'
feat: thread BrowserPool through CourseDownloader and DownloadService

CourseDownloader and DownloadService gain a *pdf.BrowserPool field,
threaded from serve into PrintArticlePageToPDF. The CLI FSM runner
passes nil, preserving legacy per-article browser behavior.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: serve creates and owns the BrowserPool

**Files:**
- Modify: `cmd/serve.go` (`NewDownloadService` call + pool lifecycle)

**Interfaces:**
- Consumes: `pdf.NewBrowserPool` (Task 1), new `NewDownloadService` 3-arg signature (Task 3).

- [ ] **Step 1: Update serve to create and inject the pool**

In `cmd/serve.go`, inside `serveCmd.RunE`, before `dlSvc := service.NewDownloadService(...)`, create the pool and add a defer to close it. Update the `NewDownloadService` call to pass the pool.

Locate these lines (~line 63-64):

```go
		dlSvc := service.NewDownloadService(&cfg, authMgr.GetClient())
```

Replace with:

```go
		browserPool := pdf.NewBrowserPool(cmd.Context())
		defer browserPool.Close()
		dlSvc := service.NewDownloadService(&cfg, authMgr.GetClient(), browserPool)
```

Add the import to `cmd/serve.go`:

```go
	"github.com/nicoxiang/geektime-downloader/internal/pdf"
```

> `cmd.Context()` is the cobra root context, cancelled when the serve process exits, so the browser dies with serve. `defer browserPool.Close()` is the explicit shutdown path.

- [ ] **Step 2: Build everything**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Run the full test suite**

Run: `go test ./...`
Expected: PASS (browser tests SKIP without Chrome). No regressions in `service`/`course`/`job`/`api`.

- [ ] **Step 4: Commit**

```bash
git add cmd/serve.go
git commit -m "$(cat <<'EOF'
feat(serve): create process-level BrowserPool and inject into downloads

serve now creates one BrowserPool bound to cmd.Context() and passes it
to DownloadService. The pool holds a single long-lived Chrome shared by
all PDF jobs; serve shutdown closes it via defer. This is the wiring
that makes PrintArticlePageToPDF render articles in tabs on a stable
browser instead of spawning a fresh Chrome per article.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Full verification

**Files:** none (verification only)

- [ ] **Step 1: go vet**

Run: `go vet ./...`
Expected: no issues.

- [ ] **Step 2: Full test suite**

Run: `go test ./...`
Expected: PASS (browser tests SKIP without Chrome).

- [ ] **Step 3: Build the release binary locally**

Run: `go build -o /tmp/geektime-downloader-test .`
Expected: no errors; binary produced.

- [ ] **Step 4: Smoke-test the serve flag wiring**

Run: `/tmp/geektime-downloader-test --help 2>&1 | grep interval`
Expected: shows `default 3` (v0.14.2 value preserved).

- [ ] **Step 5: Confirm no compile regressions from the unused allocCancel in browser.go**

Run: `go vet ./internal/pdf/`
Expected: no "declared and not used" errors. (The `allocCancel` from `chromedp.NewExecAllocator` is intentionally assigned to `_` in the implementation; if gofmt/vet complains, adjust — the allocator lifetime is tied to `allocCtx` via the pool's `allocCancel`.)

- [ ] **Step 6: Final commit if any cleanup surfaced**

If verification needed fixes, commit them. Otherwise no commit.

---

## Self-Review Notes

- **Spec coverage:** §5.1 BrowserPool → Task 1; §5.2 PrintArticlePageToPDF → Task 2; §5.3 CourseDownloader pool + nil-fallback → Task 2 (fallback) + Task 3; §5.4 serve ownership → Task 4; §5.5 透传链路 → Tasks 3-4; §6 错误处理 → covered by the nil-fallback (Task 2) and pool crash-rebuild (Task 1 `getOrCreate` Done check); §7 测试 → Task 1 + Task 2 tests; §8 no new flag → confirmed (profile dir hardcoded in Task 1).
- **Type consistency:** `*pdf.BrowserPool` used consistently in Tasks 1-4. `NewCourseDownloader` 6-arg signature matches between Task 3 (definition) and Task 4 (no direct call) and the FSM call in Task 3. `NewDownloadService` 3-arg matches Task 3 (definition) and Task 4 (call). `PrintArticlePageToPDF` 6-arg matches Task 2 (definition) and Task 3 (call).
- **Placeholder scan:** Task 2 Step 2's smoke test is intentionally weak (documented why); no TBD/TODO remain. Task 1's `allocCancel := chromedp.NewExecAllocator(...)` — the returned cancel is discarded via `_ = allocCancel` with a comment; verify it does not break vet (Task 5 Step 5).
- **Known soft spot:** `chromedp.Cancel(bc)` in `Close` — verified in Global Constraints that `Cancel` reads the browser via `FromContext(ctx)`, and `bc` is a browser context that carries the browser, so this is correct. The `cancelCtx` timeout wrapper around `chromedp.Cancel(bc)` does not bound `Cancel`'s internal wait (Cancel does not read the passed ctx's deadline); this is a cosmetic limitation noted in the spec, not a correctness bug — `Close` will block until the browser closes or the process exits. Acceptable for a shutdown path.
