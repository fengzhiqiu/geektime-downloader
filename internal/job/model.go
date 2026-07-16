package job

import (
	"encoding/json"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
	"github.com/nicoxiang/geektime-downloader/internal/service"
)

const (
	StatusPending           = "pending"
	StatusRunning           = "running"
	StatusCompleted         = "completed"
	StatusFailed            = "failed"
	StatusCancelled         = "cancelled"
	StatusWaitingAuth       = "waiting_auth"
	StatusWaitingRateLimit  = "waiting_rate_limit"
)

// Job is a persisted download task.
type Job struct {
	ID              string                 `json:"id"`
	Status          string                 `json:"status"`
	StatusReason    string                 `json:"status_reason,omitempty"`
	IdempotencyKey  string                 `json:"idempotency_key,omitempty"`
	Request         service.DownloadRequest `json:"request"`
	Course          *geektime.Course       `json:"course,omitempty"`
	Progress        Progress               `json:"progress"`
	Articles        []ArticleProgress      `json:"articles,omitempty"`
	Error           *apperr.APIError       `json:"error,omitempty"`
	DownloadFolder  string                 `json:"download_folder,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	StartedAt       *time.Time             `json:"started_at,omitempty"`
	FinishedAt      *time.Time             `json:"finished_at,omitempty"`
	RuntimeSeconds  int64                  `json:"runtime_seconds,omitempty"`
}

// Progress summarizes job-level download progress.
type Progress struct {
	Total           int              `json:"total"`
	Completed       int              `json:"completed"`
	Skipped         int              `json:"skipped"`
	Failed          int              `json:"failed"`
	CurrentArticle  *CurrentArticle  `json:"current_article,omitempty"`
}

// CurrentArticle is the article currently being downloaded.
type CurrentArticle struct {
	AID   int    `json:"aid"`
	Title string `json:"title"`
	Phase string `json:"phase"`
	Done  int    `json:"done,omitempty"`
	Total int    `json:"total,omitempty"`
}

// ArticleProgress tracks per-article status within a job.
type ArticleProgress struct {
	AID    int      `json:"aid"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Files  []string `json:"files,omitempty"`
	Error  *apperr.APIError `json:"error,omitempty"`
}

func encodeJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func decodeJSON[T any](s string, out *T) {
	if s == "" {
		return
	}
	_ = json.Unmarshal([]byte(s), out)
}
