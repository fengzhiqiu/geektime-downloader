package auth

import (
	"context"
	"database/sql"
	"time"
)

// SQLiteSessionStore stores session in SQLite.
type SQLiteSessionStore struct {
	db *sql.DB
}

// NewSQLiteSessionStore creates a session store.
func NewSQLiteSessionStore(db *sql.DB) *SQLiteSessionStore {
	return &SQLiteSessionStore{db: db}
}

// Get reads the singleton session row.
func (s *SQLiteSessionStore) Get(ctx context.Context) (gcid, gcess, status string, updatedAt time.Time, err error) {
	row := s.db.QueryRowContext(ctx, `SELECT gcid, gcess, status, updated_at FROM session WHERE id = 1`)
	var updatedAtStr string
	err = row.Scan(&gcid, &gcess, &status, &updatedAtStr)
	if err == sql.ErrNoRows {
		return "", "", SessionNotConfigured, time.Time{}, nil
	}
	if err != nil {
		return "", "", "", time.Time{}, err
	}
	if updatedAtStr != "" {
		updatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
	}
	return gcid, gcess, status, updatedAt, nil
}

// Save upserts the singleton session row.
func (s *SQLiteSessionStore) Save(ctx context.Context, gcid, gcess, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO session (id, gcid, gcess, status, updated_at)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			gcid = excluded.gcid,
			gcess = excluded.gcess,
			status = excluded.status,
			updated_at = excluded.updated_at
	`, gcid, gcess, status, now)
	return err
}
