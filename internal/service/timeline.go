package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"shiguang/internal/store"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// validDate 校验 YYYY-MM-DD 且为真实日期。
func validDate(d string) bool {
	if !dateRe.MatchString(d) {
		return false
	}
	_, err := time.Parse("2006-01-02", d)
	return err == nil
}

// encodeCursor 生成 base64("date|id") 游标。
func encodeCursor(date, id string) string {
	return base64.URLEncoding.EncodeToString([]byte(date + "|" + id))
}

// decodeCursor 解析游标；空串表示首页。
func decodeCursor(c string) (date, id string, err error) {
	if c == "" {
		return "", "", nil
	}
	raw, err := base64.URLEncoding.DecodeString(c)
	if err != nil {
		return "", "", Validationf("无效的 cursor")
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 || !validDate(parts[0]) || parts[1] == "" {
		return "", "", Validationf("无效的 cursor")
	}
	return parts[0], parts[1], nil
}

// TimelineDTO 是 GET /timeline 响应。
type TimelineDTO struct {
	Items      []*NodeDTO `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// Timeline 逆序游标分页：两条 SQL（一页节点 + 批量照片），禁止 N+1。
func (s *Service) Timeline(ctx context.Context, cursor string, limit int) (*TimelineDTO, error) {
	if limit < 1 || limit > 50 {
		limit = 10
	}
	cDate, cID, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	// 多取一条判断是否还有下一页
	nodes, err := s.st.TimelinePage(ctx, cDate, cID, limit+1)
	if err != nil {
		return nil, err
	}
	var next *string
	if len(nodes) > limit {
		nodes = nodes[:limit]
		c := encodeCursor(nodes[limit-1].Date, nodes[limit-1].ID)
		next = &c
	}
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	photosByNode, err := s.st.PhotosByNodeIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := &TimelineDTO{Items: make([]*NodeDTO, 0, len(nodes)), NextCursor: next}
	for _, n := range nodes {
		out.Items = append(out.Items, s.nodeDTO(ctx, n, photosByNode[n.ID]))
	}
	return out, nil
}

// GetNode 返回单节点详情（含照片）。
func (s *Service) GetNode(ctx context.Context, id string) (*NodeDTO, error) {
	n, err := s.st.GetNode(ctx, id, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	photos, err := s.st.PhotosByNodeIDs(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	return s.nodeDTO(ctx, n, photos[id]), nil
}

// GetPhoto 返回单张照片（状态轮询用）。
func (s *Service) GetPhoto(ctx context.Context, id string) (*PhotoDTO, error) {
	p, err := s.st.GetPhoto(ctx, id, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.photoDTO(ctx, p), nil
}

// StatsDTO 是 GET /stats 响应。upload_mode 供管理后台选择上传路径。
type StatsDTO struct {
	NodeCount  int    `json:"node_count"`
	PhotoCount int    `json:"photo_count"`
	TotalBytes int64  `json:"total_bytes"`
	YearMin    string `json:"year_min"`
	YearMax    string `json:"year_max"`
	TrashCount int    `json:"trash_count"`
	UploadMode string `json:"upload_mode"` // local | s3
}

// GetStats 返回统计，进程内缓存 60s。
func (s *Service) GetStats(ctx context.Context) (*StatsDTO, error) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	if s.statsCache != nil && time.Since(s.statsAt) < 60*time.Second {
		return s.statsCache, nil
	}
	raw, err := s.st.GetStats(ctx)
	if err != nil {
		return nil, err
	}
	s.statsCache = &StatsDTO{
		NodeCount:  raw.NodeCount,
		PhotoCount: raw.PhotoCount,
		TotalBytes: raw.TotalBytes,
		YearMin:    raw.YearMin,
		YearMax:    raw.YearMax,
		TrashCount: raw.TrashCount,
		UploadMode: s.cfg.BlobDriver,
	}
	s.statsAt = time.Now()
	return s.statsCache, nil
}

// invalidateStats 写操作后使统计缓存失效。
func (s *Service) invalidateStats() {
	s.statsMu.Lock()
	s.statsCache = nil
	s.statsMu.Unlock()
}

// ===== 节点 CRUD =====

// NodeInput 是创建/修改节点的输入。
type NodeInput struct {
	Date        *string `json:"date"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

func validateNodeFields(date, title, description string) error {
	if !validDate(date) {
		return Validationf("date 必须为 YYYY-MM-DD 格式的有效日期")
	}
	if title == "" {
		return Validationf("标题不能为空")
	}
	if len([]rune(title)) > 120 {
		return Validationf("标题不能超过 120 字")
	}
	if len([]rune(description)) > 2000 {
		return Validationf("描述不能超过 2000 字")
	}
	return nil
}

// CreateNode 新建节点。
func (s *Service) CreateNode(ctx context.Context, in NodeInput) (*NodeDTO, error) {
	date, title, desc := "", "", ""
	if in.Date != nil {
		date = *in.Date
	}
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		desc = *in.Description
	}
	if err := validateNodeFields(date, title, desc); err != nil {
		return nil, err
	}
	now := store.Now()
	n := &store.Node{
		ID: NewULID(), Date: date, Title: title, Description: desc,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateNode(ctx, n); err != nil {
		return nil, err
	}
	s.invalidateStats()
	return s.nodeDTO(ctx, n, nil), nil
}

// PatchNode 部分更新节点。
func (s *Service) PatchNode(ctx context.Context, id string, in NodeInput) (*NodeDTO, error) {
	n, err := s.st.GetNode(ctx, id, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	date, title, desc := n.Date, n.Title, n.Description
	if in.Date != nil {
		date = *in.Date
	}
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		desc = *in.Description
	}
	if err := validateNodeFields(date, title, desc); err != nil {
		return nil, err
	}
	if err := s.st.UpdateNode(ctx, id, date, title, desc, store.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.invalidateStats()
	return s.GetNode(ctx, id)
}

// DeleteNode 软删节点并级联软删照片。
func (s *Service) DeleteNode(ctx context.Context, id string) error {
	err := s.st.SoftDeleteNode(ctx, id, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.invalidateStats()
	}
	return err
}

// RestoreNode 从回收站恢复节点（含随节点删除的照片）。
func (s *Service) RestoreNode(ctx context.Context, id string) (*NodeDTO, error) {
	err := s.st.RestoreNode(ctx, id, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.invalidateStats()
	return s.GetNode(ctx, id)
}

// ===== 照片编辑 =====

// PhotoPatch 是 PATCH /photos/{id} 的输入。
type PhotoPatch struct {
	Caption *string `json:"caption"`
	Ord     *int64  `json:"ord"`
	// NodeID 非空时把照片移动到该节点末尾（批量导入后重新归类用）。
	NodeID *string `json:"node_id"`
}

// PatchPhoto 修改图注/序号，或把照片移动到另一个节点。
func (s *Service) PatchPhoto(ctx context.Context, id string, in PhotoPatch) (*PhotoDTO, error) {
	if in.Caption == nil && in.Ord == nil && in.NodeID == nil {
		return nil, Validationf("caption、ord 与 node_id 至少提供一个")
	}
	now := store.Now()
	// 先移动再改序号：移动会把 ord 重置到目标节点末尾，顺序相反会被覆盖
	if in.NodeID != nil {
		if *in.NodeID == "" {
			return nil, Validationf("node_id 不能为空字符串")
		}
		err := s.st.MovePhoto(ctx, id, *in.NodeID, now)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return nil, ErrNotFound
		case errors.Is(err, store.ErrDuplicate):
			existing, ferr := s.st.GetPhoto(ctx, id, false)
			if ferr != nil {
				return nil, ferr
			}
			return nil, &DuplicateError{Existing: s.photoDTO(ctx, existing)}
		case err != nil:
			return nil, err
		}
		s.invalidateStats()
	}
	if in.Caption != nil {
		c := strings.TrimSpace(*in.Caption)
		if len([]rune(c)) > 200 {
			return nil, Validationf("图注不能超过 200 字")
		}
		if err := s.st.UpdatePhotoCaption(ctx, id, c, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
	}
	if in.Ord != nil {
		if err := s.st.UpdatePhotoOrd(ctx, id, *in.Ord, now); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
	}
	return s.GetPhoto(ctx, id)
}

// DeletePhoto 软删照片。
func (s *Service) DeletePhoto(ctx context.Context, id string) error {
	err := s.st.SoftDeletePhoto(ctx, id, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.invalidateStats()
	}
	return err
}

// RestorePhoto 恢复软删照片。
func (s *Service) RestorePhoto(ctx context.Context, id string) (*PhotoDTO, error) {
	err := s.st.RestorePhoto(ctx, id, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.invalidateStats()
	return s.GetPhoto(ctx, id)
}

// ReorderPhotos 整组重排（设封面 = 前端把目标移到首位后调用本接口）。
func (s *Service) ReorderPhotos(ctx context.Context, nodeID string, photoIDs []string) error {
	if len(photoIDs) == 0 {
		return Validationf("photo_ids 不能为空")
	}
	seen := map[string]bool{}
	for _, id := range photoIDs {
		if seen[id] {
			return Validationf("photo_ids 存在重复项")
		}
		seen[id] = true
	}
	if _, err := s.st.GetNode(ctx, nodeID, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	err := s.st.ReorderPhotos(ctx, nodeID, photoIDs, store.Now())
	if errors.Is(err, store.ErrNotFound) {
		return Validationf("photo_ids 与节点当前照片集合不一致")
	}
	return err
}

// ===== 回收站 =====

// TrashItemDTO 是回收站条目。
type TrashItemDTO struct {
	Type      string         `json:"type"` // node | photo
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	DeletedAt string         `json:"deleted_at"`
	PurgeAt   string         `json:"purge_at"`
	Extra     map[string]any `json:"extra,omitempty"`
}

// TrashDTO 是 GET /trash 响应。
type TrashDTO struct {
	Items []*TrashItemDTO `json:"items"`
}

// ListTrash 返回回收站列表，purge_at = deleted_at + SG_TRASH_TTL_DAYS。
func (s *Service) ListTrash(ctx context.Context) (*TrashDTO, error) {
	rows, err := s.st.ListTrash(ctx)
	if err != nil {
		return nil, err
	}
	ttl := time.Duration(s.cfg.TrashTTLDays) * 24 * time.Hour
	out := &TrashDTO{Items: make([]*TrashItemDTO, 0, len(rows))}
	for _, r := range rows {
		delAt, err := store.ParseTime(r.DeletedAt)
		if err != nil {
			return nil, fmt.Errorf("trash: bad deleted_at %q: %w", r.DeletedAt, err)
		}
		item := &TrashItemDTO{
			Type:      r.Type,
			ID:        r.ID,
			Name:      r.Name,
			DeletedAt: r.DeletedAt,
			PurgeAt:   store.FormatTime(delAt.Add(ttl)),
			Extra:     map[string]any{},
		}
		if r.Type == "node" {
			item.Extra["date"] = r.Date
			item.Extra["photo_count"] = r.PhotoCount
		} else {
			item.Extra["node_title"] = r.NodeTitle
			if r.Status == "ready" && r.SHA256 != nil {
				if u, err := s.variantURL(ctx, *r.SHA256, "thumb"); err == nil {
					item.Extra["thumb"] = u
				}
			}
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}
