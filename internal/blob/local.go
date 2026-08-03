package blob

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// keyRe 是 key 白名单：只允许三个已知前缀与安全字符集，配合 Clean 双校验防穿越。
var keyRe = regexp.MustCompile(`^(orig|var|staging)/[a-z0-9/._-]+$`)

// ValidKey 校验 key 是否符合白名单约定（供 /img handler 复用）。
func ValidKey(key string) bool {
	if !keyRe.MatchString(key) {
		return false
	}
	// 第二重校验：Clean 后不得逃出前缀（拦截 ".." 与重复斜杠等）。
	clean := path.Clean(key)
	if clean != key {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// Local 是本地文件系统驱动。写入 = 同目录临时文件 → io.Copy → fsync → os.Rename，
// 保证对象要么完整可见、要么不存在。
type Local struct {
	root string
}

// NewLocal 创建 local 驱动，root 目录不存在时自动创建。
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("blob local: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("blob local: create root: %w", err)
	}
	return &Local{root: abs}, nil
}

// path 将 key 映射为磁盘路径，含白名单 + 前缀双校验。
func (l *Local) path(key string) (string, error) {
	if !ValidKey(key) {
		return "", ErrBadKey
	}
	p := filepath.Join(l.root, filepath.FromSlash(key))
	// 防御性前缀复查：Join+Clean 之后仍必须落在 root 内。
	if !strings.HasPrefix(p, l.root+string(filepath.Separator)) {
		return "", ErrBadKey
	}
	return p, nil
}

func (l *Local) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	dst, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("blob local put %s: mkdir: %w", key, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return fmt.Errorf("blob local put %s: tmp: %w", key, err)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return fmt.Errorf("blob local put %s: copy: %w", key, err)
	}
	if size >= 0 && n != size {
		tmp.Close()
		return fmt.Errorf("blob local put %s: size mismatch got %d want %d", key, n, size)
	}
	if err := tmp.Sync(); err != nil { // fsync：崩溃后不留半截文件
		tmp.Close()
		return fmt.Errorf("blob local put %s: fsync: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob local put %s: close: %w", key, err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("blob local put %s: rename: %w", key, err)
	}
	return nil
}

func (l *Local) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, fmt.Errorf("blob local open %s: %w", key, err)
	}
	return f, nil
}

// OpenFile 返回 *os.File（ReadSeeker），供 /img handler 走 http.ServeContent 支持 Range。
func (l *Local) OpenFile(key string) (*os.File, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return f, err
}

func (l *Local) Stat(ctx context.Context, key string) (int64, error) {
	p, err := l.path(key)
	if err != nil {
		return 0, err
	}
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return 0, fmt.Errorf("blob local stat %s: %w", key, err)
	}
	return fi.Size(), nil
}

// MTime 返回对象修改时间（孤儿 GC 用，仅 local 提供）。
func (l *Local) MTime(key string) (time.Time, error) {
	p, err := l.path(key)
	if err != nil {
		return time.Time{}, err
	}
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("blob local delete %s: %w", key, err)
	}
	return nil
}

func (l *Local) Rename(ctx context.Context, from, to string) error {
	src, err := l.path(from)
	if err != nil {
		return err
	}
	dst, err := l.path(to)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return fmt.Errorf("blob local rename: mkdir: %w", err)
	}
	if err := os.Rename(src, dst); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %s", ErrNotFound, from)
		}
		return fmt.Errorf("blob local rename %s -> %s: %w", from, to, err)
	}
	return nil
}

func (l *Local) List(ctx context.Context, prefix string, fn func(key string) error) error {
	// prefix 允许为白名单前缀本身（如 "var/"），逐文件回调 key。
	base := filepath.Join(l.root, filepath.FromSlash(strings.TrimSuffix(prefix, "/")))
	if !strings.HasPrefix(base, l.root) {
		return ErrBadKey
	}
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") { // 跳过写入中的临时文件
			return nil
		}
		rel, err := filepath.Rel(l.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fn(key)
	})
	if err != nil {
		return fmt.Errorf("blob local list %s: %w", prefix, err)
	}
	return nil
}

func (l *Local) PublicURL(key string) (string, bool) {
	return "/img/" + key, true
}

func (l *Local) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	return "", ErrNotSupported
}

func (l *Local) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", ErrNotSupported
}
