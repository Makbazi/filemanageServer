package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"filemanageServer/internal/fileops"
	"filemanageServer/internal/logx"
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
	out := map[string]any{
		"version":            version.Version,
		"apiVersions":        version.APIVersions,
		"instanceId":         s.Cfg.InstanceID,
		"dbDriver":           s.Store.Driver(),
		"dataDir":            s.Cfg.DataDir, // 运行数据根目录
		"usersDir":           s.Cfg.UsersDir(),
		"schemaVersion":      s.Cfg.SchemaVersion,
		"listen":             s.Cfg.Listen,
		"adminListen":        s.Cfg.AdminListen,  // 管理面独立端口
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
	}
	// 0.2.12 BFF：登录态由 Cookie 承载、前端不持凭证——info 顺带回显当前管理
	// 员概要（userView 管理面自有字段），控制台刷新页面即可恢复顶栏显示。
	if cu := currentUser(c); cu != nil {
		out["who"] = userView(cu)
	}
	writeJSON(c, http.StatusOK, out)
}

// ---------- 用户管理 ----------

// handleAdminListUsers GET /admin/users → 用户列表（附目录分配摘要）。
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
		Username     string   `json:"username"` // 0.2.11 语义=账户（唯一、不可改）
		Nickname     string   `json:"nickname"` // 0.2.11 可改可重复；空=显示回退账户
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
	u, code, err := s.createUser(req.Username, req.Nickname, req.Password, req.Role, req.AllowedRoots)
	if err != nil {
		writeErr(c, code, err)
		return
	}
	logx.InfoT(traceOf(c), "admin: 创建账户 account=%q role=%s id=%d", req.Username, req.Role, u.ID)
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
	//（此前该接口不加载目标、盲改——现在必须先知道对方是不是管理员）。
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
		Nickname     *string  `json:"nickname"` // 0.2.11：指针区分"未传/传入（含空串清除）"
		Email        *string  `json:"email"`    // 0.2.12：同上（登记邮箱；变更即清验证态）
		Role         string   `json:"role"`
		AllowedRoots []string `json:"allowedRoots"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	// 0.2.12：管理员登记/修改邮箱——不做 SMTP 验证（验证=用户自助 OTP 流程），
	// 值变化即 emailVerified=false；若该账户二验通道是 email，改后下次登录会被
	// 引导报错（须先重新验证邮箱），此处不静默改 method（治理入口=PUT /users/:id/mfa）。
	if req.Email != nil && target.Role == store.RoleUser {
		em := strings.ToLower(strings.TrimSpace(*req.Email))
		if em != "" && !validEmail(em) {
			writeErr(c, http.StatusBadRequest, errors.New("邮箱地址格式不正确"))
			return
		}
		if em != target.Email {
			verified := false
			if err := s.Store.UpdateUserAuth(target.ID, em, verified, target.MfaMethod, target.MfaSecret); err != nil {
				writeErr(c, statusOf(err), err)
				return
			}
			logx.InfoT(traceOf(c), "admin: 登记邮箱变更 id=%d account=%q clearedVerified=%v", target.ID, target.Username, target.EmailVerified)
		}
		target.Email, target.EmailVerified = em, false
	}
	if req.Nickname != nil {
		nick, code, nerr := normalizeNickname(*req.Nickname)
		if nerr != nil {
			writeErr(c, code, nerr)
			return
		}
		if err := s.Store.UpdateUserNickname(id, nick); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
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
		_ = s.Auth.RevokeUser(c.Request.Context(), id) // 改密后强制重新登录（认证图三层全清）
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
		// 角色变更即吊销该用户全部认证凭证：认证图条目的 role 是登录时刻快照
		// （不再逐请求查库），撤权强制重登以拿到新角色（0.2.3）。
		if role != target.Role {
			_ = s.Auth.RevokeUser(c.Request.Context(), id)
		}
	}
	if req.AllowedRoots != nil {
		if err := s.Store.UpdateUserRoots(id, req.AllowedRoots); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
	}
	logx.InfoT(traceOf(c), "admin: 更新账户 id=%d password=%v role=%v nickname=%v",
		id, req.Password != "", req.Role != "", req.Nickname != nil)
	s.audit(c, store.AuditUserUpdate, "", 0, "", "", fmt.Sprintf("updated user id=%d (role/password/nickname)", id))
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
	// Store.DeleteUser 已级联清 L3 auth_sessions；再清认证中心 L1/L2（散点/他实例
	// 凭证靠 L3 删除证伪 + memTTL 陈旧回源兜底，此处主动清本实例上层）。
	_ = s.Auth.RevokeUser(c.Request.Context(), id)
	logx.WarnT(traceOf(c), "admin: 删除账户 id=%d account=%q（家目录与回收站目录以孤儿态保留磁盘，同名重建不复用）", id, target.Username)
	s.audit(c, store.AuditUserDelete, "", 0, "", "", fmt.Sprintf("deleted user id=%d", id))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 用户根分配（jail 模型：管理员分配目录） ----------

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
		// 需求3：内部家目录根的分配即 <dataDir>/Users/user_<账户>_<用户id>
		// （0.2.11 命名），只允许指向本人家目录（防把他人家目录分配给该用户），
		// 且回收站内核（旧 .fmtrash/新 _trash 名）不可入分配。
		if r.RootID == store.HomeRootID {
			want := fileops.HomeDirName(target.Username, target.ID)
			legacy := fileops.CleanRel(target.Username) // 未迁移存量行旧名（v8 迁移前合法）
			if (sub != want && sub != legacy) || fileops.UsernameAsDir(target.Username) != nil {
				writeErr(c, http.StatusBadRequest, fmt.Errorf("home root assignment must be (%q, %q)", store.HomeRootID, want))
				return
			}
			if hasSegment(sub, fileops.HomeTrashName) || sub == fileops.UserTrashDirName(want) {
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
	// 本接口为全量替换——丢失家目录行则自动补回（家目录随账号生灭、
	// 不可经分配编辑器摘除；账户名不合法的历史遗留不阻塞保存）。
	hasHome := false
	for _, r := range urs {
		if r.RootID == store.HomeRootID {
			hasHome = true
			break
		}
	}
	if !hasHome && fileops.UsernameAsDir(target.Username) == nil {
		// 优先新布局名（0.2.11）；旧布局目录（v8 迁移前）也认，按实存名补行。
		home := fileops.HomeDirName(target.Username, target.ID)
		if _, serr := os.Stat(fileops.HomeDirPath(s.Cfg.UsersDir(), home)); serr == nil {
			urs = append(urs, &store.UserRoot{UserID: id, RootID: store.HomeRootID, SubPath: home, Alias: "Home"})
		} else if _, serr := os.Stat(fileops.HomeDirPath(s.Cfg.UsersDir(), target.Username)); serr == nil {
			urs = append(urs, &store.UserRoot{UserID: id, RootID: store.HomeRootID, SubPath: target.Username, Alias: "Home"})
		}
	}
	if err := s.Store.SetUserRoots(id, urs); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	logx.InfoT(traceOf(c), "admin: 目录分配已保存 user=%q dirs=%d", target.Username, len(urs))
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
		// 家目录内嵌回收站不进分配选择器（管理端口同样不可入 jail 基座）
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

// handleAdminListAudits GET /api/v1/admin/audits?limit=&userId=（审计日志）。
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

// handleAdminACLPut PUT /api/v1/admin/acl {rootId, path, entries:[{subjectType, subjectId, perms}]}
// （0.2.11 端口彻底拆分：管理台专属 ACL 写入——物理键直存、管理员全权、
// 无 jail 翻译、无角色分叉；用户端口 ACL handler 不再服务管理台）。
// entries 空数组 = 清除该路径全部 ACL；public 主体强制无下载/上传/删除/分享
// （需求6 底线与用户端口一致）。
func (s *Server) handleAdminACLPut(c *gin.Context) {
	var req struct {
		RootID  string        `json:"rootId"`
		Path    string        `json:"path"`
		Entries []aclEntryReq `json:"entries"`
	}
	if err := decode(c, &req); err != nil || req.RootID == "" {
		writeErr(c, http.StatusBadRequest, errors.New("rootId required"))
		return
	}
	root := s.Roots[req.RootID]
	if root == nil {
		writeErr(c, http.StatusBadRequest, fmt.Errorf("unknown root %q", req.RootID))
		return
	}
	phys := fileops.CleanRel(req.Path)
	if _, err := fileops.JoinRoot(root.Path, phys); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	u := currentUser(c) // 组链已 requireAdmin
	// 主体存在性校验（拒绝悬空条目；管理面数字 subjectId——它有 /admin/users）
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
	old, err := s.Store.GetFolderACLs(req.RootID, phys)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	for _, o := range old {
		_ = s.Store.DeleteFolderACL(o.ID)
	}
	for _, e := range req.Entries {
		acl := &store.FolderACL{
			RootID: req.RootID, Path: phys,
			SubjectType: e.SubjectType, SubjectID: e.SubjectID,
			Perms: e.Perms, CreatedBy: u.ID,
		}
		if e.SubjectType == store.SubjectPublic {
			acl.SubjectID = 0
			acl.Perms.Download, acl.Perms.Upload, acl.Perms.Delete, acl.Perms.Share = false, false, false, false
		}
		if err := s.Store.SetFolderACL(acl); err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
	}
	entries, _ := s.Store.GetFolderACLs(req.RootID, phys)
	if entries == nil {
		entries = []*store.FolderACL{}
	}
	logx.InfoT(traceOf(c), "admin: ACL 更新 root=%s path=%q entries=%d", req.RootID, phys, len(req.Entries))
	s.audit(c, store.AuditACLSet, "", 0, req.RootID, phys, fmt.Sprintf("admin set %d entries", len(req.Entries)))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "entries": entries})
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
