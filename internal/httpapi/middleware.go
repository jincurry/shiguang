package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ctxKey string

const reqIDKey ctxKey = "reqid"

// requestID 为每个请求生成 ID 并写响应头。
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [8]byte
		rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reqIDKey, id)))
	})
}

// recoverer 捕获 panic，记录堆栈并返回 500。
func recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					log.Error("panic", "err", v, "stack", string(debug.Stack()),
						"path", r.URL.Path)
					writeError(w, http.StatusInternalServerError, "INTERNAL", "服务器内部错误")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// statusWriter 记录响应状态码供访问日志使用。
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// accessLog 输出结构化访问日志。
func accessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			id, _ := r.Context().Value(reqIDKey).(string)
			log.Info("http",
				"method", r.Method, "path", r.URL.Path, "status", sw.status,
				"dur_ms", time.Since(start).Milliseconds(), "reqid", id)
		})
	}
}

// tokenBucket 是简单令牌桶（互斥锁实现，单机足够）。
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	max    float64
	rate   float64 // 每秒补充
	last   time.Time
}

func newBucket(rate, burst float64) *tokenBucket {
	return &tokenBucket{tokens: burst, max: burst, rate: rate, last: time.Now()}
}

// take 尝试取一个令牌；失败时返回建议等待秒数。
func (b *tokenBucket) take() (bool, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := int((1-b.tokens)/b.rate) + 1
	return false, wait
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// rateLimit 中间件：超限返回 429 + Retry-After。
func rateLimit(b *tokenBucket) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, wait := b.take()
			if !ok {
				w.Header().Set("Retry-After", strconv.Itoa(wait))
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁，请稍后重试")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken 提取 Authorization: Bearer 值。
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// tokenValid 常数时间比较 token。
func tokenValid(token, want string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}

// requireAuth 写接口鉴权：必须携带正确 Bearer token。
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !tokenValid(bearerToken(r), s.adminToken) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "口令无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// readAuth 读接口鉴权：
//   - SG_PUBLIC_READ=false 时必须携带正确 token；
//   - 公开读时无 token 放行，但"带了 token 就必须有效"——管理后台靠这一点
//     用 GET /stats 验证口令（错误口令在公开模式下也会得到 401）。
func (s *Server) readAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := bearerToken(r)
		if t == "" && s.publicRead {
			next.ServeHTTP(w, r)
			return
		}
		if !tokenValid(t, s.adminToken) {
			writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "口令无效")
			return
		}
		next.ServeHTTP(w, r)
	})
}
