// Package logx 提供服务端统一日志（0.2.7）：基于标准库 log/slog，零新依赖。
//
// 形态（用户清单：最多 3 个日志文件、单文件 ≤10MB、链路追踪、等级可配、
// 敏感信息不落日志、默认等级 warn）：
//   - 落盘：<logDir>/filemanage-server.log 按大小轮转，改名链 .log.1/.log.2/…
//     （oldest-first，溢出删除），含当前文件在内最多 maxFiles 个（默认 3）、
//     每个 ≤ maxMB（默认 10）；写侧互斥，一行一条记录（slog 文本 Handler）。
//   - 镜像：同时写 stdout（io.MultiWriter）——前台/容器部署直接可看。
//   - 等级：debug|info|warn|error，默认 warn；未知取值收敛为 warn。
//   - 桥接：Setup 把 stdlib log 的输出接进 slog（INFO 级、log.SetFlags(0) 剥离
//     自带时间戳），存量 log.Printf 站点无需改写即可落同一文件；默认 warn 下
//     这些桥接行不可见——刻意如此（生命周期/信息行降噪）。
//   - 纪律：本包与调用方一律不记录请求体、Authorization 头、口令、令牌等
//     敏感值；请求日志只带方法/路径/状态/耗时/来源与 trace id。
package logx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"filemanageServer/internal/config"
)

// baseName 日志基文件名（轮转后缀 .log.1/.log.2/… 由 rotateWriter 追加）。
const baseName = "filemanage-server.log"

// Setup 按配置装配全局 slog 与 stdlib log 桥接；日志目录不存在时创建。
// c.Dir 为空时落 <dataDir>/logs。任何一步失败返回错误（调用方 fail-fast）。
func Setup(c config.LogConf, dataDir string) error {
	dir := c.Dir
	if dir == "" {
		dir = filepath.Join(dataDir, "logs")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("logx: 创建日志目录失败: %w", err)
	}
	maxBytes := int64(c.MaxMB) << 20
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	maxFiles := c.MaxFiles
	if maxFiles < 1 {
		maxFiles = 3
	}
	rw := &rotateWriter{dir: dir, maxBytes: maxBytes, maxFiles: maxFiles}
	// stdout 在前、文件在后：文件写失败（磁盘满等）不影响控制台输出
	out := io.MultiWriter(os.Stdout, rw)
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: parseLevel(c.Level)})))
	// stdlib log 桥接：每行按 INFO 进 slog；时间戳/级别由 slog 统一添加
	log.SetFlags(0)
	log.SetOutput(&bridgeWriter{})
	return nil
}

// parseLevel 解析等级字符串（大小写不敏感）；空/未知一律 warn（默认等级）。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		return slog.LevelWarn
	}
}

// ---------- printf 风格 helpers ----------

func emit(lvl slog.Level, trace, format string, args ...any) {
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	if trace != "" {
		msg = "trace=" + trace + " " + msg
	}
	slog.Default().Log(context.Background(), lvl, msg)
}

// Debug/Info/Warn/Error：printf 风格；Warn/Error 在默认等级（warn）下可见。
func Debug(format string, args ...any) { emit(slog.LevelDebug, "", format, args...) }
func Info(format string, args ...any)  { emit(slog.LevelInfo, "", format, args...) }
func Warn(format string, args ...any)  { emit(slog.LevelWarn, "", format, args...) }
func Error(format string, args ...any) { emit(slog.LevelError, "", format, args...) }

// DebugT/InfoT/WarnT/ErrorT：链路追踪变体——trace 非空时行首带 "trace=<id> "，
// 与 loggerGin/recoverGin/writeErr 的请求面日志同格式，可按 id 串联一次请求。
func DebugT(trace, format string, args ...any) { emit(slog.LevelDebug, trace, format, args...) }
func InfoT(trace, format string, args ...any)  { emit(slog.LevelInfo, trace, format, args...) }
func WarnT(trace, format string, args ...any)  { emit(slog.LevelWarn, trace, format, args...) }
func ErrorT(trace, format string, args ...any) { emit(slog.LevelError, trace, format, args...) }

// ---------- 按大小轮转的文件写入器 ----------

// rotateWriter 互斥守护的轮转写入器：单次写入会越过上限时先轮转再写
// （写前判长，保证任一文件 ≤ maxBytes，除非单条记录本身超长——那最多一个
// 文件超限一次，继续写只会滚出，属可接受边界）。文件集合始终 ≤ maxFiles 个：
// <base>、<base>.1 … <base>.(maxFiles-1)。
type rotateWriter struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

func (w *rotateWriter) path(i int) string {
	base := filepath.Join(w.dir, baseName)
	if i <= 0 {
		return base
	}
	return fmt.Sprintf("%s.%d", base, i)
}

func (w *rotateWriter) open() error {
	f, err := os.OpenFile(w.path(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	size := int64(0)
	if st, serr := f.Stat(); serr == nil {
		size = st.Size() // 重启续写既有文件：累计到上限再轮转，绝不让单文件超 maxBytes
	}
	w.f, w.size = f, size
	return nil
}

// rotate 自旧而新依次改名（.log.(n-2)→.log.(n-1)… .log→.log.1），
// 最旧溢出删除，然后重开基文件。
func (w *rotateWriter) rotate() error {
	_ = w.f.Close()
	w.f = nil
	if w.maxFiles <= 1 {
		// 只保留当前文件：轮转即丢弃旧内容
		_ = os.Remove(w.path(0))
		return w.open()
	}
	_ = os.Remove(w.path(w.maxFiles - 1))
	for i := w.maxFiles - 2; i >= 1; i-- {
		if err := os.Rename(w.path(i), w.path(i+1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := os.Rename(w.path(0), w.path(1)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return w.open()
}

func (w *rotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.open(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// ---------- stdlib log 桥接 ----------

// bridgeWriter 把 stdlib log 的每次 Write 按行切分、逐行以 INFO 送进 slog，
// 使存量 log.Printf（含生命周期横幅）统一落到同一轮转文件与 stdout。
type bridgeWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (b *bridgeWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	for {
		i := bytes.IndexByte(b.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(b.buf[:i]), "\r")
		b.buf = b.buf[i+1:]
		if strings.TrimSpace(line) != "" {
			slog.Default().Info(line)
		}
	}
	if len(b.buf) > 64<<10 { // 防御：长期无换行的巨行直接丢弃缓冲，不涨内存
		b.buf = b.buf[:0]
	}
	return len(p), nil
}
