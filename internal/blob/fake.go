package blob

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Fake 是内存驱动，仅用于测试。行为与 local 对齐（含 key 白名单校验）。
type Fake struct {
	mu      sync.RWMutex
	objects map[string][]byte
	// FailPut 非 nil 时 Put 返回该错误，用于注入存储故障。
	FailPut error
}

// NewFake 创建内存驱动。
func NewFake() *Fake {
	return &Fake{objects: map[string][]byte{}}
}

func (f *Fake) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if !ValidKey(key) {
		return ErrBadKey
	}
	if f.FailPut != nil {
		return f.FailPut
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if size >= 0 && int64(len(b)) != size {
		return fmt.Errorf("blob fake put %s: size mismatch", key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = b
	return nil
}

func (f *Fake) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	if !ValidKey(key) {
		return nil, ErrBadKey
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, ok := f.objects[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *Fake) Stat(ctx context.Context, key string) (int64, error) {
	if !ValidKey(key) {
		return 0, ErrBadKey
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	b, ok := f.objects[key]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return int64(len(b)), nil
}

func (f *Fake) Delete(ctx context.Context, key string) error {
	if !ValidKey(key) {
		return ErrBadKey
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)
	return nil
}

func (f *Fake) Rename(ctx context.Context, from, to string) error {
	if !ValidKey(from) || !ValidKey(to) {
		return ErrBadKey
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[from]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, from)
	}
	f.objects[to] = b
	delete(f.objects, from)
	return nil
}

func (f *Fake) List(ctx context.Context, prefix string, fn func(key string) error) error {
	f.mu.RLock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	f.mu.RUnlock()
	sort.Strings(keys)
	for _, k := range keys {
		if err := fn(k); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fake) PublicURL(key string) (string, bool) {
	return "/img/" + key, true
}

func (f *Fake) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (string, error) {
	return "", ErrNotSupported
}

func (f *Fake) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "", ErrNotSupported
}

// Exists 测试辅助：对象是否存在。
func (f *Fake) Exists(key string) bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	_, ok := f.objects[key]
	return ok
}

// Keys 测试辅助：返回全部 key。
func (f *Fake) Keys() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	keys := make([]string, 0, len(f.objects))
	for k := range f.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
