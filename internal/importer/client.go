package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// APIError 是服务端返回的结构化错误。
type APIError struct {
	Status     int
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int
	// Photo 在 CONFLICT_DUPLICATE 时携带已存在的照片
	Photo *PhotoDTO `json:"photo"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%d)", e.Message, e.Status)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// IsDuplicate 判断是否为秒传冲突（该照片已在目标节点中）。
func (e *APIError) IsDuplicate() bool { return e.Status == http.StatusConflict }

// PhotoDTO / NodeDTO 只声明导入需要的字段。
type PhotoDTO struct {
	ID      string `json:"id"`
	Caption string `json:"caption"`
	Status  string `json:"status"`
}

// NodeDTO 是时间轴节点。
type NodeDTO struct {
	ID          string      `json:"id"`
	Date        string      `json:"date"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	PhotoCount  int         `json:"photo_count"`
	Photos      []*PhotoDTO `json:"photos"`
}

// Client 是拾光集 API 的最小客户端。
type Client struct {
	base  string
	token string
	http  *http.Client
}

// NewClient 创建客户端；base 形如 https://host 或 http://host:8080。
func NewClient(base, token string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimSuffix(base, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("无效的服务地址 %q（应形如 http://host:8080）", base)
	}
	return &Client{
		base:  u.String(),
		token: token,
		http:  &http.Client{Timeout: timeout},
	}, nil
}

// do 执行请求并解析错误响应。
func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))

	if res.StatusCode >= 400 {
		apiErr := &APIError{Status: res.StatusCode}
		json.Unmarshal(body, apiErr)
		if ra := res.Header.Get("Retry-After"); ra != "" {
			apiErr.RetryAfter, _ = strconv.Atoi(ra)
		}
		return apiErr
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("解析响应失败: %w", err)
		}
	}
	return nil
}

// Ping 用 GET /stats 验证地址与 token（同时拿到 upload_mode）。
func (c *Client) Ping(ctx context.Context) (uploadMode string, err error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/v1/stats", nil)
	if err != nil {
		return "", err
	}
	var stats struct {
		UploadMode string `json:"upload_mode"`
	}
	if err := c.do(req, &stats); err != nil {
		return "", err
	}
	return stats.UploadMode, nil
}

// ListNodes 取回全部节点（大 limit 循环翻页），用于复用已存在的同名节点。
func (c *Client) ListNodes(ctx context.Context) ([]*NodeDTO, error) {
	var all []*NodeDTO
	cursor := ""
	for {
		u := c.base + "/api/v1/timeline?limit=50"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Items      []*NodeDTO `json:"items"`
			NextCursor *string    `json:"next_cursor"`
		}
		if err := c.do(req, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if page.NextCursor == nil {
			return all, nil
		}
		cursor = *page.NextCursor
	}
}

// CreateNode 新建节点。
func (c *Client) CreateNode(ctx context.Context, date, title, desc string) (*NodeDTO, error) {
	body, _ := json.Marshal(map[string]string{
		"date": date, "title": title, "description": desc,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/v1/nodes",
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	var n NodeDTO
	if err := c.do(req, &n); err != nil {
		return nil, err
	}
	return &n, nil
}

// UploadLocal 以 multipart 上传一张照片（local 模式）。
func (c *Client) UploadLocal(ctx context.Context, nodeID, path, caption string) (*PhotoDTO, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 用 pipe 边读边传，避免把 30MB 原图整个读进内存
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { pw.CloseWithError(werr) }()
		fw, err := mw.CreateFormFile("file", filepathBase(path))
		if err != nil {
			werr = err
			return
		}
		if _, err := io.Copy(fw, f); err != nil {
			werr = err
			return
		}
		if caption != "" {
			if err := mw.WriteField("caption", caption); err != nil {
				werr = err
				return
			}
		}
		werr = mw.Close()
	}()

	req, err := http.NewRequestWithContext(ctx, "POST",
		c.base+"/api/v1/nodes/"+nodeID+"/photos", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var p PhotoDTO
	if err := c.do(req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PresignPut 为 s3 模式签发直传 URL。
func (c *Client) PresignPut(ctx context.Context, nodeID, filename string, size int64, contentType string) (uploadURL, photoID string, err error) {
	body, _ := json.Marshal(map[string]any{
		"node_id": nodeID, "filename": filename, "size": size, "content_type": contentType,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.base+"/api/v1/uploads/presign",
		bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		UploadURL string `json:"upload_url"`
		PhotoID   string `json:"photo_id"`
	}
	if err := c.do(req, &out); err != nil {
		return "", "", err
	}
	return out.UploadURL, out.PhotoID, nil
}

// PutObject 直传到对象存储（不带 Bearer，URL 自带签名）。
func (c *Client) PutObject(ctx context.Context, uploadURL, path, contentType string, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL, f)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.ContentLength = size
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
	if res.StatusCode >= 400 {
		return fmt.Errorf("对象存储直传失败: HTTP %d", res.StatusCode)
	}
	return nil
}

// ConfirmUpload 完成 s3 直传。
func (c *Client) ConfirmUpload(ctx context.Context, photoID string) (*PhotoDTO, error) {
	req, err := http.NewRequestWithContext(ctx, "POST",
		c.base+"/api/v1/photos/"+photoID+"/confirm", nil)
	if err != nil {
		return nil, err
	}
	var p PhotoDTO
	if err := c.do(req, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// withRetry 对 429 与 5xx / 网络错误做指数退避重试；4xx（除 429）立即返回。
func withRetry(ctx context.Context, attempts int, fn func() error) error {
	var last error
	for i := 0; i < attempts; i++ {
		last = fn()
		if last == nil {
			return nil
		}
		var apiErr *APIError
		if errors.As(last, &apiErr) {
			retryable := apiErr.Status == http.StatusTooManyRequests || apiErr.Status >= 500
			if !retryable {
				return last
			}
			if apiErr.RetryAfter > 0 {
				if err := sleepCtx(ctx, time.Duration(apiErr.RetryAfter)*time.Second); err != nil {
					return err
				}
				continue
			}
		}
		if i == attempts-1 {
			break
		}
		backoff := time.Duration(1<<uint(i)) * time.Second
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		jitter := time.Duration(rand.Int63n(int64(500 * time.Millisecond)))
		if err := sleepCtx(ctx, backoff+jitter); err != nil {
			return err
		}
	}
	return last
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// filepathBase 返回路径最后一段（避免引入 path/filepath 到本文件的多处使用）。
func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
