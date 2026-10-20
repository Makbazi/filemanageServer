package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/logx"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
)

// 目录权限（需求8/9）：查看/下载/上传/删除/分享 五个权限位，
// 主体可为 user / group / public（匿名访客）。
// 未配置 ACL 的目录：登录用户默认全权限；匿名默认只读浏览（需求6）。
// 子目录继承最近祖先的配置（就近命中即止）。

// ---------- 0.2.11 用户端口主体账户化（数字用户 id 不出服务端） ----------
//
// 存储层 ACL 行仍以 (subjectType, subjectID 数字) 键控——与历代二进制兼容、
// 与权限引擎零改动；只在 **用户端口 API 边界** 把主体表示换成
// "user:<账户>"/"group:<n>"/"public"（账户唯一且不可改，映射稳定）。
// PUT 请求体同时兼容旧 subjectId 数字入参（入参不是泄漏面，留给移动端
// 适配窗口；响应绝不回数字）。管理端口不受影响（handleAdminACLPut 直用数字）。

// aclSubjectRefs 用户端口主体名 ⇄ 数字 id 的请求级解析表（一次性 ListUsers/ListGroups）。
type aclSubjectRefs struct {
	byAccount map[string]*store.User
	byID      map[int64]*store.User
	groups    map[int64]*store.Group
}

func (s *Server) aclRefs() (*aclSubjectRefs, error) {
	users, err := s.Store.ListUsers()
	if err != nil {
		return nil, err
	}
	groups, err := s.Store.ListGroups()
	if err != nil {
		return nil, err
	}
	r := &aclSubjectRefs{byAccount: map[string]*store.User{}, byID: map[int64]*store.User{}, groups: map[int64]*store.Group{}}
	for _, u := range users {
		r.byAccount[u.Username] = u
		r.byID[u.ID] = u
	}
	for _, g := range groups {
		r.groups[g.ID] = g
	}
	return r, nil
}

// display 单条 ACL 的用户端口视图（永不含数字 subjectId/createdBy/物理 rootId）。
func (r *aclSubjectRefs) display(e *store.FolderACL) map[string]any {
	subject, name := "", ""
	switch e.SubjectType {
	case store.SubjectPublic:
		subject, name = "public", "公开（匿名访客）"
	case store.SubjectUser:
		if u, ok := r.byID[e.SubjectID]; ok {
			subject, name = "user:"+u.Username, u.Username
			if u.Nickname != "" {
				name = u.Nickname
			}
		} else {
			subject, name = "user:", "（已删除账户）"
		}
	case store.SubjectGroup:
		subject = "group:" + strconv.FormatInt(e.SubjectID, 10)
		if g, ok := r.groups[e.SubjectID]; ok {
			name = g.Name
		} else {
			name = "（已删除用户组）"
		}
	}
	return map[string]any{
		"id": e.ID, "path": e.Path, "subjectType": e.SubjectType,
		"subject": subject, "subjectName": name, "perms": e.Perms, "createdAt": e.CreatedAt,
	}
}

// resolve 请求体主体 → 存储数字键（ok=false 时 respCode/respMsg 给出 400 响应）。
func (r *aclSubjectRefs) resolve(subjectType, subject string, legacyID int64) (int64, int, string) {
	switch subjectType {
	case store.SubjectPublic:
		return 0, 0, ""
	case store.SubjectUser, store.SubjectGroup:
	default:
		return 0, http.StatusBadRequest, "subjectType must be user|group|public"
	}
	if subject != "" {
		i := strings.IndexByte(subject, ':')
		seg := subject
		if i >= 0 {
			seg = subject[i+1:]
		}
		if subjectType == store.SubjectUser {
			if u, ok := r.byAccount[seg]; ok {
				return u.ID, 0, ""
			}
			return 0, http.StatusBadRequest, "subject 对应的账户不存在: " + seg
		}
		id, err := strconv.ParseInt(seg, 10, 64)
		if err != nil || id <= 0 {
			return 0, http.StatusBadRequest, "group subject 必须形如 group:<id>"
		}
		if _, ok := r.groups[id]; !ok {
			return 0, http.StatusBadRequest, "subject 对应的用户组不存在"
		}
		return id, 0, ""
	}
	// 旧协议兼容：数字 subjectId（user 为数字 id）——仅入参侧，响应永不出数字。
	if subjectType == store.SubjectUser {
		if _, ok := r.byID[legacyID]; !ok {
			return 0, http.StatusBadRequest, "subjectId does not exist"
		}
	} else {
		if _, ok := r.groups[legacyID]; !ok {
			return 0, http.StatusBadRequest, "subjectId does not exist"
		}
	}
	return legacyID, 0, ""
}

// handleACLGet GET /api/v1/acl?rootId=&path= → 该路径的 ACL 条目与有效权限。
func (s *Server) handleACLGet(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	if d := s.permsFor(c, rootID, rel); !d.View {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: view"))
		return
	}
	// ACL 以物理键存取，响应条目路径换算回显示路径
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
	refs, err := s.aclRefs()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	view := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		v := refs.display(e)
		v["path"] = sc.Disp // 显示路径替换物理路径
		view = append(view, v)
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"rootId":    sc.VRoot,
		"path":      sc.Disp,
		"entries":   view,
		"effective": s.permsFor(c, rootID, rel),
	})
}

// aclEntryReq 权限条目请求体。0.2.11：首选 subject 账户化字符串；
// subjectId 数字入参保留兼容（旧客户端/移动端过渡期）。
type aclEntryReq struct {
	SubjectType string        `json:"subjectType"` // user | group | public
	Subject     string        `json:"subject"`     // "user:<账户>" | "group:<id>" | "public"
	SubjectID   int64         `json:"subjectId"`   // 旧协议兼容字段
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
	// 0.2.11 端口拆分：本 handler 只在用户端口（authCtx 已拒 admin/super 会话），
	// 旧的"管理员绕过 Share 位"角色分叉移除——管理端 ACL 写入走 PUT /admin/acl。
	if d := s.permsFor(c, req.RootID, rel); !d.Share {
		writeErr(c, http.StatusForbidden, errors.New("permission denied: share (manage acl)"))
		return
	}
	sc, err := s.scope(c, req.RootID, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	refs, err := s.aclRefs()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	// 主体解析+存在性校验（拒绝悬空条目），全部通过后才有替换动作
	ids := make([]int64, len(req.Entries))
	for i, e := range req.Entries {
		id, code, msg := refs.resolve(e.SubjectType, e.Subject, e.SubjectID)
		if code != 0 {
			writeErr(c, code, errors.New(msg))
			return
		}
		ids[i] = id
	}
	// 替换：先删旧条目再写新条目（物理键存储）
	old, err := s.Store.GetFolderACLs(sc.PhysID, sc.Phys)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	for _, o := range old {
		_ = s.Store.DeleteFolderACL(o.ID)
	}
	for i, e := range req.Entries {
		acl := &store.FolderACL{
			RootID: sc.PhysID, Path: sc.Phys,
			SubjectType: e.SubjectType, SubjectID: ids[i],
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
	view := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		v := refs.display(e)
		v["path"] = sc.Disp
		view = append(view, v)
	}
	logx.InfoT(traceOf(c), "acl: 用户端口权限更新 account=%q root=%s path=%q entries=%d",
		u.Username, req.RootID, req.Path, len(req.Entries))
	s.audit(c, store.AuditACLSet, "", 0, req.RootID, req.Path, fmt.Sprintf("%d entries", len(req.Entries)))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "entries": view})
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
	// 0.2.11 端口拆分：本 handler 只在用户端口（管理员会话已被 authCtx 拒绝）——
	// 只能管理自己授权范围内的 ACL 条目（跨 jail 条目直接拒绝）。
	if _, _, ok := s.displayOf(c, target.RootID, target.Path); !ok {
		writeErr(c, http.StatusForbidden, errors.New("acl entry outside your scope"))
		return
	}
	// 条目键为物理存储键，经 scopePhys 判定（jailed 用户不再误判 403）
	if !s.requirePermPhys(c, target.RootID, target.Path,
		func(d perm.Decision) bool { return d.Share }, "share (manage acl)") {
		return
	}
	if err := s.Store.DeleteFolderACL(id); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.audit(c, store.AuditACLDelete, "", 0, target.RootID, target.Path, fmt.Sprintf("acl id=%d", id))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleACLSubjects GET /api/v1/acl/subjects → 权限配置选择器数据（用户+用户组）。
// 0.2.11 需求4：用户行去数字 id/role——客户端只见 {account, nickname, subject}
// （subject 为账户化寻址串 "user:<账户>"，与 PUT /acl 往返一致）；
// 组行保留数字 id（组 id 非用户 id，不在收敛范围），但不再携带 members
// （成员列表是用户 id 数组，同样属不可见面）。
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
		// 需求12：客户端不可见管理员账号（管理台走 /admin/users）。
		if u.Role == store.RoleAdmin {
			continue
		}
		uv = append(uv, map[string]any{
			"account": u.Username, "nickname": u.Nickname, "subject": "user:" + u.Username,
		})
	}
	gv := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		gv = append(gv, map[string]any{"id": g.ID, "name": g.Name})
	}
	writeJSON(c, http.StatusOK, map[string]any{"users": uv, "groups": gv})
}
