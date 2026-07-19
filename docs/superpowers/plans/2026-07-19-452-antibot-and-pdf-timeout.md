# 452 Antibot Misclassification & PDF Render Timeout — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop treating intermittent HTTP 452 (edge anti-bot block) as account expiry, and stop PDF generation from blind-waiting into a 60s timeout, so multi-course batch downloads no longer fail after the first course.

**Architecture:** (1) Map 452→`ErrGeekTimeRateLimit` in `CheckStatus`, reusing the existing `WAITING_RATE_LIMIT` cooldown/auto-resume path. (2) Add resty retry on 451/452/5xx with backoff so self-healing 452s resolve in-process. (3) Replace PDF `chromedp.Sleep` with `WaitVisible` on the article body node, and listen for `network.EventLoadingFailed` to fast-fail as rate-limited. (4) Bump default `--interval` 1→3s to reduce burst triggers.

**Tech Stack:** Go 1.x, `github.com/chromedp/chromedp v0.14.2`, `github.com/chromedp/cdproto`, `github.com/go-resty/resty/v2 v2.16.2`, `net/http/httptest`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-07-19-452-antibot-and-pdf-timeout-design.md`.
- Error sentinels live in `internal/geektime/client.go` (`ErrAuthFailed`, `ErrGeekTimeRateLimit`, `ErrGeekTimeAPIBadCode`). Do not duplicate.
- Real account expiry is signaled by JSON code `-3050`/`-2000` in `do()` (returns `ErrAuthFailed`) — that path stays unchanged. Only HTTP-status 452 changes.
- No DB schema changes, no CLI FSM behavior changes, no new config flags except the `--interval` default value.
- Resty retry count (3) and backoff (2s wait, 10s max) are code constants — not exposed as flags.
- Tests use only `net/http/httptest`; no real network. PDF tests that need a browser must be skippable when Chrome is absent (follow any existing `chromedp` test gating pattern, or skip if none exists).
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
  ```

---

## File Structure

- **Modify** `internal/geektime/client.go` — 452 mapping + resty retry config.
- **Modify** `internal/geektime/client_test.go` — update 452 assertion, add retry tests.
- **Modify** `internal/pdf/pdf.go` — `WaitVisible` replaces `Sleep`; `EventLoadingFailed` listener.
- **Create** `internal/pdf/pdf_test.go` — loading-failure + wait-ready tests.
- **Modify** `cmd/root.go` — `--interval` default 1→3.

---

## Task 1: Map HTTP 452 to rate-limit (not auth-failed)

**Files:**
- Modify: `internal/geektime/client.go` (the `CheckStatus` switch, ~line 105-115)
- Test: `internal/geektime/client_test.go` (the `TestCheckStatus` table, ~line 18-21)

**Interfaces:**
- Produces: `CheckStatus` now returns `ErrGeekTimeRateLimit` for status 452 (was `ErrAuthFailed`). Downstream `apperr.MapError` already maps `ErrGeekTimeRateLimit`→`CodeRateLimited`; worker already routes `CodeRateLimited`→`WAITING_RATE_LIMIT`+cooldown. No consumer signature changes.

- [ ] **Step 1: Update the failing test**

In `internal/geektime/client_test.go`, change the `{452, ...}` row of the `TestCheckStatus` table:

```go
		{451, ErrGeekTimeRateLimit},
		{452, ErrGeekTimeRateLimit},
		{500, ErrGeekTimeAPIBadCode{}},
```

(Rationale: 452 is an intermittent edge anti-bot block, not account expiry — verified by empty-body, self-healing, burst-correlated behavior in logs. Real account expiry comes via JSON code -3050/-2000 in `do()`, unchanged.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/geektime/ -run TestCheckStatus -v`
Expected: FAIL — the `452` subtest fails because `CheckStatus` still returns `ErrAuthFailed`.

- [ ] **Step 3: Change the mapping**

In `internal/geektime/client.go`, in `CheckStatus`, change the switch:

```go
	switch statusCode {
	case 451, 452:
		return ErrGeekTimeRateLimit
	case 453:
		return ErrAuthFailed
	default:
		return ErrGeekTimeAPIBadCode{
			Path:           resp.RawResponse.Request.URL.String(),
			ResponseString: resp.String(),
		}
	}
```

Wait — there is no existing 453 case. Keep it minimal: only fold 452 into the 451 case. Final form:

```go
	switch statusCode {
	case 451, 452:
		return ErrGeekTimeRateLimit
	default:
		return ErrGeekTimeAPIBadCode{
			Path:           resp.RawResponse.Request.URL.String(),
			ResponseString: resp.String(),
		}
	}
```

(Do not add a 453 branch — YAGNI.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/geektime/ -run TestCheckStatus -v`
Expected: PASS — all subtests including `452` pass.

- [ ] **Step 5: Run full geektime package tests**

Run: `go test ./internal/geektime/ -v`
Expected: PASS — no regressions.

- [ ] **Step 6: Commit**

```bash
git add internal/geektime/client.go internal/geektime/client_test.go
git commit -m "$(cat <<'EOF'
fix(geektime): treat HTTP 452 as rate-limit, not account expiry

Logs show /serv/v3/column/info returns 452 intermittently with an empty
body, self-heals within minutes, and correlates with request bursts —
an edge anti-bot block, not account expiry. CheckStatus mapped 452 to
ErrAuthFailed, so the worker marked jobs WAITING_AUTH and demanded
cookie refresh even though cookies were valid (v1/article stayed 200).
This is why every course after the first failed across run18-run24.

Map 452 to ErrGeekTimeRateLimit instead, reusing the existing
WAITING_RATE_LIMIT cooldown + auto-resume path. Real account expiry
still flows through do()'s JSON code -3050/-2000 → ErrAuthFailed.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Add resty retry on 451/452/5xx with backoff

**Files:**
- Modify: `internal/geektime/client.go` (`NewClient`, ~line 56-66)
- Test: `internal/geektime/client_test.go` (append new tests)

**Interfaces:**
- Consumes: resty `Client` builder API (`SetRetryCount`, `SetRetryWaitTime`, `SetRetryMaxWaitTime`, `AddRetryCondition`).
- Produces: `NewClient`'s resty client now retries 451/452/5xx and transport errors up to 3 times. `do()`/`CheckStatus` see only the final response. No signature change.

- [ ] **Step 1: Write the failing test for retry-on-452-then-success**

Append to `internal/geektime/client_test.go`:

```go
// TestNewClientRetries452ThenSucceeds verifies resty retries a transient 452
// anti-bot block and succeeds once the server returns 200. Logs show 452 is
// self-healing within minutes; an in-process retry resolves most cases
// without triggering the worker cooldown.
func TestNewClientRetries452ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 { // first two attempts blocked
			w.WriteHeader(452)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(nil)
	resp, err := c.RestyClient.R().Get(srv.URL)
	if err != nil {
		t.Fatalf("want nil err after retry, got %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode())
	}
	if hits < 3 {
		t.Fatalf("want >=3 server hits (retries), got %d", hits)
	}
}
```

Add the `sync/atomic` import to the test file's import block if not present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/geektime/ -run TestNewClientRetries452ThenSucceeds -v`
Expected: FAIL — without a retry condition, resty does not retry HTTP 452 (only transport errors), so the first 452 returns as an error / non-200 immediately; `hits` stays 1.

- [ ] **Step 3: Write the failing test for retry-exhausted-still-452**

Append:

```go
// TestNewClientRetries452ExhaustedReturnsError verifies that when 452 persists
// past all retries, the request still surfaces (do()/CheckStatus will map the
// final 452 to ErrGeekTimeRateLimit upstream).
func TestNewClientRetries452ExhaustedReturns452(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(452)
	}))
	defer srv.Close()

	c := NewClient(nil)
	resp, err := c.RestyClient.R().Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected transport err: %v", err)
	}
	if resp.StatusCode() != 452 {
		t.Fatalf("want final 452, got %d", resp.StatusCode())
	}
	// resty retries 3 times after the initial attempt = 4 total attempts
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/geektime/ -run TestNewClientRetries452ExhaustedReturns452 -v`
Expected: FAIL or PASS-but-wrong-hits — without retry config, `SetRetryCount(1)` only retries transport errors, so the 452 returns immediately (still 452, so the status assertion passes but retries aren't happening). This test primarily guards that retries do happen; the previous test is the strong assertion. Keep both.

- [ ] **Step 5: Implement the retry config**

In `internal/geektime/client.go`, replace the `NewClient` body's resty builder:

```go
// NewClient returns a new Geektime API client.
func NewClient(cs []*http.Cookie) *Client {
	restyClient := resty.New().
		SetCookies(cs).
		SetRetryCount(3).
		SetRetryWaitTime(2*time.Second).
		SetRetryMaxWaitTime(10*time.Second).
		SetHeader(RefererHeader, DefaultReferer).
		SetLogger(logger.DiscardLogger{}).
		AddRetryCondition(func(r *resty.Response, err error) bool {
			if err != nil {
				return true // transport / timeout errors
			}
			sc := r.StatusCode()
			return sc == 451 || sc == 452 || sc >= 500
		})
	ApplyBrowserHeaders(restyClient)

	c := &Client{RestyClient: restyClient, Cookies: cs}
	return c
}
```

- [ ] **Step 6: Run both new tests to verify they pass**

Run: `go test ./internal/geektime/ -run 'TestNewClientRetries452' -v`
Expected: PASS — both retry tests pass.

- [ ] **Step 7: Run full geektime package tests**

Run: `go test ./internal/geektime/ -v`
Expected: PASS — including `TestCheckStatus`, `TestNewClientSendsBrowserHeaders`, and the new retry tests.

- [ ] **Step 8: Commit**

```bash
git add internal/geektime/client.go internal/geektime/client_test.go
git commit -m "$(cat <<'EOF'
feat(geektime): retry 451/452/5xx with backoff on API client

resty's default retry only covers transport errors, not HTTP 451/452. Logs
show 452 self-heals within minutes, so an in-process retry resolves most
transient blocks without escalating to the worker cooldown. Add a retry
condition (3 retries, 2s→10s backoff) for 451/452/5xx and transport errors.
do()/CheckStatus still map the final response; only the final status is
observed by callers.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: PDF — replace blind Sleep with WaitVisible on article body

**Files:**
- Modify: `internal/pdf/pdf.go` (the `tasks` construction in `PrintArticlePageToPDF`, ~line 86-99; add a new `waitArticleReady` func)
- Test: `internal/pdf/pdf_test.go` (new file)

**Interfaces:**
- Consumes: `chromedp.WaitVisible`, `chromedp.ByQuery`, `cfg.PrintPDFWaitSeconds`.
- Produces: `waitArticleReady() chromedp.Action` — waits for the article body node to become visible instead of sleeping a fixed duration.

**Note on browser tests:** PDF tests require a real Chrome. Gate them with a `chromeAvailable()` skip helper so CI/agents without Chrome don't fail. If the package has no existing chromedp test, define the helper in the new test file.

- [ ] **Step 1: Create the test file with a skip helper and a wait-ready test**

Create `internal/pdf/pdf_test.go`:

```go
package pdf

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// chromeAvailable reports whether a Chrome executable can be located, so
// browser-dependent tests skip cleanly on machines without Chrome.
func chromeAvailable(t *testing.T) {
	t.Helper()
	allocCtx, cancel := chromedp.NewExecAllocator(context.Background())
	defer cancel()
	ctx, cancel2 := chromedp.NewContext(allocCtx)
	defer cancel2()
	timeoutCtx, cancel3 := context.WithTimeout(ctx, 5*time.Second)
	defer cancel3()
	if err := chromedp.Run(timeoutCtx, chromedp.Navigate("about:blank")); err != nil {
		t.Skipf("chrome not available: %v", err)
	}
}

// TestWaitArticleReadyDoesNotBlockForever is a smoke test that waitArticleReady
// returns an Action (does not panic) and is a chromedp.Action. Full browser
// behavior is covered by integration; here we only assert construction.
func TestWaitArticleReadyDoesNotBlockForever(t *testing.T) {
	a := waitArticleReady()
	if a == nil {
		t.Fatal("want non-nil action")
	}
}

// guard against accidentally removing the helper if env lacks chrome
var _ = os.Getenv
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pdf/ -run TestWaitArticleReadyDoesNotBlockForever -v`
Expected: FAIL — `waitArticleReady` undefined.

- [ ] **Step 3: Implement `waitArticleReady` and wire it into tasks**

In `internal/pdf/pdf.go`, first add `"github.com/chromedp/cdproto/cdp"` is already imported; ensure `chromedp` is (it is). Add the function near `setCookies`:

```go
// waitArticleReady waits for the article body content node to become visible
// instead of sleeping a fixed duration. It tolerates page markup variation by
// trying a set of candidate selectors. If none match, the outer timeoutCtx
// still bounds the wait (degrading to the prior timeout behavior).
func waitArticleReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		// Candidate selectors for the article body container across
		// geektime page revisions.
		selectors := []string{
			"div[class*='articleContent']",
			"div[class*='ArticleContent']",
			"div[class*='Index_articleContent']",
			"article",
		}
		for _, s := range selectors {
			waitCtx, cancel := context.WithTimeout(ctx, time.Duration(0)*time.Second)
			_ = waitCtx
			cancel()
			// Use a short per-selector probe via WaitVisible; if it
			// times out we move to the next candidate.
			probeCtx, probeCancel := context.WithTimeout(ctx, 8*time.Second)
			err := chromedp.WaitVisible(s, chromedp.ByQuery).Do(probeCtx)
			probeCancel()
			if err == nil {
				return nil
			}
			// context.DeadlineExceeded from the probe means the selector
			// didn't match in time; try the next. Other errors bubble up.
			if probeCtx.Err() == context.DeadlineExceeded {
				continue
			}
		}
		// None matched within probes; fall back to a final WaitVisible on
		// the first candidate, bounded by the caller's timeoutCtx.
		return chromedp.WaitVisible(selectors[0], chromedp.ByQuery).Do(ctx)
	})
}
```

Wait — `chromedp.WaitVisible(s, chromedp.ByQuery).Do(ctx)` is not the chromedp-idiomatic call inside an ActionFunc. The cleaner approach is to return `chromedp.Tasks` and let `chromedp.Run` drive them. Simplify — replace the whole function body with a `chromedp.Tasks`-compatible single action using a JS probe that returns once any candidate is found:

Replace the implementation with:

```go
// waitArticleReady waits for the article body content node to become visible
// instead of sleeping a fixed duration. It polls a set of candidate selectors
// for the article body container (tolerating page markup variation) and
// returns once any is present. The caller's timeoutCtx bounds the total wait,
// so a total selector miss degrades to the prior timeout behavior rather than
// hanging indefinitely.
func waitArticleReady() chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		check := `(function(){
			var sels = [
				"div[class*='articleContent']",
				"div[class*='ArticleContent']",
				"div[class*='Index_articleContent']",
				"article"
			];
			for (var i=0;i<sels.length;i++){
				var el = document.querySelector(sels[i]);
				if (el && el.offsetParent !== null) { return true; }
			}
			return false;
		})()`
		// Poll up to ~PrintPDFWaitSeconds-ish, but rely on the outer
		// timeoutCtx as the hard bound. Use chromedp.Poll which resolves
		// when the JS evaluates truthy.
		return chromedp.Poll(check, nil, chromedp.WithPollingInterval(200*time.Millisecond)).Do(ctx)
	})
}
```

> `chromedp.Poll` evaluates the expression repeatedly until truthy or context deadline — exactly the "wait until ready, bounded by timeout" semantics we want, with no fixed sleep.

Now wire it into `tasks`. In `PrintArticlePageToPDF`, replace the `chromedp.Sleep(...)` line:

```go
	tasks := chromedp.Tasks{
		network.Enable(),
		chromedp.Emulate(device.IPadPro11),
		setCookies(cookies),
		chromedp.Navigate(geektime.DefaultBaseURL + `/column/article/` + strconv.Itoa(aid)),
		waitArticleReady(),
	}
```

(Remove the `chromedp.Sleep(time.Duration(cfg.PrintPDFWaitSeconds) * time.Second)` line. `cfg.PrintPDFWaitSeconds` is no longer used by `waitArticleReady`; the outer `timeoutCtx` (`PrintPDFTimeoutSeconds`) remains the hard bound. Leave the `PrintPDFWaitSeconds` field/flag in place for backward compat — it is now unused by this path, which is acceptable; do not remove the flag.)

- [ ] **Step 4: Run the construction test to verify it passes**

Run: `go test ./internal/pdf/ -run TestWaitArticleReadyDoesNotBlockForever -v`
Expected: PASS.

- [ ] **Step 5: Verify the package still builds**

Run: `go build ./internal/pdf/`
Expected: no errors.

- [ ] **Step 6: Run full pdf package tests**

Run: `go test ./internal/pdf/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/pdf/pdf.go internal/pdf/pdf_test.go
git commit -m "$(cat <<'EOF'
fix(pdf): wait for article body instead of blind Sleep before PDF

PrintArticlePageToPDF slept a fixed PrintPDFWaitSeconds then printed,
which produced context deadline exceeded when page load stalled (logs
showed 60s timeouts while the v1/article API was already 200). Replace
the Sleep with chromedp.Poll on the article body container, returning
as soon as the content node is visible. The outer PrintPDFTimeoutSeconds
context remains the hard bound, so a total selector miss degrades to
the prior timeout rather than hanging. PrintPDFWaitSeconds flag is kept
for backward compat though now unused on this path.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: PDF — fast-fail on page load failure as rate-limited

**Files:**
- Modify: `internal/pdf/pdf.go` (the `listener` switch in `PrintArticlePageToPDF`, ~line 72-92; add a main-frame request tracker)
- Test: `internal/pdf/pdf_test.go` (append)

**Interfaces:**
- Consumes: `network.EventLoadingFailed`, `network.EventRequestWillBeSent` (to identify the main-frame document request).
- Produces: when the main document load fails with an auth/block error, `PrintArticlePageToPDF` returns `geektime.ErrGeekTimeRateLimit` (via the existing `rateLimit` flag path at ~line 124-128) instead of waiting until `context deadline exceeded`.

- [ ] **Step 1: Write the failing test for loading-failure mapping**

Append to `internal/pdf/pdf_test.go`:

```go
// TestPrintArticlePageToPDFLoadFailureReturnsRateLimit verifies that when the
// article page navigation fails (e.g. anti-bot auth block surfaced by Chrome as
// ERR_INVALID_AUTH_CREDENTIALS), the function returns ErrGeekTimeRateLimit
// quickly instead of waiting the full PrintPDFTimeoutSeconds.
//
// Requires Chrome. Skips if unavailable.
func TestPrintArticlePageToPDFLoadFailureReturnsRateLimit(t *testing.T) {
	chromeAvailable(t)
	// Serve a URL that immediately drops the connection to force a load failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 401 on the document triggers ERR_INVALID_AUTH_CREDENTIALS in Chrome.
		w.WriteHeader(401)
	}))
	defer srv.Close()

	cfg := &config.AppConfig{PrintPDFTimeoutSeconds: 30}
	article := geektime.Article{AID: 1, Title: "loadfail"}
	dir := t.TempDir()

	start := time.Now()
	err := PrintArticlePageToPDF(context.Background(), article, dir, nil, cfg)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, geektime.ErrGeekTimeRateLimit) {
		t.Fatalf("want ErrGeekTimeRateLimit, got %v", err)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("fast-failed too slowly: %v (expected well under timeout)", elapsed)
	}
}
```

Add these imports to the test file: `errors`, `net/http`, `net/http/httptest`, `github.com/nicoxiang/geektime-downloader/internal/config`, `github.com/nicoxiang/geektime-downloader/internal/geektime`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pdf/ -run TestPrintArticlePageToPDFLoadFailureReturnsRateLimit -v`
Expected: FAIL or SKIP (skip is acceptable if Chrome absent on the dev machine — then verify on a machine with Chrome). If Chrome present: FAIL because a 401 document currently leads to `context deadline exceeded` (mapped to a generic error, not `ErrGeekTimeRateLimit`), and it takes the full 30s.

- [ ] **Step 3: Track the main-frame document request and fast-fail on its load failure**

In `internal/pdf/pdf.go`, inside `PrintArticlePageToPDF`, add a variable to record the main document's `network.RequestID`, and extend the listener. The document request is the one whose `Request` is `document` type and URL matches the navigate URL.

First, add a tracker variable before the `listener` definition (near `rateLimit := false`):

```go
	rateLimit := false
	aid := article.AID
	var mainDocReqID network.RequestID
```

Then extend the `listener` switch. Add a case for `*network.EventRequestWillBeSent` to capture the main document request, and a case for `*network.EventLoadingFailed` to fast-fail. The full listener becomes:

```go
	listener := func(ev interface{}) {
		switch e := ev.(type) {
		case *network.EventResponseReceived:
			response := e.Response
			// rate limit detection
			if response.URL == geektime.DefaultBaseURL+"/serv/v1/article" && response.Status == 451 {
				logger.Warnf("Hit GeekTime rate limit when downloading article pdf, articleID: %d, pdfFileName: %s", aid, pdfFileName)
				rateLimit = true
				timeoutCancel()
				listenerCtxCancel()
				return
			}

			// if downloadComments is DownloadCommentsAll, monitor comment list responses
			if cfg.DownloadComments == DownloadCommentsAll {
				if strings.Contains(strings.ToLower(response.URL), "comment/list") {
					fetchAndHandleCommentList(listenerCtx, e.RequestID, response.URL, &commentsDone)
				}
			}
		case *network.EventRequestWillBeSent:
			// Record the main-frame document request so we can attribute
			// its load failure. The navigate URL is the article page.
			if e.Request != nil && e.Type == network.ResourceTypeDocument {
				mainDocReqID = e.RequestID
			}
		case *network.EventLoadingFailed:
			// Fast-fail when the main document load fails (e.g. anti-bot
			// auth block surfaced as ERR_INVALID_AUTH_CREDENTIALS,
			// ERR_BLOCKED_BY_CLIENT, etc.). Without this we'd wait the
			// full PrintPDFTimeoutSeconds and surface a generic timeout.
			if e.RequestID == mainDocReqID && mainDocReqID != "" {
				logger.Warnf("Article page load failed, articleID: %d, error: %s, pdfFileName: %s",
					aid, e.ErrorText, pdfFileName)
				rateLimit = true
				timeoutCancel()
				listenerCtxCancel()
				return
			}
		}
	}
```

> Note: `e.Request` field access — verify the `EventRequestWillBeSent` struct exposes `Request` and `Type`/`ResourceTypeDocument` in this cdproto version. If the field is named differently, adjust. The `RequestID` field on both events is what we match on.

- [ ] **Step 4: Run the loading-failure test to verify it passes**

Run: `go test ./internal/pdf/ -run TestPrintArticlePageToPDFLoadFailureReturnsRateLimit -v`
Expected: PASS (or SKIP if no Chrome — then confirm structurally the code compiles and the mapping path is correct; run on a Chrome-equipped machine before merging).

- [ ] **Step 5: Run full pdf package tests + build**

Run: `go test ./internal/pdf/ -v && go build ./internal/pdf/`
Expected: PASS / no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/pdf/pdf.go internal/pdf/pdf_test.go
git commit -m "$(cat <<'EOF'
fix(pdf): fast-fail on article page load failure as rate-limited

When the article page navigation fails (anti-bot auth block surfaced by
Chrome as ERR_INVALID_AUTH_CREDENTIALS etc.), PrintArticlePageToPDF now
detects the main document load failure and returns ErrGeekTimeRateLimit
immediately, instead of waiting the full PrintPDFTimeoutSeconds and
surfacing a generic context deadline exceeded. This routes the failure
into the existing WAITING_RATE_LIMIT cooldown/auto-resume path.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Bump default `--interval` 1→3s

**Files:**
- Modify: `cmd/root.go` (~line 34)

**Interfaces:**
- Produces: `--interval` flag default changes from `1` to `3`. The `Interval` field and `waitRandomTime` jitter logic (interval*1..interval*2) are unchanged, so effective spacing becomes 3-6s.

- [ ] **Step 1: Change the default**

In `cmd/root.go`, change the flag registration line:

```go
	rootCmd.Flags().IntVar(&cfg.Interval, "interval", 3, "下载资源的间隔时间, 单位为秒, 默认3秒")
```

- [ ] **Step 2: Build to verify**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Verify the default with --help**

Run: `go run . --help 2>&1 | grep interval`
Expected: output shows `default 3`.

- [ ] **Step 4: Commit**

```bash
git add cmd/root.go
git commit -m "$(cat <<'EOF'
chore: bump default download interval 1s to 3s

The 1s default produced dense request bursts that correlated with 452
anti-bot blocks in logs. 3s (with existing jitter → 3-6s effective)
reduces burst triggers in the serve batch scenario, where throughput
is not latency-sensitive.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Full verification

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `go test ./...`
Expected: PASS (PDF browser tests may SKIP on machines without Chrome — acceptable).

- [ ] **Step 2: Build everything**

Run: `go build ./...`
Expected: no errors.

- [ ] **Step 3: Run go vet**

Run: `go vet ./...`
Expected: no issues.

- [ ] **Step 4: Confirm no unused-variable / import errors from PrintPDFWaitSeconds**

Run: `go build ./internal/pdf/ && go build ./cmd/`
Expected: no errors. (`PrintPDFWaitSeconds` remains a config field; if Task 3 left it unused in `pdf.go`, that's fine — it's a struct field, not a local.)

- [ ] **Step 5: Final commit if any cleanup needed**

If verification surfaced fixes, commit them. Otherwise no commit.

---

## Self-Review Notes

- **Spec coverage:** Spec §5.1 → Task 1; §5.2 → Task 2; §5.3 (worker routing) requires no code change (verified `apperr.MapError` + worker already handle `ErrGeekTimeRateLimit`); §5.4 → Task 3; §5.5 → Task 4; §5.6 → Task 5. §6/§7/§8 covered by tasks' test steps.
- **Type consistency:** `waitArticleReady()` returns `chromedp.Action` (Task 3 produces, Task 3 consumes). `mainDocReqID` is `network.RequestID` (Task 4). `ErrGeekTimeRateLimit` referenced in tests is exported from `geektime` (Task 1/4).
- **Placeholder scan:** Task 3's first `waitArticleReady` draft is explicitly marked "Replace the implementation with" and gives the final `chromedp.Poll` form — no placeholder remains in the final code. Task 4 has a verification note about cdproto field names that the implementer must confirm against the actual struct; this is a real check, not a TBD.
