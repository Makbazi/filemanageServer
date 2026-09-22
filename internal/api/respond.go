package api

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/transfer"
)

// writeJSON 统一 JSON 响应（0.3.0 起走 gin，但保持 0.2.x 逐字节一致的
// Content-Type / 编码方式——json.Encoder 自带换行，勿改用 c.JSON）。
func writeJSON(c *gin.Context, code int, v any) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(code)
	if v == nil {
		return
	}
	_ = json.NewEncoder(c.Writer).Encode(v)
}

// writeErr 统一错误响应 {error: "..."}。
// 5xx（503 降级模式 errNoDB 为契约信息除外）对外只输出通用文案，
// 原始错误写服务端日志——防止数据库驱动内部错误串泄露主机/账号（0.3.1）。
func writeErr(c *gin.Context, code int, err error) {
	msg := err.Error()
	if code >= http.StatusInternalServerError && code != http.StatusServiceUnavailable {
		log.Printf("internal error: %s %s: %v", c.Request.Method, c.Request.URL.Path, err)
		msg = "internal server error"
	}
	writeJSON(c, code, map[string]string{"error": msg})
}

// decode 解析 JSON 请求体（限 1MB，防御超大 payload）。
// 保留手写 Decoder 而非 c.ShouldBindJSON：后者强制 content-type 校验，
// 会拒绝当前契约下仍可解析的无 header JSON 请求。
func decode[T any](c *gin.Context, v *T) error {
	body := io.LimitReader(c.Request.Body, 1<<20)
	return json.NewDecoder(body).Decode(v)
}

// statusOf 把内部错误映射为 HTTP 状态码。
func statusOf(err error) int {
	switch {
	case errors.Is(err, transfer.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, transfer.ErrHashMismatch):
		return http.StatusBadRequest
	case errors.Is(err, transfer.ErrIncomplete):
		return http.StatusBadRequest
	case errors.Is(err, transfer.ErrNotOwner):
		return http.StatusForbidden
	case errors.Is(err, transfer.ErrBadSession):
		return http.StatusBadRequest
	case errors.Is(err, fileops.ErrTooLarge):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrExist), errors.Is(err, store.ErrExists):
		return http.StatusConflict
	case errors.Is(err, errNoRootAccess), errors.Is(err, errReservedName):
		return http.StatusForbidden
	case strings.Contains(err.Error(), "denied"),
		strings.Contains(err.Error(), "escapes"),
		strings.Contains(err.Error(), "traversal"),
		strings.Contains(err.Error(), "permitted"),
		strings.Contains(err.Error(), "unknown root"):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}
