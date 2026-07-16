package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/api"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

type fakeExec struct{}

func (fakeExec) SetClient(*geektime.Client)                              {}
func (fakeExec) Client() *geektime.Client                                { return &geektime.Client{} }
func (fakeExec) ExecuteDownload(context.Context, service.DownloadRequest, progress.Reporter) (geektime.Course, string, error) {
	return geektime.Course{}, "", nil
}

func newTestServer(t *testing.T) (*api.Server, *job.Store, *job.Stats) {
	t.Helper()
	store, err := job.OpenStore(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	stats := &job.Stats{}
	w := job.NewWorker(store, fakeExec{}, nil, job.Stability{
		JobTimeout: time.Minute, HeartbeatTimeout: time.Minute, RateLimitCooldown: time.Minute,
	}, stats)
	srv := api.NewServer(nil, store, w, nil, "test", "k", nil)
	return srv, store, stats
}

func TestHandleHealthNewFields(t *testing.T) {
	srv, store, stats := newTestServer(t)
	stats.Inc("AUTH_EXPIRED")
	_, _ = store.CountStaleJobs(context.Background()) // (no job needed)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.SetBasicAuth("", "k") // not used; health is unprotected
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"uptime_seconds", "last_active_at", "stale_jobs", "error_counts", "rate_limit_cooldown_until"} {
		if _, ok := env.Data[k]; !ok {
			t.Fatalf("health missing field %s", k)
		}
	}
}

func TestHandleGetDownloadRuntimeSeconds(t *testing.T) {
	srv, store, _ := newTestServer(t)
	ctx := context.Background()
	j, err := store.CreateJob(ctx, service.DownloadRequest{ProductType: "column", ProductID: 1, Mode: "all"})
	if err != nil {
		t.Fatal(err)
	}
	// mark running with started_at 10s ago
	past := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339)
	if _, err := store.DB().ExecContext(ctx, `UPDATE jobs SET status='running', started_at=?, updated_at=? WHERE id=?`, past, past, j.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/downloads/"+j.ID, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var env struct {
		Data struct {
			RuntimeSeconds int64 `json:"runtime_seconds"`
		} `json:"data"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Data.RuntimeSeconds <= 0 {
		t.Fatalf("running job runtime_seconds should be >0, got %d", env.Data.RuntimeSeconds)
	}
}
