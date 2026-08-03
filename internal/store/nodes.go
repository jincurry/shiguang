package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Node 对应 nodes 表一行。
type Node struct {
	ID          string
	Date        string // YYYY-MM-DD
	Title       string
	Description string
	CreatedAt   string
	UpdatedAt   string
	DeletedAt   *string
	PhotoCount  int // 联查得出（仅存活照片）
}

const nodeCols = "id, date, title, description, created_at, updated_at, deleted_at"

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	if err := row.Scan(&n.ID, &n.Date, &n.Title, &n.Description,
		&n.CreatedAt, &n.UpdatedAt, &n.DeletedAt); err != nil {
		return nil, err
	}
	return &n, nil
}

// CreateNode 插入节点。
func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO nodes (id, date, title, description, created_at, updated_at)
		 VALUES (?,?,?,?,?,?)`,
		n.ID, n.Date, n.Title, n.Description, n.CreatedAt, n.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: create node: %w", err)
	}
	return nil
}

// GetNode 按 id 取节点；includeDeleted=false 时软删行视为不存在。
func (s *Store) GetNode(ctx context.Context, id string, includeDeleted bool) (*Node, error) {
	q := "SELECT " + nodeCols + " FROM nodes WHERE id=?"
	if !includeDeleted {
		q += " AND deleted_at IS NULL"
	}
	n, err := scanNode(s.reader.QueryRowContext(ctx, q, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get node: %w", err)
	}
	var cnt int
	err = s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photos WHERE node_id=? AND deleted_at IS NULL`, id).Scan(&cnt)
	if err != nil {
		return nil, fmt.Errorf("store: get node count: %w", err)
	}
	n.PhotoCount = cnt
	return n, nil
}

// UpdateNode 更新节点字段（date/title/description 全量传入）。
func (s *Store) UpdateNode(ctx context.Context, id, date, title, description, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE nodes SET date=?, title=?, description=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		date, title, description, now, id)
	if err != nil {
		return fmt.Errorf("store: update node: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SoftDeleteNode 软删节点并级联软删其存活照片（同一 deleted_at 时间戳，
// 恢复时据此区分"随节点删除"与"更早单独删除"的照片）。
func (s *Store) SoftDeleteNode(ctx context.Context, id, now string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE nodes SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
			now, now, id)
		if err != nil {
			return fmt.Errorf("store: soft delete node: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE photos SET deleted_at=?, updated_at=? WHERE node_id=? AND deleted_at IS NULL`,
			now, now, id)
		if err != nil {
			return fmt.Errorf("store: cascade delete photos: %w", err)
		}
		return nil
	})
}

// RestoreNode 恢复软删节点，并恢复与节点同批软删的照片。
func (s *Store) RestoreNode(ctx context.Context, id, now string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var deletedAt sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT deleted_at FROM nodes WHERE id=?`, id).Scan(&deletedAt)
		if err == sql.ErrNoRows || (err == nil && !deletedAt.Valid) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: restore node lookup: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE nodes SET deleted_at=NULL, updated_at=? WHERE id=?`, now, id); err != nil {
			return fmt.Errorf("store: restore node: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE photos SET deleted_at=NULL, updated_at=? WHERE node_id=? AND deleted_at=?`,
			now, id, deletedAt.String); err != nil {
			return fmt.Errorf("store: restore node photos: %w", err)
		}
		return nil
	})
}

// TimelinePage 返回一页节点（date DESC, id DESC），cursor 为上一页最后一条的 (date,id)。
func (s *Store) TimelinePage(ctx context.Context, cursorDate, cursorID string, limit int) ([]*Node, error) {
	var rows *sql.Rows
	var err error
	if cursorDate == "" {
		rows, err = s.reader.QueryContext(ctx,
			`SELECT `+nodeCols+` FROM nodes WHERE deleted_at IS NULL
			 ORDER BY date DESC, id DESC LIMIT ?`, limit)
	} else {
		// (date,id) 双键 < 定位：严格早于游标位置
		rows, err = s.reader.QueryContext(ctx,
			`SELECT `+nodeCols+` FROM nodes WHERE deleted_at IS NULL
			 AND (date < ? OR (date = ? AND id < ?))
			 ORDER BY date DESC, id DESC LIMIT ?`, cursorDate, cursorDate, cursorID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("store: timeline page: %w", err)
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: timeline scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// PhotosByNodeIDs 批量取多个节点的存活照片（避免 N+1），按 node_id, ord 排序。
func (s *Store) PhotosByNodeIDs(ctx context.Context, nodeIDs []string) (map[string][]*Photo, error) {
	out := make(map[string][]*Photo, len(nodeIDs))
	if len(nodeIDs) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(nodeIDs)), ",")
	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+photoCols+` FROM photos
		 WHERE node_id IN (`+ph+`) AND deleted_at IS NULL
		 ORDER BY node_id, ord, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: photos by nodes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPhoto(rows)
		if err != nil {
			return nil, fmt.Errorf("store: photos scan: %w", err)
		}
		out[p.NodeID] = append(out[p.NodeID], p)
	}
	return out, rows.Err()
}

// Stats 是 GET /stats 的原始统计。
type Stats struct {
	NodeCount  int
	PhotoCount int
	TotalBytes int64
	YearMin    string
	YearMax    string
	TrashCount int
}

// GetStats 汇总统计（存活数据 + 回收站条目数）。
func (s *Store) GetStats(ctx context.Context) (*Stats, error) {
	var st Stats
	err := s.reader.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL),
		  (SELECT COUNT(*) FROM photos WHERE deleted_at IS NULL),
		  (SELECT COALESCE(SUM(size_bytes),0) FROM photos WHERE deleted_at IS NULL),
		  (SELECT COALESCE(MIN(substr(date,1,4)),'') FROM nodes WHERE deleted_at IS NULL),
		  (SELECT COALESCE(MAX(substr(date,1,4)),'') FROM nodes WHERE deleted_at IS NULL),
		  (SELECT COUNT(*) FROM nodes WHERE deleted_at IS NOT NULL) +
		  (SELECT COUNT(*) FROM photos p WHERE p.deleted_at IS NOT NULL
		     AND EXISTS(SELECT 1 FROM nodes n WHERE n.id=p.node_id AND n.deleted_at IS NULL))
	`).Scan(&st.NodeCount, &st.PhotoCount, &st.TotalBytes, &st.YearMin, &st.YearMax, &st.TrashCount)
	if err != nil {
		return nil, fmt.Errorf("store: stats: %w", err)
	}
	return &st, nil
}
