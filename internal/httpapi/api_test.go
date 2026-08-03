package httpapi

import (
	"bytes"
	"encoding/json"

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
	ts := newTestServer(t, 30)
	// 上传端点 10 次/分钟：第 11 次必须 429 + Retry-After
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
