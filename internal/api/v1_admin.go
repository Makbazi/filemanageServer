package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// 管理员接口（需求23）：供内嵌 /admin Web 界面与客户端管理页调用。

// handleAdminInfo GET /api/v1/admin/info → 系统信息概览。
func (s *Server) handleAdminInfo(c *gin.Context) {
	roots := make([]map[string]string, 0, len(s.Cfg.Roots))
	for i := range s.Cfg.Roots {
		rt := &s.Cfg.Roots[i]
		roots = append(roots, map[string]string{"id": rt.ID, "name": rt.Name, "path": rt.Path})
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"version":            version.Version,
		"apiVersions":        version.APIVersions,
		"instanceId":         s.Cfg.InstanceID,
		"dbDriver":           s.Store.Driver(),
		"dataDir":            s.Cfg.DataDir, // 运行数据根目录（0.7.0）
		"usersDir":           s.Cfg.UsersDir(),
		"schemaVersion":      s.Cfg.SchemaVersion,
		"listen":             s.Cfg.Listen,
		"adminListen":        s.Cfg.AdminListen,  // 管理面独立端口（0.5.0）
		"adminAccess":        s.Cfg.AdminAccess,  // 管理界面 IP 准入（0.1.0 需求38，只读展示）
		"superAdminUsername": s.superAdminName(), // 超管账户名（0.1.0 需求32；仅名称事实，不含口令）
		"maxUploadBytes":     s.Cfg.MaxUploadBytes,
		"lanOnly":            s.Cfg.LANOnly,
		"anonymousEnabled":   s.Cfg.Anonymous.Enabled,
		"tlsEnabled":         s.Cfg.TLS.Enabled,
		"rateLimit":          s.Cfg.RateLimit,
		"versions":           s.Cfg.Versions,
		"trashRetentionDays": s.Cfg.TrashRetentionDays,
		"session":            s.Cfg.Session,
		"aiProvider":         s.AI.Name(),
		"roots":              roots,
		"uptimeSec":          int64(time.Since(s.startedAt).Seconds()),
		"goVersion":          runtime.Version(),
	})
}

// ---------- 用户管理 ----------

// handleAdminListUsers GET /admin/users → 用户列表（附 0.5.0 目录分配摘要）。
func (s *Server) handleAdminListUsers(c *gin.Context) {
	users, err := s.Store.ListUsers()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	allRoots, err := s.Store.ListAllUserRoots()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	byUser := map[int64][]*store.UserRoot{}
	for _, r := range allRoots {
		byUser[r.UserID] = append(byUser[r.UserID], r)
	}
	out := make([]map[string]any, 0, len(users))
	for _, u := range users {
		v := userView(u)
		rs := byUser[u.ID]
		if rs == nil {
			rs = []*store.UserRoot{}
		}
		v["userRoots"] = rs
		out = append(out, v)
	}
	writeJSON(c, http.StatusOK, out)
}

func (s *Server) handleAdminCreateUser(c *gin.Context) {
	var req struct {
		Username     string   `json:"username"`
		Password     string   `json:"password"`
		Role         string   `json:"role"`
		AllowedRoots []string `json:"allowedRoots"`
	}
	if err := decode(c, &req); err != nil || req.Username == "" || len(req.Password) < 6 {
		writeErr(c, http.StatusBadRequest, errors.New("username + password(>=6) required"))
		return
	}
	if req.Role != store.RoleAdmin {
		req.Role = store.RoleUser
	}
	// 需求33：管理员账户的创建是超管专属能力（其他管理员只能创建普通用户）；
	// 非超管显式拒绝而非静默降级（旧管理台的 role=admin 请求将收到 403）。
	if req.Role == store.RoleAdmin && !isSuperAdmin(c) {
		writeErr(c, http.StatusForbidden, errors.New("只有超级管理员可以创建管理员账户"))
		return
	}
	u, code, err := s.createUser(req.Username, req.Password, req.Role, req.AllowedRoots)
	if err != nil {
		writeErr(c, code, err)
		return
	}
	s.audit(c, store.AuditUserCreate, "", 0, "", "", fmt.Sprintf("created user %s role=%s", req.Username, req.Role))
	writeJSON(c, http.StatusOK, userView(u))
}

func (s *Server) handleAdminUpdateUser(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	// 需求33：超管不在库内、由账户文件管理；先加载目标做分层判定
	//（0.7.x 之前该接口不加载目标、盲改——现在必须先知道对方是不是管理员）。
	if id == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户由服务端账户文件管理（见管理台\"超级管理员\"页）"))
		return
	}
	target, err := s.Store.GetUserByID(id)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if target.Role == store.RoleAdmin && !isSuperAdmin(c) {
		writeErr(c, http.StatusForbidden, errors.New("只有超级管理员可以修改管理员账户"))
		return
	}
	var req struct {
		Password     string   `json:"password"`
		Role         string   `json:"role"`
		AllowedRoots []string `json:"allowedRoots"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Password != "" {
		if len(req.Password) < 6 {
			writeErr(c, http.StatusBadRequest, errors.New("password too short"))
			return
		}
		hash, err := bcryptHash(req.Password)
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		if err := s.Store.UpdateUserPassword(id, hash); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		_ = s.Store.DeleteSessionsByUser(id) // 改密后强制重新登录
	}
	if req.Role != "" {
		role := store.RoleUser
		if req.Role == store.RoleAdmin {
			role = store.RoleAdmin
		}
		// 需求33：角色升降（普通用户↔管理员）是超管专属；目标已是管理员的
		// 情况已被上面的目标检查拦截，这里是"把普通用户提为管理员"的方向。
		if role != target.Role && !isSuperAdmin(c) {
			writeErr(c, http.StatusForbidden, errors.New("只有超级管理员可以变更账户角色"))
			return
		}
		if err := s.Store.UpdateUserRole(id, role); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	if req.AllowedRoots != nil {
		if err := s.Store.UpdateUserRoots(id, req.AllowedRoots); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	s.audit(c, store.AuditUserUpdate, "", 0, "", "", fmt.Sprintf("updated user id=%d (role/password)", id))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminDeleteUser(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	// 需求33：id=0 是超管哨兵——DeleteUser(0) 的级联会清掉 sessions 里
	// user_id=0 的全部超管会话（等同吊销超管），必须显式挡住；
	// 管理员账户的删除同样超管专属。
	if id == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户由服务端账户文件管理，不可在此删除"))
		return
	}
	target, terr := s.Store.GetUserByID(id)
	if terr != nil {
		writeErr(c, statusOf(terr), terr)
		return
	}
	// 自删守卫前置（0.1.0）：对超管与普通管理员都先给"不能删自己"的准确语义，
	// 再进入管理员分层判定。
	if me := currentUser(c); me != nil && me.ID == id {
		writeErr(c, http.StatusBadRequest, errors.New("cannot delete yourself"))
		return
	}
	if target.Role == store.RoleAdmin && !isSuperAdmin(c) {
		writeErr(c, http.StatusForbidden, errors.New("只有超级管理员可以删除管理员账户"))
		return
	}
	if err := s.Store.DeleteUser(id); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.audit(c, store.AuditUserDelete, "", 0, "", "", fmt.Sprintf("deleted user id=%d", id))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 用户根分配（0.5.0 jail 模型：管理员分配目录） ----------

// handleAdminListAllUserRoots GET /admin/userroots → 全部用户分配（总览列）。
func (s *Server) handleAdminListAllUserRoots(c *gin.Context) {
	urs, err := s.Store.ListAllUserRoots()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if urs == nil {
		urs = []*store.UserRoot{}
	}
	writeJSON(c, http.StatusOK, urs)
}

// handleAdminGetUserRoots GET /admin/users/:id/roots → 某用户的分配列表。
func (s *Server) handleAdminGetUserRoots(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	if id == store.SuperAdminID { // 超管无库内分配（需求32/33）
		writeErr(c, http.StatusBadRequest, errors.New("super admin has no db-managed assignments"))
		return
	}
	urs, err := s.Store.ListUserRoots(id)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if urs == nil {
		urs = []*store.UserRoot{}
	}
	writeJSON(c, http.StatusOK, urs)
}

// handleAdminSetUserRoots PUT /admin/users/:id/roots {roots:[{rootId,subPath,alias}]}
// 全量替换用户的目录分配。subPath 相对物理根（""=整根）；目录不存在时自动创建。
func (s *Server) handleAdminSetUserRoots(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	if id == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户由服务端账户文件管理（见管理台\"超级管理员\"页）"))
		return
	}
	target, err := s.Store.GetUserByID(id)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if target.Role == store.RoleAdmin && !isSuperAdmin(c) { // 需求33：管理员的目录分配亦超管专属
		writeErr(c, http.StatusForbidden, errors.New("只有超级管理员可以变更管理员账户的目录分配"))
		return
	}
	var req struct {
		Roots []struct {
			RootID  string `json:"rootId"`
			SubPath string `json:"subPath"`
			Alias   string `json:"alias"`
		} `json:"roots"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	seen := map[string]bool{}
	urs := make([]*store.UserRoot, 0, len(req.Roots))
	for _, r := range req.Roots {
		root := s.Roots[r.RootID]
		if root == nil {
			writeErr(c, http.StatusBadRequest, fmt.Errorf("unknown root %q", r.RootID))
			return
		}
		sub := fileops.CleanRel(r.SubPath)
		// 0.7.0 需求3：内部家目录根的分配即 <dataDir>/Users/<用户名>，
		// 只允许指向本人用户名（防把他人家目录分配给该用户），且回收站内核不可入分配。
		if r.RootID == store.HomeRootID {
			if sub != fileops.CleanRel(target.Username) || fileops.UsernameAsDir(target.Username) != nil {
				writeErr(c, http.StatusBadRequest, fmt.Errorf("home root assignment must be (%q, %q)", store.HomeRootID, target.Username))
				return
			}
			if hasSegment(sub, fileops.HomeTrashName) {
				writeErr(c, http.StatusBadRequest, errReservedName)
				return
			}
		}
		if sub != "" {
			abs, jerr := fileops.JoinRoot(root.Path, sub)
			if jerr != nil {
				writeErr(c, http.StatusBadRequest, jerr)
				return
			}
			if mkerr := os.MkdirAll(abs, 0o755); mkerr != nil {
				writeErr(c, http.StatusInternalServerError, mkerr)
				return
			}
		}
		key := r.RootID + "\x00" + sub
		if seen[key] {
			continue
		}
		seen[key] = true
		urs = append(urs, &store.UserRoot{UserID: id, RootID: r.RootID, SubPath: sub, Alias: r.Alias})
	}
	// 0.7.0：本接口为全量替换——丢失家目录行则自动补回（家目录随账号生灭、
	// 不可经分配编辑器摘除；用户名不合法的历史遗留不阻塞保存）。
	hasHome := false
	for _, r := range urs {
		if r.RootID == store.HomeRootID {
			hasHome = true
			break
		}
	}
	if !hasHome && fileops.UsernameAsDir(target.Username) == nil {
		if _, serr := os.Stat(s.Cfg.HomeDir(target.Username)); serr == nil {
			urs = append(urs, &store.UserRoot{UserID: id, RootID: store.HomeRootID, SubPath: target.Username, Alias: "Home"})
		}
	}
	if err := s.Store.SetUserRoots(id, urs); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	s.audit(c, store.AuditRootsAssign, "", 0, "", "", fmt.Sprintf("assigned %d dirs to user id=%d", len(urs), id))
	out, _ := s.Store.ListUserRoots(id)
	if out == nil {
		out = []*store.UserRoot{}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "roots": out})
}

// handleAdminBrowseFS GET /admin/fs?rootId=&path= → 物理根内目录浏览（仅目录，
// 分配目录时的选择器；只存在于管理端口，用户端口无此能力）。
func (s *Server) handleAdminBrowseFS(c *gin.Context) {
	rootID := c.Request.URL.Query().Get("rootId")
	rel := fileops.CleanRel(c.Request.URL.Query().Get("path"))
	root := s.Roots[rootID]
	if root == nil {
		writeErr(c, http.StatusBadRequest, fmt.Errorf("unknown root %q", rootID))
		return
	}
	abs, err := fileops.JoinRoot(root.Path, rel)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	dirs, err := os.ReadDir(abs)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	type dirView struct {
		Name string `json:"name"`
		Path string `json:"path"` // 相对物理根
	}
	out := make([]dirView, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		// 0.7.0：家目录内嵌回收站不进分配选择器（管理端口同样不可入 jail 基座）
		if rootID == store.HomeRootID && d.Name() == fileops.HomeTrashName {
			continue
		}
		p := rel
		if p == "" {
			p = d.Name()
		} else {
			p = p + "/" + d.Name()
		}
		out = append(out, dirView{Name: d.Name(), Path: p})
	}
	writeJSON(c, http.StatusOK, out)
}

// ---------- 用户组管理（需求9） ----------

func (s *Server) handleAdminListGroups(c *gin.Context) {
	groups, err := s.Store.ListGroups()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if groups == nil {
		groups = []*store.Group{}
	}
	writeJSON(c, http.StatusOK, groups)
}

func (s *Server) handleAdminCreateGroup(c *gin.Context) {
	var req struct {
		Name    string  `json:"name"`
		Members []int64 `json:"members"`
	}
	if err := decode(c, &req); err != nil || req.Name == "" {
		writeErr(c, http.StatusBadRequest, errors.New("name required"))
		return
	}
	g := &store.Group{Name: req.Name, Members: req.Members}
	if err := s.Store.CreateGroup(g); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, g)
}

func (s *Server) handleAdminUpdateGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	var req struct {
		Name    string  `json:"name"`
		Members []int64 `json:"members"`
	}
	if err := decode(c, &req); err != nil || req.Name == "" {
		writeErr(c, http.StatusBadRequest, errors.New("name required"))
		return
	}
	g := &store.Group{ID: id, Name: req.Name, Members: req.Members}
	if err := s.Store.UpdateGroup(g); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminDeleteGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return
	}
	if err := s.Store.DeleteGroup(id); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	// 级联清理该组的 ACL 条目
	_ = s.Store.DeleteFolderACLsBySubject(store.SubjectGroup, id)
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- ACL / 分享链接总览（管理台用） ----------

// handleAdminListAudits GET /api/v1/admin/audits?limit=&userId=（0.5.1 审计日志）。
func (s *Server) handleAdminListAudits(c *gin.Context) {
	limit := 200
	if v, err := strconv.Atoi(c.Request.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	uid, _ := strconv.ParseInt(c.Request.URL.Query().Get("userId"), 10, 64)
	events, err := s.Store.ListAuditEvents(limit, uid)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if events == nil {
		events = []*store.AuditEvent{}
	}
	writeJSON(c, http.StatusOK, events)
}

// handleAdminListACLs GET /api/v1/admin/acls?rootId=（rootId 空 = 全部）。
func (s *Server) handleAdminListACLs(c *gin.Context) {
	acls, err := s.Store.ListRootACLs(c.Request.URL.Query().Get("rootId"))
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if acls == nil {
		acls = []*store.FolderACL{}
	}
	writeJSON(c, http.StatusOK, acls)
}

// handleAdminListLinks GET /api/v1/admin/links → 全部分享链接。
func (s *Server) handleAdminListLinks(c *gin.Context) {
	links, err := s.Store.ListShareLinks(0)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(links))
	for _, l := range links {
		out = append(out, linkViewFull(l)) // 管理端口保留物理键
	}
	writeJSON(c, http.StatusOK, out)
}
