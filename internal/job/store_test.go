package job_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

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
