package importer

import (
	"bytes"
	"io"
)

// newHeadReader 把已读入的文件头包成 io.Reader 供 EXIF 解析使用。
func newHeadReader(head []byte) io.Reader { return bytes.NewReader(head) }
