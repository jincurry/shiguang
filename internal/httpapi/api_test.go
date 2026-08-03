package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"shiguang/internal/blob"
	"shiguang/internal/imgproc"
	"shiguang/internal/service"
	"shiguang/internal/store"
	"shiguang/migrations"
)

const testToken = "test-admin-token"

func newTestServer(t *testing.T, uploadLimitMB int64) *httptest.Server {
	return newTestServerRPM(t, uploadLimitMB, 0)
}

// newTestServerRPM 允许指定上传端点限流（次/分钟），0 = 用默认值。
func newTestServerRPM(t *testing.T, uploadLimitMB int64, uploadRPM float64) *httptest.Server {
	t.Helper()
	st, err := store.Open("file:"+t.TempDir()+"/test.db", migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fake := blob.NewFake()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := service.New(service.Config{
		BlobDriver: "local", PublicRead: true,
		UploadLimitMB: uploadLimitMB, PixelLimitMP: 60, TrashTTLDays: 7,
	}, st, fake, log)
	pool := imgproc.NewPool(1, svc.ProcessPhoto, log)
	svc.AttachPool(pool)
	t.Cleanup(pool.Close)

	srv := New(svc, log, Options{
		AdminToken: testToken, PublicRead: true,
		IndexHTML: []byte("<html>index</html>"), AdminHTML: []byte("<html>admin</html>"),
		UploadRPM: uploadRPM,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var data map[string]any
	json.NewDecoder(res.Body).Decode(&data)
	return res, data
}

func TestAuth(t *testing.T) {
	ts := newTestServer(t, 30)

	// 无 token 写接口 → 401 + 统一错误结构
	res, data := doJSON(t, "POST", ts.URL+"/api/v1/nodes", "",
		map[string]string{"date": "2026-01-01", "title": "x"})
	if res.StatusCode != 401 {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if data["code"] != "UNAUTHORIZED" || data["message"] == "" {
		t.Errorf("error shape: %v", data)
	}

	// 错误 token → 401（即使 public read）
	res, _ = doJSON(t, "GET", ts.URL+"/api/v1/stats", "wrong-token", nil)
	if res.StatusCode != 401 {
		t.Errorf("wrong token on read: %d", res.StatusCode)
	}

	// 正确 token → 200
	res, _ = doJSON(t, "GET", ts.URL+"/api/v1/stats", testToken, nil)
	if res.StatusCode != 200 {
		t.Errorf("good token: %d", res.StatusCode)
	}

	// public read 无 token 读接口放行
	res, _ = doJSON(t, "GET", ts.URL+"/api/v1/timeline", "", nil)
	if res.StatusCode != 200 {
		t.Errorf("public read: %d", res.StatusCode)
	}
}

func TestUploadRateLimit429(t *testing.T) {
	// 显式配置 10 次/分钟（burst 10）：第 11 次必须 429 + Retry-After
	ts := newTestServerRPM(t, 30, 10)
	var last *http.Response
	for i := 0; i < 11; i++ {
		res, _ := doJSON(t, "POST", ts.URL+"/api/v1/uploads/presign", testToken,
			map[string]any{"node_id": "missing", "filename": "x.jpg",
				"size": 100, "content_type": "image/jpeg"})
		last = res
	}
	if last.StatusCode != 429 {
		t.Fatalf("11th upload call: %d, want 429", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
}

// TestBulkUploadBurstNotLimited 回归：默认限流必须扛得住批量导入的并发首波。
// 旧默认（10 次/分钟 burst 10）会让拖入 50 张的第 11 张起全部 429。
func TestBulkUploadBurstNotLimited(t *testing.T) {
	ts := newTestServer(t, 30) // 默认 120/min，burst 60
	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "批量"})
	nodeID, _ := node["id"].(string)

	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, image.NewRGBA(image.Rect(0, 0, 16, 16)), nil)

	for i := 0; i < 50; i++ {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		fw, _ := mw.CreateFormFile("file", fmt.Sprintf("p%d.jpg", i))
		// 每张内容不同，避免命中同节点秒传 409
		fw.Write(jbuf.Bytes())
		fw.Write(bytes.Repeat([]byte{byte(i)}, i+1))
		mw.Close()
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/nodes/"+nodeID+"/photos", &buf)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == 429 {
			t.Fatalf("upload %d/50 hit rate limit under default config", i+1)
		}
		if res.StatusCode != 202 {
			t.Fatalf("upload %d/50: %d", i+1, res.StatusCode)
		}
	}
}

func TestMovePhotoBetweenNodes(t *testing.T) {
	ts := newTestServer(t, 30)
	_, nodeA := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "A"})
	_, nodeB := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-02-02", "title": "B"})
	idA, _ := nodeA["id"].(string)
	idB, _ := nodeB["id"].(string)

	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, image.NewRGBA(image.Rect(0, 0, 20, 20)), nil)
	photoID := uploadTestPhoto(t, ts, idA, "move-me.jpg", jbuf.Bytes())

	// 移动到 B
	res, data := doJSON(t, "PATCH", ts.URL+"/api/v1/photos/"+photoID, testToken,
		map[string]string{"node_id": idB})
	if res.StatusCode != 200 {
		t.Fatalf("move: %d %v", res.StatusCode, data)
	}
	_, nodeBFull := doJSON(t, "GET", ts.URL+"/api/v1/nodes/"+idB, testToken, nil)
	if int(nodeBFull["photo_count"].(float64)) != 1 {
		t.Errorf("target node should have the photo: %v", nodeBFull)
	}
	_, nodeAFull := doJSON(t, "GET", ts.URL+"/api/v1/nodes/"+idA, testToken, nil)
	if int(nodeAFull["photo_count"].(float64)) != 0 {
		t.Errorf("source node should be empty: %v", nodeAFull)
	}

	// 目标节点已有同一张图 → 409
	same := uploadTestPhoto(t, ts, idA, "same.jpg", jbuf.Bytes())
	res, data = doJSON(t, "PATCH", ts.URL+"/api/v1/photos/"+same, testToken,
		map[string]string{"node_id": idB})
	if res.StatusCode != 409 || data["code"] != "CONFLICT_DUPLICATE" {
		t.Errorf("move duplicate into target: %d %v", res.StatusCode, data)
	}

	// 目标节点不存在 → 404
	res, _ = doJSON(t, "PATCH", ts.URL+"/api/v1/photos/"+same, testToken,
		map[string]string{"node_id": "01NOPE"})
	if res.StatusCode != 404 {
		t.Errorf("move to missing node: %d", res.StatusCode)
	}
}

// uploadTestPhoto 上传一张图并返回 photo id。
func uploadTestPhoto(t *testing.T, ts *httptest.Server, nodeID, name string, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", name)
	fw.Write(data)
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/nodes/"+nodeID+"/photos", &buf)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var p map[string]any
	json.NewDecoder(res.Body).Decode(&p)
	if res.StatusCode != 202 {
		t.Fatalf("upload %s: %d %v", name, res.StatusCode, p)
	}
	id, _ := p["id"].(string)
	return id
}

func TestMultipartTooLarge413(t *testing.T) {
	ts := newTestServer(t, 1) // 1MB 上限

	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "N"})
	nodeID, _ := node["id"].(string)
	if nodeID == "" {
		t.Fatal("create node failed")
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "big.jpg")
	fw.Write([]byte{0xFF, 0xD8, 0xFF})
	fw.Write(bytes.Repeat([]byte{0}, 3<<20)) // 3MB > 1MB 上限
	mw.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/nodes/"+nodeID+"/photos", &buf)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 413 {
		t.Fatalf("oversize upload: %d, want 413", res.StatusCode)
	}
	var data map[string]any
	json.NewDecoder(res.Body).Decode(&data)
	if data["code"] != "PAYLOAD_TOO_LARGE" {
		t.Errorf("error code: %v", data)
	}
}

func TestUnsupportedMedia415(t *testing.T) {
	ts := newTestServer(t, 30)
	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "N"})
	nodeID, _ := node["id"].(string)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "fake.jpg")
	fw.Write([]byte("this is a text file pretending to be jpg"))
	mw.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/nodes/"+nodeID+"/photos", &buf)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatalf("txt as jpg: %d, want 415", res.StatusCode)
	}
	var data map[string]any
	json.NewDecoder(res.Body).Decode(&data)
	if data["code"] != "UNSUPPORTED_MEDIA" {
		t.Errorf("error code: %v", data)
	}
}

func TestTimelineShapeAndFlow(t *testing.T) {
	ts := newTestServer(t, 30)

	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-05-21", "title": "黄山两日", "description": "云海"})
	nodeID, _ := node["id"].(string)

	// 真实 jpeg 上传 → 202
	img := image.NewRGBA(image.Rect(0, 0, 32, 24))
	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, img, nil)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "photo.jpg")
	fw.Write(jbuf.Bytes())
	mw.WriteField("caption", "云海翻涌")
	mw.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/api/v1/nodes/"+nodeID+"/photos", &buf)
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var photo map[string]any
	json.NewDecoder(res.Body).Decode(&photo)
	res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatalf("upload: %d %v", res.StatusCode, photo)
	}
	photoID, _ := photo["id"].(string)

	// 轮询直到 ready
	deadline := time.Now().Add(10 * time.Second)
	for {
		res, p := doJSON(t, "GET", ts.URL+"/api/v1/photos/"+photoID, "", nil)
		if res.StatusCode != 200 {
			t.Fatalf("get photo: %d", res.StatusCode)
		}
		if p["status"] == "ready" {
			break
		}
		if p["status"] == "failed" || time.Now().After(deadline) {
			t.Fatalf("photo never ready: %v", p)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// timeline 响应结构逐字段核对
	res2, err := http.Get(ts.URL + "/api/v1/timeline?limit=10")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	var tlr struct {
		Items []struct {
			ID          string `json:"id"`
			Date        string `json:"date"`
			Title       string `json:"title"`
			Description string `json:"description"`
			PhotoCount  int    `json:"photo_count"`
			Photos      []struct {
				ID         string            `json:"id"`
				Caption    string            `json:"caption"`
				Status     string            `json:"status"`
				FailReason *string           `json:"fail_reason"`
				BlurHash   *string           `json:"blurhash"`
				Dominant   *string           `json:"dominant"`
				Width      *int              `json:"width"`
				Height     *int              `json:"height"`
				Variants   map[string]string `json:"variants"`
			} `json:"photos"`
		} `json:"items"`
		NextCursor *string `json:"next_cursor"`
	}
	raw, _ := io.ReadAll(res2.Body)
	if err := json.Unmarshal(raw, &tlr); err != nil {
		t.Fatalf("timeline decode: %v\n%s", err, raw)
	}
	if len(tlr.Items) != 1 || tlr.Items[0].PhotoCount != 1 {
		t.Fatalf("timeline items: %s", raw)
	}
	p := tlr.Items[0].Photos[0]
	if p.Caption != "云海翻涌" || p.Status != "ready" ||
		p.BlurHash == nil || p.Dominant == nil || p.Width == nil {
		t.Errorf("photo fields: %s", raw)
	}
	for _, v := range []string{"thumb", "md", "lg"} {
		if !strings.HasPrefix(p.Variants[v], "/img/var/") {
			t.Errorf("variant %s url: %q", v, p.Variants[v])
		}
	}
	if tlr.NextCursor != nil {
		t.Errorf("single page should have null next_cursor")
	}

	// limit 越界 → 422
	res3, data := doJSON(t, "GET", ts.URL+"/api/v1/timeline?limit=999", "", nil)
	if res3.StatusCode != 422 || data["code"] != "VALIDATION_FAILED" {
		t.Errorf("bad limit: %d %v", res3.StatusCode, data)
	}
}

func TestNotFoundShape(t *testing.T) {
	ts := newTestServer(t, 30)
	res, data := doJSON(t, "GET", ts.URL+"/api/v1/nodes/01NOPE", "", nil)
	if res.StatusCode != 404 || data["code"] != "NOT_FOUND" {
		t.Errorf("missing node: %d %v", res.StatusCode, data)
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t, 30)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("healthz: %d", res.StatusCode)
	}
}

func TestPagesServed(t *testing.T) {
	ts := newTestServer(t, 30)
	for _, path := range []string{"/", "/admin"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 || !bytes.Contains(body, []byte("<html>")) {
			t.Errorf("%s: %d %q", path, res.StatusCode, body)
		}
	}
}

// TestLongFilenameCaptionClamped 回归：文件名超过 200 字时图注被截断而非
// 撞 DB CHECK 约束返回 500——照片是不可再生资产，不能因文件名长就拒收。
func TestLongFilenameCaptionClamped(t *testing.T) {
	ts := newTestServer(t, 30)
	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "N"})
	nodeID, _ := node["id"].(string)

	var jbuf bytes.Buffer
	jpeg.Encode(&jbuf, image.NewRGBA(image.Rect(0, 0, 12, 12)), nil)
	longName := strings.Repeat("a", 210) + ".jpg"

	photoID := uploadTestPhoto(t, ts, nodeID, longName, jbuf.Bytes())
	_, p := doJSON(t, "GET", ts.URL+"/api/v1/photos/"+photoID, testToken, nil)
	cap, _ := p["caption"].(string)
	if n := len([]rune(cap)); n != 200 {
		t.Errorf("caption should be clamped to 200 runes, got %d", n)
	}

	// 显式改注超长仍应 422（用户明确意图，不该悄悄截断）
	res, data := doJSON(t, "PATCH", ts.URL+"/api/v1/photos/"+photoID, testToken,
		map[string]string{"caption": strings.Repeat("b", 201)})
	if res.StatusCode != 422 || data["code"] != "VALIDATION_FAILED" {
		t.Errorf("explicit over-long caption: %d %v", res.StatusCode, data)
	}
}

// TestImgNotThrottledByAPILimit 回归：一个节点展开就是几百个 /img 请求，
// 不能让 API 的 50 rps 限流把自己相册的图片挡掉（浏览器会当成加载失败，
// 在好照片上盖出「曝光失败」章）。
func TestImgNotThrottledByAPILimit(t *testing.T) {
	st, err := store.Open("file:"+t.TempDir()+"/test.db", migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root := t.TempDir()
	local, err := blob.NewLocal(root)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := service.New(service.Config{
		BlobDriver: "local", PublicRead: true,
		UploadLimitMB: 30, PixelLimitMP: 60, TrashTTLDays: 7,
	}, st, local, log)
	pool := imgproc.NewPool(1, svc.ProcessPhoto, log)
	svc.AttachPool(pool)
	t.Cleanup(pool.Close)

	// 直接放一个变体对象，绕开上传管线
	key := "var/ab/cd/" + strings.Repeat("a", 64) + "/thumb.webp"
	if err := local.Put(context.Background(), key,
		bytes.NewReader([]byte("fake-webp-bytes")), 15, "image/webp"); err != nil {
		t.Fatal(err)
	}

	srv := New(svc, log, Options{
		AdminToken: testToken, PublicRead: true, LocalBlob: local,
		IndexHTML: []byte("<html>i</html>"), AdminHTML: []byte("<html>a</html>"),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// 200 次连续取图（相当于展开一个 200 张的节点），一个 429 都不该有
	for i := 0; i < 200; i++ {
		res, err := http.Get(ts.URL + "/img/" + key)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == 429 {
			t.Fatalf("image %d/200 was rate limited — gallery must not throttle its own images", i+1)
		}
		if res.StatusCode != 200 {
			t.Fatalf("image %d/200: %d", i+1, res.StatusCode)
		}
	}
}

func TestBatchPhotos(t *testing.T) {
	ts := newTestServer(t, 30)
	_, nodeA := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-01-01", "title": "A"})
	_, nodeB := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-02-02", "title": "B"})
	idA, _ := nodeA["id"].(string)
	idB, _ := nodeB["id"].(string)

	var base bytes.Buffer
	jpeg.Encode(&base, image.NewRGBA(image.Rect(0, 0, 14, 14)), nil)
	mk := func(nodeID string, seed byte) string {
		data := append(append([]byte{}, base.Bytes()...), bytes.Repeat([]byte{seed}, int(seed)+1)...)
		return uploadTestPhoto(t, ts, nodeID, fmt.Sprintf("p%d.jpg", seed), data)
	}
	ids := []string{mk(idA, 1), mk(idA, 2), mk(idA, 3)}

	// 批量移动到 B
	res, data := doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "move", "photo_ids": ids, "node_id": idB})
	if res.StatusCode != 200 {
		t.Fatalf("batch move: %d %v", res.StatusCode, data)
	}
	if int(data["succeeded"].(float64)) != 3 {
		t.Errorf("want 3 moved, got %v", data)
	}
	_, nb := doJSON(t, "GET", ts.URL+"/api/v1/nodes/"+idB, testToken, nil)
	if int(nb["photo_count"].(float64)) != 3 {
		t.Errorf("target node count: %v", nb["photo_count"])
	}

	// 批量删除其中 2 张
	res, data = doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "delete", "photo_ids": ids[:2]})
	if res.StatusCode != 200 || int(data["succeeded"].(float64)) != 2 {
		t.Fatalf("batch delete: %d %v", res.StatusCode, data)
	}
	_, nb = doJSON(t, "GET", ts.URL+"/api/v1/nodes/"+idB, testToken, nil)
	if int(nb["photo_count"].(float64)) != 1 {
		t.Errorf("after delete count: %v", nb["photo_count"])
	}

	// 部分失败要逐条汇报，不能整批失败
	dup := mk(idA, 9)
	same := mk(idB, 9) // 与 dup 内容相同，已在 B
	_ = same
	res, data = doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "move", "photo_ids": []string{dup, "01NOPE"}, "node_id": idB})
	if res.StatusCode != 200 {
		t.Fatalf("partial failure should still be 200: %d %v", res.StatusCode, data)
	}
	failed, _ := data["failed"].([]any)
	if len(failed) != 2 {
		t.Errorf("expected 2 failures (duplicate + missing), got %v", data)
	}

	// 参数校验
	res, _ = doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "move", "photo_ids": ids})
	if res.StatusCode != 422 {
		t.Errorf("move without node_id should be 422, got %d", res.StatusCode)
	}
	res, _ = doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "nonsense", "photo_ids": ids})
	if res.StatusCode != 422 {
		t.Errorf("bad action should be 422, got %d", res.StatusCode)
	}
	res, _ = doJSON(t, "POST", ts.URL+"/api/v1/photos/batch", testToken,
		map[string]any{"action": "delete", "photo_ids": []string{}})
	if res.StatusCode != 422 {
		t.Errorf("empty ids should be 422, got %d", res.StatusCode)
	}
}

// TestTimelineWithoutPhotos include_photos=false 只回节点元数据与计数，
// 后台左栏靠它避免登录时把整库照片拉下来。
func TestTimelineWithoutPhotos(t *testing.T) {
	ts := newTestServer(t, 30)
	_, node := doJSON(t, "POST", ts.URL+"/api/v1/nodes", testToken,
		map[string]string{"date": "2026-03-03", "title": "N"})
	nodeID, _ := node["id"].(string)
	var jb bytes.Buffer
	jpeg.Encode(&jb, image.NewRGBA(image.Rect(0, 0, 10, 10)), nil)
	uploadTestPhoto(t, ts, nodeID, "a.jpg", jb.Bytes())

	_, lite := doJSON(t, "GET", ts.URL+"/api/v1/timeline?include_photos=false", testToken, nil)
	items, _ := lite["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %v", lite)
	}
	it := items[0].(map[string]any)
	if int(it["photo_count"].(float64)) != 1 {
		t.Errorf("photo_count should still be accurate: %v", it["photo_count"])
	}
	if photos, _ := it["photos"].([]any); len(photos) != 0 {
		t.Errorf("photos should be empty, got %v", photos)
	}

	// 默认（不带参数）仍返回照片，前台时间轴依赖它
	_, full := doJSON(t, "GET", ts.URL+"/api/v1/timeline", testToken, nil)
	fit := full["items"].([]any)[0].(map[string]any)
	if photos, _ := fit["photos"].([]any); len(photos) != 1 {
		t.Errorf("default must include photos, got %v", photos)
	}
}
