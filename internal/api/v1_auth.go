package api

import (
	"github.com/gin-gonic/gin"

	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// issueSession 为登录用户签发 access token + refresh 会话（需求22）。
// refresh 令牌只入库哈希，明文仅本次响应下发；后续每次 refresh 轮换。
func (s *Server) issueSession(u *store.User) (accessToken, refreshToken string, err error) {
	accessToken, err = auth.IssueAccess(s.Cfg.JWTSecret, u.ID, u.Username, u.Role, s.Cfg.AccessTTLDuration())
	if err != nil {
		return "", "", err
	}
	rt, hash, err := auth.NewRefreshToken()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	sess := &store.Session{
		UserID:    u.ID,
		TokenHash: hash,
		ExpiresAt: now.Add(s.Cfg.IdleDuration()),
	}
	if err := s.Store.CreateSession(sess); err != nil {
		return "", "", err
	}
	return accessToken, rt, nil
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

// issueSuperSession 为超管签发访问令牌（嵌入当前凭据纪元，需求32/34 吊销机制）。
// 有库时 refresh 会话以 UserID=0 哨兵入库（与 DB 用户同表同轮换语义）；
// 降级模式无法持久化会话——refreshToken 为空，accessTTL 即会话长度，
// 到期后管理台回登录页（控制台本就不做令牌续期，见 app.js）。
func (s *Server) issueSuperSession() (accessToken, refreshToken string, err error) {
	name, gerr := s.Super.Username()
	if gerr != nil {
		return "", "", gerr
	}
	gen, gerr := s.Super.Generation()
	if gerr != nil {
		return "", "", gerr
	}
	accessToken, err = auth.IssueSuperAccess(s.Cfg.JWTSecret, name, gen, s.Cfg.AccessTTLDuration())
	if err != nil {
		return "", "", err
	}
	if !s.dbReady() {
		return accessToken, "", nil
	}
	rt, hash, err := auth.NewRefreshToken()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	sess := &store.Session{
		UserID:    store.SuperAdminID,
		TokenHash: hash,
		ExpiresAt: now.Add(s.Cfg.IdleDuration()),
	}
	if err := s.Store.CreateSession(sess); err != nil {
		if !s.dbReady() { // 与热切换竞态：检查后库被摘除，按降级路径处理
			return accessToken, "", nil
		}
		return "", "", err
	}
	return accessToken, rt, nil
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
	// 登录失败防爆破（0.3.1）：与分享口令共用 bruteGuard，键="login|用户名"+客户端 IP，
	// 同一来源 10 次失败锁定 15 分钟。
	ip := clientIP(c)
	bKey := "login|" + req.Username
	if s.guard.Blocked(bKey, ip) {
		s.audit(c, store.AuditLoginFail, req.Username, 0, "", "", "locked (too many failures) from "+ip)
		writeErr(c, http.StatusTooManyRequests, errors.New("too many failed attempts, try later"))
		return
	}
	// ---- 超管优先（0.1.0 需求32/33/36）：账户文件不依赖数据库，降级模式同样可登录；
	// 用户端口对超管一律 403（与 0.6.0 的 DB 管理员规则同文案，不泄露口令正确性）----
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
			at, rt, err := s.issueSuperSession()
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
	// 0.6.0（需求11）：管理员账号不可在客户端（用户端口）登录，仅限管理台。
	if u.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
		s.audit(c, store.AuditLoginFail, req.Username, u.ID, "", "", "admin login rejected on user port")
		writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
		return
	}
	s.guard.Reset(bKey, ip)
	s.audit(c, store.AuditLogin, u.Username, u.ID, "", "", "login ok")
	at, rt, err := s.issueSession(u)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"token":        at, // 兼容旧客户端字段名
		"accessToken":  at,
		"refreshToken": rt,
		"expiresIn":    int(s.Cfg.AccessTTLDuration().Seconds()),
		"user":         userView(u),
	})
}

// handleRefresh POST /api/v1/auth/refresh {refreshToken}
// 轮换 refresh 令牌并滑动续期：活跃使用则过期时间顺延（最长闲置 3 周）。
func (s *Server) handleRefresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := decode(c, &req); err != nil || req.RefreshToken == "" {
		writeErr(c, http.StatusBadRequest, errors.New("refreshToken required"))
		return
	}
	hash := auth.HashToken(req.RefreshToken)
	sess, err := s.Store.GetSessionByTokenHash(hash)
	if err != nil {
		writeErr(c, http.StatusUnauthorized, errors.New("invalid refresh token, please login again"))
		return
	}
	now := time.Now()
	if sess.ExpiresAt.Before(now) {
		_ = s.Store.DeleteSession(sess.ID)
		writeErr(c, http.StatusUnauthorized, errors.New("session expired (idle over limit), please login again"))
		return
	}
	// 0.1.0 需求32：UserID=0 哨兵 = 超管会话（仅管理端口；新令牌嵌入当前纪元，
	// 口令轮换/文件重置后 PUT /admin/super 已删除旧会话，走到这里即有效凭据）。
	if sess.UserID == store.SuperAdminID {
		if !c.GetBool(adminModeKey) || s.Super == nil {
			writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
			return
		}
		at, rt, err := s.issueSuperSession()
		if err != nil {
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		_ = s.Store.DeleteSession(sess.ID)
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
	u, err := s.Store.GetUserByID(sess.UserID)
	if err != nil {
		_ = s.Store.DeleteSession(sess.ID)
		writeErr(c, http.StatusUnauthorized, errors.New("user not found"))
		return
	}
	// 0.6.0（需求11）：admin 的既有会话也不能在用户端口续期。
	if u.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
		writeErr(c, http.StatusForbidden, errors.New("管理员账号请使用服务端管理界面登录"))
		return
	}
	at, rt, err := s.issueSession(u)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	_ = s.Store.DeleteSession(sess.ID) // 旧令牌作废（轮换）
	writeJSON(c, http.StatusOK, map[string]any{
		"token":        at,
		"accessToken":  at,
		"refreshToken": rt,
		"expiresIn":    int(s.Cfg.AccessTTLDuration().Seconds()),
		"user":         userView(u),
	})
}

// handleLogout POST /api/v1/auth/logout {refreshToken}
func (s *Server) handleLogout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refreshToken"`
	}
	if err := decode(c, &req); err == nil && req.RefreshToken != "" {
		if sess, err := s.Store.GetSessionByTokenHash(auth.HashToken(req.RefreshToken)); err == nil {
			_ = s.Store.DeleteSession(sess.ID)
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
	_ = s.Store.DeleteSessionsByUser(cur.ID) // 全部会话作废（含本次），强制重新登录
	s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "password changed by self")
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "reauthRequired": true})
}

// handleMe GET /api/v1/auth/me → 当前用户信息。
func (s *Server) handleMe(c *gin.Context) {
	writeJSON(c, http.StatusOK, userView(currentUser(c)))
}

// handleHealth GET /api/v1/health（公开，balancer 健康检查也依赖它）。
// 0.3.0 加性字段：降级模式（未配置元数据库）db="none"（新增字段，兼容 0.2.x 客户端）。
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

// createUser 唯一的用户创建入口（管理台建号 / 引导注册共用，0.6.0 起无自助注册）。
// 0.7.0 需求3：用户名必须可作目录名；建号成功后自动分配家目录
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
