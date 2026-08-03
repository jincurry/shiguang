package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"shiguang/internal/blob"
	"shiguang/internal/imgproc"
	"shiguang/internal/store"
)

// 上传失败原因常量。重复 confirm 时要据此还原出与首次相同的错误，
// 否则用户会看到"会话已过期"这种与真实原因无关的误导性提示。
const (
	failUploadIncomplete = "上传未完成"
	failSizeMismatch     = "对象大小与声明不符"
	failReadBack         = "对象读取失败"
	failNotAnImage       = "文件不是有效图片"
)

// errForFailReason 把已记录的失败原因还原成对应的语义错误，
// 使得对同一张失败照片重复 confirm 得到稳定一致的响应。
func errForFailReason(reason string) error {
	switch reason {
	case failNotAnImage:
		return ErrUnsupportedMedia
	case failSizeMismatch:
		return Validationf("%s", failSizeMismatch)
	case failReadBack:
		return fmt.Errorf("%w: %s", ErrStorage, failReadBack)
	default: // 含 failUploadIncomplete：会话确实过期了
		return ErrSessionExpired
	}
}

// maxUploadBytes 返回单张上限字节数。
func (s *Service) maxUploadBytes() int64 { return s.cfg.UploadLimitMB << 20 }

// maxPixels 返回像素上限。
func (s *Service) maxPixels() int64 { return s.cfg.PixelLimitMP * 1_000_000 }

// maxCaptionRunes 与 photos.caption 的 CHECK 约束一致。
const maxCaptionRunes = 200

// clampCaption 把图注截断到 200 字。上传路径用截断而非报错：照片是不可再生
// 资产，不能因为文件名过长就拒收整张原图（DB 的 CHECK 会直接抛 500）。
// 显式改注走 PatchPhoto，那里超长仍返回 422。
func clampCaption(c string) string {
	r := []rune(c)
	if len(r) > maxCaptionRunes {
		r = r[:maxCaptionRunes]
	}
	return string(r)
}

// captionFromFilename 从文件名派生默认图注（去扩展名、截断到 200 字）。
func captionFromFilename(name string) string {
	return clampCaption(strings.TrimSuffix(path.Base(name), path.Ext(name)))
}

// UploadLocal 处理 local 模式 multipart 上传：
// 魔数校验 → sha256 → 同节点秒传判定 → 原图落盘（内容寻址，幂等）→
// 建 processing 照片行 → 入队。队列满不拒收（202 已收下，恢复扫描兜底）。
func (s *Service) UploadLocal(ctx context.Context, nodeID, filename, caption string, r io.Reader) (*PhotoDTO, error) {
	if _, err := s.st.GetNode(ctx, nodeID, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	raw, err := io.ReadAll(r)
	if err != nil {
		// http.MaxBytesReader 超限会在这里露头
		if strings.Contains(err.Error(), "request body too large") {
			return nil, ErrPayloadTooLarge
		}
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(raw)) > s.maxUploadBytes() {
		return nil, ErrPayloadTooLarge
	}
	if len(raw) < 12 {
		return nil, ErrUnsupportedMedia
	}
	ext, err := imgproc.SniffFormat(raw[:12])
	if err != nil {
		return nil, ErrUnsupportedMedia
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	// 秒传：同节点同 sha 已存在 → 409 附已有照片
	if dup, err := s.st.FindDuplicate(ctx, nodeID, sha); err == nil {
		return nil, &DuplicateError{Existing: s.photoDTO(ctx, dup)}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	// blob 先行、DB 断后：原图先落盘（内容寻址，重复 Put 幂等覆盖同内容）。
	origKey := blob.OrigKey(sha, ext)
	if _, err := s.bl.Stat(ctx, origKey); err != nil {
		if !errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
		if err := s.bl.Put(ctx, origKey, bytes.NewReader(raw), int64(len(raw)),
			imgproc.ContentTypeForExt(ext)); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
	}

	if caption == "" {
		caption = captionFromFilename(filename)
	} else {
		caption = clampCaption(caption)
	}
	ord, err := s.st.NextOrd(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	size := int64(len(raw))
	p := &store.Photo{
		ID: NewULID(), NodeID: nodeID, Caption: caption, Ord: ord,
		// local 模式跳过 pending 直接 processing
		Status: "processing", SHA256: &sha, Ext: &ext, SizeBytes: &size,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreatePhoto(ctx, p); err != nil {
		return nil, err
	}
	s.invalidateStats()
	s.pool.Enqueue(p.ID) // 满了也没关系：StuckPhotos 扫描会重新入队
	return s.photoDTO(ctx, p), nil
}

// ===== s3 直传三步：presign → 浏览器 PUT → confirm =====

// PresignInput 是 POST /uploads/presign 的输入。
type PresignInput struct {
	NodeID      string `json:"node_id"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
}

// PresignDTO 是 presign 响应。
type PresignDTO struct {
	UploadURL string `json:"upload_url"`
	PhotoID   string `json:"photo_id"`
}

// PresignUpload 创建 pending 照片与上传会话，签发直传 URL（s3 模式专用）。
func (s *Service) PresignUpload(ctx context.Context, in PresignInput) (*PresignDTO, error) {
	if s.cfg.BlobDriver != "s3" {
		return nil, Validationf("当前为 local 模式，请使用 POST /nodes/{id}/photos 上传")
	}
	if _, err := s.st.GetNode(ctx, in.NodeID, false); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if in.Size <= 0 {
		return nil, Validationf("size 必须大于 0")
	}
	if in.Size > s.maxUploadBytes() {
		return nil, ErrPayloadTooLarge
	}
	ext := imgproc.ExtForContentType(in.ContentType)
	if ext == "" {
		return nil, ErrUnsupportedMedia
	}

	ord, err := s.st.NextOrd(ctx, in.NodeID)
	if err != nil {
		return nil, err
	}
	now := store.Now()
	p := &store.Photo{
		ID: NewULID(), NodeID: in.NodeID, Caption: captionFromFilename(in.Filename),
		Ord: ord, Status: "pending", Ext: &ext,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreatePhoto(ctx, p); err != nil {
		return nil, err
	}

	sessID := NewULID()
	objectKey := blob.StagingKey(sessID, ext)
	uploadURL, err := s.bl.PresignPut(ctx, objectKey, in.ContentType, in.Size, s.cfg.PresignTTL)
	if err != nil {
		return nil, fmt.Errorf("%w: presign: %v", ErrStorage, err)
	}
	sess := &store.UploadSession{
		ID: sessID, PhotoID: p.ID, ObjectKey: objectKey,
		ExpectSize: in.Size, ContentType: in.ContentType,
		State:     "issued",
		ExpiresAt: store.FormatTime(time.Now().Add(s.cfg.PresignTTL)),
		CreatedAt: now,
	}
	if err := s.st.CreateSession(ctx, sess); err != nil {
		return nil, err
	}
	s.invalidateStats()
	return &PresignDTO{UploadURL: uploadURL, PhotoID: p.ID}, nil
}

// ConfirmUpload 完成 s3 直传第三步：
// Stat 比对大小 → 回读复检魔数 + 计算 sha → 秒传判定 → Rename 转正 → 入队。
func (s *Service) ConfirmUpload(ctx context.Context, photoID string) (*PhotoDTO, error) {
	p, err := s.st.GetPhoto(ctx, photoID, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sess, err := s.st.IssuedSessionByPhoto(ctx, photoID)
	if errors.Is(err, store.ErrNotFound) {
		if p.Status == "processing" || p.Status == "ready" {
			return s.photoDTO(ctx, p), nil // confirm 幂等：已转正直接返回
		}
		// 会话已被终结（文件无效、大小不符、或 reaper 判过期）。重复 confirm
		// 要还原出与首次相同的错误——否则"文件不是有效图片"会变成
		// "会话已过期，请重新上传"，把人往错误方向引。
		if p.Status == "failed" && p.FailReason != nil {
			return nil, errForFailReason(*p.FailReason)
		}
		return nil, ErrSessionExpired
	}
	if err != nil {
		return nil, err
	}

	expiresAt, err := store.ParseTime(sess.ExpiresAt)
	if err == nil && time.Now().After(expiresAt) {
		s.st.SetSessionState(ctx, sess.ID, "expired")
		s.st.MarkPhotoFailed(ctx, photoID, failUploadIncomplete, store.Now())
		s.bl.Delete(ctx, sess.ObjectKey)
		return nil, ErrSessionExpired
	}

	size, err := s.bl.Stat(ctx, sess.ObjectKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, Validationf("对象尚未上传完成")
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	if size != sess.ExpectSize {
		s.abortSession(ctx, sess, photoID, failSizeMismatch)
		return nil, Validationf("对象大小与声明不符：got %d want %d", size, sess.ExpectSize)
	}

	rc, err := s.bl.Open(ctx, sess.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	raw, err := imgproc.ReadAllLimit(rc, s.maxUploadBytes())
	rc.Close()
	if err != nil {
		s.abortSession(ctx, sess, photoID, failReadBack)
		return nil, fmt.Errorf("%w: read back: %v", ErrStorage, err)
	}
	// 服务端回读复检魔数：presign 的 Content-Type 锁不住真实内容
	if len(raw) < 12 {
		s.abortSession(ctx, sess, photoID, failNotAnImage)
		return nil, ErrUnsupportedMedia
	}
	ext, err := imgproc.SniffFormat(raw[:12])
	if err != nil {
		s.abortSession(ctx, sess, photoID, failNotAnImage)
		return nil, ErrUnsupportedMedia
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	if dup, err := s.st.FindDuplicate(ctx, p.NodeID, sha); err == nil {
		// 秒传：暂存对象与 pending 行都不再需要
		s.st.SetSessionState(ctx, sess.ID, "aborted")
		s.bl.Delete(ctx, sess.ObjectKey)
		s.st.HardDeletePhotos(ctx, []string{photoID})
		s.invalidateStats()
		return nil, &DuplicateError{Existing: s.photoDTO(ctx, dup)}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	origKey := blob.OrigKey(sha, ext)
	if _, err := s.bl.Stat(ctx, origKey); err == nil {
		// 跨节点同图：原图已在，丢弃暂存对象即可
		if err := s.bl.Delete(ctx, sess.ObjectKey); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStorage, err)
		}
	} else if errors.Is(err, blob.ErrNotFound) {
		if err := s.bl.Rename(ctx, sess.ObjectKey, origKey); err != nil {
			return nil, fmt.Errorf("%w: rename: %v", ErrStorage, err)
		}
	} else {
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}

	now := store.Now()
	if err := s.st.SetPhotoUploaded(ctx, photoID, sha, ext, size, now); err != nil {
		return nil, err
	}
	s.st.SetSessionState(ctx, sess.ID, "confirmed")
	s.pool.Enqueue(photoID)
	out, err := s.GetPhoto(ctx, photoID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// abortSession 会话失败收尾：置 aborted、照片 failed、删暂存对象。
func (s *Service) abortSession(ctx context.Context, sess *store.UploadSession, photoID, reason string) {
	s.st.SetSessionState(ctx, sess.ID, "aborted")
	s.st.MarkPhotoFailed(ctx, photoID, reason, store.Now())
	if err := s.bl.Delete(ctx, sess.ObjectKey); err != nil {
		s.log.Error("abort session: delete staging", "key", sess.ObjectKey, "err", err)
	}
}

// Reprocess 对 failed 照片重新入队（原图已在则直接重跑管线）。
func (s *Service) Reprocess(ctx context.Context, photoID string) (*PhotoDTO, error) {
	p, err := s.st.GetPhoto(ctx, photoID, false)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Status != "failed" {
		return nil, Validationf("仅 failed 状态的照片可以重试")
	}
	if p.SHA256 == nil || p.Ext == nil {
		return nil, Validationf("原图缺失，请重新上传")
	}
	if _, err := s.bl.Stat(ctx, blob.OrigKey(*p.SHA256, *p.Ext)); err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, Validationf("原图缺失，请重新上传")
		}
		return nil, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	if err := s.st.RequeuePhoto(ctx, photoID, store.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, Validationf("仅 failed 状态的照片可以重试")
		}
		return nil, err
	}
	s.pool.Enqueue(photoID)
	return s.GetPhoto(ctx, photoID)
}

// ===== 处理管线（worker handler）=====

// ProcessPhoto 是 worker 池的 handler：抢占领取 → 读原图 → 管线处理 →
// 写变体（blob 先行）→ 单事务回写元数据 + ready（DB 断后）。
// 每步幂等可重入；任意失败自动重试 1 次后置 failed。
func (s *Service) ProcessPhoto(ctx context.Context, photoID string) {
	claimed, err := s.st.ClaimPhoto(ctx, photoID, store.Now())
	if err != nil {
		s.log.Error("claim photo", "photo", photoID, "err", err)
		return
	}
	if !claimed { // 已 ready/failed/软删，无事可做
		return
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // 失败自动重试 1 次
		lastErr = s.processOnce(ctx, photoID)
		if lastErr == nil {
			return
		}
		// 确定性失败（坏文件）重试无意义，直接终止
		if errors.Is(lastErr, imgproc.ErrUnsupported) ||
			errors.Is(lastErr, imgproc.ErrTooManyPixels) ||
			errors.Is(lastErr, imgproc.ErrCorrupt) {
			break
		}
	}
	reason := failReason(lastErr)
	s.log.Warn("photo processing failed", "photo", photoID, "err", lastErr)
	if err := s.st.MarkPhotoFailed(ctx, photoID, reason, store.Now()); err != nil {
		s.log.Error("mark failed", "photo", photoID, "err", err)
	}
}

// failReason 把处理错误翻译成给用户看的失败原因。
func failReason(err error) string {
	switch {
	case errors.Is(err, imgproc.ErrTooManyPixels):
		return "pixel bomb"
	case errors.Is(err, imgproc.ErrUnsupported):
		return failNotAnImage
	case errors.Is(err, imgproc.ErrCorrupt):
		return "图片已损坏或被截断"
	default:
		return "处理失败，可重试"
	}
}

func (s *Service) processOnce(ctx context.Context, photoID string) error {
	p, err := s.st.GetPhoto(ctx, photoID, false)
	if err != nil {
		return err
	}
	if p.SHA256 == nil || p.Ext == nil {
		return imgproc.ErrCorrupt
	}
	rc, err := s.bl.Open(ctx, blob.OrigKey(*p.SHA256, *p.Ext))
	if err != nil {
		return fmt.Errorf("open orig: %w", err)
	}
	raw, err := imgproc.ReadAllLimit(rc, s.maxUploadBytes()*2)
	rc.Close()
	if err != nil {
		return fmt.Errorf("read orig: %w", err)
	}

	res, err := imgproc.Process(raw, s.maxPixels())
	if err != nil {
		return err
	}

	// blob 先行：变体全部写成功后才回写 DB。
	// local 驱动 Put 内部即"临时文件 → fsync → rename"原子覆盖，可重入。
	for _, name := range imgproc.VariantNames {
		data := res.Variants[name]
		key := blob.VariantKey(*p.SHA256, name)
		if err := s.bl.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "image/webp"); err != nil {
			return fmt.Errorf("put variant %s: %w", name, err)
		}
	}

	var takenAt *string
	if res.TakenAt != nil {
		t := store.FormatTime(*res.TakenAt)
		takenAt = &t
	}
	meta := store.PhotoMeta{
		Width: int64(res.Width), Height: int64(res.Height),
		BlurHash: res.BlurHash, Dominant: res.Dominant,
		SizeBytes: int64(len(raw)), TakenAt: takenAt,
	}
	if err := s.st.MarkPhotoReady(ctx, photoID, meta, store.Now()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil // 处理期间被软删：变体已写不碍事，GC 会清
		}
		return err
	}
	s.invalidateStats()
	return nil
}
