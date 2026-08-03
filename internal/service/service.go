// Package service 是业务编排层：DB 与 blob 的一致性责任集中在此。
// 依赖方向：httpapi → service → (store | blob | imgproc)。
package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"shiguang/internal/blob"
	"shiguang/internal/imgproc"
	"shiguang/internal/store"
)

// Config 是 service 运行参数（由 main 从环境变量装配）。
type Config struct {
	BlobDriver    string // local | s3
	PublicRead    bool
	SignSecret    string // PublicRead=false 时用于 local 变体 URL 的 HMAC 签名
	UploadLimitMB int64
	PixelLimitMP  int64
	TrashTTLDays  int
	PresignTTL    time.Duration // s3 直传会话与签名有效期
	SignedGetTTL  time.Duration // 读侧签名 URL 有效期
}

// Service 聚合 store/blob/worker 池并实现全部业务用例。
type Service struct {
	cfg  Config
	st   *store.Store
	bl   blob.Store
	log  *slog.Logger
	pool *imgproc.Pool

	statsMu    sync.Mutex
	statsCache *StatsDTO
	statsAt    time.Time
}

// New 创建 Service；worker 池随后用 AttachPool 注入（池的 handler 需要引用 Service）。
func New(cfg Config, st *store.Store, bl blob.Store, log *slog.Logger) *Service {
	if cfg.PresignTTL == 0 {
		cfg.PresignTTL = 30 * time.Minute
	}
	if cfg.SignedGetTTL == 0 {
		cfg.SignedGetTTL = 10 * time.Minute
	}
	return &Service{cfg: cfg, st: st, bl: bl, log: log}
}

// AttachPool 注入 worker 池。
func (s *Service) AttachPool(p *imgproc.Pool) { s.pool = p }

// Config 返回运行配置（httpapi 读取限额等）。
func (s *Service) Config() Config { return s.cfg }

// Store 返回底层 store（jobs 与测试用）。
func (s *Service) Store() *store.Store { return s.st }

// Blob 返回底层 blob（httpapi /img 与 healthz 用）。
func (s *Service) Blob() blob.Store { return s.bl }

// NewULID 生成一个 ULID 字符串（小写十六进制不适用，ULID 本身是 Crockford base32）。
func NewULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulid.DefaultEntropy()).String()
}

// ===== 变体 URL 生成 =====

// SignKey 计算 local 变体 URL 的 HMAC 签名：hex(hmac_sha256(secret, key+"|"+e))。
func SignKey(secret, key string, expires int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s|%d", key, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignedKey 校验签名与有效期（httpapi /img 用）。
func VerifySignedKey(secret, key, eStr, sig string) bool {
	e, err := strconv.ParseInt(eStr, 10, 64)
	if err != nil || time.Now().Unix() > e {
		return false
	}
	want := SignKey(secret, key, e)
	return subtle.ConstantTimeCompare([]byte(want), []byte(sig)) == 1
}

// variantURL 为某变体生成可访问 URL。
//   - local + 公开读：/img/<key>
//   - local + 私有读：/img/<key>?e=<unix>&s=<hmac>
//   - s3 + CDN：CDN URL；s3 无 CDN：PresignGet(10min)
func (s *Service) variantURL(ctx context.Context, sha, name string) (string, error) {
	key := blob.VariantKey(sha, name)
	if s.cfg.BlobDriver == "s3" {
		if u, ok := s.bl.PublicURL(key); ok && s.cfg.PublicRead {
			return u, nil
		}
		return s.bl.PresignGet(ctx, key, s.cfg.SignedGetTTL)
	}
	u, _ := s.bl.PublicURL(key)
	if !s.cfg.PublicRead {
		e := time.Now().Add(s.cfg.SignedGetTTL).Unix()
		u = fmt.Sprintf("%s?e=%d&s=%s", u, e, SignKey(s.cfg.SignSecret, key, e))
	}
	return u, nil
}

// ===== DTO =====

// PhotoDTO 是照片的 API 表示（字段名与提示词契约一致）。
type PhotoDTO struct {
	ID         string            `json:"id"`
	Caption    string            `json:"caption"`
	Status     string            `json:"status"`
	FailReason *string           `json:"fail_reason"`
	BlurHash   *string           `json:"blurhash"`
	Dominant   *string           `json:"dominant"`
	Width      *int64            `json:"width"`
	Height     *int64            `json:"height"`
	TakenAt    *string           `json:"taken_at"`
	Ord        int64             `json:"ord"`
	Variants   map[string]string `json:"variants"`
}

// NodeDTO 是节点的 API 表示。
type NodeDTO struct {
	ID          string      `json:"id"`
	Date        string      `json:"date"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	PhotoCount  int         `json:"photo_count"`
	Photos      []*PhotoDTO `json:"photos"`
}

// photoDTO 将 store 行转为 API 表示；ready 状态生成变体 URL。
func (s *Service) photoDTO(ctx context.Context, p *store.Photo) *PhotoDTO {
	d := &PhotoDTO{
		ID:         p.ID,
		Caption:    p.Caption,
		Status:     p.Status,
		FailReason: p.FailReason,
		BlurHash:   p.BlurHash,
		Dominant:   p.Dominant,
		Width:      p.Width,
		Height:     p.Height,
		TakenAt:    p.TakenAt,
		Ord:        p.Ord,
		Variants:   map[string]string{},
	}
	if p.Status == "ready" && p.SHA256 != nil {
		for _, name := range imgproc.VariantNames {
			u, err := s.variantURL(ctx, *p.SHA256, name)
			if err != nil {
				s.log.Error("variant url", "photo", p.ID, "variant", name, "err", err)
				continue
			}
			d.Variants[name] = u
		}
	}
	return d
}

func (s *Service) nodeDTO(ctx context.Context, n *store.Node, photos []*store.Photo) *NodeDTO {
	d := &NodeDTO{
		ID:          n.ID,
		Date:        n.Date,
		Title:       n.Title,
		Description: n.Description,
		Photos:      make([]*PhotoDTO, 0, len(photos)),
	}
	for _, p := range photos {
		d.Photos = append(d.Photos, s.photoDTO(ctx, p))
	}
	d.PhotoCount = len(d.Photos)
	return d
}

// ===== healthz =====

// Health 探测 DB 与 blob 写能力。
func (s *Service) Health(ctx context.Context) error {
	if err := s.st.Ping(ctx); err != nil {
		return fmt.Errorf("db: %w", err)
	}
	// blob 写探测：写入并删除一个 staging 小对象。
	var nonce [8]byte
	rand.Read(nonce[:])
	key := "staging/healthz-" + hex.EncodeToString(nonce[:]) + ".bin"
	if err := s.bl.Put(ctx, key, bytesReader([]byte("ok")), 2, "application/octet-stream"); err != nil {
		return fmt.Errorf("blob: %w", err)
	}
	return s.bl.Delete(ctx, key)
}
