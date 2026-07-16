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
	})
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
