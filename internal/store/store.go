// Package store 封装 SQLite 访问：单写连接 + 多读连接，全部 SQL 集中在此。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

// TimeFormat 是库内统一的时间存储格式：UTC、定宽毫秒，保证字符串比较即时间比较。
const TimeFormat = "2006-01-02T15:04:05.000Z"

// ErrNotFound 表示行不存在（或已软删且调用方要求存活行）。
var ErrNotFound = errors.New("store: not found")

// Now 返回当前 UTC 时间的存储格式字符串。
func Now() string { return time.Now().UTC().Format(TimeFormat) }

// FormatTime 将时间转为存储格式。
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime 解析存储格式时间。
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

// Store 持有读写两个连接池。SQLite 同一时刻只允许一个写者：
// writer 上限 1 条连接天然串行化写入；reader 用 8 条连接并发读（WAL 下读写不互斥）。
type Store struct {
	writer *sql.DB
	reader *sql.DB
}

// Open 打开数据库、应用 PRAGMA 并执行 goose 迁移。
// 注意：modernc.org/sqlite 的 DSN 用 _pragma=key(value) 传参（与 mattn 驱动的
// _journal/_busy_timeout 形式不同），这里统一在 DSN 后追加所需 PRAGMA，
// 调用方只需给出 file: 路径部分。
func Open(dsn string, migrations fs.FS) (*Store, error) {
	full := dsn
	sep := "?"
	for _, p := range []string{
		"_pragma=journal_mode(WAL)",
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(1)",
		"_pragma=synchronous(NORMAL)",
	} {
		if containsRune(full, '?') {
			sep = "&"
		}
		full += sep + p
	}

	writer, err := sql.Open("sqlite", full)
	if err != nil {
		return nil, fmt.Errorf("store: open writer: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetConnMaxIdleTime(0)

	reader, err := sql.Open("sqlite", full)
	if err != nil {
		writer.Close()
		return nil, fmt.Errorf("store: open reader: %w", err)
	}
	reader.SetMaxOpenConns(8)

	if err := writer.Ping(); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}

	goose.SetBaseFS(migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("sqlite3"); err != nil {
		return nil, fmt.Errorf("store: goose dialect: %w", err)
	}
	if err := goose.Up(writer, "."); err != nil {
		writer.Close()
		reader.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Store{writer: writer, reader: reader}, nil
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

// Close 关闭两个连接池。
func (s *Store) Close() error {
	err1 := s.writer.Close()
	err2 := s.reader.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

// Ping 探测数据库连通性（healthz 用）。
func (s *Store) Ping(ctx context.Context) error {
	return s.reader.PingContext(ctx)
}

// withTx 在写连接上执行事务。
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin tx: %w", err)
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}
