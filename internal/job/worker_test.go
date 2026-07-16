package job

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

type fakeExecutor struct {
	err error
	c   *geektime.Client
}

func (f *fakeExecutor) SetClient(*geektime.Client) {}
func (f *fakeExecutor) Client() *geektime.Client {
	if f.c != nil {
		return f.c
	}
	return &geektime.Client{} // non-nil so runJob proceeds past the session check
}
func (f *fakeExecutor) ExecuteDownload(ctx context.Context, req service.DownloadRequest, rep progress.Reporter) (geektime.Course, string, error) {
	return geektime.Course{}, "", f.err
}

func newTestStoreTB(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRunJobWatchdogTimeout(t *testing.T) {
	store := newTestStoreTB(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}

	w := NewWorker(store, &fakeExecutor{err: context.DeadlineExceeded}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	}, nil)
	w.runJob(ctx, j.ID)

	got, _ := store.GetJob(ctx, j.ID)
	if got.Status != StatusFailed {
		t.Fatalf("want failed, got %q", got.Status)
	}
	if got.StatusReason != "watchdog_timeout" {
		t.Fatalf("want reason watchdog_timeout, got %q", got.StatusReason)
	}
	if got.Error == nil || got.Error.Code != "TIMEOUT" {
		t.Fatalf("want TIMEOUT error, got %+v", got.Error)
	}
}

func TestRateLimitCooldownGate(t *testing.T) {
	store := newTestStoreTB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := NewWorker(store, &fakeExecutor{err: nil}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: 200 * time.Millisecond,
	}, nil)
	w.Start(ctx)

	// 模拟一次限流：置冷却到 200ms 后
	w.applyRateLimitCooldown()
	if w.rateLimitUntil.Load() == 0 {
		t.Fatal("rateLimitUntil should be set")
	}
	if !w.cooldownActive() {
		t.Fatal("cooldown should be active immediately after rate limit")
	}

	// 等冷却过期
	time.Sleep(300 * time.Millisecond)
	if w.cooldownActive() {
		t.Fatal("cooldown should have expired")
	}
}

func TestRateLimitCooldownGrows(t *testing.T) {
	store := newTestStoreTB(t)
	w := NewWorker(store, &fakeExecutor{}, nil, Stability{RateLimitCooldown: 100 * time.Millisecond}, nil)
	w.applyRateLimitCooldown()
	first := w.rateLimitUntil.Load()
	w.applyRateLimitCooldown()
	second := w.rateLimitUntil.Load()
	if second-first < int64(100*time.Millisecond) {
		t.Fatal("cooldown should grow on repeated rate limits")
	}
}

func TestWorkerStatsAndExposure(t *testing.T) {
	store := newTestStoreTB(t)
	stats := &Stats{}
	w := NewWorker(store, &fakeExecutor{err: context.DeadlineExceeded}, nil, Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	}, stats)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	w.runJob(ctx, j.ID)

	snap := w.Stats()
	if snap["TIMEOUT"] != 1 {
		t.Fatalf("want TIMEOUT=1, got %v", snap)
	}
	if w.Uptime() < 0 {
		t.Fatal("uptime should be non-negative")
	}
	if w.RateLimitUntil().IsZero() {
		// no rate limit applied in this path -> zero is fine
	}
	_ = w.LastActiveAt() // must not panic
}
