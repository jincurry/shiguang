package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Photo 对应 photos 表一行。
type Photo struct {
	ID         string
	NodeID     string
	Caption    string
	Ord        int64
	Status     string // pending | processing | ready | failed
	FailReason *string
	SHA256     *string
	Ext        *string
	Width      *int64
	Height     *int64
	BlurHash   *string
	Dominant   *string
	SizeBytes  *int64
	TakenAt    *string
	CreatedAt  string
	UpdatedAt  string
	DeletedAt  *string
}

const photoCols = `id, node_id, caption, ord, status, fail_reason, sha256, ext,
	width, height, blurhash, dominant, size_bytes, taken_at,
	created_at, updated_at, deleted_at`

func scanPhoto(row interface{ Scan(...any) error }) (*Photo, error) {
	var p Photo
	if err := row.Scan(&p.ID, &p.NodeID, &p.Caption, &p.Ord, &p.Status, &p.FailReason,
		&p.SHA256, &p.Ext, &p.Width, &p.Height, &p.BlurHash, &p.Dominant,
		&p.SizeBytes, &p.TakenAt, &p.CreatedAt, &p.UpdatedAt, &p.DeletedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

// CreatePhoto 插入照片行。
func (s *Store) CreatePhoto(ctx context.Context, p *Photo) error {
	_, err := s.writer.ExecContext(ctx,
		`INSERT INTO photos (id, node_id, caption, ord, status, fail_reason, sha256, ext,
		   width, height, blurhash, dominant, size_bytes, taken_at, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.NodeID, p.Caption, p.Ord, p.Status, p.FailReason, p.SHA256, p.Ext,
		p.Width, p.Height, p.BlurHash, p.Dominant, p.SizeBytes, p.TakenAt,
		p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: create photo: %w", err)
	}
	return nil
}

// GetPhoto 按 id 取照片；includeDeleted=false 时软删行视为不存在。
func (s *Store) GetPhoto(ctx context.Context, id string, includeDeleted bool) (*Photo, error) {
	q := "SELECT " + photoCols + " FROM photos WHERE id=?"
	if !includeDeleted {
		q += " AND deleted_at IS NULL"
	}
	p, err := scanPhoto(s.reader.QueryRowContext(ctx, q, id))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get photo: %w", err)
	}
	return p, nil
}

// NextOrd 返回节点下一个留缝序号（max(ord)+100，空节点从 100 起）。
func (s *Store) NextOrd(ctx context.Context, nodeID string) (int64, error) {
	var maxOrd sql.NullInt64
	err := s.reader.QueryRowContext(ctx,
		`SELECT MAX(ord) FROM photos WHERE node_id=? AND deleted_at IS NULL`, nodeID).
		Scan(&maxOrd)
	if err != nil {
		return 0, fmt.Errorf("store: next ord: %w", err)
	}
	return maxOrd.Int64 + 100, nil
}

// FindDuplicate 查同节点下相同 sha256 的存活照片（秒传判定）。
func (s *Store) FindDuplicate(ctx context.Context, nodeID, sha string) (*Photo, error) {
	p, err := scanPhoto(s.reader.QueryRowContext(ctx,
		`SELECT `+photoCols+` FROM photos
		 WHERE node_id=? AND sha256=? AND deleted_at IS NULL`, nodeID, sha))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: find duplicate: %w", err)
	}
	return p, nil
}

// UpdatePhotoCaption 修改图注。
func (s *Store) UpdatePhotoCaption(ctx context.Context, id, caption, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET caption=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		caption, now, id)
	if err != nil {
		return fmt.Errorf("store: update caption: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdatePhotoOrd 单独调整某张照片的 ord。
func (s *Store) UpdatePhotoOrd(ctx context.Context, id string, ord int64, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET ord=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		ord, now, id)
	if err != nil {
		return fmt.Errorf("store: update ord: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MovePhoto 把照片移到另一个节点：事务内校验目标节点存活、目标节点无同 sha
// 照片（否则违反 uq_node_sha 唯一索引），然后挂到目标节点末尾并重算 ord。
// 返回 ErrNotFound（照片或目标节点不存在）或 ErrDuplicate（目标节点已有同图）。
func (s *Store) MovePhoto(ctx context.Context, photoID, targetNodeID, now string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var sha sql.NullString
		var curNode string
		err := tx.QueryRowContext(ctx,
			`SELECT node_id, sha256 FROM photos WHERE id=? AND deleted_at IS NULL`,
			photoID).Scan(&curNode, &sha)
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: move photo lookup: %w", err)
		}
		if curNode == targetNodeID {
			return nil // 已在目标节点，无操作
		}

		var exists int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM nodes WHERE id=? AND deleted_at IS NULL`,
			targetNodeID).Scan(&exists); err != nil {
			return fmt.Errorf("store: move photo target: %w", err)
		}
		if exists == 0 {
			return ErrNotFound
		}

		// 目标节点已有同一张图 → 移动会撞唯一索引，交给上层报 409
		if sha.Valid {
			var dup int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM photos
				 WHERE node_id=? AND sha256=? AND deleted_at IS NULL`,
				targetNodeID, sha.String).Scan(&dup); err != nil {
				return fmt.Errorf("store: move photo dup check: %w", err)
			}
			if dup > 0 {
				return ErrDuplicate
			}
		}

		var maxOrd sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(ord) FROM photos WHERE node_id=? AND deleted_at IS NULL`,
			targetNodeID).Scan(&maxOrd); err != nil {
			return fmt.Errorf("store: move photo ord: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE photos SET node_id=?, ord=?, updated_at=? WHERE id=?`,
			targetNodeID, maxOrd.Int64+100, now, photoID); err != nil {
			return fmt.Errorf("store: move photo: %w", err)
		}
		return nil
	})
}

// SoftDeletePhoto 软删照片。
func (s *Store) SoftDeletePhoto(ctx context.Context, id, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET deleted_at=?, updated_at=? WHERE id=? AND deleted_at IS NULL`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("store: soft delete photo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RestorePhoto 恢复软删照片（父节点必须存活，否则应恢复节点）。
func (s *Store) RestorePhoto(ctx context.Context, id, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET deleted_at=NULL, updated_at=?
		 WHERE id=? AND deleted_at IS NOT NULL
		 AND EXISTS(SELECT 1 FROM nodes n WHERE n.id=photos.node_id AND n.deleted_at IS NULL)`,
		now, id)
	if err != nil {
		return fmt.Errorf("store: restore photo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReorderPhotos 事务内整组重赋 ord=100,200,…。ids 必须恰好等于节点当前存活照片集合。
func (s *Store) ReorderPhotos(ctx context.Context, nodeID string, ids []string, now string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM photos WHERE node_id=? AND deleted_at IS NULL`, nodeID)
		if err != nil {
			return fmt.Errorf("store: reorder list: %w", err)
		}
		current := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			current[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(ids) != len(current) {
			return fmt.Errorf("store: reorder: id set mismatch: %w", ErrNotFound)
		}
		for _, id := range ids {
			if !current[id] {
				return fmt.Errorf("store: reorder: photo %s not in node: %w", id, ErrNotFound)
			}
		}
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE photos SET ord=?, updated_at=? WHERE id=?`,
				(int64(i)+1)*100, now, id); err != nil {
				return fmt.Errorf("store: reorder update: %w", err)
			}
		}
		return nil
	})
}

// ClaimPhoto 抢占式领取待处理照片：影响行数=1 才可处理（防止重复执行）。
func (s *Store) ClaimPhoto(ctx context.Context, id, now string) (bool, error) {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET status='processing', updated_at=?
		 WHERE id=? AND status IN ('pending','processing') AND deleted_at IS NULL`,
		now, id)
	if err != nil {
		return false, fmt.Errorf("store: claim photo: %w", err)
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// PhotoMeta 是处理管线完成后回写的元数据。
type PhotoMeta struct {
	Width     int64
	Height    int64
	BlurHash  string
	Dominant  string
	SizeBytes int64
	TakenAt   *string
}

// MarkPhotoReady 单事务回写元数据并置 ready。
func (s *Store) MarkPhotoReady(ctx context.Context, id string, m PhotoMeta, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET status='ready', fail_reason=NULL,
		   width=?, height=?, blurhash=?, dominant=?, size_bytes=?, taken_at=?, updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		m.Width, m.Height, m.BlurHash, m.Dominant, m.SizeBytes, m.TakenAt, now, id)
	if err != nil {
		return fmt.Errorf("store: mark ready: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkPhotoFailed 置 failed 并记录原因。
func (s *Store) MarkPhotoFailed(ctx context.Context, id, reason, now string) error {
	_, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET status='failed', fail_reason=?, updated_at=? WHERE id=?`,
		reason, now, id)
	if err != nil {
		return fmt.Errorf("store: mark failed: %w", err)
	}
	return nil
}

// SetPhotoUploaded 在 s3 confirm 成功后回填 sha/ext 并置 pending 等待处理。
func (s *Store) SetPhotoUploaded(ctx context.Context, id, sha, ext string, size int64, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET sha256=?, ext=?, size_bytes=?, status='pending', updated_at=?
		 WHERE id=? AND deleted_at IS NULL`,
		sha, ext, size, now, id)
	if err != nil {
		return fmt.Errorf("store: set uploaded: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RequeuePhoto 将 failed 照片重置为 pending（reprocess 用）。
func (s *Store) RequeuePhoto(ctx context.Context, id, now string) error {
	res, err := s.writer.ExecContext(ctx,
		`UPDATE photos SET status='pending', fail_reason=NULL, updated_at=?
		 WHERE id=? AND status='failed' AND deleted_at IS NULL AND sha256 IS NOT NULL`,
		now, id)
	if err != nil {
		return fmt.Errorf("store: requeue photo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// StuckPhotos 返回需要重新入队的照片：
// processing 停留超过 staleBefore，或 sha 已就绪却仍是 pending 且无在途上传会话
// （local 崩溃遗留 / s3 confirm 后入队失败）。
func (s *Store) StuckPhotos(ctx context.Context, staleBefore string) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT id FROM photos
		 WHERE deleted_at IS NULL AND sha256 IS NOT NULL AND (
		   (status='processing' AND updated_at < ?) OR
		   (status='pending' AND NOT EXISTS(
		      SELECT 1 FROM upload_sessions us
		      WHERE us.photo_id=photos.id AND us.state='issued'))
		 )`, staleBefore)
	if err != nil {
		return nil, fmt.Errorf("store: stuck photos: %w", err)
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
	return ids, rows.Err()
}

// CountLiveSHARefs 统计引用某 sha256 的存活照片数（删 blob 前校验共享）。
func (s *Store) CountLiveSHARefs(ctx context.Context, sha string) (int, error) {
	var n int
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photos WHERE sha256=? AND deleted_at IS NULL`, sha).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count sha refs: %w", err)
	}
	return n, nil
}

// CountAnySHARefs 统计引用某 sha256 的全部照片行（含软删，孤儿对账与物理清除用：
// 只要还有任何行引用——包括未到期的回收站条目——blob 就不能删，否则恢复即断链）。
func (s *Store) CountAnySHARefs(ctx context.Context, sha string) (int, error) {
	var n int
	err := s.reader.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM photos WHERE sha256=?`, sha).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count any sha refs: %w", err)
	}
	return n, nil
}

// TrashItemRow 是回收站条目原始行。
type TrashItemRow struct {
	Type      string // node | photo
	ID        string
	Name      string
	DeletedAt string
	// 节点附加
	Date       string
	PhotoCount int
	// 照片附加
	NodeTitle string
	SHA256    *string
	Ext       *string
	Status    string
}

// ListTrash 返回回收站内容：软删节点 + （父节点存活的）软删照片，按删除时间倒序。
func (s *Store) ListTrash(ctx context.Context) ([]*TrashItemRow, error) {
	var out []*TrashItemRow
	rows, err := s.reader.QueryContext(ctx,
		`SELECT n.id, n.title, n.deleted_at, n.date,
		   (SELECT COUNT(*) FROM photos p WHERE p.node_id=n.id AND p.deleted_at=n.deleted_at)
		 FROM nodes n WHERE n.deleted_at IS NOT NULL ORDER BY n.deleted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list trash nodes: %w", err)
	}
	for rows.Next() {
		t := &TrashItemRow{Type: "node"}
		if err := rows.Scan(&t.ID, &t.Name, &t.DeletedAt, &t.Date, &t.PhotoCount); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.reader.QueryContext(ctx,
		`SELECT p.id, p.caption, p.deleted_at, n.title, p.sha256, p.ext, p.status
		 FROM photos p JOIN nodes n ON n.id=p.node_id
		 WHERE p.deleted_at IS NOT NULL AND n.deleted_at IS NULL
		 ORDER BY p.deleted_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list trash photos: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		t := &TrashItemRow{Type: "photo"}
		if err := rows.Scan(&t.ID, &t.Name, &t.DeletedAt, &t.NodeTitle,
			&t.SHA256, &t.Ext, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PurgeablePhoto 是可物理清除的照片（软删超期）。
type PurgeablePhoto struct {
	ID     string
	SHA256 *string
}

// ListPurgeablePhotos 列出软删早于 cutoff 的照片（含父节点已删的——节点清除时一并处理，
// 这里只挑父节点存活的独立删除照片）。
func (s *Store) ListPurgeablePhotos(ctx context.Context, cutoff string) ([]*PurgeablePhoto, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT p.id, p.sha256 FROM photos p
		 JOIN nodes n ON n.id=p.node_id
		 WHERE p.deleted_at IS NOT NULL AND p.deleted_at < ? AND n.deleted_at IS NULL`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: purgeable photos: %w", err)
	}
	defer rows.Close()
	var out []*PurgeablePhoto
	for rows.Next() {
		p := &PurgeablePhoto{}
		if err := rows.Scan(&p.ID, &p.SHA256); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPurgeableNodes 列出软删早于 cutoff 的节点及其全部照片（含照片 sha）。
func (s *Store) ListPurgeableNodes(ctx context.Context, cutoff string) (map[string][]*PurgeablePhoto, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT n.id, p.id, p.sha256 FROM nodes n
		 LEFT JOIN photos p ON p.node_id=n.id
		 WHERE n.deleted_at IS NOT NULL AND n.deleted_at < ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("store: purgeable nodes: %w", err)
	}
	defer rows.Close()
	out := map[string][]*PurgeablePhoto{}
	for rows.Next() {
		var nodeID string
		var photoID, sha sql.NullString
		if err := rows.Scan(&nodeID, &photoID, &sha); err != nil {
			return nil, err
		}
		if _, ok := out[nodeID]; !ok {
			out[nodeID] = nil
		}
		if photoID.Valid {
			p := &PurgeablePhoto{ID: photoID.String}
			if sha.Valid {
				s := sha.String
				p.SHA256 = &s
			}
			out[nodeID] = append(out[nodeID], p)
		}
	}
	return out, rows.Err()
}

// HardDeletePhotos 物理删除照片行（及其上传会话）。
func (s *Store) HardDeletePhotos(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM upload_sessions WHERE photo_id IN (`+ph+`)`, args...); err != nil {
			return fmt.Errorf("store: purge sessions: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM photos WHERE id IN (`+ph+`)`, args...); err != nil {
			return fmt.Errorf("store: purge photos: %w", err)
		}
		return nil
	})
}

// HardDeleteNode 物理删除节点行（其照片行须先删）。
func (s *Store) HardDeleteNode(ctx context.Context, id string) error {
	_, err := s.writer.ExecContext(ctx, `DELETE FROM nodes WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("store: purge node: %w", err)
	}
	return nil
}

// SHAKnown 判断某 sha 是否被任何照片行引用（孤儿对账用）。
func (s *Store) SHAKnown(ctx context.Context, sha string) (bool, error) {
	n, err := s.CountAnySHARefs(ctx, sha)
	return n > 0, err
}
