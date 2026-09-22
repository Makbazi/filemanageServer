package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"net/http"
	"strconv"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
)

// 目录权限（需求8/9）：查看/下载/上传/删除/分享 五个权限位，
// 主体可为 user / group / public（匿名访客）。
// 未配置 ACL 的目录：登录用户默认全权限；匿名默认只读浏览（需求6）。
// 子目录继承最近祖先的配置（就近命中即止）。

// handleACLGet GET /api/v1/acl?rootId=&path= → 该路径的 ACL 条目与有效权限。
func (s *Server) handleACLGet(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if d := s.permsFor(c, rootID, rel); !d.View {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: view"))
		return
	}
	// 0.5.0：ACL 以物理键存取，响应条目路径换算回显示路径
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	entries, err := s.Store.GetFolderACLs(sc.PhysID, sc.Phys)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if entries == nil {
		entries = []*store.FolderACL{}
	}
	for _, e := range entries {
		e.Path = sc.Disp
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"rootId":    sc.VRoot,
		"path":      sc.Disp,
		"entries":   entries,
		"effective": s.permsFor(c, rootID, rel),
	})
}

// aclEntryReq 权限条目请求体。
type aclEntryReq struct {
	SubjectType string        `json:"subjectType"` // user | group | public
	SubjectID   int64         `json:"subjectId"`
	Perms       store.PermSet `json:"perms"`
}

// handleACLPut PUT /api/v1/acl {rootId, path, entries} —— 整体替换该路径的 ACL。
// 需要管理员或该路径的 Share 权限。entries 为空数组 = 清除该路径全部 ACL（恢复默认策略）。
func (s *Server) handleACLPut(c *gin.Context) {
	var req struct {
		RootID  string        `json:"rootId"`
		Path    string        `json:"path"`
		Entries []aclEntryReq `json:"entries"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	rel := fileops.CleanRel(req.Path)
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	if u.Role != store.RoleAdmin {
		if d := s.permsFor(c, req.RootID, rel); !d.Share {
			writeErr(c, http.StatusForbidden, errors.New("permission denied: share (manage acl)"))
			return
		}
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	for _, e := range req.Entries {
		switch e.SubjectType {
		case store.SubjectUser, store.SubjectGroup, store.SubjectPublic:
		default:
			writeErr(c, http.StatusBadRequest, errors.New("subjectType must be user|group|public"))
			return
		}
		if e.SubjectType == store.SubjectPublic {
			continue
		}
		if e.SubjectID <= 0 {
			writeErr(c, http.StatusBadRequest, errors.New("subjectId required for user/group"))
			return
		}
		// 主体存在性校验，拒绝悬空 ACL 条目（0.3.1）
		var serr error
		if e.SubjectType == store.SubjectUser {
			_, serr = s.Store.GetUserByID(e.SubjectID)
		} else {
			_, serr = s.Store.GetGroupByID(e.SubjectID)
		}
		if serr != nil {
			writeErr(c, http.StatusBadRequest, errors.New("subjectId does not exist"))
			return
		}
	}
	// 替换：先删旧条目再写新条目（物理键存储，0.5.0）
	old, err := s.Store.GetFolderACLs(sc.PhysID, sc.Phys)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	for _, o := range old {
		_ = s.Store.DeleteFolderACL(o.ID)
	}
	for _, e := range req.Entries {
		acl := &store.FolderACL{
			RootID: sc.PhysID, Path: sc.Phys,
			SubjectType: e.SubjectType, SubjectID: e.SubjectID,
			Perms: e.Perms, CreatedBy: u.ID,
		}
		if e.SubjectType == store.SubjectPublic {
			acl.SubjectID = 0
			// 匿名主体不授予下载/上传/删除/分享（需求6：登录后才可下载/修改/删除）
			acl.Perms.Download, acl.Perms.Upload, acl.Perms.Delete, acl.Perms.Share = false, false, false, false
		}
		if err := s.Store.SetFolderACL(acl); err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	entries, _ := s.Store.GetFolderACLs(sc.PhysID, sc.Phys)
	for _, e := range entries {
		e.Path = sc.Disp
	}
	s.audit(c, store.AuditACLSet, "", 0, req.RootID, req.Path, fmt.Sprintf("%d entries", len(req.Entries)))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "entries": entries})
}

// handleACLDelete DELETE /api/v1/acl/{id} —— 删除单条 ACL。
func (s *Server) handleACLDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	u := s.requireLoginUser(c)
	if u == nil {
		return
	}
	// 找到条目以定位路径做权限校验
	all, err := s.Store.ListRootACLs("")
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	var target *store.FolderACL
	for _, a := range all {
		if a.ID == id {
			target = a
			break
		}
	}
	if target == nil {
		writeErr(c, http.StatusNotFound, store.ErrNotFound)
		return
	}
	if u.Role != store.RoleAdmin {
		// 0.5.0：只能管理自己授权范围内的 ACL 条目（跨 jail 条目直接拒绝）
		if _, _, ok := s.displayOf(c, target.RootID, target.Path); !ok {
			writeErr(c, http.StatusForbidden, errors.New("acl entry outside your scope"))
			return
		}
		// 0.6.0：条目键为物理存储键，经 scopePhys 判定（jailed 用户不再误判 403）
		if !s.requirePermPhys(c, target.RootID, target.Path,
			func(d perm.Decision) bool { return d.Share }, "share (manage acl)") {
			return
		}
	}
	if err := s.Store.DeleteFolderACL(id); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.audit(c, store.AuditACLDelete, "", 0, target.RootID, target.Path, fmt.Sprintf("acl id=%d", id))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleACLSubjects GET /api/v1/acl/subjects → 权限配置选择器数据（用户+用户组）。
func (s *Server) handleACLSubjects(c *gin.Context) {
	users, err := s.Store.ListUsers()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	groups, err := s.Store.ListGroups()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	uv := make([]map[string]any, 0, len(users))
	for _, u := range users {
		// 0.6.0（需求12）：客户端不可见管理员账号（管理台走 /admin/users）。
		if u.Role == store.RoleAdmin {
			continue
		}
		uv = append(uv, map[string]any{"id": u.ID, "username": u.Username, "role": u.Role})
	}
	gv := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		gv = append(gv, map[string]any{"id": g.ID, "name": g.Name, "members": g.Members})
	}
	writeJSON(c, http.StatusOK, map[string]any{"users": uv, "groups": gv})
}
