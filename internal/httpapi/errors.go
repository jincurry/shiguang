package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"shiguang/internal/service"
)

// apiError 是统一错误响应体 {"code":"…","message":"…"}。
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// 秒传命中时附已存在照片
	Photo *service.PhotoDTO `json:"photo,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{Code: code, Message: msg})
}

// writeServiceError 将 service 语义错误映射为 HTTP 状态码与错误码。
func writeServiceError(w http.ResponseWriter, err error) {
	var ve *service.ValidationError
	var de *service.DuplicateError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", ve.Msg)
	case errors.As(err, &de):
		writeJSON(w, http.StatusConflict, apiError{
			Code: "CONFLICT_DUPLICATE", Message: "已存在相同照片", Photo: de.Existing})
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
	case errors.Is(err, service.ErrUnsupportedMedia):
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA", "仅支持 jpg / png / webp")
	case errors.Is(err, service.ErrPayloadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "文件超过大小限制")
	case errors.Is(err, service.ErrSessionExpired):
		writeError(w, http.StatusGone, "UPLOAD_SESSION_EXPIRED", "上传会话已过期，请重新上传")
	case errors.Is(err, service.ErrStorage):
		writeError(w, http.StatusServiceUnavailable, "STORAGE_UNAVAILABLE", "存储暂不可用，请稍后重试")
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL", "服务器内部错误")
	}
}
