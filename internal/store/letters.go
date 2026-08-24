package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Letter 是信箱里的一封信。
type Letter struct {
	ID        string
	Title     string
	Body      string
	Sender    string
	DeliverAt string // 到这个时刻才出现在前台信箱里
	ReadAt    *string
	CreatedAt string
	UpdatedAt string
	DeletedAt *string
}

const letterCols = `id, title, body, sender, deliver_at, read_at,
	created_at, updated_at, deleted_at`

func scanLetter(row interface{ Scan(...any) error }) (*Letter, error) {
	var l Letter
	if err := row.Scan(&l.ID, &l.Title, &l.Body, &l.Sender, &l.DeliverAt, &l.ReadAt,
		&l.CreatedAt, &l.UpdatedAt, &l.DeletedAt); err != nil {
		return nil, err
	}
	return &l, nil
}

// CreateLetter 写一封信。
func (s *Store) CreateLetter(ctx context.Context, l *Letter) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO letters (id, title, body, sender, deliver_at, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?)`,
		l.ID, l.Title, l.Body, l.Sender, l.DeliverAt, l.CreatedAt, l.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: create letter: %w", err)
	}
	return nil
}

// Letters 列出信件。until 非空时只取已投递的（deliver_at <= until），
// 前台走这条，未到投递时间的信连标题都不会离开数据库。
func (s *Store) Letters(ctx context.Context, until string, limit int) ([]*Letter, error) {
	q := `SELECT ` + letterCols + ` FROM letters WHERE deleted_at IS NULL`
	args := []any{}
	if until != "" {
		q += ` AND deliver_at <= ?`
		args = append(args, until)
	}
	q += ` ORDER BY deliver_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: letters: %w", err)
	}
	defer rows.Close()
	var out []*Letter
	for rows.Next() {
		l, err := scanLetter(rows)
		if err != nil {
			return nil, fmt.Errorf("store: letters scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetLetter 取一封信。until 非空时，未投递的信按“不存在”处理。
func (s *Store) GetLetter(ctx context.Context, id, until string) (*Letter, error) {
	q := `SELECT ` + letterCols + ` FROM letters WHERE id=? AND deleted_at IS NULL`
	args := []any{id}
	if until != "" {
		q += ` AND deliver_at <= ?`
		args = append(args, until)
	}
	l, err := scanLetter(s.reader.QueryRowContext(ctx, q, args...))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get letter: %w", err)
	}
	return l, nil
}

// UpdateLetter 改信的内容与投递时间。
func (s *Store) UpdateLetter(ctx context.Context, id, title, body, sender, deliverAt, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE letters SET title=?, body=?, sender=?, deliver_at=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		title, body, sender, deliverAt, now, id)
	if err != nil {
		return fmt.Errorf("store: update letter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkLetterRead 记下首次读取的时刻；已读过的不覆盖。
func (s *Store) MarkLetterRead(ctx context.Context, id, now string) error {
	_, err := s.writer.ExecContext(ctx,
		`UPDATE letters SET read_at=? WHERE id=? AND read_at IS NULL AND deleted_at IS NULL`,
		now, id)
	if err != nil {
		return fmt.Errorf("store: mark letter read: %w", err)
	}
	return nil
}

// DeleteLetter 软删一封信。
func (s *Store) DeleteLetter(ctx context.Context, id, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE letters SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("store: delete letter: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
