// Package logx 提供服务端统一日志（0.2.7 引入；0.2.11 六级化）：基于标准库
// log/slog 日志库，零新依赖（离线编译镜像纪律）。
//
// 形态（用户清单：最多 3 个日志文件、单文件 ≤10MB、链路追踪、等级可配、
// 敏感信息不落日志、默认等级 warn）：
//   - 落盘：<logDir>/filemanage-server.log 按大小轮转，改名链 .log.1/.log.2/…
//     （oldest-first，溢出删除），含当前文件在内最多 maxFiles 个（默认 3）、
//     每个 ≤ maxMB（默认 10）；写侧互斥，一行一条记录（slog 文本 Handler）。
//   - 镜像：同时写 stdout（io.MultiWriter）——前台/容器部署直接可看。
//   - 等级（0.2.11 六级，低→高）：**trace < debug < info < warn < error < fatal**，
//     配置 log.level 取 trace|debug|info|warn|error|fatal，默认 warn；空/未知取值
//     收敛为 warn。语义：trace=逐请求调试细节（认证解析/缩略图判据等，生产常闭）；
//     debug=调试级事件（登录失败原因、端点退化等，噪声可控）；info=生命周期与
//     安全事件（启动横幅、登录成功、账户/权限/分享变更——审计事实源仍在库
//     audit_events，日志为运维可读镜像）；warn=需要关注但可继续运行（迁移未完成
//     重试、撤权失败、清理故障、认证拒绝）；error=功能受损（5xx、写盘失败等）；
//     fatal=不可恢复——记录后立即 os.Exit(1)（启动 fail-fast 站点专用；默认 warn
//     下依然可见，修复了 stdlib log.Fatalf 经 INFO 桥接被默认等级吞没的盲区）。
//   - 桥接：Setup 把 stdlib log 的输出接进 slog（INFO 级、log.SetFlags(0) 剥离
//     自带时间戳），存量 log.Printf 站点无需改写即可落同一文件；默认 warn 下
//     这些桥接行不可见——刻意如此（生命周期/信息行降噪）；**必须默认可见的
//     站点请改用 logx.Warn/Fatal，勿再走 log.Fatalf**。
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
	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{
		Level: parseLevel(c.Level),
		// 六级等级名渲染（slog 原生只熟 debug/info/warn/error，越界会打成
		// "ERROR+4" 形态——统一替换成 TRACE/FATAL，日志文件观感一致）。
		ReplaceAttr: replaceLevelNames,
	})))
	// stdlib log 桥接：每行按 INFO 进 slog；时间戳/级别由 slog 统一添加
	log.SetFlags(0)
	log.SetOutput(&bridgeWriter{})
	return nil
}

// 0.2.11 六级：trace(-8) < debug(0) < info(4) < warn(8) < error(12) < fatal(16)。
// debug/info/warn/error 直接用 slog 原生常量（与任何按 Level() 比较的代码兼容）。
const (
	LevelTrace = slog.Level(-8)
	LevelFatal = slog.Level(16)
)

// replaceLevelNames 文本 Handler 的属性改写钩子：level 属性渲染自定义名。
func replaceLevelNames(_ []string, a slog.Attr) slog.Attr {
	if a.Key != slog.LevelKey {
		return a
	}
	lvl, ok := a.Value.Any().(slog.Level)
	if !ok {
		return a
	}
	switch {
	case lvl <= LevelTrace:
		a.Value = slog.StringValue("TRACE")
	case lvl >= LevelFatal:
		a.Value = slog.StringValue("FATAL")
	}
	return a
}

// parseLevel 解析等级字符串（大小写不敏感，六级）；空/未知一律 warn（默认等级）。
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace":
		return LevelTrace
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	case "fatal":
		return LevelFatal
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
// Trace（0.2.11 六级新增）：逐请求调试细节，生产建议保持默认等级关闭。
func Trace(format string, args ...any) { emit(LevelTrace, "", format, args...) }
func Debug(format string, args ...any) { emit(slog.LevelDebug, "", format, args...) }
func Info(format string, args ...any)  { emit(slog.LevelInfo, "", format, args...) }
func Warn(format string, args ...any)  { emit(slog.LevelWarn, "", format, args...) }
func Error(format string, args ...any) { emit(slog.LevelError, "", format, args...) }

// Fatal：不可恢复错误——记录后 os.Exit(1)。启动 fail-fast 站点专用（替代
// log.Fatalf——后者经 stdlib 桥接按 INFO 走、默认 warn 等级下会被吞掉、进程死得
// 无声无息）。落行使用 **fatal 等级（六级最高）**：任何配置等级（阈值 ≤ fatal）
// 下都必然可见。绝不用于可恢复路径。
func Fatal(format string, args ...any) {
	emit(LevelFatal, "", format, args...)
	_ = os.Stdout.Sync() // stdout/文件写侧同步落（rotateWriter 本就逐条 write）
	os.Exit(1)
}

// DebugT/InfoT/WarnT/ErrorT：链路追踪变体——trace 非空时行首带 "trace=<id> "，
// 与 loggerGin/recoverGin/writeErr 的请求面日志同格式，可按 id 串联一次请求。
// TraceT（0.2.11）：trace 级请求内细节。
func TraceT(trace, format string, args ...any) { emit(LevelTrace, trace, format, args...) }
func DebugT(trace, format string, args ...any)  { emit(slog.LevelDebug, trace, format, args...) }
func InfoT(trace, format string, args ...any)   { emit(slog.LevelInfo, trace, format, args...) }
func WarnT(trace, format string, args ...any)   { emit(slog.LevelWarn, trace, format, args...) }
func ErrorT(trace, format string, args ...any)  { emit(slog.LevelError, trace, format, args...) }

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
