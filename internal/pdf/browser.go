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
	_ = allocCancel // allocator lifetime tied to allocCtx via p.allocCancel

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
		// gracefully closes it (first==true path).
		_ = chromedp.Cancel(bc)
	}
	p.allocCancel()
}
