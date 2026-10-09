package api

import (
	"github.com/gin-gonic/gin"

	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// issueSession 为登录用户签发认证凭证（0.2.3 去 JWT：客户端只拿到一个随机哈希
// 凭证，身份存服务端三层认证图）。有效期策略：普通用户=滑动闲置窗口
// （session.idleDays，每次认证请求续期）；DB 管理员（仅管理台可用）=绝对
// session.accessTTL 不滑动（保持"控制台每小时重登"节奏）。
// refreshToken 字段回传**同一凭证**（兼容别名：旧客户端 ≤0.3.3 的 401→refresh
// 链路原样工作，客户端实际只持有一个凭证）。
func (s *Server) issueSession(ctx context.Context, u *store.User) (accessToken, refreshToken string, err error) {
	ttl := s.Cfg.IdleDuration()
	if u.Role == store.RoleAdmin {
		ttl = s.Cfg.AccessTTLDuration()
	}
	cred, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindUser, UserID: u.ID, Username: u.Username, Role: u.Role,
		TTL: ttl, Persist: true,
	})
	if err != nil {
		return "", "", err
	}
	return cred, cred, nil
}

// userView 对外输出的用户信息（永不含密码哈希）。
// 超管为合成用户（0.1.0 需求32）：id=0 + super:true，无建号时间。
func userView(u *store.User) map[string]any {
	if u != nil && u.ID == store.SuperAdminID {
		return map[string]any{
			"id": u.ID, "username": u.Username, "role": u.Role, "super": true,
		}
	}
	return map[string]any{
		"id": u.ID, "username": u.Username, "role": u.Role,
		"allowedRoots": u.AllowedRoots, "createdAt": u.CreatedAt,
	}
}

// issueSuperSession 为超管签发认证凭证（嵌入当前凭据纪元，需求32/34 吊销机制：
// 纪元推进由 authCtx 逐请求核对，PUT /admin/super 时另显式 RevokeUser(0) 三层清）。
// 策略：管理台会话绝对 session.accessTTL（不滑动——控制台本就不做续期，见 app.js）。
// 有库时条目以 UserID=0 哨兵持久化（auth_sessions，与 DB 用户同表同语义）；
// 降级异常路径无法持久化——refreshToken 为空（同旧行为），重启即失效。
func (s *Server) issueSuperSession(ctx context.Context) (accessToken, refreshToken string, err error) {
	name, gerr := s.Super.Username()
	if gerr != nil {
		return "", "", gerr
	}
	gen, gerr := s.Super.Generation()
	if gerr != nil {
		return "", "", gerr
	}
	persist := s.dbReady()
	cred, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindUser, UserID: store.SuperAdminID, Username: name,
		Role: store.RoleAdmin, Super: true, Gen: gen,
		TTL: s.Cfg.AccessTTLDuration(), Persist: persist,
	})
	if err != nil {
		return "", "", err
	}
	if !persist {
		return cred, "", nil
	}
	return cred, cred, nil
}

// handleLogin POST /api/v1/auth/login {username,password}
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil || req.Username == "" {
		writeErr(c, http.StatusBadRequest, errors.New("username and password required"))
		return
	}
	// 登录失败防爆破：与分享口令共用 bruteGuard，键="login|用户名"+客户端 IP，
	// 同一来源 10 次失败锁定 15 分钟。
	ip := clientIP(c)
	bKey := "login|" + req.Username
	if s.guard.Blocked(bKey, ip) {
		s.audit(c, store.AuditLoginFail, req.Username, 0, "", "", "locked (too many failures) from "+ip)
		writeErr(c, http.StatusTooManyRequests, errors.New("too many failed attempts, try later"))
		return
	}
	// ---- 超管优先（0.1.0 需求32/33/36）：账户文件不依赖数据库，降级模式同样可登录；
	// 用户端口对超管一律 403（与 DB 管理员规则同文案，不泄露口令正确性）----
	if s.Super != nil {
		if m, err := s.Super.Match(req.Username); err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		} else if m {
			ok, verr := s.Super.Verify(req.Username, req.Password)
			if verr != nil {
				writeErr(c, http.StatusInternalServerError, verr)
				return
			}
			if !ok {
				s.guard.Fail(bKey, ip)
				s.audit(c, store.AuditLoginFail, req.Username, store.SuperAdminID, "", "", "bad username or password (superadmin)")
				writeErr(c, http.StatusUnauthorized, errors.New("bad username or password"))
				return
			}
			if !c.GetBool(adminModeKey) {
				s.audit(c, store.AuditLoginFail, req.Username, store.SuperAdminID, "", "", "admin login rejected on user port")
				writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
				return
			}
			s.guard.Reset(bKey, ip)
			s.audit(c, store.AuditLogin, req.Username, store.SuperAdminID, "", "", "superadmin login ok")
			at, rt, err := s.issueSuperSession(c.Request.Context())
			if err != nil {
				writeErr(c, http.StatusInternalServerError, err)
				return
			}
			writeJSON(c, http.StatusOK, map[string]any{
				"token":        at,
				"accessToken":  at,
				"refreshToken": rt,
				"expiresIn":    int(s.Cfg.AccessTTLDuration().Seconds()),
				"user":         userView(&store.User{ID: store.SuperAdminID, Username: req.Username, Role: store.RoleAdmin}),
			})
			return
		}
	}
	// ---- 普通/其他管理员账户：需要元数据库（0.1.0 起 503 判定从路由移入此处）----
	if !s.dbReady() {
		writeErr(c, http.StatusServiceUnavailable, errNoDB)
		return
	}
	u, err := s.Store.GetUserByName(req.Username)
	if err != nil {
		s.guard.Fail(bKey, ip)
		s.audit(c, store.AuditLoginFail, req.Username, 0, "", "", "bad username or password")
		writeErr(c, http.StatusUnauthorized, errors.New("bad username or password"))
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)); err != nil {
		s.guard.Fail(bKey, ip)
		s.audit(c, store.AuditLoginFail, req.Username, u.ID, "", "", "bad username or password")
		writeErr(c, http.StatusUnauthorized, errors.New("bad username or password"))
		return
	}
	// 需求11：管理员账号不可在客户端（用户端口）登录，仅限管理台。
	if u.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
		s.audit(c, store.AuditLoginFail, req.Username, u.ID, "", "", "admin login rejected on user port")
		writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
		return
	}
	s.guard.Reset(bKey, ip)
	s.audit(c, store.AuditLogin, u.Username, u.ID, "", "", "login ok")
	at, rt, err := s.issueSession(c.Request.Context(), u)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	expiresIn := int(s.Cfg.IdleDuration().Seconds()) // 距到期秒数（滑动窗口起点）
	if u.Role == store.RoleAdmin {
		expiresIn = int(s.Cfg.AccessTTLDuration().Seconds())
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"token":        at, // 兼容旧客户端字段名
		"accessToken":  at,
		"refreshToken": rt, // 兼容别名：与 token 同一凭证（客户端实际只持有一个）
		"expiresIn":    expiresIn,
		"user":         userView(u),
	})
}

// handleRefresh POST /api/v1/auth/refresh {refreshToken}
// 凭证轮换（0.2.3）：以现有凭证换取新随机凭证（旧即刻作废），身份与策略不变。
// 普通用户凭证的滑动续期在每次认证请求已自动完成（authCtx→Touch），本端点
// 承担显式轮换（旧客户端 401 自动刷新链路的兼容通道）；超管条目为绝对 TTL。
func (s *Server) handleRefresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := decode(c, &req); err != nil || req.RefreshToken == "" {
		writeErr(c, http.StatusBadRequest, errors.New("refreshToken required"))
		return
	}
	e, err := s.Auth.Lookup(c.Request.Context(), req.RefreshToken)
	if err != nil {
		writeErr(c, http.StatusUnauthorized, errors.New("invalid refresh token, please login again"))
		return
	}
	if e.Kind != store.AuthKindUser {
		writeErr(c, http.StatusUnauthorized, errors.New("invalid refresh token, please login again"))
		return
	}
	// UserID=0 哨兵 = 超管凭证（仅管理端口；纪元在重签时嵌入当前值——口令轮换/
	// 文件重置后 PUT /admin/super 已 RevokeUser(0) 三层清空，走到这里即有效凭据）。
	if e.UserID == store.SuperAdminID {
		if !c.GetBool(adminModeKey) || s.Super == nil {
			writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
			return
		}
		at, rt, err := s.issueSuperSession(c.Request.Context())
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		_ = s.Auth.Revoke(c.Request.Context(), req.RefreshToken) // 旧凭证作废（轮换）
		name, _ := s.Super.Username()
		writeJSON(c, http.StatusOK, map[string]any{
			"token":        at,
			"accessToken":  at,
			"refreshToken": rt,
			"expiresIn":    int(s.Cfg.AccessTTLDuration().Seconds()),
			"user":         userView(&store.User{ID: store.SuperAdminID, Username: name, Role: store.RoleAdmin}),
		})
		return
	}
	u, err := s.Store.GetUserByID(e.UserID) // 低频端点保留一次现读：档案回显完整 + 删号即拒
	if err != nil {
		_ = s.Auth.Revoke(c.Request.Context(), req.RefreshToken)
		writeErr(c, http.StatusUnauthorized, errors.New("user not found"))
		return
	}
	// 管理员的既有凭证不能在用户端口续期（需求11 规则不变，判据取库内现角色）。
	if u.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
		writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
		return
	}
	at, rt, err := s.issueSession(c.Request.Context(), u)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	_ = s.Auth.Revoke(c.Request.Context(), req.RefreshToken) // 旧凭证作废（轮换）
	expiresIn := int(s.Cfg.IdleDuration().Seconds())
	if u.Role == store.RoleAdmin {
		expiresIn = int(s.Cfg.AccessTTLDuration().Seconds())
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"token":        at,
		"accessToken":  at,
		"refreshToken": rt,
		"expiresIn":    expiresIn,
		"user":         userView(u),
	})
}

// handleLogout POST /api/v1/auth/logout {refreshToken}
// 撤权三层同步（L1 立删/L2 共享立删/L3 事实源删除；他实例 L1 陈旧 ≤ auth.memTTL）。
func (s *Server) handleLogout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := decode(c, &req); err == nil && req.RefreshToken != "" {
		if err := s.Auth.Revoke(c.Request.Context(), req.RefreshToken); err != nil {
			log.Printf("auth: warn: logout revoke failed: %v", err) // 旧行为一致：尽力而为，客户端本地一律清除
		}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// handleChangePassword POST /api/v1/auth/change-password {oldPassword,newPassword}
// （0.1.0 需求31）：登录用户自助改密——校验旧密码、bcrypt 存新哈希、
// 全部会话作废（reauthRequired：客户端据此清 token 并更新已记住的口令）。
// 超管不经此端点（账户文件管理，见 /admin/super）。
func (s *Server) handleChangePassword(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, errors.New("login required"))
		return
	}
	if u.ID == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户经管理台\"超级管理员\"页（账户文件）维护"))
		return
	}
	var req struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if len(req.NewPassword) < 6 {
		writeErr(c, http.StatusBadRequest, errors.New("newPassword too short (min 6)"))
		return
	}
	cur, err := s.Store.GetUserByID(u.ID) // 以库内哈希为准，不信任上下文缓存
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(cur.PasswordHash), []byte(req.OldPassword)); err != nil {
		// 防爆破与登录共用守卫（键含 IP，独立于登录计数键）
		ip := clientIP(c)
		s.guard.Fail("chpasswd|"+cur.Username, ip)
		if s.guard.Blocked("chpasswd|"+cur.Username, ip) {
			writeErr(c, http.StatusTooManyRequests, errors.New("too many failed attempts, try later"))
			return
		}
		s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "change-password: bad old password")
		writeErr(c, http.StatusUnauthorized, errors.New("old password incorrect"))
		return
	}
	if req.OldPassword == req.NewPassword {
		writeErr(c, http.StatusBadRequest, errors.New("new password must differ from old"))
		return
	}
	hash, err := bcryptHash(req.NewPassword)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := s.Store.UpdateUserPassword(cur.ID, hash); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	_ = s.Auth.RevokeUser(c.Request.Context(), cur.ID) // 认证图三层全清（含本次），强制重新登录
	s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "password changed by self")
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "reauthRequired": true})
}

// handleMe GET /api/v1/auth/me → 当前用户信息。
// 0.2.3：authCtx 注入的是认证图条目的合成用户（不含 allowedRoots/createdAt
// 档案字段）——账户页低频，直达库取完整档案回显（形状与旧版逐字段一致）。
func (s *Server) handleMe(c *gin.Context) {
	u := currentUser(c)
	if u != nil && u.ID != store.SuperAdminID && s.dbReady() {
		if fresh, err := s.Store.GetUserByID(u.ID); err == nil {
			u = fresh
		}
	}
	writeJSON(c, http.StatusOK, userView(u))
}

// handleHealth GET /api/v1/health（公开，balancer 健康检查也依赖它）。
// 加性字段（基线已含）：异常降级路径（元数据库不可用）db="none"（兼容 0.2.x 客户端；
// 0.1.1 起无库=文件存储 db="file"）。
func (s *Server) handleHealth(c *gin.Context) {
	db := "none"
	if s.dbReady() {
		db = s.Store.Driver()
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"ok":         true,
		"version":    version.Version,
		"api":        version.APIVersions,
		"instanceId": s.Cfg.InstanceID,
		"db":         db,
		"uptimeSec":  int64(time.Since(s.startedAt).Seconds()),
	})
}

// ---------- 工具 ----------

func bcryptHash(pw string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h), err
}

// createUser 唯一的用户创建入口（管理台建号专用，无自助注册）。
// 需求3：用户名必须可作目录名；建号成功后自动分配家目录
// <dataDir>/Users/<用户名>（含 .fmtrash 回收站所在 jail，用户只能访问该目录，
// 管理员可再追加其他分配）。
func (s *Server) createUser(username, password, role string, roots []string) (*store.User, int, error) {
	if err := fileops.UsernameAsDir(username); err != nil {
		return nil, http.StatusBadRequest, err
	}
	// 需求33：超管账户名保留（超管在文件、不在 users 表——若放同名 DB 账户进来，
	// handleLogin 的超管优先分支会永久遮蔽它，制造无法登录的死账户）。
	if s.Super != nil {
		if m, err := s.Super.Match(username); err == nil && m {
			return nil, http.StatusBadRequest, errors.New("该用户名已被超级管理员保留，请换一个")
		}
	}
	hash, err := bcryptHash(password)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	u := &store.User{Username: username, PasswordHash: hash, Role: role, AllowedRoots: roots}
	if err := s.Store.CreateUser(u); err != nil {
		if errors.Is(err, store.ErrExists) {
			return nil, http.StatusConflict, errors.New("username already exists")
		}
		return nil, http.StatusInternalServerError, err
	}
	if err := fileops.EnsureHomeFor(s.Store, s.Cfg.UsersDir(), u); err != nil {
		// 建目录失败回滚账户，避免出现"无家目录"的用户
		_ = s.Store.DeleteUser(u.ID)
		return nil, http.StatusInternalServerError, err
	}
	u.PasswordHash = ""
	return u, http.StatusOK, nil
}
