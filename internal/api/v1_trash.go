package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"net/http"
	"os"
	"path/filepath"

	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
)

// 服务端回收站 = 磁盘实体 + 库中条目记录（0.7.0 需求3）：
//   - 有家目录（_users 分配，正常建用户即有）→ 实体在 <dataDir>/Users/<用户名>/.fmtrash/<trashID>；
//   - 无家目录（0.6.x 存量升级前的遗留用户等）→ 回退中央 trashDir/u<userID>/<trashID>。
// 超过 trashRetentionDays（默认 30 天）的条目由后台 Cleaner 自动清理（需求4）。
// trash_items 行不存实体路径，故还原/删除按"家目录优先、中央兜底"双候选解析，
// 0.6.x 遗留中央 blob 无需搬移即可继续还原（无 schema 变更）。

// homeOf 用户的家目录分配（_users 条目 SubPath）；无家目录返回 ""。
func (s *Server) homeOf(u *store.User) string {
	urs, err := s.Store.ListUserRoots(u.ID)
	if err != nil {
		return ""
	}
	for _, r := range urs {
		if r.RootID == store.HomeRootID {
			return fileops.CleanRel(r.SubPath)
		}
	}
	return ""
}

// trashBlobPath 新入回收站实体的路径（家目录优先）。
func (s *Server) trashBlobPath(u *store.User, trashID string) string {
	if home := s.homeOf(u); home != "" {
		return fileops.HomeTrashPath(s.Cfg.UsersDir(), home, trashID)
	}
	return fileops.TrashPath(s.Cfg.TrashDir, u.ID, trashID)
}

// resolveTrashBlob 还原用：家目录候选存在则用之，否则中央遗留路径。
func (s *Server) resolveTrashBlob(u *store.User, trashID string) string {
	if home := s.homeOf(u); home != "" {
		p := fileops.HomeTrashPath(s.Cfg.UsersDir(), home, trashID)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return fileops.TrashPath(s.Cfg.TrashDir, u.ID, trashID)
}

// trashBlobCandidates 彻底删除时清理的两候选路径。
func (s *Server) trashBlobCandidates(u *store.User, trashID string) []string {
	out := []string{fileops.TrashPath(s.Cfg.TrashDir, u.ID, trashID)}
	if home := s.homeOf(u); home != "" {
		out = append([]string{fileops.HomeTrashPath(s.Cfg.UsersDir(), home, trashID)}, out...)
	}
	return out
}

// handleTrashList GET /api/v1/trash?rootId=
// 回收站条目按物理键存储（0.5.0）；rootId 过滤取虚拟根 id 的物理段，响应转显示键。
func (s *Server) handleTrashList(c *gin.Context) {
	u := currentUser(c)
	filter := c.Request.URL.Query().Get("rootId")
	physFilter := ""
	if filter != "" {
		physFilter, _ = parseVRootID(filter)
	}
	items, err := s.Store.ListTrashItems(u.ID, physFilter)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, it := range items {
		rid, disp, ok := s.displayOf(c, it.RootID, it.RelPath)
		if !ok {
			continue // 授权已回收的条目不再暴露
		}
		out = append(out, map[string]any{
			"trashId": it.TrashID, "rootId": rid, "relPath": disp, "name": it.Name,
			"isDir": it.IsDir, "size": it.Size, "deletedAt": it.DeletedAt,
		})
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"items":         out,
		"retentionDays": s.Cfg.TrashRetentionDays,
	})
}

// handleTrashRestore POST /api/v1/trash/restore {trashId}
func (s *Server) handleTrashRestore(c *gin.Context) {
	var req struct {
		TrashID string `json:"trashId"`
	}
	if err := decode(c, &req); err != nil || req.TrashID == "" {
		writeErr(c, http.StatusBadRequest, errors.New("trashId required"))
		return
	}
	u := currentUser(c)
	item, err := s.Store.TakeTrashItem(u.ID, req.TrashID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	src := s.resolveTrashBlob(u, item.TrashID)           // 家目录优先、中央遗留兜底（0.7.0）
	sc, err := s.scopePhys(c, item.RootID, item.RelPath) // 物理键形式反查授权（分配被回收则拒绝，0.6.0）
	if err != nil {
		s.restoreFailed(item)
		writeErr(c, statusOf(err), err)
		return
	}
	dstAbs := sc.Abs
	if _, err := os.Stat(dstAbs); err == nil {
		dstAbs = fileops.Dedup(dstAbs) // 同名冲突自动加后缀
	}
	if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
		s.restoreFailed(item)
		writeErr(c, statusOf(err), err)
		return
	}
	if err := fileops.MovePath(src, dstAbs); err != nil {
		s.restoreFailed(item)
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "path": fileops.RelOf(sc.BaseAbs, dstAbs)})
}

func (s *Server) restoreFailed(item *store.TrashItem) {
	_ = s.Store.AddTrashItem(item)
}

// handleTrashRemove POST /api/v1/trash/remove {trashId}
func (s *Server) handleTrashRemove(c *gin.Context) {
	var req struct {
		TrashID string `json:"trashId"`
	}
	if err := decode(c, &req); err != nil || req.TrashID == "" {
		writeErr(c, http.StatusBadRequest, errors.New("trashId required"))
		return
	}
	u := currentUser(c)
	if err := s.Store.RemoveTrashItem(u.ID, req.TrashID); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	for _, p := range s.trashBlobCandidates(u, req.TrashID) {
		_ = os.RemoveAll(p)
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleTrashClear POST /api/v1/trash/clear?rootId=
func (s *Server) handleTrashClear(c *gin.Context) {
	u := currentUser(c)
	physFilter := ""
	if rid := c.Request.URL.Query().Get("rootId"); rid != "" {
		physFilter, _ = parseVRootID(rid)
	}
	items, err := s.Store.ClearTrash(u.ID, physFilter)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	for i := range items {
		for _, p := range s.trashBlobCandidates(u, items[i].TrashID) {
			_ = os.RemoveAll(p)
		}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "removed": len(items)})
}
