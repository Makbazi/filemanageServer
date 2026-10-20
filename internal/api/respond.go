package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/transfer"
)

// writeJSON 统一 JSON 响应（现走 gin，但保持 0.2.x 逐字节一致的
// Content-Type / 编码方式——json.Encoder 自带换行，勿改用 c.JSON）。
func writeJSON(c *gin.Context, code int, v any) {
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(code)
	if v == nil {
		return
	}
	_ = json.NewEncoder(c.Writer).Encode(v)
}

// statusClientClosedRequest 非标准码 499（nginx 惯例）：客户端在请求完成前
// 断开连接（上传大分片时被中间设备/对端 reset 是典型场景，0.2.1）。
const statusClientClosedRequest = 499

// isNetDisconnect 判定"客户端断开/连接被重置"类错误：这类错误不是服务端缺陷，
// 不得按 internal error 记日志（0.2.1 修复上传断连刷 5xx 日志）。
func isNetDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, http.ErrAbortHandler) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "connection aborted")
}

// writeErr 统一错误响应 {error: "..."}。
// 5xx（503 降级模式 errNoDB 为契约信息除外）对外只输出通用文案，
// 原始错误写服务端日志——防止数据库驱动内部错误串泄露主机/账号。
// 499（客户端断连，0.2.1）只记一行 info，绝不记 internal error。
// 0.2.7：internal error 走 logx.Error（默认等级 warn 下可见）并带链路 trace；
// 断连行沿用 stdlib log 桥接=INFO 形态（网络常态，刻意不进 warn 面）。
// 日志内容只有方法/路径/错误串——绝不落请求头与请求体（敏感纪律）。
func writeErr(c *gin.Context, code int, err error) {
	msg := err.Error()
	// 0.2.11：路径经 sanitizeLogTarget 脱敏（/s/<token> 与 st/口令参数值不落日志）
	target := sanitizeLogTarget(c.Request.URL)
	switch {
	case code == statusClientClosedRequest:
		log.Printf("client disconnected: %s %s: %v", c.Request.Method, target, err)
		msg = "client closed request"
	case code >= http.StatusInternalServerError && code != http.StatusServiceUnavailable:
		logx.ErrorT(traceOf(c), "internal error: %s %s: %v", c.Request.Method, target, err)
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
	case isNetDisconnect(err):
		return statusClientClosedRequest // 客户端断连（0.2.1）：非服务端缺陷
	default:
		return http.StatusInternalServerError
	}
}
