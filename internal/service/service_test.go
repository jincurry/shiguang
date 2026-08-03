package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"os"
	"testing"
	"time"

	"shiguang/internal/blob"
	"shiguang/internal/imgproc"
	"shiguang/internal/store"
	"shiguang/migrations"
)

// newTestService 组装 fake blob + 临时 SQLite + 单 worker 池。
func newTestService(t *testing.T, cfg Config) (*Service, *blob.Fake) {
	t.Helper()
	st, err := store.Open("file:"+t.TempDir()+"/test.db", migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := blob.NewFake()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	if cfg.UploadLimitMB == 0 {
		cfg.UploadLimitMB = 30
	}
	if cfg.PixelLimitMP == 0 {
		cfg.PixelLimitMP = 60
	}
	if cfg.BlobDriver == "" {
		cfg.BlobDriver = "local"
	}
	cfg.PublicRead = true
	svc := New(cfg, st, fake, log)
	pool := imgproc.NewPool(1, svc.ProcessPhoto, log)
	svc.AttachPool(pool)
	t.Cleanup(pool.Close)
	return svc, fake
}

func testJPEG(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	for i := range img.Pix {
		img.Pix[i] = uint8(int(seed) + i%97)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mkNode(t *testing.T, svc *Service, date, title string) *NodeDTO {
	t.Helper()
	n, err := svc.CreateNode(context.Background(), NodeInput{Date: &date, Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// waitStatus 轮询照片直到到达目标状态。
func waitStatus(t *testing.T, svc *Service, id, want string) *PhotoDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p, err := svc.GetPhoto(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Status == want {
			return p
		}
		if p.Status == "failed" && want != "failed" {
			t.Fatalf("photo failed: %v", p.FailReason)
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("photo %s never reached %s", id, want)
	return nil
}

func TestUploadToReady(t *testing.T) {
	svc, fake := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-05-21", "黄山两日")

	p, err := svc.UploadLocal(ctx, n.ID, "cloud.jpg", "云海", bytes.NewReader(testJPEG(t, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "processing" {
		t.Errorf("local upload should start processing, got %s", p.Status)
	}
	ready := waitStatus(t, svc, p.ID, "ready")
	if ready.BlurHash == nil || ready.Dominant == nil || *ready.Width != 64 {
		t.Errorf("metadata incomplete: %+v", ready)
	}
	for _, v := range []string{"thumb", "md", "lg"} {
		if ready.Variants[v] == "" {
			t.Errorf("missing variant %s", v)
		}
	}
	// blob 里应有 1 个 orig + 3 个变体
	var orig, variants int
	for _, k := range fake.Keys() {
		switch {
		case len(k) > 5 && k[:5] == "orig/":
			orig++
		case len(k) > 4 && k[:4] == "var/":
			variants++
		}
	}
	if orig != 1 || variants != 3 {
		t.Errorf("blob contents: orig=%d variants=%d", orig, variants)
	}
}

func TestDuplicateUpload409(t *testing.T) {
	svc, _ := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-05-21", "节点")
	raw := testJPEG(t, 2)

	first, err := svc.UploadLocal(ctx, n.ID, "a.jpg", "", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, first.ID, "ready")

	_, err = svc.UploadLocal(ctx, n.ID, "b.jpg", "", bytes.NewReader(raw))
	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("second upload: got %v, want DuplicateError", err)
	}
	if dup.Existing.ID != first.ID {
		t.Errorf("duplicate should carry existing photo %s, got %s", first.ID, dup.Existing.ID)
	}
}

// TestCrossNodeSharedBlob 同图入两个节点 → 删一个不误删共享 blob，两个都清后 blob 才消失。
func TestCrossNodeSharedBlob(t *testing.T) {
	svc, fake := newTestService(t, Config{TrashTTLDays: 0})
	ctx := context.Background()
	raw := testJPEG(t, 3)
	nA := mkNode(t, svc, "2026-01-01", "A")
	nB := mkNode(t, svc, "2026-01-02", "B")

	pA, err := svc.UploadLocal(ctx, nA.ID, "x.jpg", "", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, pA.ID, "ready")
	pB, err := svc.UploadLocal(ctx, nB.ID, "x.jpg", "", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, pB.ID, "ready")

	countOrig := func() int {
		n := 0
		for _, k := range fake.Keys() {
			if len(k) > 5 && k[:5] == "orig/" {
				n++
			}
		}
		return n
	}
	if countOrig() != 1 {
		t.Fatalf("shared upload should store single orig, got %d", countOrig())
	}

	// 删 A 的照片并 GC：blob 必须保留（B 仍引用）
	if err := svc.DeletePhoto(ctx, pA.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // 保证 deleted_at < cutoff
	if err := svc.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if countOrig() != 1 {
		t.Fatal("GC deleted a blob still referenced by another node")
	}
	if _, err := svc.GetPhoto(ctx, pA.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("photo A should be purged, got %v", err)
	}

	// 删 B 后 GC：orig 与变体应全部清除
	if err := svc.DeletePhoto(ctx, pB.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := svc.GC(ctx); err != nil {
		t.Fatal(err)
	}
	if len(fake.Keys()) != 0 {
		t.Errorf("blobs should be empty after both refs purged, got %v", fake.Keys())
	}
}

func TestReaperExpiredSession(t *testing.T) {
	svc, fake := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-02-02", "N")

	// 手工制造一个已过期的 issued 会话（presign 需要 s3 驱动，这里直插 store）
	st := svc.Store()
	now := store.Now()
	photoID := NewULID()
	if err := st.CreatePhoto(ctx, &store.Photo{
		ID: photoID, NodeID: n.ID, Ord: 100, Status: "pending",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	objKey := blob.StagingKey(NewULID(), "jpg")
	fake.Put(ctx, objKey, bytes.NewReader([]byte("partial")), 7, "image/jpeg")
	if err := st.CreateSession(ctx, &store.UploadSession{
		ID: NewULID(), PhotoID: photoID, ObjectKey: objKey,
		ExpectSize: 100, ContentType: "image/jpeg", State: "issued",
		ExpiresAt: store.FormatTime(time.Now().Add(-time.Minute)), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := svc.GetPhoto(ctx, photoID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "failed" || p.FailReason == nil || *p.FailReason != "上传未完成" {
		t.Errorf("reaped photo: status=%s reason=%v", p.Status, p.FailReason)
	}
	if fake.Exists(objKey) {
		t.Error("staging object should be deleted")
	}
}

// TestStartupRecovery 崩溃遗留的 processing 记录在恢复扫描后重新入队并完成。
func TestStartupRecovery(t *testing.T) {
	svc, fake := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-03-03", "N")

	raw := testJPEG(t, 4)
	sha := shaOf(raw)
	ext := "jpg"
	if err := fake.Put(ctx, blob.OrigKey(sha, ext), bytes.NewReader(raw),
		int64(len(raw)), "image/jpeg"); err != nil {
		t.Fatal(err)
	}
	st := svc.Store()
	now := store.Now()
	photoID := NewULID()
	size := int64(len(raw))
	if err := st.CreatePhoto(ctx, &store.Photo{
		ID: photoID, NodeID: n.ID, Ord: 100, Status: "processing",
		SHA256: &sha, Ext: &ext, SizeBytes: &size,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond) // 让 updated_at 落在 cutoff 之前
	if _, err := svc.RecoverStuck(ctx, 0); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, photoID, "ready")
}

// TestCursorPagination 25 个节点按 10/页翻完：不重不漏。
func TestCursorPagination(t *testing.T) {
	svc, _ := newTestService(t, Config{})
	ctx := context.Background()
	want := map[string]bool{}
	for i := 0; i < 25; i++ {
		n := mkNode(t, svc, fmt.Sprintf("2026-01-%02d", i%28+1), fmt.Sprintf("节点%d", i))
		want[n.ID] = true
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		out, err := svc.Timeline(ctx, cursor, 10, true)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		var prevDate, prevID string
		for _, item := range out.Items {
			if seen[item.ID] {
				t.Fatalf("duplicate node across pages: %s", item.ID)
			}
			seen[item.ID] = true
			// 页内有序：date DESC, id DESC
			if prevDate != "" && (item.Date > prevDate || (item.Date == prevDate && item.ID > prevID)) {
				t.Fatalf("page out of order: %s/%s after %s/%s", item.Date, item.ID, prevDate, prevID)
			}
			prevDate, prevID = item.Date, item.ID
		}
		if out.NextCursor == nil {
			break
		}
		cursor = *out.NextCursor
	}
	if len(seen) != 25 {
		t.Errorf("saw %d nodes, want 25 (pages=%d)", len(seen), pages)
	}
	if pages != 3 {
		t.Errorf("expected 3 pages, got %d", pages)
	}
}

func TestTrashListAndRestore(t *testing.T) {
	svc, _ := newTestService(t, Config{TrashTTLDays: 7})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-04-04", "要删的节点")
	p, err := svc.UploadLocal(ctx, n.ID, "x.jpg", "留影", bytes.NewReader(testJPEG(t, 5)))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, p.ID, "ready")

	// 先单删照片 → trash 出现 photo 条目
	if err := svc.DeletePhoto(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	tr, err := svc.ListTrash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Items) != 1 || tr.Items[0].Type != "photo" || tr.Items[0].ID != p.ID {
		t.Fatalf("trash after photo delete: %+v", tr.Items)
	}
	if tr.Items[0].PurgeAt <= tr.Items[0].DeletedAt {
		t.Error("purge_at should be after deleted_at")
	}

	// 恢复照片
	if _, err := svc.RestorePhoto(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetPhoto(ctx, p.ID)
	if got == nil || got.Status != "ready" {
		t.Fatal("restored photo should be back and ready")
	}

	// 删节点 → trash 只有一个 node 条目（照片随节点，不单列）
	if err := svc.DeleteNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	tr, _ = svc.ListTrash(ctx)
	if len(tr.Items) != 1 || tr.Items[0].Type != "node" {
		t.Fatalf("trash after node delete: %+v", tr.Items)
	}
	if tr.Items[0].Extra["photo_count"] != 1 {
		t.Errorf("node trash extra: %+v", tr.Items[0].Extra)
	}

	// 恢复节点 → 照片一并回来
	if _, err := svc.RestoreNode(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	nd, err := svc.GetNode(ctx, n.ID)
	if err != nil || nd.PhotoCount != 1 {
		t.Fatalf("restored node: %+v err=%v", nd, err)
	}
}

func TestReorderAndCover(t *testing.T) {
	svc, _ := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-06-06", "整理")
	var ids []string
	for i := 0; i < 3; i++ {
		p, err := svc.UploadLocal(ctx, n.ID, fmt.Sprintf("p%d.jpg", i), "",
			bytes.NewReader(testJPEG(t, uint8(10+i*7))))
		if err != nil {
			t.Fatal(err)
		}
		waitStatus(t, svc, p.ID, "ready")
		ids = append(ids, p.ID)
	}
	// 设封面：最后一张移到最前
	newOrder := []string{ids[2], ids[0], ids[1]}
	if err := svc.ReorderPhotos(ctx, n.ID, newOrder); err != nil {
		t.Fatal(err)
	}
	nd, err := svc.GetNode(ctx, n.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range nd.Photos {
		if p.ID != newOrder[i] {
			t.Errorf("position %d: got %s want %s", i, p.ID, newOrder[i])
		}
		if p.Ord != int64(i+1)*100 {
			t.Errorf("position %d ord: got %d want %d", i, p.Ord, (i+1)*100)
		}
	}
	// 集合不一致要拒绝
	if err := svc.ReorderPhotos(ctx, n.ID, ids[:2]); err == nil {
		t.Error("partial reorder should fail")
	}
}

func TestReprocessFailed(t *testing.T) {
	svc, fake := newTestService(t, Config{})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-07-07", "N")

	// 上传合法魔数但内容损坏的"jpeg" → failed
	bad := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 100)...)
	p, err := svc.UploadLocal(ctx, n.ID, "bad.jpg", "", bytes.NewReader(bad))
	if err != nil {
		t.Fatal(err)
	}
	failed := waitStatus(t, svc, p.ID, "failed")
	if failed.FailReason == nil {
		t.Error("failed photo should carry reason")
	}

	// 原图还在：把 blob 里换成好图后 reprocess 应成功（模拟"原图已在直接重跑管线"）
	good := testJPEG(t, 9)
	stored, _ := svc.Store().GetPhoto(ctx, p.ID, false)
	fake.Put(ctx, blob.OrigKey(*stored.SHA256, *stored.Ext),
		bytes.NewReader(good), int64(len(good)), "image/jpeg")
	if _, err := svc.Reprocess(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, p.ID, "ready")
}

func shaOf(b []byte) string {
	return fmt.Sprintf("%x", sha256Sum(b))
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}

// TestConfirmFailedIsIdempotent 对同一张已失败的照片重复 confirm，必须每次
// 都返回与首次相同的错误。回归：曾经第二次会变成「会话已过期，请重新上传」，
// 而真实原因是文件不是有效图片——把人往错误方向引。
func TestConfirmFailedIsIdempotent(t *testing.T) {
	svc, fake := newTestService(t, Config{BlobDriver: "s3"})
	ctx := context.Background()
	n := mkNode(t, svc, "2026-09-09", "N")

	cases := []struct {
		reason string
		check  func(error) bool
		name   string
	}{
		{failNotAnImage, func(e error) bool { return errors.Is(e, ErrUnsupportedMedia) }, "非图片"},
		{failUploadIncomplete, func(e error) bool { return errors.Is(e, ErrSessionExpired) }, "上传未完成"},
		{failSizeMismatch, func(e error) bool {
			var ve *ValidationError
			return errors.As(e, &ve)
		}, "大小不符"},
		{failReadBack, func(e error) bool { return errors.Is(e, ErrStorage) }, "回读失败"},
	}

	for _, c := range cases {
		now := store.Now()
		photoID := NewULID()
		ext := "jpg"
		if err := svc.Store().CreatePhoto(ctx, &store.Photo{
			ID: photoID, NodeID: n.ID, Ord: 100, Status: "pending", Ext: &ext,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		// 会话已被终结（aborted），照片记着失败原因
		sessID := NewULID()
		objKey := blob.StagingKey(sessID, ext)
		fake.Put(ctx, objKey, bytes.NewReader([]byte("x")), 1, "image/jpeg")
		if err := svc.Store().CreateSession(ctx, &store.UploadSession{
			ID: sessID, PhotoID: photoID, ObjectKey: objKey, ExpectSize: 1,
			ContentType: "image/jpeg", State: "issued",
			ExpiresAt: store.FormatTime(time.Now().Add(time.Hour)), CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		svc.Store().SetSessionState(ctx, sessID, "aborted")
		svc.Store().MarkPhotoFailed(ctx, photoID, c.reason, store.Now())

		// 连续三次 confirm 必须给出同一个错误
		var first error
		for i := 0; i < 3; i++ {
			_, err := svc.ConfirmUpload(ctx, photoID)
			if err == nil {
				t.Fatalf("%s: confirm #%d should fail", c.name, i+1)
			}
			if !c.check(err) {
				t.Errorf("%s: confirm #%d wrong error type: %v", c.name, i+1, err)
			}
			if i == 0 {
				first = err
			} else if err.Error() != first.Error() {
				t.Errorf("%s: confirm #%d differs from first: %v vs %v", c.name, i+1, err, first)
			}
		}
	}
}
