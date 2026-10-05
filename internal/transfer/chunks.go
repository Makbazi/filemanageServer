// Package transfer 实现分片上传/断点续传/并发写保护/限速（需求3、18）。
//
// 协议流程：
//  1. POST /upload/session：创建（或恢复）上传会话。同路径仅允许一个活动
//     会话——同一用户重复创建返回原会话与已传分片（断点续传）；不同用户
//     创建则报冲突（多客户端修改相同文件保护）。
//  2. PUT /upload/chunk?sessionId&index：逐分片上传，落盘到暂存目录并入库标记。
//  3. POST /upload/complete：校验分片齐全与内容哈希，持路径锁执行
//     BaseHash 乐观并发校验（目标文件在上传期间被他人改动则报冲突），
//     合并为临时文件后原子 rename 到目标路径。
//
// 会话元数据存于 internal/store、分片内容存于共享暂存目录，因此多实例
// （共享 sqlite + 共享存储目录）与重启后均可续传。
package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
)

// 管理器错误语义。
var (
	ErrConflict     = errors.New("upload conflict: file is being modified by another client")
	ErrHashMismatch = errors.New("content hash mismatch")
	ErrIncomplete   = errors.New("upload incomplete: missing chunks")
	ErrNotOwner     = errors.New("not the owner of this upload session")
	ErrBadSession   = errors.New("invalid upload session state")
)

// 分片大小约束。
const (
	MinChunkSize     int64 = 256 << 10 // 256KB
	MaxChunkSize     int64 = 32 << 20  // 32MB
	DefaultChunkSize int64 = 4 << 20   // 4MB
	MaxChunkSlack    int64 = 1 << 20   // 末分片外允许的单片冗余上限
)

// Manager 分片上传会话管理器。
type Manager struct {
	Store      store.Store
	StagingDir string
	Locks      *LockRegistry
}

// NewManager 创建管理器并确保暂存目录存在。
func NewManager(store store.Store, stagingDir string, locks *LockRegistry) (*Manager, error) {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{Store: store, StagingDir: stagingDir, Locks: locks}, nil
}

// CreateOrResume 创建新会话；若同路径已有活动会话：同一用户 → 返回原会话
// （resumed=true，断点续传），其他用户 → ErrConflict。
func (m *Manager) CreateOrResume(userID int64, rootID, relPath string, size, chunkSize int64, contentHash, baseHash string) (*store.UploadSession, bool, error) {
	if size < 0 {
		return nil, false, errors.New("invalid size")
	}
	// chunkSize 先归一再比对：省略/越界的请求按默认片归一后再与会话比对，
	// 避免"参数缺省"被误判成文件换版而误废会话。
	if chunkSize < MinChunkSize || chunkSize > MaxChunkSize {
		chunkSize = DefaultChunkSize
	}
	if existing, err := m.Store.GetActiveUploadSession(rootID, relPath); err == nil {
		if existing.UserID != userID {
			return nil, false, ErrConflict
		}
		// 0.2.1：同用户命中旧会话但 size/chunkSize 与本次请求不符 = 文件已换成
		// 另一版本，旧分片不可复用——此前原样返回旧会话会让客户端拿新片数填
		// 旧会话（越界 400 / assemble size 不符，该路径永久卡死）。作废重建。
		if existing.Size == size && existing.ChunkSize == chunkSize {
			return existing, true, nil
		}
		_ = m.Abort(existing)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	sess := &store.UploadSession{
		ID:             fileops.RandomHex(12),
		UserID:         userID,
		RootID:         rootID,
		Path:           relPath,
		Size:           size,
		ChunkSize:      chunkSize,
		ContentHash:    contentHash,
		BaseHash:       baseHash,
		UploadedChunks: []int64{},
		Status:         store.UploadStatusUploading,
	}
	if err := os.MkdirAll(m.sessionDir(sess.ID), 0o755); err != nil {
		return nil, false, err
	}
	if err := m.Store.CreateUploadSession(sess); err != nil {
		_ = os.RemoveAll(m.sessionDir(sess.ID))
		return nil, false, err
	}
	return sess, false, nil
}

// ChunkCount 会话应有的分片总数。
func ChunkCount(sess *store.UploadSession) int64 {
	if sess.Size == 0 {
		return 1 // 空文件也走一个分片（长度为 0），简化客户端逻辑
	}
	n := sess.Size / sess.ChunkSize
	if sess.Size%sess.ChunkSize != 0 {
		n++
	}
	return n
}

// MaxChunkBytes 指定分片允许的最大字节数（末分片按余数，其余等于 ChunkSize）。
func MaxChunkBytes(sess *store.UploadSession, index int64) int64 {
	if sess.Size == 0 {
		return 0 // 空文件会话：占位分片必须为 0 字节（0.3.1 修复 size=0 超额灌入）
	}
	total := ChunkCount(sess)
	if index == total-1 && sess.Size%sess.ChunkSize != 0 {
		return sess.Size % sess.ChunkSize
	}
	return sess.ChunkSize
}

// WriteChunk 写入一个分片：先落盘（tmp+rename），再入库标记。
func (m *Manager) WriteChunk(sess *store.UploadSession, index int64, body io.Reader) error {
	total := ChunkCount(sess)
	if index < 0 || index >= total {
		return fmt.Errorf("%w: chunk index %d out of range [0,%d)", ErrBadSession, index, total)
	}
	if sess.Status != store.UploadStatusUploading {
		return ErrBadSession
	}
	dir := m.sessionDir(sess.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	final := filepath.Join(dir, strconv.FormatInt(index, 10)+".part")
	// 限制单片大小，防御恶意超发（0 字节分片严格执行，不给冗余，0.3.1）
	limit := MaxChunkBytes(sess, index)
	if limit > 0 {
		limit += MaxChunkSlack
	}
	n, err := fileops.WriteFileAtomic(final, io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		_ = os.Remove(final)
		return fmt.Errorf("%w: chunk %d exceeds max size", ErrBadSession, index)
	}
	// 标记入库用 per-session 锁包住"重读→append→回写"（0.2.1）：旧实现直接
	// 在请求持有的 sess 副本上读-改-写整列覆盖，同会话并发传多片会互相丢标记
	// （片在盘上但 complete 报缺片）。多实例共享库时竞态窗口仍在——标记丢失
	// 最坏结果是多传一片（幂等覆盖），不损坏数据。
	lockKey := Key("#sess", sess.ID)
	unlock := m.Locks.Lock(lockKey)
	defer unlock()
	fresh, ferr := m.Store.GetUploadSession(sess.ID)
	if ferr != nil {
		return ferr
	}
	sess.UploadedChunks = fresh.UploadedChunks
	if contains64(fresh.UploadedChunks, index) {
		return nil // 重复上传同分片：幂等
	}
	fresh.UploadedChunks = append(fresh.UploadedChunks, index)
	sort.Slice(fresh.UploadedChunks, func(i, j int) bool { return fresh.UploadedChunks[i] < fresh.UploadedChunks[j] })
	if err := m.Store.UpdateUploadSession(fresh); err != nil {
		return err
	}
	sess.UploadedChunks = fresh.UploadedChunks
	return nil
}

// Abort 终止会话：删除暂存与记录。
func (m *Manager) Abort(sess *store.UploadSession) error {
	_ = os.RemoveAll(m.sessionDir(sess.ID))
	return m.Store.DeleteUploadSession(sess.ID)
}

// Complete 校验并合并分片到 destAbs。
// beforeReplace 在持路径锁后、替换目标文件前调用（用于版本快照），可返回错误中止。
// 返回合并后文件的 SHA-256 与大小。
func (m *Manager) Complete(sess *store.UploadSession, destAbs string, beforeReplace func() error) (string, int64, error) {
	total := ChunkCount(sess)
	if int64(len(sess.UploadedChunks)) < total {
		return "", 0, ErrIncomplete
	}
	// 1) 合并到临时文件并计算哈希（不持锁，IO 重的部分放锁外）
	tmp := destAbs + ".fmmerge-" + fileops.RandomHex(4)
	if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
		return "", 0, err
	}
	hash, size, err := m.assemble(sess, tmp, total)
	if err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	if sess.ContentHash != "" && !equalFoldHex(hash, sess.ContentHash) {
		_ = os.Remove(tmp)
		return "", 0, ErrHashMismatch
	}
	// 2) 持路径锁：BaseHash 乐观并发校验 → 版本快照 → 原子替换
	unlock := m.Locks.Lock(Key(sess.RootID, sess.Path))
	defer unlock()
	if sess.BaseHash != "" {
		cur, herr := fileops.SHA256File(destAbs)
		if herr != nil {
			_ = os.Remove(tmp)
			return "", 0, herr
		}
		if cur != "" && !equalFoldHex(cur, sess.BaseHash) {
			_ = os.Remove(tmp)
			return "", 0, ErrConflict // 上传期间文件被其他客户端修改
		}
		if cur == "" && sess.BaseHash != "-" {
			_ = os.Remove(tmp)
			return "", 0, ErrConflict // 上传期间文件被创建
		}
	}
	if beforeReplace != nil {
		if err := beforeReplace(); err != nil {
			_ = os.Remove(tmp)
			return "", 0, err
		}
	}
	if err := os.Rename(tmp, destAbs); err != nil {
		_ = os.Remove(tmp)
		return "", 0, err
	}
	// 3) 标记完成并清理暂存（记录保留 24h 供查询，由 Cleaner 清理）。
	// 0.2.1：文件已原子替换成功，标记/清账失败只告警不回错（客户端轮询会话
	// 状态判完成的语义仍由 DB 状态决定，DB 写失败时状态滞留 uploading 会在
	// 下次巡检或重试路径暴露，绝不能再改文件）。
	sess.Status = store.UploadStatusCompleted
	if err := m.Store.UpdateUploadSession(sess); err != nil {
		log.Printf("transfer: session %s complete mark failed (file already replaced): %v", sess.ID, err)
	}
	if err := os.RemoveAll(m.sessionDir(sess.ID)); err != nil {
		log.Printf("transfer: staging cleanup for session %s: %v", sess.ID, err)
	}
	return hash, size, nil
}

// assemble 顺序合并分片到 tmp，返回 sha256 与总字节数。
func (m *Manager) assemble(sess *store.UploadSession, tmp string, total int64) (string, int64, error) {
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	defer out.Close()
	h := sha256.New()
	var size int64
	for i := int64(0); i < total; i++ {
		cf, err := os.Open(filepath.Join(m.sessionDir(sess.ID), strconv.FormatInt(i, 10)+".part"))
		if err != nil {
			return "", 0, fmt.Errorf("%w: chunk %d missing on disk", ErrIncomplete, i)
		}
		n, err := io.Copy(io.MultiWriter(out, h), cf)
		cf.Close()
		if err != nil {
			return "", 0, err
		}
		size += n
	}
	if size != sess.Size {
		return "", 0, fmt.Errorf("%w: assembled size %d != declared %d", ErrIncomplete, size, sess.Size)
	}
	if err := out.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func (m *Manager) sessionDir(id string) string {
	return filepath.Join(m.StagingDir, id)
}

func contains64(list []int64, x int64) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

func equalFoldHex(a, b string) bool {
	return strings.EqualFold(a, b)
}
