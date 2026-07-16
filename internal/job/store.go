package job

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/service"
	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS session (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  gcid TEXT NOT NULL DEFAULT '',
  gcess TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'not_configured',
  updated_at TEXT NOT NULL DEFAULT ''
);

INSERT OR IGNORE INTO session (id, gcid, gcess, status, updated_at) VALUES (1, '', '', 'not_configured', '');

CREATE TABLE IF NOT EXISTS jobs (
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  status_reason TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT UNIQUE,
  request_json TEXT NOT NULL,
  course_json TEXT NOT NULL DEFAULT '',
  progress_json TEXT NOT NULL DEFAULT '{}',
  error_json TEXT NOT NULL DEFAULT '',
  download_folder TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  started_at TEXT NOT NULL DEFAULT '',
  finished_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs(status);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs(created_at DESC);

CREATE TABLE IF NOT EXISTS job_articles (
  job_id TEXT NOT NULL,
  aid INTEGER NOT NULL,
  title TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  files_json TEXT NOT NULL DEFAULT '[]',
  error_json TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (job_id, aid),
  FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
`

// Store persists jobs in SQLite.
type Store struct {
	db *sql.DB
}

// OpenStore opens the job database and runs migrations.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying database for session store sharing.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Close closes the database.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateJob inserts a new pending job.
func (s *Store) CreateJob(ctx context.Context, req service.DownloadRequest) (*Job, error) {
	if req.IdempotencyKey != "" {
		if existing, err := s.FindByIdempotencyKey(ctx, req.IdempotencyKey); err != nil {
			return nil, err
		} else if existing != nil {
			return existing, nil
		}
	}
	now := time.Now().UTC()
	job := &Job{
		ID:       "job_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		Status:   StatusPending,
		Request:  req,
		Progress: Progress{},
		CreatedAt: now,
		UpdatedAt: now,
	}
	var idem any
	if req.IdempotencyKey != "" {
		idem = req.IdempotencyKey
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO jobs (id, status, idempotency_key, request_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, job.ID, job.Status, idem, encodeJSON(req), now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	return job, nil
}

// FindByIdempotencyKey returns an existing job for the key.
func (s *Store) FindByIdempotencyKey(ctx context.Context, key string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id FROM jobs WHERE idempotency_key = ?`, key)
	var id string
	if err := row.Scan(&id); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return s.GetJob(ctx, id)
}

// GetJob loads a job by id.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, status, status_reason, idempotency_key, request_json, course_json,
		       progress_json, error_json, download_folder, created_at, updated_at, started_at, finished_at
		FROM jobs WHERE id = ?
	`, id)
	job, err := scanJob(row)
	if err == sql.ErrNoRows {
		return nil, &apperr.APIError{Code: apperr.CodeNotFound, Message: "job not found", HTTPStatus: 404}
	}
	if err != nil {
		return nil, err
	}
	articles, err := s.listArticles(ctx, id)
	if err != nil {
		return nil, err
	}
	job.Articles = articles
	return job, nil
}

// ListJobs returns jobs matching optional status filter.
func (s *Store) ListJobs(ctx context.Context, statuses []string, limit, offset int) ([]*Job, error) {
	if limit <= 0 {
		limit = 20
	}
	query := `SELECT id, status, status_reason, idempotency_key, request_json, course_json,
		progress_json, error_json, download_folder, created_at, updated_at, started_at, finished_at
		FROM jobs`
	var args []any
	if len(statuses) > 0 {
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}
		query += " WHERE status IN (" + strings.Join(placeholders, ",") + ")"
	}
	query += " ORDER BY created_at DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []*Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// RecoverRunningJobs resets interrupted running jobs to pending on startup.
func (s *Store) RecoverRunningJobs(ctx context.Context) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, updated_at = ? WHERE status = ?
	`, StatusPending, now, StatusRunning)
	return err
}

// ResumeWaitingAuthJobs moves waiting_auth jobs back to pending after cookie refresh.
func (s *Store) ResumeWaitingAuthJobs(ctx context.Context) ([]string, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE status = ?`, StatusWaitingAuth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, rows.Err()
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, status_reason = '', error_json = '', updated_at = ?
		WHERE status = ?
	`, StatusPending, now, StatusWaitingAuth)
	return ids, err
}

// MarkStaleJobs flags running jobs whose progress has not updated within
// heartbeatTimeout as stale_progress (status stays running). Does not force-end.
func (s *Store) MarkStaleJobs(ctx context.Context, heartbeatTimeout time.Duration) error {
	cutoff := time.Now().UTC().Add(-heartbeatTimeout).Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status_reason = ?, updated_at = ?
		WHERE status = ? AND updated_at < ? AND status_reason = ''
	`, "stale_progress", now, StatusRunning, cutoff)
	return err
}

// ResumeRateLimitJobs moves waiting_rate_limit jobs back to pending after cooldown.
func (s *Store) ResumeRateLimitJobs(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, status_reason = '', error_json = '', updated_at = ?
		WHERE status = ?
	`, StatusPending, now, StatusWaitingRateLimit)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpdateJobStatus updates job status and optional error.
func (s *Store) UpdateJobStatus(ctx context.Context, id, status, reason string, apiErr *apperr.APIError) error {
	now := time.Now().UTC().Format(time.RFC3339)
	errJSON := ""
	if apiErr != nil {
		errJSON = encodeJSON(apiErr)
	}
	finished := ""
	if status == StatusCompleted || status == StatusFailed || status == StatusCancelled {
		finished = now
	}
	started := ""
	if status == StatusRunning {
		started = now
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET status = ?, status_reason = ?, error_json = ?, updated_at = ?,
			started_at = CASE WHEN ? != '' THEN ? ELSE started_at END,
			finished_at = CASE WHEN ? != '' THEN ? ELSE finished_at END
		WHERE id = ?
	`, status, reason, errJSON, now, started, started, finished, finished, id)
	return err
}

// UpdateJobProgress updates progress snapshot and course info.
func (s *Store) UpdateJobProgress(ctx context.Context, id string, progress Progress, courseJSON, downloadFolder string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE jobs SET progress_json = ?, course_json = ?, download_folder = ?, updated_at = ?
		WHERE id = ?
	`, encodeJSON(progress), courseJSON, downloadFolder, now, id)
	return err
}

// UpsertArticleProgress updates per-article progress.
func (s *Store) UpsertArticleProgress(ctx context.Context, jobID string, article ArticleProgress) error {
	now := time.Now().UTC().Format(time.RFC3339)
	errJSON := ""
	if article.Error != nil {
		errJSON = encodeJSON(article.Error)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO job_articles (job_id, aid, title, status, files_json, error_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id, aid) DO UPDATE SET
			title = excluded.title,
			status = excluded.status,
			files_json = excluded.files_json,
			error_json = excluded.error_json,
			updated_at = excluded.updated_at
	`, jobID, article.AID, article.Title, article.Status, encodeJSON(article.Files), errJSON, now)
	return err
}

// NextPendingJob returns the oldest pending job id.
func (s *Store) NextPendingJob(ctx context.Context) (string, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id FROM jobs WHERE status = ? ORDER BY created_at ASC LIMIT 1
	`, StatusPending)
	var id string
	if err := row.Scan(&id); err == sql.ErrNoRows {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return id, nil
}

func (s *Store) listArticles(ctx context.Context, jobID string) ([]ArticleProgress, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT aid, title, status, files_json, error_json FROM job_articles
		WHERE job_id = ? ORDER BY aid
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var articles []ArticleProgress
	for rows.Next() {
		var a ArticleProgress
		var filesJSON, errJSON string
		if err := rows.Scan(&a.AID, &a.Title, &a.Status, &filesJSON, &errJSON); err != nil {
			return nil, err
		}
		decodeJSON(filesJSON, &a.Files)
		if errJSON != "" {
			var apiErr apperr.APIError
			decodeJSON(errJSON, &apiErr)
			a.Error = &apiErr
		}
		articles = append(articles, a)
	}
	return articles, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanJob(row scannable) (*Job, error) {
	var job Job
	var idem sql.NullString
	var requestJSON, courseJSON, progressJSON, errorJSON, startedAt, finishedAt string
	var createdAt, updatedAt string
	if err := row.Scan(
		&job.ID, &job.Status, &job.StatusReason, &idem, &requestJSON, &courseJSON,
		&progressJSON, &errorJSON, &job.DownloadFolder, &createdAt, &updatedAt, &startedAt, &finishedAt,
	); err != nil {
		return nil, err
	}
	decodeJSON(requestJSON, &job.Request)
	if idem.Valid {
		job.IdempotencyKey = idem.String
	}
	if courseJSON != "" {
		var course geektime.Course
		decodeJSON(courseJSON, &course)
		job.Course = &course
	}
	decodeJSON(progressJSON, &job.Progress)
	if errorJSON != "" {
		var apiErr apperr.APIError
		decodeJSON(errorJSON, &apiErr)
		job.Error = &apiErr
	}
	job.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	job.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAt)
	if startedAt != "" {
		t, _ := time.Parse(time.RFC3339, startedAt)
		job.StartedAt = &t
	}
	if finishedAt != "" {
		t, _ := time.Parse(time.RFC3339, finishedAt)
		job.FinishedAt = &t
	}
	return &job, nil
}
