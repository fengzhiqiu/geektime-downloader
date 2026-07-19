package pdf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/nicoxiang/geektime-downloader/internal/config"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
)

// chromeAvailable skips the calling test if a Chrome executable cannot be
// launched, so browser-dependent tests do not fail on machines without Chrome.
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

// TestWaitArticleReadyIsAction is a smoke test that waitArticleReady returns a
// non-nil chromedp.Action (construction only; no browser). Full browser
// behavior is covered by TestPrintArticlePageToPDFLoadFailureReturnsRateLimit.
func TestWaitArticleReadyIsAction(t *testing.T) {
	a := waitArticleReady()
	if a == nil {
		t.Fatal("want non-nil action")
	}
}

// TestPrintArticlePageToPDFLoadFailureReturnsRateLimit verifies that when the
// article page navigation fails (a 401 document surfaces in Chrome as an auth
// error), PrintArticlePageToPDF returns ErrGeekTimeRateLimit quickly instead
// of waiting the full PrintPDFTimeoutSeconds and surfacing a generic timeout.
//
// Requires Chrome; skips if unavailable.
func TestPrintArticlePageToPDFLoadFailureReturnsRateLimit(t *testing.T) {
	chromeAvailable(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 401 on the document triggers an auth load failure in Chrome.
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
		t.Fatalf("fast-failed too slowly: %v (expected well under the 30s timeout)", elapsed)
	}
}
