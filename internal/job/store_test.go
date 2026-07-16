package job_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

func newTestStore(t *testing.T) *job.Store {
	t.Helper()
	s, err := job.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStoreCreateAndGetJob(t *testing.T) {
	dir := t.TempDir()
	store, err := job.OpenStore(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	created, err := store.CreateJob(ctx, service.DownloadRequest{
		ProductType: service.ProductTypeColumn,
		ProductID:   100,
		Mode:        "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != job.StatusPending {
		t.Fatalf("status %s", got.Status)
	}
}

func TestStoreIdempotencyKey(t *testing.T) {
	dir := t.TempDir()
	store, err := job.OpenStore(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	req := service.DownloadRequest{
		ProductType:    service.ProductTypeColumn,
		ProductID:      100,
		Mode:           "all",
		IdempotencyKey: "idem-1",
	}
	first, err := store.CreateJob(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateJob(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same id, got %s and %s", first.ID, second.ID)
	}
}

func TestRecoverRunningJobs(t *testing.T) {
	dir := t.TempDir()
	store, err := job.OpenStore(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	created, err := store.CreateJob(ctx, service.DownloadRequest{
		ProductType: service.ProductTypeColumn,
		ProductID:   1,
		Mode:        "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateJobStatus(ctx, created.ID, job.StatusRunning, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverRunningJobs(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != job.StatusPending {
		t.Fatalf("status %s", got.Status)
	}
}

func TestMarkStaleJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// 标记 running，updated_at 设为 1 小时前
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET status='running', updated_at=? WHERE id=?`, past, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkStaleJobs(ctx, 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetJob(ctx, j.ID)
	if got.StatusReason != "stale_progress" {
		t.Fatalf("want status_reason=stale_progress, got %q", got.StatusReason)
	}
	if got.Status != "running" {
		t.Fatalf("status must stay running, got %q", got.Status)
	}
}

func TestResumeRateLimitJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 2, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	_ = store.UpdateJobStatus(ctx, j.ID, job.StatusWaitingRateLimit, "rate limited", nil)
	n, err := store.ResumeRateLimitJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 resumed, got %d", n)
	}
	got, _ := store.GetJob(ctx, j.ID)
	if got.Status != job.StatusPending {
		t.Fatalf("want pending, got %q", got.Status)
	}
}

func TestCountStaleJobs(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// running but NOT stale -> count 0
	_ = store.UpdateJobStatus(ctx, j.ID, job.StatusRunning, "", nil)
	n, err := store.CountStaleJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("want 0 stale, got %d", n)
	}
	// mark stale
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET status_reason='stale_progress', updated_at=? WHERE id=?`, past, j.ID); err != nil {
		t.Fatal(err)
	}
	n, err = store.CountStaleJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 stale, got %d", n)
	}
}
