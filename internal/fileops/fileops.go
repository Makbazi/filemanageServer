// Package fileops 提供文件系统操作原语：路径安全、类型判定、复制/移动、
// 哈希、回收站与版本库的路径布局。上层（api）只组合这些原语。
package fileops

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// CleanRel 把用户输入的相对路径规范为 root 内 slash 路径（""=根）。
func CleanRel(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "." {
		return ""
	}
	return p
}

// SafeRel 规范化并校验相对路径，拒绝任何 ".." 段（chroot 式防逃逸）。
func SafeRel(rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(rel, "/") {
		rel = rel[1:]
	}
	rel = path.Clean("/" + rel)
	rel = strings.TrimPrefix(rel, "/")
	if rel == "." {
		rel = ""
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", errors.New("path traversal denied")
		}
	}
	return rel, nil
}

// JoinRoot 把相对路径拼接到根目录并确保结果不逃逸根目录。
func JoinRoot(rootPath, rel string) (string, error) {
	rel, err := SafeRel(rel)
	if err != nil {
		return "", err
	}
	abs := filepath.Join(rootPath, filepath.FromSlash(rel))
	rootAbs := filepath.Clean(rootPath)
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return "", errors.New("path escapes root")
	}
	return abs, nil
}

// EnsureNoEscape 防御 root 内预存符号链接被跟随（0.5.1 安全加固）：
// 对目标路径（或其最近的已存在祖先）做 EvalSymlinks，要求解链后的真实路径
// 仍位于授权基目录之内；不存在的路径（写入/创建场景）校验其已存在祖先。
// baseDir 自身不可解析时返回 nil（让后续真实 IO 报出准确错误）。
func EnsureNoEscape(baseDir, target string) error {
	realBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return nil
	}
	p := target
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			if real != realBase && !strings.HasPrefix(real, realBase+string(filepath.Separator)) {
				return errors.New("path escapes root via symlink")
			}
			return nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// RelOf 把绝对路径转回相对根目录的 slash 路径。
func RelOf(rootPath, abs string) string {
	rel, err := filepath.Rel(rootPath, abs)
	if err != nil {
		return filepath.Base(abs)
	}
	return filepath.ToSlash(rel)
}

// KindOf 按扩展名粗分类，供前端图标、分类显示与 AI 描述使用。
func KindOf(p string, isDir bool) string {
	if isDir {
		return "dir"
	}
	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".svg", ".ico", ".heic", ".tiff":
		return "image"
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".flv", ".wmv":
		return "video"
	case ".mp3", ".wav", ".flac", ".m4a", ".aac", ".ogg", ".opus":
		return "audio"
	case ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".txt", ".md", ".rtf", ".epub":
		return "document"
	case ".zip", ".tar", ".gz", ".bz2", ".7z", ".rar", ".xz":
		return "archive"
	case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".java", ".c", ".cpp", ".h", ".css", ".html", ".json", ".yaml", ".yml", ".toml", ".sh", ".sql":
		return "code"
	}
	return "other"
}

// IsSameOrChild 判断 dest 是否为 src 本身或其子路径。
func IsSameOrChild(src, dest string) bool {
	return src == dest || strings.HasPrefix(dest, src+string(filepath.Separator))
}

// MovePath 优先 rename；跨设备（EXDEV）时退化为 copy+remove。
// 需求7：rename 会原样保留源文件的 mode——move/rename/回收站还原等全部
// 经由此函数的路径统一在执行位剥离之后收尾（仅常规文件；目录保留 x 位供穿越）。
func MovePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		stripExec(dst)
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := CopyDir(src, dst); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	if err := CopyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// stripExec 剥离常规文件的执行位（0.1.0 需求37：用户文件在服务端磁盘上
// 永不可执行）。best-effort：失败只影响 mode，绝不影响操作结果。
func stripExec(p string) {
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return
	}
	_ = os.Chmod(p, info.Mode().Perm()&^0o111)
}

// SweepExec 启动清扫（0.1.0 需求37）：递归遍历给定目录，为其中所有常规文件
// 去除执行位（已是干净的文件经 DirEntry.Type 短路，零 syscall 开销之外的
// 成本仅为 stat）。返回被修改的文件数；单个条目失败跳过并继续。
// 由 main 在服务启动后异步执行一次（大树不拖慢开机）。
func SweepExec(dirs []string) int {
	stripped := 0
	for _, d := range dirs {
		_ = filepath.WalkDir(d, func(p string, ent fs.DirEntry, err error) error {
			if err != nil {
				return nil // 无权限/竞态删除等：跳过该子树，不中断
			}
			if ent.IsDir() {
				return nil
			}
			info, ierr := ent.Info()
			if ierr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
				return nil
			}
			if cerr := os.Chmod(p, info.Mode().Perm()&^0o111); cerr == nil {
				stripped++
			}
			return nil
		})
	}
	return stripped
}

// CopyFile 复制单个文件。O_TRUNC 覆盖已有 dst 时复用其 inode 并保留旧 mode，
// 故收尾剥离执行位（需求37，同 MovePath）。
func CopyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	stripExec(dst)
	return nil
}

// CopyDir 递归复制目录。
func CopyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return CopyFile(p, target)
	})
}

// Dedup 同名冲突时自动追加 (n) 后缀，返回可用路径。
func Dedup(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
		if _, err := os.Stat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand
		}
	}
}

// SHA256File 计算文件内容哈希；文件不存在返回空串（不视为错误）。
func SHA256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RandomHex 生成 n 字节随机 hex 串（trashID / 上传会话 ID / 分享 token）。
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// TrashPath 回收站实体布局：trashDir/u<userID>/<trashID>。
func TrashPath(trashDir string, userID int64, trashID string) string {
	return filepath.Join(trashDir, fmt.Sprintf("u%d", userID), trashID)
}

// ErrTooLarge 表示写入流超过配置的硬性大小上限（上传限额，0.3.1）。
var ErrTooLarge = errors.New("content too large")

// WriteFileAtomic 先写临时文件再 rename，避免半成品（并发保护的一部分）。
func WriteFileAtomic(dst string, r io.Reader) (int64, error) {
	return WriteFileAtomicLimit(dst, r, 0)
}

// WriteFileAtomicLimit 是 WriteFileAtomic 加字节硬上限（max<=0 表示不限）。
// 上限在 rename 之前校验：超限则清理临时文件并返回 ErrTooLarge，绝不落地下半成品。
func WriteFileAtomicLimit(dst string, r io.Reader, max int64) (n int64, err error) {
	tmp := dst + ".fmtmp-" + RandomHex(4)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	if max > 0 {
		n, err = io.Copy(out, io.LimitReader(r, max+1))
	} else {
		n, err = io.Copy(out, r)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	if max > 0 && n > max {
		_ = os.Remove(tmp)
		return n, ErrTooLarge
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, nil
}
