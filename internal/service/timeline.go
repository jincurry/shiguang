package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"shiguang/internal/blob"
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
// withPhotos=false 时只返回节点元数据与 photo_count，不带 photos——
// 管理后台的左栏列表用它，避免登录时把整库照片都拉下来。
// Timeline 返回逆序时间轴一页。withPhotos=false 只要节点元数据；
// photoLimit>0 时每个节点最多带 photoLimit 张照片（photo_count 仍是真实总数），
// 前台首屏只需折叠摞露出的那几张，几百张的节点没必要整摞下发。
func (s *Service) Timeline(ctx context.Context, cursor string, limit int, withPhotos bool, photoLimit int) (*TimelineDTO, error) {
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
	out := &TimelineDTO{Items: make([]*NodeDTO, 0, len(nodes)), NextCursor: next}

	if !withPhotos {
		ids := make([]string, len(nodes))
		for i, n := range nodes {
			ids[i] = n.ID
		}
		counts, err := s.st.PhotoCountsByNodeIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			// 走 nodeDTO 而不是在这里手写字段：手写过一次就漏了 place，
			// 后台左栏正是拿这条，结果重新登录后地点全是空的，一保存就清掉
			d := s.nodeDTO(ctx, n, nil)
			d.PhotoCount = counts[n.ID]
			out.Items = append(out.Items, d)
		}
		return out, nil
	}

	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	if photoLimit > 0 {
		photosByNode, err := s.st.PhotosByNodeIDsLimited(ctx, ids, photoLimit)
		if err != nil {
			return nil, err
		}
		// 照片被截断了，总数得单独查，否则前台会以为节点只有这几张
		counts, err := s.st.PhotoCountsByNodeIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			d := s.nodeDTO(ctx, n, photosByNode[n.ID])
			d.PhotoCount = counts[n.ID]
			out.Items = append(out.Items, d)
		}
		return out, nil
	}
	photosByNode, err := s.st.PhotosByNodeIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
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

// OriginalMeta 是原图下载所需的信息。
type OriginalMeta struct {
	Reader      io.ReadCloser
	Size        int64
	ContentType string
	Filename    string // 建议的落地文件名：图注（或 id）+ 原扩展名
	SHA256      string
}

// OpenOriginal 打开某张照片的原图（未经重编码的那一份）。
// 只给管理员用：/img 只服务 var/ 变体，原图不对外。
func (s *Service) OpenOriginal(ctx context.Context, id string) (*OriginalMeta, error) {
	p, err := s.st.GetPhoto(ctx, id, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.SHA256 == nil || p.Ext == nil || p.Status != "ready" {
		return nil, Validationf("这张照片还没有可导出的原图")
	}
	key := blob.OrigKey(*p.SHA256, *p.Ext)
	rc, err := s.bl.Open(ctx, key)
	if err != nil {
		return nil, ErrNotFound
	}
	size, _ := s.bl.Stat(ctx, key)
	ct := "application/octet-stream"
	switch *p.Ext {
	case "jpg":
		ct = "image/jpeg"
	case "png":
		ct = "image/png"
	case "webp":
		ct = "image/webp"
	}
	name := strings.TrimSpace(p.Caption)
	if name == "" {
		name = p.ID
	}
	return &OriginalMeta{
		Reader: rc, Size: size, ContentType: ct,
		Filename: safeFilename(name) + "." + *p.Ext,
		SHA256:   *p.SHA256,
	}, nil
}

// safeFilename 把图注收拾成能落地的文件名：去掉路径分隔符与控制字符，
// 保留中文，长度按字符数截断（不能按字节，会截断出半个汉字）。
func safeFilename(s string) string {
	repl := strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_", "\n", " ", "\r", " ", "\t", " ")
	out := strings.TrimSpace(repl.Replace(s))
	out = strings.Trim(out, ".") // Windows 不接受结尾的点
	r := []rune(out)
	if len(r) > 60 {
		out = string(r[:60])
	}
	if out == "" {
		out = "photo"
	}
	return out
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
	Place       *string `json:"place"`
}

func validateNodeFields(date, title, description, place string) error {
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
	if len([]rune(place)) > 80 {
		return Validationf("地点不能超过 80 字")
	}
	return nil
}

// CreateNode 新建节点。
func (s *Service) CreateNode(ctx context.Context, in NodeInput) (*NodeDTO, error) {
	date, title, desc, place := "", "", "", ""
	if in.Date != nil {
		date = *in.Date
	}
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		desc = *in.Description
	}
	if in.Place != nil {
		place = strings.TrimSpace(*in.Place)
	}
	if err := validateNodeFields(date, title, desc, place); err != nil {
		return nil, err
	}
	now := store.Now()
	n := &store.Node{
		ID: NewULID(), Date: date, Title: title, Description: desc, Place: place,
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
	date, title, desc, place := n.Date, n.Title, n.Description, n.Place
	if in.Date != nil {
		date = *in.Date
	}
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
	}
	if in.Description != nil {
		desc = *in.Description
	}
	if in.Place != nil {
		place = strings.TrimSpace(*in.Place)
	}
	if err := validateNodeFields(date, title, desc, place); err != nil {
		return nil, err
	}
	if err := s.st.UpdateNode(ctx, id, date, title, desc, place, store.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	s.invalidateStats()
	return s.GetNode(ctx, id)
}

// OnThisDayDTO 是「那年今日」的一条：往年同一天的节点，附一张封面。
type OnThisDayDTO struct {
	Node       *NodeDTO `json:"node"`
	YearsAgo   int      `json:"years_ago"`
	CoverThumb string   `json:"cover_thumb"`
}

// OnThisDay 取往年的今天（月-日相同、年份不同）的节点，最近的在前。
// 每条附一张封面缩略图，前台横幅直接用。
func (s *Service) OnThisDay(ctx context.Context, today time.Time, limit int) ([]*OnThisDayDTO, error) {
	if limit < 1 || limit > 20 {
		limit = 5
	}
	y := today.Format("2006")
	nodes, err := s.st.NodesOnMonthDay(ctx, today.Format("01-02"), y, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*OnThisDayDTO, 0, len(nodes))
	if len(nodes) == 0 {
		return out, nil
	}
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	// 每个节点只要封面那一张
	covers, err := s.st.PhotosByNodeIDsLimited(ctx, ids, 1)
	if err != nil {
		return nil, err
	}
	counts, err := s.st.PhotoCountsByNodeIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	nowYear, _ := strconv.Atoi(y)
	for _, n := range nodes {
		d := s.nodeDTO(ctx, n, nil)
		d.PhotoCount = counts[n.ID]
		item := &OnThisDayDTO{Node: d}
		if yr, err := strconv.Atoi(n.Date[:4]); err == nil {
			item.YearsAgo = nowYear - yr
		}
		if ps := covers[n.ID]; len(ps) > 0 {
			item.CoverThumb = s.photoDTO(ctx, ps[0]).Variants["thumb"]
		}
		out = append(out, item)
	}
	return out, nil
}

// PlaceDTO 是一处地点及它收着的节点数。
type PlaceDTO struct {
	Place string `json:"place"`
	Count int    `json:"count"`
}

// Places 列出用过的地点（按节点数倒序）。
func (s *Service) Places(ctx context.Context) ([]*PlaceDTO, error) {
	m, err := s.st.Places(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*PlaceDTO, 0, len(m))
	for pl, c := range m {
		out = append(out, &PlaceDTO{Place: pl, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Place < out[j].Place
	})
	return out, nil
}

// NodesAtPlace 取同一地点的节点（时间倒序），每个带一张封面。
func (s *Service) NodesAtPlace(ctx context.Context, place string, limit int) ([]*NodeDTO, error) {
	place = strings.TrimSpace(place)
	if place == "" {
		return nil, Validationf("place 不能为空")
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	nodes, err := s.st.NodesByPlace(ctx, place, limit)
	if err != nil {
		return nil, err
	}
	out := make([]*NodeDTO, 0, len(nodes))
	if len(nodes) == 0 {
		return out, nil
	}
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	covers, err := s.st.PhotosByNodeIDsLimited(ctx, ids, 1)
	if err != nil {
		return nil, err
	}
	counts, err := s.st.PhotoCountsByNodeIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		d := s.nodeDTO(ctx, n, covers[n.ID])
		d.PhotoCount = counts[n.ID]
		out = append(out, d)
	}
	return out, nil
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
	Note    *string `json:"note"` // 相纸背面的手记
	Ord     *int64  `json:"ord"`
	// NodeID 非空时把照片移动到该节点末尾（批量导入后重新归类用）。
	NodeID *string `json:"node_id"`
}

// PatchPhoto 修改图注/序号，或把照片移动到另一个节点。
func (s *Service) PatchPhoto(ctx context.Context, id string, in PhotoPatch) (*PhotoDTO, error) {
	if in.Caption == nil && in.Note == nil && in.Ord == nil && in.NodeID == nil {
		return nil, Validationf("caption、note、ord 与 node_id 至少提供一个")
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
	if in.Note != nil {
		note := strings.TrimRight(*in.Note, " \t\n")
		if len([]rune(note)) > 2000 {
			return nil, Validationf("手记不能超过 2000 字")
		}
		if err := s.st.UpdatePhotoNote(ctx, id, note, now); err != nil {
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

// MaxBatchPhotos 是单次批量操作的照片数上限，避免一个请求占住写连接太久。
const MaxBatchPhotos = 500

// BatchPhotoInput 是 POST /photos/batch 的输入。
type BatchPhotoInput struct {
	Action   string   `json:"action"` // move | delete
	PhotoIDs []string `json:"photo_ids"`
	NodeID   *string  `json:"node_id"` // action=move 时必填
}

// BatchFailure 记录单张照片的失败原因，让前端能精确告知用户哪几张没成功。
type BatchFailure struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// BatchResult 是批量操作结果。逐条汇报而非整体失败：批量移动时个别照片
// 因目标节点已有同图而冲突是常态，不该因此回滚其余成功的操作。
type BatchResult struct {
	Succeeded int            `json:"succeeded"`
	Failed    []BatchFailure `json:"failed"`
}

// BatchPhotos 批量移动或删除照片。
func (s *Service) BatchPhotos(ctx context.Context, in BatchPhotoInput) (*BatchResult, error) {
	if len(in.PhotoIDs) == 0 {
		return nil, Validationf("photo_ids 不能为空")
	}
	if len(in.PhotoIDs) > MaxBatchPhotos {
		return nil, Validationf("单次最多处理 %d 张，收到 %d 张", MaxBatchPhotos, len(in.PhotoIDs))
	}

	var targetNode string
	switch in.Action {
	case "move":
		if in.NodeID == nil || *in.NodeID == "" {
			return nil, Validationf("action=move 时必须提供 node_id")
		}
		targetNode = *in.NodeID
		if _, err := s.st.GetNode(ctx, targetNode, false); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
	case "delete":
	default:
		return nil, Validationf("action 只能是 move 或 delete")
	}

	now := store.Now()
	res := &BatchResult{}
	seen := map[string]bool{}
	for _, id := range in.PhotoIDs {
		if seen[id] {
			continue // 重复 id 静默跳过，不重复计数
		}
		seen[id] = true

		var err error
		if in.Action == "move" {
			err = s.st.MovePhoto(ctx, id, targetNode, now)
		} else {
			err = s.st.SoftDeletePhoto(ctx, id, now)
		}
		switch {
		case err == nil:
			res.Succeeded++
		case errors.Is(err, store.ErrDuplicate):
			res.Failed = append(res.Failed, BatchFailure{ID: id, Reason: "目标节点已有同一张照片"})
		case errors.Is(err, store.ErrNotFound):
			res.Failed = append(res.Failed, BatchFailure{ID: id, Reason: "照片不存在或已删除"})
		default:
			return nil, err // 非预期错误（如 DB 故障）直接中止
		}
	}
	if res.Succeeded > 0 {
		s.invalidateStats()
	}
	return res, nil
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
