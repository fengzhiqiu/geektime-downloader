package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nicoxiang/geektime-downloader/internal/apperr"
	"github.com/nicoxiang/geektime-downloader/internal/config"
	"github.com/nicoxiang/geektime-downloader/internal/geektime"
)

const (
	SessionValid          = "valid"
	SessionExpired        = "expired"
	SessionUnknown        = "unknown"
	SessionNotConfigured  = "not_configured"
)

// SessionStatus describes cookie session health.
type SessionStatus struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// SessionStore persists cookie credentials.
type SessionStore interface {
	Get(ctx context.Context) (gcid, gcess, status string, updatedAt time.Time, err error)
	Save(ctx context.Context, gcid, gcess, status string) error
}

// Manager manages geektime authentication state.
type Manager struct {
	store  SessionStore
	client atomic.Pointer[geektime.Client]
	mu     sync.RWMutex
}

// NewManager creates an Auth Manager.
func NewManager(store SessionStore) *Manager {
	return &Manager{store: store}
}

// Init loads session from store and optionally validates startup cookies.
func (m *Manager) Init(ctx context.Context, startupGcid, startupGcess string) error {
	gcid, gcess, status, _, err := m.store.Get(ctx)
	if err != nil {
		return err
	}
	if startupGcid != "" && startupGcess != "" {
		_, err = m.UpdateCookies(ctx, startupGcid, startupGcess)
		return err
	}
	if gcid != "" && gcess != "" {
		cookies := config.BuildCookies(gcid, gcess)
		client := geektime.NewClient(cookies)
		m.client.Store(client)
		if status == SessionValid {
			return nil
		}
		if err := geektime.Auth(cookies); err != nil {
			_ = m.store.Save(ctx, gcid, gcess, SessionExpired)
			return nil
		}
		_ = m.store.Save(ctx, gcid, gcess, SessionValid)
	}
	return nil
}

// GetClient returns the current geektime API client, or nil if not configured.
func (m *Manager) GetClient() *geektime.Client {
	return m.client.Load()
}

// Status returns current session status.
func (m *Manager) Status(ctx context.Context) SessionStatus {
	gcid, gcess, status, updatedAt, err := m.store.Get(ctx)
	if err != nil || (gcid == "" && gcess == "") {
		return SessionStatus{Status: SessionNotConfigured}
	}
	if status == "" {
		status = SessionUnknown
	}
	return SessionStatus{Status: status, UpdatedAt: updatedAt}
}

// UpdateCookies validates and persists new cookies.
func (m *Manager) UpdateCookies(ctx context.Context, gcid, gcess string) (*SessionStatus, error) {
	if gcid == "" || gcess == "" {
		return nil, &apperr.APIError{
			Code: apperr.CodeBadRequest, Message: "gcid and gcess are required",
			HTTPStatus: 400,
		}
	}
	cookies := config.BuildCookies(gcid, gcess)
	if err := geektime.Auth(cookies); err != nil {
		_ = m.store.Save(ctx, gcid, gcess, SessionExpired)
		return nil, &apperr.APIError{
			Code: apperr.CodeAuthInvalid, Message: err.Error(),
			Action: "UPDATE_COOKIES", Retryable: false, HTTPStatus: 400, Underlying: err,
		}
	}
	if err := m.store.Save(ctx, gcid, gcess, SessionValid); err != nil {
		return nil, err
	}
	client := geektime.NewClient(cookies)
	m.client.Store(client)
	now := time.Now().UTC()
	return &SessionStatus{Status: SessionValid, UpdatedAt: now}, nil
}

// RequireClient returns client or error if session not configured.
func (m *Manager) RequireClient() (*geektime.Client, error) {
	client := m.client.Load()
	if client == nil {
		return nil, &apperr.APIError{
			Code: apperr.CodeAuthInvalid, Message: "session not configured",
			Action: "UPDATE_COOKIES",
			ActionHint: "调用 PUT /api/v1/session/cookies 设置 gcid/gcess",
			Retryable: false, HTTPStatus: 401,
		}
	}
	return client, nil
}
