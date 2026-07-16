package job

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/progress"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

// Stability holds watchdog / cooldown durations for the worker.
type Stability struct {
	JobTimeout        time.Duration
	HeartbeatTimeout  time.Duration
	RateLimitCooldown time.Duration
}

// executor decouples the worker from DownloadService for testability.
// *service.DownloadService satisfies it; tests inject a fake.
type executor interface {
	SetClient(c *geektime.Client)
	Client() *geektime.Client
	ExecuteDownload(ctx context.Context, req service.DownloadRequest, reporter progress.Reporter) (geektime.Course, string, error)
}

// Worker executes download jobs sequentially.
type Worker struct {
	store          *Store
	exec           executor
	clientProvider func() *geektime.Client
	stability      Stability
	pausedAuth     atomic.Bool
	rateLimitUntil atomic.Int64 // unix nano when global rate-limit cooldown ends; 0 = none
	cooldownStep   atomic.Int64

	mu          sync.Mutex
	activeJobID string
	cancel      context.CancelFunc
	notify      chan struct{}
}

// NewWorker creates a job worker.
func NewWorker(store *Store, exec executor, clientProvider func() *geektime.Client, st Stability) *Worker {
	return &Worker{
		store: store, exec: exec, clientProvider: clientProvider,
		stability: st,
		notify:    make(chan struct{}, 1),
	}
}

func (w *Worker) jobTimeoutOrDefault() time.Duration {
	if w.stability.JobTimeout > 0 {
		return w.stability.JobTimeout
	}
	return 60 * time.Minute
}

const rateLimitCooldownCap = 30 * time.Minute

func (w *Worker) rateLimitCooldownOrDefault() time.Duration {
	if w.stability.RateLimitCooldown > 0 {
		return w.stability.RateLimitCooldown
	}
	return 120 * time.Second
}

func (w *Worker) applyRateLimitCooldown() {
	step := w.cooldownStep.Load()
	base := int64(w.rateLimitCooldownOrDefault())
	shift := step
	if shift > 2 {
		shift = 2 // cap at 4x (120s->240s->480s)
	}
	cooldown := base << shift
	if cooldown > int64(rateLimitCooldownCap) {
		cooldown = int64(rateLimitCooldownCap)
	}
	w.rateLimitUntil.Store(time.Now().UnixNano() + cooldown)
	w.cooldownStep.Add(1)
}

func (w *Worker) cooldownActive() bool {
	until := w.rateLimitUntil.Load()
	if until == 0 {
		return false
	}
	return time.Now().UnixNano() < until
}

// Start runs the worker loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	go w.loop(ctx)
	go w.stabilityLoop(ctx)
}

// Enqueue notifies the worker that a new job is available.
func (w *Worker) Enqueue() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// OnCookiesUpdated resumes jobs waiting for auth.
func (w *Worker) OnCookiesUpdated(ctx context.Context) ([]string, error) {
	w.pausedAuth.Store(false)
	ids, err := w.store.ResumeWaitingAuthJobs(ctx)
	if err != nil {
		return nil, err
	}
	w.Enqueue()
	return ids, nil
}

// CancelActive cancels the currently running job's context.
func (w *Worker) CancelActive() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cancel != nil {
		w.cancel()
	}
}

// ActiveJobID returns the running job id, if any.
func (w *Worker) ActiveJobID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeJobID
}

func (w *Worker) loop(ctx context.Context) {
	for {
		if w.pausedAuth.Load() {
			select {
			case <-ctx.Done():
				return
			case <-w.notify:
			}
			continue
		}
		if w.cooldownActive() {
			residue := time.Until(time.Unix(0, w.rateLimitUntil.Load()))
			select {
			case <-ctx.Done():
				return
			case <-time.After(residue):
			}
			continue
		}
		jobID, err := w.store.NextPendingJob(ctx)
		if err != nil || jobID == "" {
			select {
			case <-ctx.Done():
				return
			case <-w.notify:
			}
			continue
		}
		w.runJob(ctx, jobID)
	}
}

func (w *Worker) stabilityLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb := w.stability.HeartbeatTimeout
			if hb <= 0 {
				hb = 10 * time.Minute
			}
			_ = w.store.MarkStaleJobs(ctx, hb)
			if w.rateLimitUntil.Load() != 0 && !w.cooldownActive() {
				if _, err := w.store.ResumeRateLimitJobs(ctx); err == nil {
					w.rateLimitUntil.Store(0)
					if step := w.cooldownStep.Load(); step > 0 {
						w.cooldownStep.Store(step / 2)
					}
					w.Enqueue()
				}
			}
		}
	}
}

func (w *Worker) runJob(parent context.Context, jobID string) {
	jobCtx, cancel := context.WithTimeout(parent, w.jobTimeoutOrDefault())
	w.mu.Lock()
	w.activeJobID = jobID
	w.cancel = cancel
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.activeJobID = ""
		w.cancel = nil
		w.mu.Unlock()
		cancel()
	}()

	job, err := w.store.GetJob(jobCtx, jobID)
	if err != nil {
		return
	}
	_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusRunning, "", nil)

	if w.clientProvider != nil {
		w.exec.SetClient(w.clientProvider())
	}
	if w.exec.Client() == nil {
		apiErr := &apperr.APIError{
			Code: apperr.CodeAuthExpired, Message: "session not configured",
			Action: "UPDATE_COOKIES", Retryable: true, HTTPStatus: 401,
		}
		w.pausedAuth.Store(true)
		_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingAuth, apiErr.Message, apiErr)
		return
	}

	reporter := &storeReporter{
		ctx:    jobCtx,
		store:  w.store,
		jobID:  jobID,
		titles: map[int]string{},
	}

	course, folder, err := w.exec.ExecuteDownload(jobCtx, job.Request, reporter)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			apiErr := apperr.MapError(err)
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusFailed, "watchdog_timeout", apiErr)
			return
		}
		apiErr := apperr.MapError(err)
		switch apiErr.Code {
		case apperr.CodeAuthExpired:
			w.pausedAuth.Store(true)
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingAuth, apiErr.Message, apiErr)
		case apperr.CodeRateLimited:
			w.applyRateLimitCooldown()
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusWaitingRateLimit, apiErr.Message, apiErr)
		case apperr.CodeCancelled:
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusCancelled, apiErr.Message, apiErr)
		default:
			_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusFailed, apiErr.Message, apiErr)
		}
		return
	}
	courseJSON := encodeJSON(course)
	_ = w.store.UpdateJobProgress(jobCtx, jobID, reporter.progress, courseJSON, folder)
	_ = w.store.UpdateJobStatus(jobCtx, jobID, StatusCompleted, "", nil)
}

type storeReporter struct {
	ctx      context.Context
	store    *Store
	jobID    string
	titles   map[int]string
	progress Progress
}

func (r *storeReporter) OnArticleStart(aid int, title, phase string) {
	r.titles[aid] = title
	r.progress.CurrentArticle = &CurrentArticle{AID: aid, Title: title, Phase: phase}
	_ = r.store.UpsertArticleProgress(r.ctx, r.jobID, ArticleProgress{
		AID: aid, Title: title, Status: StatusRunning,
	})
	_ = r.store.UpdateJobProgress(r.ctx, r.jobID, r.progress, "", "")
}

func (r *storeReporter) OnArticleComplete(aid int, files []string) {
	title := r.titles[aid]
	r.progress.Completed++
	r.progress.CurrentArticle = nil
	_ = r.store.UpsertArticleProgress(r.ctx, r.jobID, ArticleProgress{
		AID: aid, Title: title, Status: StatusCompleted, Files: files,
	})
	_ = r.store.UpdateJobProgress(r.ctx, r.jobID, r.progress, "", "")
}

func (r *storeReporter) OnArticleSkipped(aid int, reason string) {
	title := r.titles[aid]
	if title == "" {
		title = reason
	}
	r.progress.Skipped++
	_ = r.store.UpsertArticleProgress(r.ctx, r.jobID, ArticleProgress{
		AID: aid, Title: title, Status: "skipped",
	})
	_ = r.store.UpdateJobProgress(r.ctx, r.jobID, r.progress, "", "")
}

func (r *storeReporter) OnArticleFailed(aid int, err error) {
	title := r.titles[aid]
	r.progress.Failed++
	apiErr := apperr.MapError(err)
	_ = r.store.UpsertArticleProgress(r.ctx, r.jobID, ArticleProgress{
		AID: aid, Title: title, Status: StatusFailed, Error: apiErr,
	})
	_ = r.store.UpdateJobProgress(r.ctx, r.jobID, r.progress, "", "")
}

func (r *storeReporter) OnArticleProgress(aid, done, total int) {
	if r.progress.CurrentArticle == nil || r.progress.CurrentArticle.AID != aid {
		r.progress.CurrentArticle = &CurrentArticle{AID: aid, Phase: "downloading_video"}
	}
	r.progress.CurrentArticle.Done = done
	r.progress.CurrentArticle.Total = total
	_ = r.store.UpdateJobProgress(r.ctx, r.jobID, r.progress, "", "")
}

var _ progress.Reporter = (*storeReporter)(nil)

// InitJobArticles seeds article rows after course lookup for progress tracking.
func InitJobArticles(ctx context.Context, store *Store, jobID string, course geektime.Course) error {
	progress := Progress{Total: len(course.Articles)}
	for _, a := range course.Articles {
		if err := store.UpsertArticleProgress(ctx, jobID, ArticleProgress{
			AID: a.AID, Title: a.Title, Status: StatusPending,
		}); err != nil {
			return err
		}
	}
	courseJSON := encodeJSON(course)
	return store.UpdateJobProgress(ctx, jobID, progress, courseJSON, "")
}

// RetryJob resets a job to pending.
func (s *Store) RetryJob(ctx context.Context, id string) error {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	switch job.Status {
	case StatusFailed, StatusWaitingAuth, StatusWaitingRateLimit, StatusCancelled:
	default:
		return &apperr.APIError{
			Code: apperr.CodeBadRequest,
			Message: "job cannot be retried in status " + job.Status,
			HTTPStatus: 400,
		}
	}
	return s.UpdateJobStatus(ctx, id, StatusPending, "", nil)
}

// CancelJob marks a job cancelled.
func (s *Store) CancelJob(ctx context.Context, id string) error {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == StatusCompleted {
		return &apperr.APIError{Code: apperr.CodeBadRequest, Message: "completed job cannot be cancelled", HTTPStatus: 400}
	}
	return s.UpdateJobStatus(ctx, id, StatusCancelled, "cancelled by user", nil)
}

// DeleteJob removes a job record.
func (s *Store) DeleteJob(ctx context.Context, id string) error {
	job, err := s.GetJob(ctx, id)
	if err != nil {
		return err
	}
	if job.Status == StatusRunning {
		return &apperr.APIError{Code: apperr.CodeBadRequest, Message: "cancel running job first", HTTPStatus: 400}
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	return err
}
