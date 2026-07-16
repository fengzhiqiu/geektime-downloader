package api

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
)

type envelope struct {
	Data  any             `json:"data"`
	Error *apperr.APIError `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, data any, err *apperr.APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Data: data, Error: err})
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, data, nil)
}

func writeAccepted(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusAccepted, data, nil)
}

func writeAPIError(w http.ResponseWriter, err error) {
	apiErr := apperr.MapError(err)
	if e, ok := err.(*apperr.APIError); ok {
		apiErr = e
	}
	status := apiErr.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, nil, apiErr)
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func apiKeyMiddleware(expected string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expected == "" {
			writeAPIError(w, &apperr.APIError{
				Code: apperr.CodeInternal, Message: "api key not configured", HTTPStatus: 500,
			})
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") != expected {
			writeAPIError(w, &apperr.APIError{
				Code: apperr.CodeUnauthorized, Message: "invalid api key", HTTPStatus: 401,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func chromeAvailable() bool {
	if path := os.Getenv("CHROME_PATH"); path != "" {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
	} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// isoTimePtr returns an RFC3339 string for non-zero t, or JSON null for zero.
func isoTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
