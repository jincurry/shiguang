// Package web 通过 go:embed 将前端两页打入二进制。
package web

import _ "embed"

//go:embed index.html
var IndexHTML []byte

//go:embed admin.html
var AdminHTML []byte
