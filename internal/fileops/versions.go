package fileops

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	store "filemanageServer/internal/store"
)

// VersionStore 文件版本库（需求13）：内容存于 Dir/<rootID>/<storeID>，
// 元数据存于 internal/store。覆盖写/回滚前自动快照，保留最近 MaxVersions 版。
type VersionStore struct {
	Store       store.Store
	Dir         string
	MaxVersions int
	Enabled     bool
}

// Snapshot 把 absPath 当前内容存为一个新版本（文件不存在则跳过）。
// 成功后修剪超出 MaxVersions 的旧版本。
func (v *VersionStore) Snapshot(userID int64, rootID, relPath, absPath string) error {
	if v == nil || !v.Enabled {
		return nil
	}
	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // 新文件，无旧版本可存
		}
		return err
	}
	if info.IsDir() {
		return nil // 目录不做版本
	}
	storeID := RandomHex(8)
	dir := filepath.Join(v.Dir, rootID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := CopyFile(absPath, filepath.Join(dir, storeID)); err != nil {
		return err
	}
	hash, err := SHA256File(absPath)
	if err != nil {
		return err
	}
	ver := &store.FileVersion{
		RootID: rootID, Path: relPath, Size: info.Size(),
		Hash: hash, StoreID: storeID, CreatedBy: userID,
	}
	if err := v.Store.AddFileVersion(ver); err != nil {
		_ = os.Remove(filepath.Join(dir, storeID))
		return err
	}
	return v.Prune(rootID, relPath)
}

// ContentPath 返回版本内容文件的磁盘路径。
func (v *VersionStore) ContentPath(ver *store.FileVersion) string {
	return filepath.Join(v.Dir, ver.RootID, ver.StoreID)
}

// Restore 用版本内容覆盖 absPath（tmp+rename 原子替换）。
// 调用方应先 Snapshot 当前内容，并持有路径锁。
func (v *VersionStore) Restore(ver *store.FileVersion, absPath string) error {
	src := v.ContentPath(ver)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	tmp := absPath + ".fmver-" + RandomHex(4)
	if err := CopyFile(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, absPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Prune 修剪某路径超出 MaxVersions 的旧版本（记录+内容文件）。
func (v *VersionStore) Prune(rootID, relPath string) error {
	if v == nil || !v.Enabled || v.MaxVersions <= 0 {
		return nil
	}
	vers, err := v.Store.ListFileVersions(rootID, relPath)
	if err != nil {
		return err
	}
	for i := v.MaxVersions; i < len(vers); i++ {
		_ = os.Remove(v.ContentPath(vers[i]))
		_ = v.Store.DeleteFileVersion(vers[i].ID)
	}
	return nil
}

// RemoveVersion 删除单个版本（记录+内容文件）。
func (v *VersionStore) RemoveVersion(id int64) error {
	ver, err := v.Store.GetFileVersion(id)
	if err != nil {
		return err
	}
	_ = os.Remove(v.ContentPath(ver))
	return v.Store.DeleteFileVersion(id)
}

// RemoveAllFor 删除某路径（含子路径）的全部版本，目录删除时调用。
func (v *VersionStore) RemoveAllFor(rootID, relPath string) error {
	if v == nil || !v.Enabled {
		return nil
	}
	vers, err := v.Store.ListFileVersions(rootID, relPath)
	if err != nil {
		return err
	}
	for _, ver := range vers {
		_ = os.Remove(v.ContentPath(ver))
	}
	return v.Store.DeleteFileVersions(rootID, relPath)
}
