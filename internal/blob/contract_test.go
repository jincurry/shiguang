package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// runContract 对任意 Store 实现跑同一套契约测试。
func runContract(t *testing.T, s Store) {
	ctx := context.Background()
	key := "staging/contract-test.bin"
	data := "hello, 拾光集"

	// Put + Stat + Open
	if err := s.Put(ctx, key, strings.NewReader(data), int64(len(data)), "application/octet-stream"); err != nil {
		t.Fatalf("put: %v", err)
	}
	size, err := s.Stat(ctx, key)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("stat: size=%d err=%v", size, err)
	}
	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != data {
		t.Fatalf("open content mismatch: %q", got)
	}

	// Rename
	dst := "orig/ab/cd/abcdef0123456789.jpg"
	if err := s.Rename(ctx, key, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := s.Stat(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename src still exists: %v", err)
	}
	if _, err := s.Stat(ctx, dst); err != nil {
		t.Errorf("rename dst missing: %v", err)
	}

	// List
	var keys []string
	if err := s.List(ctx, "orig/", func(k string) error { keys = append(keys, k); return nil }); err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, k := range keys {
		if k == dst {
			found = true
		}
	}
	if !found {
		t.Errorf("list missing %s, got %v", dst, keys)
	}

	// Delete + 幂等
	if err := s.Delete(ctx, dst); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Stat(ctx, dst); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted object still exists: %v", err)
	}
	if err := s.Delete(ctx, dst); err != nil {
		t.Errorf("delete twice should be idempotent: %v", err)
	}

	// Open/Stat/Rename 不存在的对象
	if _, err := s.Open(ctx, "staging/nope.bin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("open missing: %v", err)
	}
	if err := s.Rename(ctx, "staging/nope.bin", "staging/nope2.bin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing: %v", err)
	}

	// 防穿越：非法 key 全部拒绝
	for _, bad := range []string{
		"../etc/passwd", "orig/../../x", "orig/a/../b", "/orig/a", "secret/x",
		"orig/A/UPPER", "orig/a//b", "orig/./a", "", "orig/a\x00b",
	} {
		if err := s.Put(ctx, bad, strings.NewReader("x"), 1, ""); !errors.Is(err, ErrBadKey) {
			t.Errorf("put bad key %q: got %v, want ErrBadKey", bad, err)
		}
		if _, err := s.Open(ctx, bad); !errors.Is(err, ErrBadKey) {
			t.Errorf("open bad key %q: got %v, want ErrBadKey", bad, err)
		}
	}
}

func TestFakeContract(t *testing.T) {
	runContract(t, NewFake())
}

func TestLocalContract(t *testing.T) {
	l, err := NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runContract(t, l)
}

// TestLocalAtomicVisibility 写入过程中不应出现半截文件（临时文件以 .tmp- 开头且 List 跳过）。
func TestLocalAtomicVisibility(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := l.Put(ctx, "var/aa/bb/x/thumb.webp", strings.NewReader("abc"), 3, "image/webp"); err != nil {
		t.Fatal(err)
	}
	// 目录里不应残留 .tmp-*
	entries, _ := os.ReadDir(dir + "/var/aa/bb/x")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// TestS3Contract 检测到 SG_TEST_S3_ENDPOINT 时连 MinIO 执行，否则 skip。
func TestS3Contract(t *testing.T) {
	endpoint := os.Getenv("SG_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SG_TEST_S3_ENDPOINT not set; skipping s3 contract test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := NewS3(ctx, S3Config{
		Endpoint:  endpoint,
		Bucket:    envOr("SG_TEST_S3_BUCKET", "shiguang-test"),
		Region:    "us-east-1",
		AccessKey: envOr("SG_TEST_S3_AK", "minioadmin"),
		SecretKey: envOr("SG_TEST_S3_SK", "minioadmin"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runContract(t, s)

	// Presign URL 应可生成
	if _, err := s.PresignPut(ctx, "staging/presign-test.jpg", "image/jpeg", 100, time.Minute); err != nil {
		t.Errorf("presign put: %v", err)
	}
	if _, err := s.PresignGet(ctx, "staging/presign-test.jpg", time.Minute); err != nil {
		t.Errorf("presign get: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
