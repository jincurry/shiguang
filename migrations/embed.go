// Package migrations 将 goose SQL 迁移文件 embed 进二进制。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
