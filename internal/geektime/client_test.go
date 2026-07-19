package geektime

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-resty/resty/v2"
)

func TestCheckStatus(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{200, nil},
		{204, nil},
		{451, ErrGeekTimeRateLimit},
		{452, ErrGeekTimeRateLimit},
		{500, ErrGeekTimeAPIBadCode{}},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			resp, err := resty.New().R().Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = CheckStatus(resp)
			if tc.want == nil && err != nil {
				t.Fatalf("want nil, got %v", err)
			}
			if tc.want != nil {
				if _, ok := tc.want.(ErrGeekTimeAPIBadCode); ok {
					// For struct errors, check type assertion
					if _, ok := err.(ErrGeekTimeAPIBadCode); !ok {
						t.Fatalf("want ErrGeekTimeAPIBadCode, got %v", err)
					}
				} else if !errors.Is(err, tc.want) {
					t.Fatalf("want %v, got %v", tc.want, err)
				}
			}
		})
	}
}

// TestNewClientSendsBrowserHeaders guards against the 452 regression where the
// main API client omitted the browser headers geektime requires on API calls
// (Accept, Accept-Language, Referer) — the same set Auth() uses to avoid 452.
// Commit b29e319 added these to Auth() but not NewClient, so every API call
// (V1ArticleInfo, VideoPlayAuth, CourseInfo, ...) returned 452.
func TestNewClientSendsBrowserHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(nil)
	resp, err := c.RestyClient.R().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode())
	}
	for _, h := range []string{"Accept", "Accept-Language", "Referer", "User-Agent"} {
		if got.Get(h) == "" {
			t.Errorf("NewClient request missing header %q", h)
		}
	}
}

// TestNewClientRetries452ThenSucceeds verifies resty retries a transient 452
// anti-bot block and succeeds once the server returns 200. Logs show 452 is
// self-healing within minutes; an in-process retry resolves most cases
// without escalating to the worker cooldown. resty's default retry only
// covers transport errors, not HTTP 452, so a retry condition is required.
func TestNewClientRetries452ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n < 3 { // first two attempts blocked
			w.WriteHeader(452)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := NewClient(nil)
	resp, err := c.RestyClient.R().Get(srv.URL)
	if err != nil {
		t.Fatalf("want nil err after retry, got %v", err)
	}
	if resp.StatusCode() != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode())
	}
	if hits < 3 {
		t.Fatalf("want >=3 server hits (retries), got %d", hits)
	}
}

// TestNewClientRetries452ExhaustedReturns452 verifies that when 452 persists
// past all retries, the final response still surfaces as 452 so do()/CheckStatus
// can map it to ErrGeekTimeRateLimit upstream (which drives the worker cooldown).
func TestNewClientRetries452ExhaustedReturns452(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(452)
	}))
	defer srv.Close()

	c := NewClient(nil)
	resp, err := c.RestyClient.R().Get(srv.URL)
	if err != nil {
		t.Fatalf("unexpected transport err: %v", err)
	}
	if resp.StatusCode() != 452 {
		t.Fatalf("want final 452, got %d", resp.StatusCode())
	}
	// initial attempt + 3 retries = 4 total hits
	if hits != 4 {
		t.Fatalf("want 4 total attempts (1 + 3 retries), got %d", hits)
	}
}
