package service

import (
	"errors"
	"fmt"
)

// 语义错误：httpapi 层据此映射状态码与错误码，service 内部不感知 HTTP。
var (
	ErrNotFound         = errors.New("not found")
	ErrUnsupportedMedia = errors.New("unsupported media type")
	ErrSessionExpired   = errors.New("upload session expired")
	ErrStorage          = errors.New("storage unavailable")
	ErrPayloadTooLarge  = errors.New("payload too large")
)

// ValidationError 携带面向用户的校验失败信息（422）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Validationf 构造校验错误。
func Validationf(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// DuplicateError 表示同节点秒传命中（409），携带已存在的照片。
type DuplicateError struct{ Existing *PhotoDTO }

func (e *DuplicateError) Error() string { return "duplicate photo in node" }
