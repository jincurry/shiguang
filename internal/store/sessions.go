package store

import (
	"context"
	"database/sql"
	"fmt"
)

// UploadSession 对应 upload_sessions 表一行（s3 直传会话）。
type UploadSession struct {
	ID          string
	PhotoID     string
	ObjectKey   string
	ExpectSize  int64
	ContentType string
	State       string // issued | confirmed | expired | aborted
	ExpiresAt   string
	CreatedAt   string
}

const sessionCols = "id, photo_id, object_key, expect_size, content_type, state, expires_at, created_at"

func scanSession(row interface{ Scan(...any) error }) (*UploadSession, error) {
	var u UploadSession
	if err := row.Scan(&u.ID, &u.PhotoID, &u.ObjectKey, &u.ExpectSize,
		&u.ContentType, &u.State, &u.ExpiresAt, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// CreateSession 插入上传会话。
func (s *Store) CreateSession(ctx context.Context, u *UploadSession) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO upload_sessions (id, photo_id, object_key, expect_size, content_type,
		   state, expires_at, created_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		u.ID, u.PhotoID, u.ObjectKey, u.ExpectSize, u.ContentType,
		u.State, u.ExpiresAt, u.CreatedAt)
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// IssuedSessionByPhoto 取某照片当前 issued 状态的会话。
func (s *Store) IssuedSessionByPhoto(ctx context.Context, photoID string) (*UploadSession, error) {
	u, err := scanSession(s.reader.QueryRowContext(ctx,
		`SELECT `+sessionCols+` FROM upload_sessions
		 WHERE photo_id=? AND state='issued' ORDER BY created_at DESC LIMIT 1`, photoID))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: session by photo: %w", err)
	}
	return u, nil
}

// SetSessionState 迁移会话状态（仅允许从 issued 出发，防止重复 confirm）。
func (s *Store) SetSessionState(ctx context.Context, id, state string) (bool, error) {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE upload_sessions SET state=? WHERE id=? AND state='issued'`, state, id)
	if err != nil {
		return false, fmt.Errorf("store: set session state: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ExpiredSessions 列出已过期但仍 issued 的会话（reaper 用）。
func (s *Store) ExpiredSessions(ctx context.Context, now string) ([]*UploadSession, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM upload_sessions
		 WHERE state='issued' AND expires_at < ?`, now)
	if err != nil {
		return nil, fmt.Errorf("store: expired sessions: %w", err)
	}
	defer rows.Close()
	var out []*UploadSession
	for rows.Next() {
		u, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}
