// Package httpapi 提供 HTTP 路由、中间件与 handler；业务全部下沉 service。
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"shiguang/internal/blob"
	"shiguang/internal/service"
)

// Server 装配路由与中间件。
type Server struct {
	svc        *service.Service
	log        *slog.Logger
	adminToken string
	publicRead bool
	signSecret string
	localBlob  *blob.Local // 非 nil 表示 local 模式，/img 走文件直出
	index      []byte
	admin      []byte
	favicon    []byte
	globalRPS  float64
	uploadRPM  float64
}

// Options 是 Server 构造参数。
type Options struct {
	AdminToken string
	PublicRead bool
	SignSecret string
	LocalBlob  *blob.Local // local 模式传入，s3 模式为 nil
	IndexHTML  []byte
	AdminHTML  []byte
	FaviconSVG []byte
	// GlobalRPS 全局令牌桶速率（每秒），0 用默认 50。
	GlobalRPS float64
	// UploadRPM 上传端点速率（每分钟），0 用默认 600。
	// 上传 handler 只做落盘 + 入队（I/O 廉价），真正的 CPU 消耗由 worker 池
	// 自行限流，所以这里不需要卡得很死——卡死只会阻碍正常的批量导入：
	// 实测 120/min 下导 200 张会产生 400+ 次 429。
	UploadRPM float64
}

// New 创建 Server。
func New(svc *service.Service, log *slog.Logger, opt Options) *Server {
	if opt.GlobalRPS <= 0 {
		opt.GlobalRPS = 50
	}
	if opt.UploadRPM <= 0 {
		opt.UploadRPM = 600
	}
	return &Server{
		svc:        svc,
		log:        log,
		adminToken: opt.AdminToken,
		publicRead: opt.PublicRead,
		signSecret: opt.SignSecret,
		localBlob:  opt.LocalBlob,
		index:      opt.IndexHTML,
		admin:      opt.AdminHTML,
		favicon:    opt.FaviconSVG,
		globalRPS:  opt.GlobalRPS,
		uploadRPM:  opt.UploadRPM,
	}
}

// Handler 构建完整路由。
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, recoverer(s.log), accessLog(s.log))

	// 变体图片单独限流，且远高于 API：一个节点展开就是几百个 /img 请求，
	// 套用 API 的 50 rps 会让相册限流自己的图片——浏览器收到 429 后
	// onerror 触发，好照片上会盖出"曝光失败"章。这些是 immutable 缓存资源，
	// 真要控带宽应放在反向代理/CDN 层，而不是靠 API 预算。
	r.Get("/favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(s.favicon)
	})
	if s.localBlob != nil {
		imgLimit := rateLimit(newBucket(500, 1000))
		r.Group(func(g chi.Router) {
			g.Use(imgLimit)
			g.Get("/img/*", s.handleImg)
			g.Head("/img/*", s.handleImg)
		})
	}

	// 其余路由走全局令牌桶（默认 50 rps）；上传端点再叠一层（默认 600 次/分钟）。
	// burst 给到速率的两倍，让批量导入的并发首波能一次进桶而不是立刻 429。
	global := rateLimit(newBucket(s.globalRPS, s.globalRPS*2))
	uploadLimit := rateLimit(newBucket(s.uploadRPM/60.0, max(10, s.uploadRPM/2)))

	r.Group(func(r chi.Router) {
		r.Use(global)

		// 前端两页（embed）
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(s.index)
		})
		r.Get("/admin", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(s.admin)
		})

		r.Get("/healthz", s.handleHealthz)

		r.Route("/api/v1", func(api chi.Router) {
			// 读接口
			api.Group(func(g chi.Router) {
				g.Use(s.readAuth)
				g.Get("/timeline", s.handleTimeline)
				g.Get("/nodes/{id}", s.handleGetNode)
				g.Get("/photos/{id}", s.handleGetPhoto)
				g.Get("/stats", s.handleStats)
				g.Get("/on-this-day", s.handleOnThisDay)
				g.Get("/places", s.handlePlaces)
				g.Get("/places/{place}", s.handleNodesAtPlace)
			})
			// 写接口（管理员）
			api.Group(func(g chi.Router) {
				g.Use(s.requireAuth)
				g.Get("/photos/{id}/original", s.handleOriginal)
				g.Post("/nodes", s.handleCreateNode)
				g.Patch("/nodes/{id}", s.handlePatchNode)
				g.Delete("/nodes/{id}", s.handleDeleteNode)
				g.Post("/nodes/{id}/restore", s.handleRestoreNode)
				g.Put("/nodes/{id}/photos/order", s.handleReorder)
				g.Patch("/photos/{id}", s.handlePatchPhoto)
				g.Delete("/photos/{id}", s.handleDeletePhoto)
				g.Post("/photos/{id}/restore", s.handleRestorePhoto)
				g.Post("/photos/{id}/reprocess", s.handleReprocess)
				g.Post("/photos/batch", s.handleBatchPhotos)
				g.Get("/trash", s.handleTrash)
				// 上传端点：单独限流 10 次/分钟
				g.Group(func(u chi.Router) {
					u.Use(uploadLimit)
					u.Post("/nodes/{id}/photos", s.handleUploadLocal)
					u.Post("/uploads/presign", s.handlePresign)
					u.Post("/photos/{id}/confirm", s.handleConfirm)
				})
			})
		})
	})
	return r
}

// ===== 读 handler =====

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "limit 取值 1-50")
			return
		}
		limit = n
	}
	// include_photos=false：只要节点元数据（后台左栏用）
	withPhotos := r.URL.Query().Get("include_photos") != "false"
	// photo_limit=N：每个节点最多带 N 张照片（photo_count 仍是真实总数），
	// 前台首屏只取折叠摞露出的那几张，展开时再按 id 拉全节点
	photoLimit := 0
	if v := r.URL.Query().Get("photo_limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "photo_limit 取值 1-200")
			return
		}
		photoLimit = n
	}
	out, err := s.svc.Timeline(r.Context(), r.URL.Query().Get("cursor"), limit, withPhotos, photoLimit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetPhoto(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetPhoto(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleOnThisDay 往年的今天。日期取服务器本地时区的今天，前端也可以传
// ?date=YYYY-MM-DD 覆盖（时区差一天时前端更知道用户的"今天"是哪天）。
func (s *Server) handleOnThisDay(w http.ResponseWriter, r *http.Request) {
	day := time.Now()
	if v := r.URL.Query().Get("date"); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "date 必须为 YYYY-MM-DD")
			return
		}
		day = t
	}
	limit := 5
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 20 {
			writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "limit 取值 1-20")
			return
		}
		limit = n
	}
	items, err := s.svc.OnThisDay(r.Context(), day, limit)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handlePlaces 列出用过的地点。
func (s *Server) handlePlaces(w http.ResponseWriter, r *http.Request) {
	items, err := s.svc.Places(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleNodesAtPlace 同一地点下的节点（各带一张封面）。
func (s *Server) handleNodesAtPlace(w http.ResponseWriter, r *http.Request) {
	place, err := url.PathUnescape(chi.URLParam(r, "place"))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "place 参数无法解析")
		return
	}
	items, err := s.svc.NodesAtPlace(r.Context(), place, 50)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"place": place, "items": items})
}

// handleOriginal 下载原图（未经重编码的那一份）。仅管理员：
// /img 只服务 var/ 变体，orig/ 从不对外，导出走这里。
func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	meta, err := s.svc.OpenOriginal(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	defer meta.Reader.Close()
	w.Header().Set("Content-Type", meta.ContentType)
	if meta.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	}
	w.Header().Set("ETag", `"`+meta.SHA256+`"`)
	// 文件名同时给 ASCII 兜底与 UTF-8 版本：中文图注在老客户端上不至于乱码
	w.Header().Set("Content-Disposition",
		`attachment; filename="photo"; filename*=UTF-8''`+url.PathEscape(meta.Filename))
	if _, err := io.Copy(w, meta.Reader); err != nil {
		s.log.Warn("原图下载中断", "err", err)
	}
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.GetStats(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Health(r.Context()); err != nil {
		s.log.Error("healthz", "err", err)
		writeError(w, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", "unhealthy")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ===== 写 handler =====

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // JSON 请求体 1MB 上限
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "请求体不是有效 JSON")
		return false
	}
	return true
}

func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	var in service.NodeInput
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.svc.CreateNode(r.Context(), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handlePatchNode(w http.ResponseWriter, r *http.Request) {
	var in service.NodeInput
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.svc.PatchNode(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteNode(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestoreNode(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.RestoreNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReorder(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PhotoIDs []string `json:"photo_ids"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	if err := s.svc.ReorderPhotos(r.Context(), chi.URLParam(r, "id"), in.PhotoIDs); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePatchPhoto(w http.ResponseWriter, r *http.Request) {
	var in service.PhotoPatch
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.svc.PatchPhoto(r.Context(), chi.URLParam(r, "id"), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeletePhoto(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeletePhoto(r.Context(), chi.URLParam(r, "id")); err != nil {
		writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestorePhoto(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.RestorePhoto(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReprocess(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.Reprocess(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) handleBatchPhotos(w http.ResponseWriter, r *http.Request) {
	var in service.BatchPhotoInput
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.svc.BatchPhotos(r.Context(), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ListTrash(r.Context())
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ===== 上传 handler =====

func (s *Server) handleUploadLocal(w http.ResponseWriter, r *http.Request) {
	maxBytes := s.svc.Config().UploadLimitMB << 20
	// +1MB 余量给 multipart 边界与表单字段
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+(1<<20))
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "文件超过大小限制")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "无效的 multipart 请求")
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "缺少 file 字段")
		return
	}
	defer file.Close()
	caption := r.FormValue("caption")
	out, err := s.svc.UploadLocal(r.Context(), chi.URLParam(r, "id"),
		header.Filename, caption, io.LimitReader(file, maxBytes+1))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) handlePresign(w http.ResponseWriter, r *http.Request) {
	var in service.PresignInput
	if !decodeBody(w, r, &in) {
		return
	}
	out, err := s.svc.PresignUpload(r.Context(), in)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	out, err := s.svc.ConfirmUpload(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, out)
}

// ===== /img（仅 local 模式）=====

// handleImg 直出本地变体文件：
//   - 只允许 var/ 前缀（原图 orig/ 永不直出）；
//   - key 白名单 + Clean 双校验防穿越；
//   - Cache-Control immutable + ETag(sha)；http.ServeContent 天然支持 Range；
//   - SG_PUBLIC_READ=false 时必须带 HMAC 签名 ?e=&s=。
func (s *Server) handleImg(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "*")
	if !strings.HasPrefix(key, "var/") || !blob.ValidKey(key) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	if !s.publicRead {
		q := r.URL.Query()
		if !service.VerifySignedKey(s.signSecret, key, q.Get("e"), q.Get("s")) {
			writeError(w, http.StatusForbidden, "UNAUTHORIZED", "签名无效或已过期")
			return
		}
	}
	f, err := s.localBlob.OpenFile(key)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "读取失败")
		return
	}
	// key 含内容 sha，天然是强 ETag
	w.Header().Set("ETag", `"`+etagFromKey(key)+`"`)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Type", "image/webp")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

// etagFromKey 提取 key 中的 sha 段作为 ETag。
func etagFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) >= 4 {
		return parts[3] + "-" + strings.TrimSuffix(parts[len(parts)-1], ".webp")
	}
	return key
}
