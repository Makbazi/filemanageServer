package api

import (
	"github.com/gin-gonic/gin"

	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/fileops"
	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// issueSession 为登录用户签发认证凭证（0.2.3 去 JWT：客户端只拿到一个随机哈希
// 凭证，身份存服务端三层认证图；0.2.4 起响应面收敛——**只回这一个凭证**，
// 不再带 accessToken/refreshToken/expiresIn 别名）。有效期策略：
// 普通用户=滑动闲置窗口 session.idleMinutes（默认 **1 小时**，每次认证请求
// 服务端无感续期、凭证字符串不变，超过未使用必须重新登录）；
// DB 管理员（仅管理台可用）=绝对 session.accessTTL 不滑动（保持"控制台
// 每小时重登"节奏，且不做客户端绑定）。
// clientID：客户端唯一标识（0.2.4）——非空即把本凭证绑定到该客户端，
// authCtx 逐请求核对 X-Client-Id（不符=401，需重新登录）；多客户端同账号
// 各持己凭证互不挤占。管理台（role=admin）会话一律不绑定（控制台无标识头）。
func (s *Server) issueSession(ctx context.Context, u *store.User, clientID string) (string, error) {
	ttl := s.Cfg.IdleDuration()
	if u.Role == store.RoleAdmin {
		ttl = s.Cfg.AccessTTLDuration()
		clientID = ""
	}
	cred, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindUser, UserID: u.ID, Username: u.Username, Role: u.Role,
		ClientID: clientID, TTL: ttl, Persist: true,
	})
	if err != nil {
		return "", err
	}
	return cred, nil
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
// 策略：管理台会话绝对 session.accessTTL（不滑动——控制台本就不做续期，见 app.js），
// 不做客户端绑定（管理台无标识头）。0.2.4 起只返回单一凭证。
// 有库时条目以 UserID=0 哨兵持久化（auth_sessions，与 DB 用户同表同语义）；
// 降级异常路径无法持久化（Persist=false，同旧行为），重启即失效。
func (s *Server) issueSuperSession(ctx context.Context) (string, error) {
	name, gerr := s.Super.Username()
	if gerr != nil {
		return "", gerr
	}
	gen, gerr := s.Super.Generation()
	if gerr != nil {
		return "", gerr
	}
	persist := s.dbReady()
	cred, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindUser, UserID: store.SuperAdminID, Username: name,
		Role: store.RoleAdmin, Super: true, Gen: gen,
		TTL: s.Cfg.AccessTTLDuration(), Persist: persist,
	})
	if err != nil {
		return "", err
	}
	return cred, nil
}

// handleLogin POST /api/v1/auth/login {username,password,clientId?}
// 0.2.4 响应收敛：用户端口普通用户成功**只回 {"token}**（单一无透明随机凭证，
// 身份/角色/有效期全存服务端认证图，客户端零格式假设）；管理台登录（admin 端口，
// adminModeKey）保留 {"token","user"}——控制台靠 user.role 做进入门禁与顶栏显示。
// clientId：客户端唯一标识（可选，≤64 字符）——提供即绑定本凭证（逐请求核对
// X-Client-Id 头）；不提供=未绑定（旧客户端/移动端，行为同前）。
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		ClientID string `json:"clientId"`
	}
	if err := decode(c, &req); err != nil || req.Username == "" {
		writeErr(c, http.StatusBadRequest, errors.New("username and password required"))
		return
	}
	if len(req.ClientID) > 64 {
		writeErr(c, http.StatusBadRequest, errors.New("clientId too long (max 64)"))
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
			cred, err := s.issueSuperSession(c.Request.Context())
			if err != nil {
				writeErr(c, http.StatusInternalServerError, err)
				return
			}
			writeJSON(c, http.StatusOK, map[string]any{
				"token": cred,
				"user":  userView(&store.User{ID: store.SuperAdminID, Username: req.Username, Role: store.RoleAdmin}),
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
	cred, err := s.issueSession(c.Request.Context(), u, req.ClientID)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	if u.Role == store.RoleAdmin {
		// 走到这里必是管理台登录（用户端口 admin 已在上面 403）：保留 user 概要
		// ——控制台门禁与顶栏依赖（0.2.4 明示决策）。
		writeJSON(c, http.StatusOK, map[string]any{"token": cred, "user": userView(u)})
		return
	}
	// 用户端口普通用户：只回单一无透明凭证，身份全存认证图。
	writeJSON(c, http.StatusOK, map[string]any{"token": cred})
}

// handleRefresh 已于 0.2.4 移除：活跃滑动续期由 authCtx→Touch 在服务端无感完成
// （凭证字符串永不轮换），"重新验证才换凭证"即重登语义，/auth/refresh 不再有
// 存在价值。旧客户端（≤0.3.5）/移动端 401→refresh 链路自然失败后退化为
// "过期即重登"（登录对话框承接，行为可接受、已在兼容矩阵明示）。

// handleLogout POST /api/v1/auth/logout {token}（兼容读取旧字段 refreshToken，
// 供 ≤0.3.5 桌面/移动端尽力而为撤权；两者皆无=本地清理请求，直接 ok）。
// 撤权三层同步（L1 立删/L2 共享立删/L3 事实源删除；他实例 L1 陈旧 ≤ auth.memTTL）。
func (s *Server) handleLogout(c *gin.Context) {
	var req struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
	}
	cred := ""
	if err := decode(c, &req); err == nil {
		cred = req.Token
		if cred == "" {
			cred = req.RefreshToken
		}
	}
	if cred == "" {
		// 未带 body 凭证时撤销 Authorization 头里正在使用的凭证（本路由要求
		// 登录态——登出"当前会话"是最常见语义）
		if h := c.Request.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			cred = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if cred != "" {
		if err := s.Auth.Revoke(c.Request.Context(), cred); err != nil {
			// 尽力而为，客户端本地一律清除；0.2.7 走 logx.Warn 保持默认可见
			// （日志只含错误串——绝不打印凭证值本身，敏感纪律）
			logx.WarnT(traceOf(c), "auth: logout revoke failed: %v", err)
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

// handleMe GET /api/v1/auth/me → **仅回 {"username"}（0.2.4 收敛）**。
// 客户端只持 token、不查询自身身份：角色/权限全在服务端认证图内判给（jail/ACL），
// 客户端 UI 永不需要 role；档案字段（id/role/allowedRoots/createdAt）不再对外
// （roots 端点本就给出可访问目录清单，是业务数据而非身份）。不查库——用户名取自
// 认证图条目；旧客户端读取 role 的分支自然落默认值（桌面 0.3.5 显示"普通用户"、
// 移动端 MeInfo.role 空串），无害降级。
func (s *Server) handleMe(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, errors.New("login required"))
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"username": u.Username})
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
