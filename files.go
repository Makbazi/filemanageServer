package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	filemanagedb "filemanageDb"
)

// FileEntry 前端展示用的文件条目，Path 为相对根目录的 slash 路径。
type FileEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	IsDir bool      `json:"isDir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Kind  string    `json:"kind"`
}

// userRoots 返回用户可访问的根目录（AllowedRoots 为空表示全部）。
func (s *server) userRoots(u *filemanagedb.User) []*RootCfg {
	var out []*RootCfg
	for i := range s.cfg.Roots {
		r := &s.cfg.Roots[i]
		if len(u.AllowedRoots) == 0 || contains(u.AllowedRoots, r.ID) {
			out = append(out, r)
		}
	}
	return out
}

func contains(list []string, x string) bool {
	for _, v := range list {
		if v == x {
			return true
		}
	}
	return false
}

// resolve 把 (rootID, relPath) 映射到磁盘绝对路径，并确保不逃逸出根目录。
// 这是"只允许管理用户配置的目录"的唯一入口。
func (s *server) resolve(r *http.Request, rootID, rel string) (*RootCfg, string, error) {
	u := currentUser(r)
	if u == nil {
		return nil, "", errors.New("unauthorized")
	}
	root := s.roots[rootID]
	if root == nil {
		return nil, "", fmt.Errorf("unknown root %q", rootID)
	}
	if len(u.AllowedRoots) > 0 && !contains(u.AllowedRoots, rootID) {
		return nil, "", fmt.Errorf("root %q not permitted", rootID)
	}
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
			return nil, "", errors.New("path traversal denied")
		}
	}
	abs := filepath.Join(root.Path, filepath.FromSlash(rel))
	rootAbs := filepath.Clean(root.Path)
	if abs != rootAbs && !strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
		return nil, "", errors.New("path escapes root")
	}
	return root, abs, nil
}

// relOf 把绝对路径转回相对根目录的 slash 路径。
func relOf(root *RootCfg, abs string) string {
	rel, err := filepath.Rel(root.Path, abs)
	if err != nil {
		return filepath.Base(abs)
	}
	return filepath.ToSlash(rel)
}

func entryFrom(root *RootCfg, abs string, info fs.FileInfo) FileEntry {
	rel := relOf(root, abs)
	return FileEntry{
		Name:  info.Name(),
		Path:  rel,
		IsDir: info.IsDir(),
		Size:  info.Size(),
		Mtime: info.ModTime(),
		Kind:  kindOf(rel, info.IsDir()),
	}
}

// kindOf 按扩展名粗分类，供前端图标与 AI 描述选择提示词。
func kindOf(p string, isDir bool) string {
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

// ---------- handlers ----------

func (s *server) handleRoots(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	type rootView struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	roots := s.userRoots(u)
	out := make([]rootView, 0, len(roots))
	for _, rt := range roots {
		out = append(out, rootView{ID: rt.ID, Name: rt.Name})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	_, abs, err := s.resolve(r, r.URL.Query().Get("rootId"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	dirs, err := os.ReadDir(abs)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	rootID := r.URL.Query().Get("rootId")
	entries := make([]FileEntry, 0, len(dirs))
	for _, d := range dirs {
		info, err := d.Info()
		if err != nil {
			continue
		}
		e := entryFrom(s.roots[rootID], filepath.Join(abs, d.Name()), info)
		entries = append(entries, e)
		s.recordMeta(currentUser(r), rootID, e)
	}
	sortEntries(entries)
	writeJSON(w, http.StatusOK, map[string]any{"rootId": rootID, "path": cleanRel(r.URL.Query().Get("path")), "entries": entries})
}

func (s *server) handleStat(w http.ResponseWriter, r *http.Request) {
	_, abs, err := s.resolve(r, r.URL.Query().Get("rootId"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	rootID := r.URL.Query().Get("rootId")
	writeJSON(w, http.StatusOK, entryFrom(s.roots[rootID], abs, info))
}

func cleanRel(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.Trim(path.Clean("/"+p), "/")
	if p == "." {
		return ""
	}
	return p
}

func sortEntries(entries []FileEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

func statusOf(err error) int {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound
	case errors.Is(err, fs.ErrExist):
		return http.StatusConflict
	case errors.Is(err, filemanagedb.ErrNotFound):
		return http.StatusNotFound
	case strings.Contains(err.Error(), "denied"), strings.Contains(err.Error(), "escapes"), strings.Contains(err.Error(), "permitted"), strings.Contains(err.Error(), "unknown root"):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// reqPath 从 JSON 请求体中取 rootId/path。
type pathReq struct {
	RootID string   `json:"rootId"`
	Path   string   `json:"path"`
	Paths  []string `json:"paths"`
	Dest   string   `json:"dest"`
	Name   string   `json:"name"`
	NewName string  `json:"newName"`
}

func (s *server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	_, abs, err := s.resolve(r, req.RootID, req.Path)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleRename(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil || req.NewName == "" || strings.ContainsAny(req.NewName, "/\\") {
		writeErr(w, http.StatusBadRequest, errors.New("path + newName required"))
		return
	}
	_, abs, err := s.resolve(r, req.RootID, req.Path)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	target := filepath.Join(filepath.Dir(abs), req.NewName)
	if err := os.Rename(abs, target); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	// 同步元数据（记录归属，失败不影响主流程）
	_ = s.store.RenameFileMeta(currentUser(r).ID, req.RootID,
		cleanRel(req.Path), relOf(s.roots[req.RootID], target))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": relOf(s.roots[req.RootID], target)})
}

func (s *server) handleMove(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil || req.Dest == "" {
		writeErr(w, http.StatusBadRequest, errors.New("paths + dest required"))
		return
	}
	_, destAbs, err := s.resolve(r, req.RootID, req.Dest)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if err := os.MkdirAll(destAbs, 0o755); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	for _, p := range req.Paths {
		_, src, err := s.resolve(r, req.RootID, p)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		if isSameOrChild(src, destAbs) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("cannot move %s into itself", p))
			return
		}
		target := filepath.Join(destAbs, filepath.Base(src))
		if src == target {
			continue
		}
		if err := movePath(src, target); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func isSameOrChild(src, dest string) bool {
	return src == dest || strings.HasPrefix(dest, src+string(filepath.Separator))
}

// movePath 优先 rename；跨设备（EXDEV）时退化为 copy+remove。
func movePath(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := copyDir(src, dst); err != nil {
			return err
		}
		return os.RemoveAll(src)
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyFile(src, dst string) error {
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
	return out.Close()
}

func copyDir(src, dst string) error {
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
		return copyFile(p, target)
	})
}

func (s *server) handleCopy(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil || req.Dest == "" {
		writeErr(w, http.StatusBadRequest, errors.New("paths + dest required"))
		return
	}
	_, destAbs, err := s.resolve(r, req.RootID, req.Dest)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	for _, p := range req.Paths {
		_, src, err := s.resolve(r, req.RootID, p)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		if isSameOrChild(src, destAbs) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("cannot copy %s into itself", p))
			return
		}
		target := filepath.Join(destAbs, filepath.Base(src))
		info, err := os.Stat(src)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		if info.IsDir() {
			if err := copyDir(src, target); err != nil {
				writeErr(w, statusOf(err), err)
				return
			}
		} else if err := copyFile(src, target); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNameSearch 按文件名搜索（walk 限制深度与结果数）。
func (s *server) handleNameSearch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootID string `json:"rootId"`
		Query  string `json:"query"`
	}
	if err := decode(r, &req); err != nil || req.Query == "" {
		writeErr(w, http.StatusBadRequest, errors.New("rootId + query required"))
		return
	}
	root, abs, err := s.resolve(r, req.RootID, "")
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	lq := strings.ToLower(req.Query)
	entries := make([]FileEntry, 0, 50)
	maxDepth := 8
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if len(entries) >= 200 || depth > maxDepth {
			return
		}
		ds, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, d := range ds {
			if strings.HasPrefix(d.Name(), ".") {
				continue
			}
			p := filepath.Join(dir, d.Name())
			if strings.Contains(strings.ToLower(d.Name()), lq) {
				if info, err := d.Info(); err == nil {
					entries = append(entries, entryFrom(root, p, info))
				}
			}
			if d.IsDir() {
				walk(p, depth+1)
			}
		}
	}
	walk(abs, 0)
	sortEntries(entries)
	writeJSON(w, http.StatusOK, entries)
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	_, abs, err := s.resolve(r, r.URL.Query().Get("rootId"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		writeErr(w, http.StatusNotFound, errors.New("file not found"))
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(abs)))
	http.ServeFile(w, r, abs)
}

// handleUpload POST /api/v1/upload?rootId=&path=<相对目标路径含文件名>，body 为原始字节流。
func (s *server) handleUpload(w http.ResponseWriter, r *http.Request) {
	rootID := r.URL.Query().Get("rootId")
	target := r.URL.Query().Get("path")
	if target == "" {
		writeErr(w, http.StatusBadRequest, errors.New("path required"))
		return
	}
	if strings.HasSuffix(target, "/") {
		writeErr(w, http.StatusBadRequest, errors.New("path must be full file path"))
		return
	}
	_, abs, err := s.resolve(r, rootID, target)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	defer f.Close()
	n, err := io.Copy(f, r.Body)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	e := FileEntry{Name: filepath.Base(abs), Path: cleanRel(target), IsDir: false, Size: n, Mtime: time.Now(), Kind: kindOf(target, false)}
	s.recordMeta(currentUser(r), rootID, e)
	writeJSON(w, http.StatusOK, e)
}

// ---------- 回收站 ----------
// 服务端回收站是磁盘上的 trashDir + filemanageDb 中的条目记录，
// 物理移动由服务端完成，数据库仅存归属/日期等元数据。

func newTrashID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *server) trashPath(userID int64, trashID string) string {
	return filepath.Join(s.cfg.TrashDir, fmt.Sprintf("u%d", userID), trashID)
}

// handleDelete 删除（进回收站）：把文件移入 trashDir，并在库中记录条目。
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if err := decode(r, &req); err != nil || len(req.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("rootId + paths required"))
		return
	}
	u := currentUser(r)
	items := make([]filemanagedb.TrashItem, 0, len(req.Paths))
	for _, p := range req.Paths {
		_, abs, err := s.resolve(r, req.RootID, p)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		if abs == s.roots[req.RootID].Path {
			writeErr(w, http.StatusBadRequest, errors.New("cannot delete root itself"))
			return
		}
		info, err := os.Stat(abs)
		if err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		trashID := newTrashID()
		dst := s.trashPath(u.ID, trashID)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		if err := movePath(abs, dst); err != nil {
			writeErr(w, statusOf(err), err)
			return
		}
		item := filemanagedb.TrashItem{
			TrashID: trashID, UserID: u.ID, RootID: req.RootID,
			RelPath: cleanRel(p), Name: info.Name(), IsDir: info.IsDir(),
			Size: info.Size(), DeletedAt: time.Now(),
		}
		if err := s.store.AddTrashItem(&item); err != nil {
			// 入库失败则把文件放回，保证一致性
			_ = movePath(dst, abs)
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		_ = s.store.DeleteFileMeta(u.ID, req.RootID, cleanRel(p))
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) handleTrashList(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	items, err := s.store.ListTrashItems(u.ID, r.URL.Query().Get("rootId"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []*filemanagedb.TrashItem{}
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TrashID string `json:"trashId"`
	}
	if err := decode(r, &req); err != nil || req.TrashID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("trashId required"))
		return
	}
	u := currentUser(r)
	item, err := s.store.TakeTrashItem(u.ID, req.TrashID)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	src := s.trashPath(u.ID, item.TrashID)
	_, dstAbs, err := s.resolve(r, item.RootID, item.RelPath)
	if err != nil {
		s.restoreFailed(item)
		writeErr(w, statusOf(err), err)
		return
	}
	if _, err := os.Stat(dstAbs); err == nil {
		dstAbs = dedup(dstAbs) // 同名冲突时自动加后缀
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		s.restoreFailed(item)
		writeErr(w, statusOf(err), err)
		return
	}
	if err := movePath(src, dstAbs); err != nil {
		s.restoreFailed(item)
		writeErr(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": relOf(s.roots[item.RootID], dstAbs)})
}

func (s *server) restoreFailed(item *filemanagedb.TrashItem) {
	_ = s.store.AddTrashItem(item)
}

func dedup(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s(%d)%s", base, i, ext)
		if _, err := os.Stat(cand); errors.Is(err, fs.ErrNotExist) {
			return cand
		}
	}
}

func (s *server) handleTrashRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TrashID string `json:"trashId"`
	}
	if err := decode(r, &req); err != nil || req.TrashID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("trashId required"))
		return
	}
	u := currentUser(r)
	if err := s.store.RemoveTrashItem(u.ID, req.TrashID); err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	_ = os.RemoveAll(s.trashPath(u.ID, req.TrashID))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) handleTrashClear(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	items, err := s.store.ClearTrash(u.ID, r.URL.Query().Get("rootId"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	for i := range items {
		_ = os.RemoveAll(s.trashPath(u.ID, items[i].TrashID))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": len(items)})
}

// recordMeta 把文件元数据（归属、日期等）记入 filemanageDb，供 AI 描述关联。
func (s *server) recordMeta(u *filemanagedb.User, rootID string, e FileEntry) {
	if u == nil {
		return
	}
	_ = s.store.PutFileMeta(&filemanagedb.FileMeta{
		UserID: u.ID, RootID: rootID, Path: e.Path, Name: e.Name,
		IsDir: e.IsDir, Size: e.Size, Mtime: e.Mtime, Kind: e.Kind,
		UpdatedAt: time.Now(),
	})
}
