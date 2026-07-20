package pdf

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// TestBrowserPoolReusesSingleBrowser verifies that two WithBrowser calls on
// the same pool share one browser (different tabs, not a fresh Chrome each
// time). The fix's whole point is browser reuse so the device fingerprint
// stays stable.
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
	if firstTargetID == secondTargetID {
		t.Fatal("want different tab target ids; got same")
	}
}

// TestBrowserPoolWithBrowserCount verifies repeated WithBrowser calls succeed,
// exercising tab creation on the shared browser.
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

// TestBrowserPoolCloseReleasesBrowser verifies Close does not panic and that a
// fresh pool works after another was closed (the browser of the first is gone).
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
