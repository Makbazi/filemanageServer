// v1_oauth.go：0.2.12 认证批——OAuth 2.0 + OIDC 授权码流（含 PKCE，RFC 8252
// loopback/自定义 scheme 公共客户端）与设备授权流（RFC 8628，无浏览器输入端）。
//
// 契约要点（与推荐方案表对齐：Windows/Linux 桌面、Android 移动端 =
// 授权码+PKCE + Bearer Access Token；系统安全凭证存储由客户端负责）：
//   - 本服务端=私有 IdP/OP：issuer=<配置或按请求 Host 派生>/api/v1；发现文档
//     GET /api/v1/.well-known/openid-configuration。
//   - Access Token 仍是认证图不透明随机凭证（Bearer）——0.2.3 起"凭证不编码
//     身份"的红线不动；id_token 为最小 HS256 JWT（密钥 <dataDir>/oidc.key），
//     仅作 OIDC 声明回执，**不参与任何鉴权**（sub=账户名，数字用户 id 不可见
//     纪律延伸）。第一方客户端不验签。
//   - Refresh Token = 认证图 kind=refresh 条目（绝对 session.refreshDays 天，
//     刷新即换发=rotation）；X-Client-Id 绑定语义沿用 0.2.4（绑定了就逐请求核对）。
//   - 全部协议态（flow/code/browser/device/devuser）落 auth_sessions 三层
//     认证图（多实例共享库=任一节点签发全节点互认；FlowData 只存哈希互引，
//     绝不含明文凭证）。撤权（改密/删号）经 RevokeUser 三层即时清全部 kind。
//   - 登录页=服务端内嵌 HTML（/oauth/authorize），二次验证（TOTP/邮箱验证码）
//     在同一页面闭环；fm_browser/fm_flow Cookie 皆 HttpOnly+SameSite=Strict。
//   - 旧 POST /auth/login 保留（维护者决策：兼容旧客户端/移动端降级登录，
//     已启用二次验证的账户在旧端点一律 403 拒绝——见 v1_auth.go）。
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/config"
	"filemanageServer/internal/logx"
	"filemanageServer/internal/mailx"
	store "filemanageServer/internal/store"
)

// ---------- FlowData：认证图条目的协议状态（内部 JSON，绝不出 API/日志） ----------

type flowData struct {
	Mode      string   `json:"mode,omitempty"`      // authz | device | devuser | emailbind
	Client    string   `json:"client,omitempty"`    // OAuth client_id（desktop/mobile/web/自定义）
	Challenge string   `json:"challenge,omitempty"` // PKCE
	Method    string   `json:"method,omitempty"`    // S256 | plain
	Redirect  string   `json:"redirect,omitempty"`  // 已校验回调 URI
	State     string   `json:"state,omitempty"`     // 原样回带
	Nonce     string   `json:"nonce,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	Phase     string   `json:"phase,omitempty"` // flow 行：mfa | enroll
	Mfa       string   `json:"mfa,omitempty"`   // flow 行：totp | email
	UID       int64    `json:"uid,omitempty"`   // flow 行：账密已过的用户
	Username  string   `json:"username,omitempty"`
	Amr       []string `json:"amr,omitempty"`       // 浏览器行/批准记录：认证方式
	NewSecret string   `json:"newSecret,omitempty"` // totp 强制开通暂存密钥（确认才落库）
	Email     string   `json:"email,omitempty"`     // emailbind：待验证邮箱
	OtpHash   string   `json:"otpHash,omitempty"`
	OtpSentAt int64    `json:"otpSentAt,omitempty"`
	UcHash    string   `json:"uc,omitempty"` // device 行：user_code 哈希
	DcHash    string   `json:"dc,omitempty"` // devuser 行：device_code 哈希
	Status    string   `json:"status,omitempty"`
}

const (
	deviceStatusPending  = "pending"
	deviceStatusApproved = "approved"
	deviceStatusDenied   = "denied"
)

func encodeFlow(fd *flowData) string {
	b, _ := json.Marshal(fd)
	return string(b)
}

func decodeFlow(s string) (*flowData, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("empty flow data")
	}
	var fd flowData
	if err := json.Unmarshal([]byte(s), &fd); err != nil {
		return nil, err
	}
	return &fd, nil
}

// ---------- Cookie 与小工具 ----------

const (
	browserCookie = "fm_browser" // 授权页浏览器登录态（kind=browser 凭证）
	flowCookie    = "fm_flow"    // 登录中转会话（kind=flow 凭证）
	adminCookie   = "fm_admin"   // 管理台 BFF 会话（kind=user 角色 admin/超管）
)

func (s *Server) setCookie(c *gin.Context, name, value string, ttl time.Duration) {
	secure := s.Cfg != nil && s.Cfg.TLS.Enabled
	maxAge := int(ttl.Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	path := "/api/v1/oauth"
	if name == adminCookie {
		path = "/"
	}
	c.SetCookie(name, value, maxAge, path, "", secure, true)
	// SameSite=Strict：BFF/中转全在同源，跨站一律不带凭证（CSRF 面收敛）
	c.Header("X-Frame-Options", "DENY")
}

func (s *Server) clearCookie(c *gin.Context, name string) {
	path := "/api/v1/oauth"
	if name == adminCookie {
		path = "/"
	}
	c.SetCookie(name, "", -1, path, "", s.Cfg != nil && s.Cfg.TLS.Enabled, true)
}

// issuer 发现/签发用 issuer：配置显式值优先；否则按请求 Host 派生
// （scheme 跟随 tls.enabled）。多域名/反代部署必须显式配置 oauth.issuer。
func (s *Server) issuer(c *gin.Context) string {
	if s.Cfg.OAuth.Issuer != "" {
		return strings.TrimRight(s.Cfg.OAuth.Issuer, "/")
	}
	scheme := "http"
	if s.Cfg.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host + "/api/v1"
}

// oauthBase 登录页 JS 的绝对路径前缀（页面同时服务 /oauth/authorize 与
// /oauth/device/input 两种 URL，相对 fetch 会错位——一律走绝对 base）。
// 按**请求 Host** 派生而非配置 issuer（反代后 issuer 可能是对外域名，页面
// 同源请求仍应打到当前 Host——Cookie 同源约束也要求如此）。
func (s *Server) oauthBase(c *gin.Context) string {
	scheme := "http"
	if s.Cfg.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://" + c.Request.Host + "/api/v1/oauth"
}

func randDigits6() string {
	const digits = "0123456789"
	b := make([]byte, 6)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			b[i] = digits[0]
			continue
		}
		b[i] = digits[n.Int64()]
	}
	return string(b)
}

// generateUserCode RFC 8628 风格：8 位去混淆字母表，形如 XXXX-XXXX。
func generateUserCode() string {
	const alpha = "BCDFGHJKLMNPQRSTVWXZ"
	s := make([]byte, 8)
	for i := range s {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alpha))))
		if err != nil {
			return ""
		}
		s[i] = alpha[n.Int64()]
	}
	return string(s[:4]) + "-" + string(s[4:])
}

// ---------- 已注册客户端与回调 URI 校验 ----------

func validOAuthParam(v string, max int) bool {
	return len(v) <= max && utf8.ValidString(v)
}

// allowedRedirect 回调 URI 白名单：精确匹配（scheme://host[?大小写不敏感 host/scheme]
// + path 精确）；AllowLoopback 客户端额外放行任意端口的 http loopback（RFC 8252 §7.3）。
func allowedRedirect(cl *config.OAuthClient, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || !u.IsAbs() || u.User != nil || u.Fragment != "" {
		return false
	}
	for _, reg := range cl.RedirectUris {
		r, err := url.Parse(reg)
		if err != nil {
			continue
		}
		if strings.EqualFold(u.Scheme, r.Scheme) && strings.EqualFold(u.Host, r.Host) &&
			strings.TrimRight(u.Path, "/") == strings.TrimRight(r.Path, "/") {
			return true
		}
	}
	if cl.AllowLoopback && u.Scheme == "http" {
		h := u.Hostname()
		if h == "127.0.0.1" || h == "localhost" || h == "::1" {
			return true
		}
	}
	return false
}

// verifyPKCE 授权码兑换时的 challenge 核对。
func verifyPKCE(method, challenge, verifier string) bool {
	if verifier == "" || len(verifier) > 128 {
		return false
	}
	switch method {
	case "S256":
		sum := sha256.Sum256([]byte(verifier))
		return subtle.ConstantTimeCompare([]byte(challenge), []byte(base64.RawURLEncoding.EncodeToString(sum[:]))) == 1
	case "plain", "":
		return subtle.ConstantTimeCompare([]byte(challenge), []byte(verifier)) == 1
	default:
		return false
	}
}

// ---------- 会话签发/消费助手 ----------

// issueBrowserSession 登录成功后签发浏览器登录态（授权页静默续用）。
func (s *Server) issueBrowserSession(ctx context.Context, u *store.User, amr []string) (string, error) {
	fd := &flowData{Mode: "authz", Amr: amr}
	return s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindBrowser, UserID: u.ID, Username: u.Username, Role: u.Role,
		FlowData: encodeFlow(fd), TTL: s.Cfg.LoginDuration(), Persist: true,
	})
}

// issueAuthCode 消费 flow 行后签发授权码（60 秒一次性）。返回完整回调 URL。
func (s *Server) issueAuthCode(ctx context.Context, fd *flowData, u *store.User, amr []string) (string, error) {
	code := &flowData{Mode: "authz", Client: fd.Client, Challenge: fd.Challenge, Method: fd.Method,
		Redirect: fd.Redirect, Nonce: fd.Nonce, Scope: fd.Scope,
		UID: u.ID, Username: u.Username, Amr: amr}
	plain, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindCode, UserID: u.ID, Username: u.Username, Role: store.RoleUser,
		FlowData: encodeFlow(code), TTL: 60 * time.Second, Persist: true,
	})
	if err != nil {
		return "", err
	}
	sep := "?"
	if strings.Contains(fd.Redirect, "?") {
		sep = "&"
	}
	redirect := fd.Redirect + sep + "code=" + url.QueryEscape(plain)
	if fd.State != "" {
		redirect += "&state=" + url.QueryEscape(fd.State)
	}
	return redirect, nil
}

// oauthError 授权页侧错误（HTML 页经 JSON 消费；错误信封与全站一致）。
func oauthError(c *gin.Context, code int, msg string) {
	writeErr(c, code, errors.New(msg))
}

// ---------- GET /api/v1/oauth/authorize ----------

func (s *Server) handleOAuthAuthorize(c *gin.Context) {
	q := c.Request.URL.Query()
	respType := q.Get("response_type")
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	scope := q.Get("scope")
	prompt := q.Get("prompt")

	write := func(code int, msg string) {
		// 参数/注册校验失败**绝不**重定向（防开放重定向）；直接错误页
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(code, oauthErrorPageHTML(msg))
	}

	if respType != "code" {
		write(http.StatusBadRequest, "response_type 必须为 code")
		return
	}
	cl := s.Cfg.OAuthClientOf(clientID)
	if cl == nil {
		write(http.StatusBadRequest, "未知的 client_id（未在 oauth.clients 注册）")
		return
	}
	if redirectURI == "" || !allowedRedirect(cl, redirectURI) || !validOAuthParam(redirectURI, 512) {
		write(http.StatusBadRequest, "redirect_uri 不在该客户端注册白名单内")
		return
	}
	if challenge == "" || !validOAuthParam(challenge, 128) ||
		(method != "" && method != "S256" && method != "plain") {
		// 公共客户端 PKCE 强制；缺省 method 视为 S256（plain 仅历史兼容，
		// 兑换时按声明方法核对——RFC 8252 §4.2/7.4.3）
		write(http.StatusBadRequest, "必须携带 code_challenge，方法仅支持 S256/plain")
		return
	}
	if method == "" {
		method = "S256"
	}
	if len(scope) > 128 || !validOAuthParam(scope, 128) {
		write(http.StatusBadRequest, "scope 过长")
		return
	}
	fd := &flowData{Mode: "authz", Client: cl.ID, Challenge: challenge, Method: method,
		Redirect: redirectURI, State: q.Get("state"), Nonce: q.Get("nonce"), Scope: scope}
	if !validOAuthParam(fd.State, 256) || !validOAuthParam(fd.Nonce, 256) {
		write(http.StatusBadRequest, "state/nonce 非法（≤256 字符）")
		return
	}

	// 静默授权：浏览器登录态有效且未要求重新登录 → 直接出码 302
	if prompt != "login" {
		if e, ok := s.browserFromRequest(c); ok {
			u, err := s.oauthLoadUser(e.Username)
			if err == nil && u != nil {
				var bfd flowData
				if derr := json.Unmarshal([]byte(e.FlowData), &bfd); derr == nil && len(bfd.Amr) > 0 {
					redirect, cerr := s.issueAuthCode(c.Request.Context(), fd, u, bfd.Amr)
					if cerr == nil {
						c.Redirect(http.StatusFound, redirect)
						return
					}
				}
			}
			// 用户已不存在/角色变化 → 登录态不再可用，落回手动登录
		}
	}

	// 渲染登录页（账密 + 按需二次验证，POST 回来）
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, oauthLoginPageHTML("authz", map[string]any{
		"base": s.oauthBase(c), "clientId": cl.ID, "clientName": cl.Name, "challenge": challenge,
		"method": method, "redirect": redirectURI, "state": fd.State, "nonce": fd.Nonce, "scope": scope,
	}))
}

// browserFromRequest 解析 fm_browser Cookie → kind=browser 条目。
func (s *Server) browserFromRequest(c *gin.Context) (*store.AuthSession, bool) {
	v, err := c.Request.Cookie(browserCookie)
	if err != nil || v.Value == "" {
		return nil, false
	}
	e, lerr := s.Auth.Lookup(c.Request.Context(), v.Value)
	if lerr != nil || e == nil || e.Kind != store.AuthKindBrowser {
		return nil, false
	}
	return e, true
}

// oauthLoadUser 按账户名加载且必须仍是**普通用户**（管理员/超管不可经客户端授权）。
func (s *Server) oauthLoadUser(username string) (*store.User, error) {
	u, err := s.Store.GetUserByName(username)
	if err != nil {
		return nil, err
	}
	if u.Role == store.RoleAdmin || u.ID == store.SuperAdminID {
		return nil, errors.New("管理员账户请使用服务端管理界面登录")
	}
	return u, nil
}

// ---------- POST /api/v1/oauth/login（登录页账密步骤） ----------

type oauthLoginReq struct {
	FlowID    string `json:"flowId,omitempty"`
	ClientID  string `json:"clientId,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Method    string `json:"method,omitempty"`
	Redirect  string `json:"redirect,omitempty"`
	State     string `json:"state,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Scope     string `json:"scope,omitempty"`
	UserCode  string `json:"userCode,omitempty"` // 设备授权页形态
	Username  string `json:"username"`
	Password  string `json:"password"`
}

func (s *Server) handleOAuthLogin(c *gin.Context) {
	if !s.dbReady() {
		oauthError(c, http.StatusServiceUnavailable, errNoDB.Error())
		return
	}
	var req oauthLoginReq
	if err := decode(c, &req); err != nil || req.Username == "" || req.Password == "" {
		oauthError(c, http.StatusBadRequest, "账户与密码必填")
		return
	}
	ip := clientIP(c)
	bKey := "login|" + req.Username
	if s.guard.Blocked(bKey, ip) {
		logx.WarnT(traceOf(c), "oauth: 防爆破锁定生效 account=%q ip=%s", req.Username, ip)
		s.audit(c, store.AuditLoginFail, req.Username, 0, "", "", "oauth login locked from "+ip)
		oauthError(c, http.StatusTooManyRequests, "失败次数过多，请稍后再试")
		return
	}
	if s.Super != nil {
		if m, _ := s.Super.Match(req.Username); m {
			s.guard.Fail(bKey, ip)
			s.audit(c, store.AuditLoginFail, req.Username, store.SuperAdminID, "", "", "oauth login super rejected")
			oauthError(c, http.StatusForbidden, "管理员账户请使用服务端管理界面登录")
			return
		}
	}
	u, err := s.oauthLoadUser(req.Username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.guard.Fail(bKey, ip)
			s.audit(c, store.AuditLoginFail, req.Username, 0, "", "", "oauth login bad credentials")
			oauthError(c, http.StatusUnauthorized, "账户或密码错误")
			return
		}
		oauthError(c, http.StatusForbidden, err.Error())
		return
	}
	if !checkPassword(u.PasswordHash, req.Password) {
		s.guard.Fail(bKey, ip)
		s.audit(c, store.AuditLoginFail, req.Username, u.ID, "", "", "oauth login bad password")
		oauthError(c, http.StatusUnauthorized, "账户或密码错误")
		return
	}
	s.guard.Reset(bKey, ip)

	ctx := c.Request.Context()
	// 设备授权页形态：userCode 必须命中待批准条目
	dev := false
	var devRow *store.AuthSession
	if req.UserCode != "" {
		uc, derr := s.Auth.Lookup(ctx, normalizeUserCode(req.UserCode))
		if derr != nil || uc == nil || uc.Kind != store.AuthKindDevUser {
			oauthError(c, http.StatusBadRequest, "用户码无效或已过期")
			return
		}
		dev = true
		devRow = uc
	}

	// 已登录浏览器（同页二次进入）：直接放行
	var fd *flowData
	if dev {
		var dfd flowData
		_ = json.Unmarshal([]byte(devRow.FlowData), &dfd)
		fd = &flowData{Mode: "device", Client: dfd.Client, DcHash: dfd.DcHash, Username: u.Username, UID: u.ID}
	} else {
		if !validOAuthParam(req.Redirect, 512) || req.Redirect == "" {
			oauthError(c, http.StatusBadRequest, "非法请求（缺 redirect）")
			return
		}
		cl := s.Cfg.OAuthClientOf(req.ClientID)
		if cl == nil || !allowedRedirect(cl, req.Redirect) ||
			req.Challenge == "" || (req.Method != "S256" && req.Method != "plain" && req.Method != "") {
			oauthError(c, http.StatusBadRequest, "非法请求（客户端/回调/PKCE 校验失败）")
			return
		}
		method := req.Method
		if method == "" {
			method = "plain"
		}
		fd = &flowData{Mode: "authz", Client: cl.ID, Challenge: req.Challenge, Method: method,
			Redirect: req.Redirect, State: req.State, Nonce: req.Nonce, Scope: req.Scope,
			Username: u.Username, UID: u.ID}
	}

	switch u.MfaMethod {
	case store.MfaOff:
		s.finishLogin(c, u, []string{"pwd"}, fd)
		return
	case store.MfaTotp:
		if u.MfaSecret == "" {
			// 管理员强制 TOTP 且未开通 → 现场开通（新密钥只存 flow，确认才落库）
			sec, gerr := auth.GenerateTotpSecret()
			if gerr != nil {
				oauthError(c, http.StatusInternalServerError, "密钥生成失败")
				return
			}
			fd.Phase = "enroll"
			fd.Mfa = store.MfaTotp
			fd.NewSecret = sec
			s.startFlow(c, ctx, fd, u)
			return
		}
		fd.Phase = "mfa"
		fd.Mfa = store.MfaTotp
		s.startFlow(c, ctx, fd, u)
		return
	case store.MfaEmail:
		if !u.EmailVerified || u.Email == "" {
			oauthError(c, http.StatusBadRequest, "该账户的邮箱验证码二步验证尚未绑定完成，请联系管理员")
			return
		}
		fd.Phase = "mfa"
		fd.Mfa = store.MfaEmail
		s.startFlow(c, ctx, fd, u)
		return
	default:
		s.finishLogin(c, u, []string{"pwd"}, fd)
		return
	}
}

// startFlow 签发 kind=flow 中转条目并进入二次验证（返回 flowId；enroll/email 附带提示）。
func (s *Server) startFlow(c *gin.Context, ctx context.Context, fd *flowData, u *store.User) {
	plain, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindFlow, UserID: u.ID, Username: u.Username, Role: u.Role,
		FlowData: encodeFlow(fd), TTL: 10 * time.Minute, Persist: true,
	})
	if err != nil {
		oauthError(c, http.StatusInternalServerError, "登录中转创建失败")
		return
	}
	s.setCookie(c, flowCookie, plain, 10*time.Minute)
	out := map[string]any{"mfa": fd.Mfa, "flowId": plain}
	if fd.Phase == "enroll" {
		out["enroll"] = map[string]any{"secret": fd.NewSecret, "otpauth": auth.TotpURI(u.Username, fd.NewSecret)}
	}
	if fd.Mfa == store.MfaEmail {
		// 立即投递验证码（失败也不回滚 flow——可页内重发）
		if serr := s.sendOtp(ctx, plain, fd, u); serr != nil {
			out["sendError"] = serr.Error()
		} else {
			out["hint"] = "验证码已发送至绑定邮箱（" + maskEmail(u.Email) + "）"
		}
	}
	writeJSON(c, http.StatusOK, out)
}

// ---------- POST /api/v1/oauth/mfa（二次验证步骤） ----------

func (s *Server) handleOAuthMfa(c *gin.Context) {
	var req struct {
		FlowID string `json:"flowId"`
		Code   string `json:"code"`
	}
	if err := decode(c, &req); err != nil {
		oauthError(c, http.StatusBadRequest, "参数错误")
		return
	}
	if req.FlowID == "" {
		if v, err := c.Request.Cookie(flowCookie); err == nil {
			req.FlowID = v.Value
		}
	}
	if req.FlowID == "" {
		oauthError(c, http.StatusBadRequest, "会话已过期，请重新登录")
		return
	}
	ctx := c.Request.Context()
	e, err := s.Auth.Lookup(ctx, req.FlowID)
	if err != nil || e == nil || e.Kind != store.AuthKindFlow {
		oauthError(c, http.StatusBadRequest, "会话已过期，请重新登录")
		return
	}
	fd, derr := decodeFlow(e.FlowData)
	if derr != nil || (fd.Mode != "authz" && fd.Mode != "device") {
		oauthError(c, http.StatusInternalServerError, "流程状态异常")
		return
	}
	u, lerr := s.oauthLoadUser(fd.Username)
	if lerr != nil || u == nil || u.ID != fd.UID {
		oauthError(c, http.StatusBadRequest, "账户状态已变化，请重新登录")
		return
	}
	ip := clientIP(c)
	gKey := "mfa|" + u.Username
	if s.guard.Blocked(gKey, ip) {
		oauthError(c, http.StatusTooManyRequests, "失败次数过多，请稍后再试")
		return
	}

	switch fd.Mfa {
	case store.MfaTotp:
		secret := u.MfaSecret
		if fd.Phase == "enroll" {
			secret = fd.NewSecret
		}
		if !auth.ValidateTotp(secret, req.Code, time.Now()) {
			s.guard.Fail(gKey, ip)
			oauthError(c, http.StatusUnauthorized, "动态验证码错误")
			return
		}
		if fd.Phase == "enroll" { // 确认开通才落库（放弃开通不留残密）
			if uerr := s.Store.UpdateUserAuth(u.ID, u.Email, u.EmailVerified, store.MfaTotp, fd.NewSecret); uerr != nil {
				oauthError(c, http.StatusInternalServerError, uerr.Error())
				return
			}
			logx.InfoT(traceOf(c), "oauth: 强制 TOTP 开通确认 account=%q", u.Username)
		}
	case store.MfaEmail:
		if req.Code == "" { // 重发（60s 冷却）
			if time.Now().Unix()-fd.OtpSentAt < 60 {
				oauthError(c, http.StatusTooManyRequests, "验证码发送过于频繁，请稍候")
				return
			}
			if serr := s.sendOtp(ctx, req.FlowID, fd, u); serr != nil {
				oauthError(c, http.StatusServiceUnavailable, serr.Error())
				return
			}
			writeJSON(c, http.StatusOK, map[string]any{"sent": true, "mfa": "email", "flowId": req.FlowID})
			return
		}
		if fd.OtpHash == "" || subtle.ConstantTimeCompare([]byte(fd.OtpHash), []byte(hashOtp(req.FlowID, req.Code))) != 1 ||
			time.Now().Unix()-fd.OtpSentAt > 5*60 {
			s.guard.Fail(gKey, ip)
			oauthError(c, http.StatusUnauthorized, "邮箱验证码错误或已过期")
			return
		}
	default:
		oauthError(c, http.StatusBadRequest, "流程状态异常")
		return
	}

	s.guard.Reset(gKey, ip)
	amr := []string{"pwd", "mfa"}
	// flow 行一次性消费
	_ = s.Auth.Revoke(ctx, req.FlowID)
	s.clearCookie(c, flowCookie)
	if fd.Mode == "device" {
		s.approveDevice(c, ctx, fd, u, amr)
		return
	}
	s.finishLogin(c, u, amr, fd)
}

// sendOtp 生成并投递邮箱验证码（哈希存回 flow 行）。
func (s *Server) sendOtp(ctx context.Context, flowID string, fd *flowData, u *store.User) error {
	if !s.Cfg.Mail.Enabled() {
		return errors.New("服务端未配置邮箱（mail.host），邮箱验证码通道不可用")
	}
	code := randDigits6()
	fd.OtpHash = hashOtp(flowID, code)
	fd.OtpSentAt = time.Now().Unix()
	if err := s.Auth.UpdateFlow(ctx, flowEntryForUpdate(ctx, s, flowID), encodeFlow(fd)); err != nil {
		return err
	}
	body := "FileManage 登录验证码：" + code + "\n\n5 分钟内有效。若非本人操作请忽略并修改密码。"
	return mailx.Send(s.Cfg.Mail, u.Email, "FileManage 登录验证码", body)
}

func hashOtp(salt, code string) string {
	return auth.HashToken(salt + "|" + code)
}

func normalizeUserCode(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	if len(s) == 8 {
		return s[:4] + "-" + s[4:]
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}

func maskEmail(e string) string {
	at := strings.Index(e, "@")
	if at <= 1 {
		return "**"
	}
	name := e[:at]
	return name[:1] + "***" + e[at:]
}

// flowEntryForUpdate sendOtp 需要 flow 行的权威快照（含 ID/TokenHash）才能
// UpdateFlow 三层同步——按明文凭证解析回条目。
func flowEntryForUpdate(ctx context.Context, s *Server, flowID string) *store.AuthSession {
	e, err := s.Auth.Lookup(ctx, flowID)
	if err != nil {
		return nil
	}
	return e
}

// finishLogin 登录闭环（账密/二验均过）：签发浏览器态；授权码流回
// JSON {redirect}，设备批准形态转 approveDevice。
func (s *Server) finishLogin(c *gin.Context, u *store.User, amr []string, fd *flowData) {
	ctx := c.Request.Context()
	browser, err := s.issueBrowserSession(ctx, u, amr)
	if err != nil {
		oauthError(c, http.StatusInternalServerError, "登录态签发失败")
		return
	}
	s.setCookie(c, browserCookie, browser, s.Cfg.LoginDuration())
	if fd.Mode == "device" {
		s.approveDevice(c, ctx, fd, u, amr)
		return
	}
	redirect, err := s.issueAuthCode(ctx, fd, u, amr)
	if err != nil {
		oauthError(c, http.StatusInternalServerError, "授权码签发失败")
		return
	}
	s.audit(c, store.AuditLogin, u.Username, u.ID, "", "", "oauth login ok amr="+strings.Join(amr, "+"))
	logx.InfoT(traceOf(c), "oauth: 授权码签发成功 account=%q client=%q amr=%v", u.Username, fd.Client, amr)
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "redirect": redirect})
}

// ---------- 设备授权（RFC 8628，无浏览器输入端：移动端/CLI） ----------

func (s *Server) handleOAuthDevice(c *gin.Context) {
	if !s.dbReady() {
		oauthError(c, http.StatusServiceUnavailable, errNoDB.Error())
		return
	}
	var req struct {
		ClientID string `json:"client_id"`
		ClientId string `json:"clientId"`
		Scope    string `json:"scope"`
	}
	_ = decode(c, &req)
	if req.ClientID == "" {
		req.ClientID = req.ClientId
	}
	cl := s.Cfg.OAuthClientOf(req.ClientID)
	if cl == nil {
		oauthError(c, http.StatusBadRequest, "未知的 client_id")
		return
	}
	ctx := c.Request.Context()
	dcPlain, _, err := auth.NewCredential()
	if err != nil {
		oauthError(c, http.StatusInternalServerError, err.Error())
		return
	}
	var ucPlain string
	ucOK := false
	for i := 0; i < 5; i++ {
		ucPlain = generateUserCode()
		if ucPlain == "" {
			oauthError(c, http.StatusInternalServerError, "随机源不可用")
			return
		}
		_, ierr := s.Auth.Issue(ctx, auth.Spec{Kind: store.AuthKindDevUser, UserID: 0,
			Username: "", FlowData: encodeFlow(&flowData{Mode: "devuser", Client: cl.ID,
				DcHash: auth.HashToken(dcPlain)}), Plain: ucPlain, TTL: 10 * time.Minute, Persist: true})
		if ierr == nil {
			ucOK = true
			break
		}
		if errors.Is(ierr, store.ErrExists) || strings.Contains(ierr.Error(), "unique") {
			if i == 4 {
				oauthError(c, http.StatusConflict, "用户码生成冲突，请重试")
				return
			}
			continue
		}
		oauthError(c, http.StatusInternalServerError, ierr.Error())
		return
	}
	if !ucOK {
		oauthError(c, http.StatusConflict, "用户码生成冲突，请重试")
		return
	}
	if _, err := s.Auth.Issue(ctx, auth.Spec{Kind: store.AuthKindDevice, UserID: 0,
		FlowData: encodeFlow(&flowData{Mode: "device", Status: deviceStatusPending, Client: cl.ID,
			Scope: req.Scope, UcHash: auth.HashToken(ucPlain)}),
		Plain: dcPlain, TTL: 10 * time.Minute, Persist: true}); err != nil {
		oauthError(c, http.StatusInternalServerError, err.Error())
		return
	}
	base := s.issuer(c)
	writeJSON(c, http.StatusOK, map[string]any{
		"device_code":               dcPlain,
		"user_code":                 ucPlain,
		"verification_uri":          base + "/oauth/device/input",
		"verification_uri_complete": base + "/oauth/device/input?user_code=" + ucPlain,
		"expires_in":                600,
		"interval":                  5,
	})
}

// GET /oauth/device/input 设备码输入/批准页（同一 HTML，mode=device）。
func (s *Server) handleOAuthDeviceInput(c *gin.Context) {
	uc := normalizeUserCode(c.Query("user_code"))
	if len(uc) > 16 {
		oauthError(c, http.StatusBadRequest, "user_code 过长")
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, oauthLoginPageHTML("device", map[string]any{
		"base": s.oauthBase(c), "clientName": "移动端 / 设备", "userCode": uc,
	}))
}

// approveDevice 登录闭环的设备形态：把 device 行置为 approved。
func (s *Server) approveDevice(c *gin.Context, ctx context.Context, fd *flowData, u *store.User, amr []string) {
	dev, err := s.Auth.LookupHash(ctx, fd.DcHash)
	if err != nil || dev == nil || dev.Kind != store.AuthKindDevice {
		oauthError(c, http.StatusBadRequest, "设备码已过期，请在设备上重新发起")
		return
	}
	dfd, derr := decodeFlow(dev.FlowData)
	if derr != nil || dfd.Status != deviceStatusPending {
		oauthError(c, http.StatusBadRequest, "设备码状态已变化（已批准/已拒绝/已过期）")
		return
	}
	dfd.Status = deviceStatusApproved
	dfd.UID = u.ID
	dfd.Username = u.Username
	dfd.Amr = amr
	if uerr := s.Auth.UpdateFlow(ctx, dev, encodeFlow(dfd)); uerr != nil {
		oauthError(c, http.StatusInternalServerError, "批准写入失败")
		return
	}
	_ = s.Auth.RevokeByHash(ctx, dfd.UcHash) // devuser 行消费（用户码即凭证，一次一用）
	s.audit(c, store.AuditLogin, u.Username, u.ID, "", "", "oauth device approved client="+fd.Client)
	logx.InfoT(traceOf(c), "oauth: 设备授权批准 account=%q client=%q", u.Username, fd.Client)
	writeJSON(c, http.StatusOK, map[string]any{"done": true})
}

// ---------- POST /api/v1/oauth/token ----------

type oauthTokenResp struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *Server) handleOAuthToken(c *gin.Context) {
	if !s.dbReady() {
		writeJSON(c, http.StatusServiceUnavailable, map[string]any{"error": "server_unavailable", "error_description": errNoDB.Error()})
		return
	}
	form := map[string]string{}
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		_ = c.Request.ParseForm()
		for k, v := range c.Request.PostForm {
			if len(v) > 0 {
				form[k] = v[0]
			}
		}
	} else {
		var j map[string]any
		if err := decode(c, &j); err == nil {
			for k, v := range j {
				if sv, ok := v.(string); ok {
					form[k] = sv
				}
			}
		}
	}
	switch form["grant_type"] {
	case "authorization_code":
		s.tokenAuthCode(c, form)
	case "refresh_token":
		s.tokenRefresh(c, form)
	case "urn:ietf:params:oauth:grant-type:device_code":
		s.tokenDevice(c, form)
	default:
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "unsupported_grant_type"})
	}
}

// bindClientID 安装实例标识（0.2.4 X-Client-Id 语义沿用）：头优先，body clientId 兜底。
func bindClientID(c *gin.Context, form map[string]string) string {
	cid := c.GetHeader("X-Client-Id")
	if cid == "" {
		cid = form["clientId"]
	}
	if len(cid) > 64 {
		return ""
	}
	return cid
}

func (s *Server) tokenAuthCode(c *gin.Context, form map[string]string) {
	ctx := c.Request.Context()
	codePlain := form["code"]
	if codePlain == "" || len(codePlain) > 128 {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": "code required"})
		return
	}
	e, err := s.Auth.Lookup(ctx, codePlain)
	if err != nil || e == nil || e.Kind != store.AuthKindCode {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	fd, derr := decodeFlow(e.FlowData)
	if derr != nil || fd.Mode != "authz" {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if fd.Client != form["client_id"] {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_client"})
		return
	}
	if ru := form["redirect_uri"]; ru != "" && ru != fd.Redirect {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "redirect_uri mismatch"})
		return
	}
	if !verifyPKCE(fd.Method, fd.Challenge, form["code_verifier"]) {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "PKCE verification failed"})
		return
	}
	// 授权码一次性：先消费（撤销条目）再签发票据
	if rerr := s.Auth.Revoke(ctx, codePlain); rerr != nil {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	s.issueAccessTokenPair(c, fd.UID, fd.Username, fd.Client, fd.Scope, fd.Nonce, fd.Amr, bindClientID(c, form))
}

func (s *Server) tokenRefresh(c *gin.Context, form map[string]string) {
	ctx := c.Request.Context()
	rt := form["refresh_token"]
	if rt == "" || len(rt) > 128 {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	e, err := s.Auth.Lookup(ctx, rt)
	if err != nil || e == nil || e.Kind != store.AuthKindRefresh {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	if cid := bindClientID(c, form); e.ClientID != "" && cid != e.ClientID {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "client identity mismatch"})
		return
	}
	fd, derr := decodeFlow(e.FlowData)
	if derr != nil || fd.Mode != "refresh" {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	// rotation：先吊销旧刷新令牌（新 access/refresh 沿用原客户端绑定）
	if rerr := s.Auth.Revoke(ctx, rt); rerr != nil {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	s.issueAccessTokenPair(c, e.UserID, e.Username, fd.Client, fd.Scope, fd.Nonce, fd.Amr, e.ClientID)
}

func (s *Server) tokenDevice(c *gin.Context, form map[string]string) {
	ctx := c.Request.Context()
	dc := form["device_code"]
	if dc == "" || len(dc) > 128 {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_request"})
		return
	}
	e, err := s.Auth.Lookup(ctx, dc)
	if err != nil || e == nil || e.Kind != store.AuthKindDevice {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "expired_token"})
		return
	}
	fd, derr := decodeFlow(e.FlowData)
	if derr != nil || fd.Mode != "device" {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
		return
	}
	switch fd.Status {
	case deviceStatusPending:
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "authorization_pending"})
		return
	case deviceStatusDenied:
		_ = s.Auth.Revoke(ctx, dc)
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "access_denied"})
		return
	case deviceStatusApproved:
		// 一次性消费：吊销 device + 残存 devuser 索引行
		_ = s.Auth.Revoke(ctx, dc)
		_ = s.Auth.RevokeByHash(ctx, fd.UcHash)
		s.issueAccessTokenPair(c, fd.UID, fd.Username, fd.Client, fd.Scope, "", fd.Amr, bindClientID(c, form))
	default:
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant"})
	}
}

// issueAccessTokenPair 校验账户现状后签发 access（user kind）+ refresh（换发）。
// bind=本次要绑定的 X-Client-Id（安装实例标识；刷新路径恒沿用原绑定值——
// 由调用方决定传法，头核对已在各 grant 前置完成）。
func (s *Server) issueAccessTokenPair(c *gin.Context, uid int64, username, clientID, scope, nonce string, amr []string, bind string) {
	ctx := c.Request.Context()
	u, err := s.Store.GetUserByID(uid)
	if err != nil || u == nil || u.Username != username || u.Role == store.RoleAdmin {
		writeJSON(c, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "account no longer usable"})
		return
	}
	cred, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindUser, UserID: u.ID, Username: u.Username, Role: u.Role,
		ClientID: bind, TTL: s.Cfg.IdleDuration(), Persist: true,
	})
	if err != nil {
		writeJSON(c, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	rfd := encodeFlow(&flowData{Mode: "refresh", Client: clientID, Scope: scope, Nonce: nonce, Amr: amr})
	rt, err := s.Auth.Issue(ctx, auth.Spec{
		Kind: store.AuthKindRefresh, UserID: u.ID, Username: u.Username, Role: u.Role,
		ClientID: bind, FlowData: rfd, TTL: s.Cfg.RefreshDuration(), Persist: true,
	})
	if err != nil {
		_ = s.Auth.Revoke(ctx, cred)
		writeJSON(c, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	resp := oauthTokenResp{AccessToken: cred, TokenType: "Bearer",
		ExpiresIn: int64(s.Cfg.IdleDuration().Seconds()), RefreshToken: rt, Scope: scope}
	if tok, ierr := s.buildIDToken(c, u, clientID, nonce, amr); ierr == nil {
		resp.IDToken = tok
	}
	logx.InfoT(traceOf(c), "oauth: token 签发成功 account=%q client=%q bound=%v", u.Username, clientID, bind != "")
	writeJSON(c, http.StatusOK, resp)
}

// buildIDToken 最小 OIDC 声明回执（HS256；sub=账户名——数字用户 id 不可见）。
func (s *Server) buildIDToken(c *gin.Context, u *store.User, clientID, nonce string, amr []string) (string, error) {
	key, err := s.oidcKey()
	if err != nil {
		return "", err
	}
	now := auth.OidcNow()
	claims := map[string]any{
		"iss": s.issuer(c), "sub": u.Username, "aud": clientID,
		"exp": now + int64(s.Cfg.IdleDuration().Seconds()), "iat": now,
		"auth_time": now, "preferred_username": u.Username,
	}
	if u.Nickname != "" {
		claims["nickname"] = u.Nickname
	}
	if u.Email != "" {
		claims["email"] = u.Email
		claims["email_verified"] = u.EmailVerified
	}
	if len(amr) > 0 {
		claims["amr"] = amr
	} else {
		claims["amr"] = []string{"pwd"}
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	return auth.SignIDToken(key, claims)
}

// ---------- POST /api/v1/oauth/revoke ----------

// handleOAuthRevoke RFC 7009：撤 access/refresh/浏览器态任一凭证；未知/已失效
// 一律 200（不泄露存在性）。客户端退出尽力而为（沿用旧 logout 语义）。
func (s *Server) handleOAuthRevoke(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if decode(c, &req) != nil || req.Token == "" {
		if h := c.Request.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			req.Token = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if req.Token != "" {
		if err := s.Auth.Revoke(c.Request.Context(), req.Token); err != nil {
			logx.WarnT(traceOf(c), "oauth: revoke failed: %v", err)
		}
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- GET /api/v1/oauth/userinfo + discovery ----------

func (s *Server) handleUserInfo(c *gin.Context) {
	u := currentUser(c)
	if u == nil {
		oauthError(c, http.StatusUnauthorized, "login required")
		return
	}
	out := map[string]any{"sub": u.Username, "username": u.Username}
	if cur, err := s.Store.GetUserByID(u.ID); err == nil && cur != nil {
		out["username"], out["nickname"] = cur.Username, cur.Nickname
		out["email"], out["emailVerified"], out["mfa"] = cur.Email, cur.EmailVerified, cur.MfaMethod
	}
	writeJSON(c, http.StatusOK, out)
}

func (s *Server) handleOpenIDConfig(c *gin.Context) {
	base := s.issuer(c)
	methods := []string{"S256", "plain"}
	writeJSON(c, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"device_authorization_endpoint":         base + "/oauth/device",
		"userinfo_endpoint":                     base + "/oauth/userinfo",
		"revocation_endpoint":                   base + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token", "urn:ietf:params:oauth:grant-type:device_code"},
		"scopes_supported":                      []string{"openid", "profile", "files"},
		"code_challenge_methods_supported":      methods,
		"id_token_signing_alg_values_supported": []string{"HS256"},
		"subject_types_supported":               []string{"public"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}
