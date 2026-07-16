package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadFileConcurrentlyStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.WriteHeader(452)
			return
		}
		w.WriteHeader(452)
		_, _ = io.WriteString(w, "auth expired body")
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out.bin")
	_, err := DownloadFileConcurrently(context.Background(), dst, srv.URL, nil, 2, time.Second)
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != 452 {
		t.Fatalf("want StatusError{452}, got %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("坏文件不应落盘")
	}
}

func TestDownloadFileConcurrentlyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(200)
			return
		}
		time.Sleep(2 * time.Second) // 超过 segmentTimeout
		w.WriteHeader(200)
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out.bin")
	ctx := context.Background()
	_, err := DownloadFileConcurrently(ctx, dst, srv.URL, nil, 1, 100*time.Millisecond)
	if err == nil {
		t.Fatal("want timeout error, got nil")
	}
}

func TestRetryDoesNotRetryStatusError(t *testing.T) {
	calls := 0
	err := retry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return &StatusError{StatusCode: 403, Body: "forbidden"}
	})
	if calls != 1 {
		t.Fatalf("StatusError must not retry, calls=%d", calls)
	}
	if err == nil {
		t.Fatal("want error")
	}
}

func TestRetryRetriesNetworkError(t *testing.T) {
	calls := 0
	_ = retry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return fmt.Errorf("transient")
	})
	if calls != 3 {
		t.Fatalf("network error should retry up to 3, calls=%d", calls)
	}
}
