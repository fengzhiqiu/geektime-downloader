package video

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/pkg/downloader"
)

func TestTranslateDownloadErr(t *testing.T) {
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 452}); !errors.Is(err, geektime.ErrAuthFailed) {
		t.Fatalf("452 want ErrAuthFailed, got %v", err)
	}
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 451}); !errors.Is(err, geektime.ErrGeekTimeRateLimit) {
		t.Fatalf("451 want ErrGeekTimeRateLimit, got %v", err)
	}
	if err := translateDownloadErr(&downloader.StatusError{StatusCode: 403}); err == nil {
		t.Fatal("403 should surface as non-nil error")
	}
}

func TestDownloadMP4ReturnsErrorOn403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte("forbidden"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	err := DownloadMP4(context.Background(), "t", dir, []string{srv.URL + "/v.mp4"}, false)
	if err == nil {
		t.Fatal("want error on 403, got nil")
	}
}

func TestGetPlayInfoNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	c := geektime.NewClient(nil)
	_, err := getPlayInfo(c, srv.URL, "sd")
	if err == nil {
		t.Fatal("want error on non-200 getPlayInfo")
	}
	_, _ = os.Stat(filepath.Join(t.TempDir(), "x")) // keep imports used
}
