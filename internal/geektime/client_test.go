package geektime

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
