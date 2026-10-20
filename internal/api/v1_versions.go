package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/transfer"
)

// 文件版本管理（需求13）：覆盖上传/回滚前自动快照，保留最近 N 版（配置 versions.maxVersions）。

// handleVersionList GET /api/v1/versions?rootId=&path=（需 View 权限）。
func (s *Server) handleVersionList(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if d := s.permsFor(c, rootID, rel); !d.View {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: view"))
		return
	}
	// 版本索引按物理键存储
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	vers, err := s.Store.ListFileVersions(sc.PhysID, sc.Phys)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	// 0.2.11 响应面收敛：不再转储 store 行（原含物理 rootId/path、storeId 与
	// 数字 createdBy——物理键与用户 id 均不对客户端可见），只回展示所需字段。
	view := make([]map[string]any, 0, len(vers))
	for _, v := range vers {
		view = append(view, map[string]any{
			"id": v.ID, "versionNo": v.VersionNo, "size": v.Size,
			"hash": v.Hash, "createdAt": v.CreatedAt,
		})
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"versions":    view,
		"maxVersions": s.Cfg.Versions.MaxVersions,
		"enabled":     s.Cfg.Versions.Enabled,
	})
}

// versionByID 取版本并校验其路径的可见性。
func (s *Server) versionByID(c *gin.Context) *store.FileVersion {
	id, err := strconv.ParseInt(c.Request.URL.Query().Get("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad version id"))
		return nil
	}
	ver, err := s.Store.GetFileVersion(id)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return nil
	}
	return ver
}

// handleVersionContent GET /api/v1/versions/content?id=（需该路径 Download 权限）。
func (s *Server) handleVersionContent(c *gin.Context) {
	ver := s.versionByID(c)
	if ver == nil {
		return
	}
	if !s.requirePermPhys(c, ver.RootID, ver.Path, func(d perm.Decision) bool { return d.Download }, "download(version)") {
		return
	}
	content := s.Versions.ContentPath(ver)
	if _, err := os.Stat(content); err != nil {
		writeErr(c, http.StatusNotFound, errors.New("version content missing"))
		return
	}
	name := fmt.Sprintf("%s.v%d%s", filepath.Base(ver.Path), ver.VersionNo, filepath.Ext(ver.Path))
	s.serveContent(c, content, name, false)
}

// handleVersionRollback POST /api/v1/versions/rollback {rootId,path,versionId}
// 需 Upload 权限；回滚前当前内容也会存为新版本（可再次回滚回来）。
func (s *Server) handleVersionRollback(c *gin.Context) {
	var req struct {
		RootID    string `json:"rootId"`
		Path      string `json:"path"`
		VersionID int64  `json:"versionId"`
	}
	if err := decode(c, &req); err != nil || req.VersionID == 0 {
		writeErr(c, http.StatusBadRequest, errors.New("versionId required"))
		return
	}
	rel := fileops.CleanRel(req.Path)
	if req.RootID == "" {
		writeErr(c, http.StatusBadRequest, errors.New("rootId required"))
		return
	}
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	if d := s.permsFor(c, req.RootID, rel); !d.Upload {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: upload(rollback)"))
		return
	}
	ver, err := s.Store.GetFileVersion(req.VersionID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if ver.RootID != sc.PhysID || ver.Path != sc.Phys {
		writeErr(c, http.StatusBadRequest, errors.New("version does not belong to this path"))
		return
	}
	unlock := s.Locks.Lock(transfer.Key(sc.PhysID, sc.Phys))
	defer unlock()
	if err := s.Versions.Snapshot(u.ID, sc.PhysID, sc.Phys, sc.Abs); err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(sc.Abs), 0o755); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if err := s.Versions.Restore(ver, sc.Abs); err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	info, err := os.Stat(sc.Abs)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	e := sc.entry(sc.Abs, info)
	// 0.2.1：不再把 mtime 覆盖为 time.Now()——sc.entry 已是真实 stat，与
	// handleList/上传口径一致（content_hash 保留判据依赖秒级 mtime 一致）；
	// 回滚等于写入新字节，现算 SHA-256 入库供 /check 比对。
	if !info.IsDir() {
		if h, herr := fileops.SHA256File(sc.Abs); herr == nil {
			e.Hash = h
		}
	}
	s.recordMeta(u, sc, e)
	s.audit(c, store.AuditRollback, "", 0, req.RootID, rel, fmt.Sprintf("rolled back to v%d", ver.VersionNo))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "restored": ver.VersionNo, "entry": e})
}

// handleVersionDelete DELETE /api/v1/versions/{id}（需该路径 Delete 权限）。
func (s *Server) handleVersionDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	ver, err := s.Store.GetFileVersion(id)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if !s.requirePermPhys(c, ver.RootID, ver.Path, func(d perm.Decision) bool { return d.Delete }, "delete(version)") {
		return
	}
	if err := s.Versions.RemoveVersion(id); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}
