package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/auth"
	"github.com/nicoxiang/geektime-downloader/internal/job"
	"github.com/nicoxiang/geektime-downloader/internal/pkg/logger"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

// Server is the HTTP API server.
type Server struct {
	authMgr   *auth.Manager
	store     *job.Store
	worker    *job.Worker
	svc       *service.DownloadService
	version   string
	apiKey    string
	onCookies func(context.Context) ([]string, error)
}

// NewServer creates an API server.
func NewServer(
	authMgr *auth.Manager,
	store *job.Store,
	worker *job.Worker,
	svc *service.DownloadService,
	version, apiKey string,
	onCookies func(context.Context) ([]string, error),
) *Server {
	return &Server{
		authMgr: authMgr, store: store, worker: worker, svc: svc,
		version: version, apiKey: apiKey, onCookies: onCookies,
	}
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.Handle("GET /api/v1/session/status", s.protect(http.HandlerFunc(s.handleSessionStatus)))
	mux.Handle("PUT /api/v1/session/cookies", s.protect(http.HandlerFunc(s.handleUpdateCookies)))
	mux.Handle("GET /api/v1/product-types", s.protect(http.HandlerFunc(s.handleProductTypes)))
	mux.Handle("POST /api/v1/courses/lookup", s.protect(http.HandlerFunc(s.handleCourseLookup)))
	mux.Handle("POST /api/v1/downloads", s.protect(http.HandlerFunc(s.handleCreateDownload)))
	mux.Handle("GET /api/v1/downloads", s.protect(http.HandlerFunc(s.handleListDownloads)))
	mux.Handle("GET /api/v1/downloads/{id}", s.protect(http.HandlerFunc(s.handleGetDownload)))
	mux.Handle("POST /api/v1/downloads/{id}/retry", s.protect(http.HandlerFunc(s.handleRetryDownload)))
	mux.Handle("POST /api/v1/downloads/{id}/cancel", s.protect(http.HandlerFunc(s.handleCancelDownload)))
	mux.Handle("DELETE /api/v1/downloads/{id}", s.protect(http.HandlerFunc(s.handleDeleteDownload)))
	return mux
}

func (s *Server) protect(h http.Handler) http.Handler {
	return apiKeyMiddleware(s.apiKey, h)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	workerStatus := "idle"
	activeJob := s.worker.ActiveJobID()
	if activeJob != "" {
		workerStatus = "busy"
	}
	staleCount, err := s.store.CountStaleJobs(r.Context())
	if err != nil {
		logger.Warnf("health: CountStaleJobs failed: %v", err)
		staleCount = -1
	}
	writeOK(w, map[string]any{
		"status":                    "ok",
		"version":                   s.version,
		"chrome_available":          chromeAvailable(),
		"worker_status":             workerStatus,
		"active_job_id":             nilString(activeJob),
		"uptime_seconds":            int64(s.worker.Uptime().Seconds()),
		"last_active_at":            isoTimePtr(s.worker.LastActiveAt()),
		"stale_jobs":                staleCount,
		"error_counts":              s.worker.Stats(),
		"rate_limit_cooldown_until": isoTimePtr(s.worker.RateLimitUntil()),
	})
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	writeOK(w, s.authMgr.Status(r.Context()))
}

func (s *Server) handleUpdateCookies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Gcid  string `json:"gcid"`
		Gcess string `json:"gcess"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeAPIError(w, &apperr.APIError{Code: apperr.CodeBadRequest, Message: err.Error(), HTTPStatus: 400})
		return
	}
	status, err := s.authMgr.UpdateCookies(r.Context(), body.Gcid, body.Gcess)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	s.svc.SetClient(s.authMgr.GetClient())
	var resumed []string
	if s.onCookies != nil {
		resumed, _ = s.onCookies(r.Context())
	}
	writeOK(w, map[string]any{
		"status":       status.Status,
		"updated_at":   status.UpdatedAt,
		"resumed_jobs": resumed,
	})
}

func (s *Server) handleProductTypes(w http.ResponseWriter, r *http.Request) {
	enterprise := r.URL.Query().Get("enterprise") == "true"
	writeOK(w, map[string]any{"types": service.ListProductTypes(enterprise)})
}

func (s *Server) handleCourseLookup(w http.ResponseWriter, r *http.Request) {
	var req service.LookupRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, &apperr.APIError{Code: apperr.CodeBadRequest, Message: err.Error(), HTTPStatus: 400})
		return
	}
	client, err := s.authMgr.RequireClient()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	s.svc.SetClient(client)
	course, err := s.svc.LookupCourse(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeOK(w, course)
}

func (s *Server) handleCreateDownload(w http.ResponseWriter, r *http.Request) {
	var req service.DownloadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, &apperr.APIError{Code: apperr.CodeBadRequest, Message: err.Error(), HTTPStatus: 400})
		return
	}
	if req.Mode == "" {
		pt, ok := service.GetProductType(req.ProductType, req.Enterprise)
		if !ok {
			writeAPIError(w, &apperr.APIError{Code: apperr.CodeBadRequest, Message: "unknown product_type", HTTPStatus: 400})
			return
		}
		if pt.NeedSelectArticle {
			req.Mode = "all"
		} else {
			req.Mode = "single_video"
		}
	}
	if _, err := s.authMgr.RequireClient(); err != nil {
		writeAPIError(w, err)
		return
	}
	j, err := s.store.CreateJob(r.Context(), req)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Mode != "single_video" {
		client := s.authMgr.GetClient()
		s.svc.SetClient(client)
		if course, lerr := s.svc.LookupCourse(r.Context(), service.LookupRequest{
			ProductType: req.ProductType,
			ProductID:   req.ProductID,
			Enterprise:  req.Enterprise,
		}); lerr == nil {
			_ = job.InitJobArticles(r.Context(), s.store, j.ID, course)
		}
	}
	s.worker.Enqueue()
	writeAccepted(w, map[string]any{
		"id":         j.ID,
		"status":     j.Status,
		"created_at": j.CreatedAt,
		"poll_url":   "/api/v1/downloads/" + j.ID,
	})
}

func (s *Server) handleListDownloads(w http.ResponseWriter, r *http.Request) {
	var statuses []string
	if st := r.URL.Query().Get("status"); st != "" {
		statuses = strings.Split(st, ",")
	}
	jobs, err := s.store.ListJobs(r.Context(), statuses, 20, 0)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeOK(w, map[string]any{"jobs": jobs})
}

func (s *Server) handleGetDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	j, err := s.store.GetJob(r.Context(), id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if j.Status == job.StatusRunning && j.StartedAt != nil {
		j.RuntimeSeconds = int64(time.Since(*j.StartedAt).Seconds())
	}
	writeOK(w, j)
}

func (s *Server) handleRetryDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.RetryJob(r.Context(), id); err != nil {
		writeAPIError(w, err)
		return
	}
	s.worker.Enqueue()
	j, _ := s.store.GetJob(r.Context(), id)
	writeOK(w, j)
}

func (s *Server) handleCancelDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.worker.ActiveJobID() == id {
		s.worker.CancelActive()
	}
	if err := s.store.CancelJob(r.Context(), id); err != nil {
		writeAPIError(w, err)
		return
	}
	j, _ := s.store.GetJob(r.Context(), id)
	writeOK(w, j)
}

func (s *Server) handleDeleteDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteJob(r.Context(), id); err != nil {
		writeAPIError(w, err)
		return
	}
	writeOK(w, map[string]any{"deleted": id})
}

func nilString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
