// Package blob 提供内容寻址对象存储抽象，local / s3 / fake 三个驱动实现同一接口。
//
// Key 约定（两驱动一致）：
//
//	orig/ab/cd/<sha256>.<ext>       原图，永久保留
//	var/ab/cd/<sha256>/thumb.webp   变体（thumb 336w / md 1200w / lg 2048w）
//	staging/<ulid>.<ext>            s3 暂存，confirm 校验后 Rename 转正
package blob

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

var (
	// ErrNotSupported 表示驱动不支持该操作（如 local 的 PresignPut）。
	ErrNotSupported = errors.New("blob: operation not supported")
	// ErrBadKey 表示 key 不符合白名单约定（防路径穿越）。
	ErrBadKey = errors.New("blob: invalid key")
	// ErrNotFound 表示对象不存在。
	ErrNotFound = errors.New("blob: not found")
)

// Store 是对象存储统一接口。所有实现必须保证 Put 之后对象立即可见（原子写入）。
type Store interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Stat(ctx context.Context, key string) (size int64, err error)
	Delete(ctx context.Context, key string) error
	Rename(ctx context.Context, from, to string) error
	List(ctx context.Context, prefix string, fn func(key string) error) error
	// PublicURL 返回对象可直接访问的 URL；local 返回 ("/img/"+key,true)，
	// s3 有 CDN base 时返回 CDN URL，否则 (_, false)。
	PublicURL(key string) (url string, ok bool)
	// PresignPut 签发直传 URL 并锁定 Content-Length 与 Content-Type；
	// local 驱动返回 ErrNotSupported。
	PresignPut(ctx context.Context, key, contentType string, size int64,
		ttl time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// OrigKey 返回原图的内容寻址 key，sha 为 64 位十六进制 sha256。
func OrigKey(sha, ext string) string {
	return "orig/" + sha[0:2] + "/" + sha[2:4] + "/" + sha + "." + ext
}

// VariantKey 返回变体 key，name 为 thumb/md/lg。
func VariantKey(sha, name string) string {
	return "var/" + sha[0:2] + "/" + sha[2:4] + "/" + sha + "/" + name + ".webp"
}

// VariantPrefix 返回某张原图全部变体的公共前缀（用于删除）。
func VariantPrefix(sha string) string {
	return "var/" + sha[0:2] + "/" + sha[2:4] + "/" + sha + "/"
}

// StagingKey 返回 s3 直传暂存 key。ULID 转小写以满足 key 白名单字符集
// （ULID 是 Crockford base32，大小写不敏感）。
func StagingKey(ulid, ext string) string {
	return "staging/" + strings.ToLower(ulid) + "." + ext
}
