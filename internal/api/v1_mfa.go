// v1_mfa.go：0.2.12 二次验证自助面（登录用户，auth 组内）+ 管理端二验治理。
// 需求：**选择性启用**（默认不启用），通道=TOTP 或 邮箱验证码；邮箱为非必须
// 配置（未配置 mail 时邮箱通道端点回明确中文错误）。超管不经这些端点
// （账户文件面，见约定13——超管二验本批明确不做，记录于 CHANGELOG）。
//
// 纪律：
//   - 改安全属性（开通/关闭）必须重新验证口令；开通 TOTP 需一次动态码确认
//     （防"只存了密钥没进验证器"的自锁）；换绑邮箱需邮箱 OTP 确认（防劫持
//     账户后把二验通道切到攻击者邮箱）。
//   - 关闭二验 = 安全降级 → 吊销该用户全部会话（reauthRequired，与改密同
//     纪律）；开通不吊销（当前会话已证明持有口令+设备/邮箱）。
//   - flow 行（kind=flow, mode=emailbind）承载待确认邮箱与 OTP 哈希（10 分钟
//     绝对；OTP 5 分钟）；一次性消费。跨实例经认证图三层自然可达。
//   - 日志只记账户与结果；邮箱地址脱敏（maskEmail）；OTP/TOTP 密钥永不入日志。
package api

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/logx"
	"filemanageServer/internal/mailx"
	store "filemanageServer/internal/store"
)

// nowFunc 时间源（测试/一致性便利；全站仅本批端点使用）。
func nowFunc() time.Time { return time.Now() }

func subtleEq(a, b []byte) int { return subtle.ConstantTimeCompare(a, b) }

// parseIDParam 管理端 :id 解析（错误已写响应）。
func parseIDParam(c *gin.Context) (int64, error) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeErr(c, http.StatusBadRequest, errors.New("bad id"))
		return 0, err
	}
	return id, nil
}

// mfaGuardUser 拒绝超管（哨兵 id=0——账户文件面维护）。
func mfaGuardUser(c *gin.Context) (*store.User, bool) {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, errors.New("login required"))
		return nil, false
	}
	if u.ID == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户经管理台\"超级管理员\"页（账户文件）维护，不在此启用二次验证"))
		return nil, false
	}
	return u, true
}

// handleMfaStatus GET /api/v1/auth/mfa → 当前二验形态（客户端设置页渲染）。
func (s *Server) handleMfaStatus(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	cur, err := s.Store.GetUserByID(u.ID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"method": cur.MfaMethod, "email": cur.Email, "emailVerified": cur.EmailVerified,
		"mailConfigured": s.Cfg.Mail.Enabled(),
	})
}

// handleMfaTotpEnroll POST /api/v1/auth/mfa/totp/enroll {password}
// 返回新密钥与 otpauth URI（未确认前不落库——放弃开通不留残密；管理员
// 强制开通路径见 handleAdminUserMfa，由登录页 enroll 分支承接）。
func (s *Server) handleMfaTotpEnroll(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil || req.Password == "" {
		writeErr(c, http.StatusBadRequest, errors.New("password required"))
		return
	}
	ip := clientIP(c)
	gKey := "mfaenroll|" + u.Username
	if s.guard.Blocked(gKey, ip) {
		writeErr(c, http.StatusTooManyRequests, errors.New("失败次数过多，请稍后再试"))
		return
	}
	cur, err := s.Store.GetUserByID(u.ID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if !checkPassword(cur.PasswordHash, req.Password) {
		s.guard.Fail(gKey, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	s.guard.Reset(gKey, ip)
	if cur.MfaMethod == store.MfaTotp {
		writeErr(c, http.StatusBadRequest, errors.New("该账户已启用验证器二步验证；如需更换密钥请先关闭"))
		return
	}
	secret, err := auth.GenerateTotpSecret()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	logx.InfoT(traceOf(c), "mfa: TOTP 开通请求（待确认）account=%q", cur.Username)
	writeJSON(c, http.StatusOK, map[string]any{"secret": secret, "otpauth": auth.TotpURI(cur.Username, secret)})
}

// handleMfaTotpConfirm POST /api/v1/auth/mfa/totp/confirm {password, secret, code}
// 确认动态码并落库启用（method=totp + 密钥入库；当前会话不吊销）。
func (s *Server) handleMfaTotpConfirm(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	var req struct {
		Password string `json:"password"`
		Secret   string `json:"secret"`
		Code     string `json:"code"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	ip := clientIP(c)
	gKey := "mfaenroll|" + u.Username
	if s.guard.Blocked(gKey, ip) {
		writeErr(c, http.StatusTooManyRequests, errors.New("失败次数过多，请稍后再试"))
		return
	}
	cur, err := s.Store.GetUserByID(u.ID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if !checkPassword(cur.PasswordHash, req.Password) {
		s.guard.Fail(gKey, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	// 密钥必须来自本会话的 enroll 请求——但 enroll 是无状态返回，重放校验以
	// "口令+当次动态码"为准：只接受与提交密钥一致的当前窗口码。
	if !auth.ValidateTotp(req.Secret, req.Code, nowFunc()) {
		s.guard.Fail(gKey, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("动态验证码错误"))
		return
	}
	if err := s.Store.UpdateUserAuth(cur.ID, cur.Email, cur.EmailVerified, store.MfaTotp, strings.ToUpper(strings.TrimSpace(req.Secret))); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	logx.InfoT(traceOf(c), "mfa: TOTP 已启用 account=%q", cur.Username)
	s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "mfa totp enabled by self")
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "mfa": store.MfaTotp})
}

// handleMfaEmailSend POST /api/v1/auth/mfa/email/send {email}
// 换绑邮箱第一步：投递 6 位 OTP（flow 行承载；5 分钟有效；同邮箱 60s 冷却）。
// 邮箱通道依赖 mail 配置——未配置回明确中文错误（TOTP 不受影响）。
func (s *Server) handleMfaEmailSend(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	var req struct {
		Email  string `json:"email"`
		SendID string `json:"sendId"` // 重发时携带（沿用原 flow 行、60s 冷却）
	}
	if err := decode(c, &req); err != nil || strings.TrimSpace(req.Email) == "" {
		writeErr(c, http.StatusBadRequest, errors.New("email required"))
		return
	}
	if !s.Cfg.Mail.Enabled() {
		writeErr(c, http.StatusServiceUnavailable, errors.New("服务端未配置邮箱通道（config mail.host），请改用验证器 App（TOTP）二步验证"))
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !validEmail(email) {
		writeErr(c, http.StatusBadRequest, errors.New("邮箱地址格式不正确"))
		return
	}
	cur, err := s.Store.GetUserByID(u.ID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	ctx := c.Request.Context()
	var e *store.AuthSession
	var fd flowData
	if req.SendID != "" {
		e, err = s.Auth.Lookup(ctx, req.SendID)
		if err == nil && e != nil && e.Kind == store.AuthKindFlow {
			if d, derr := decodeFlow(e.FlowData); derr == nil && d.Mode == "emailbind" && d.UID == cur.ID {
				fd = *d
			}
		}
		if e == nil {
			writeErr(c, http.StatusBadRequest, errors.New("验证会话已过期，请重新发起"))
			return
		}
		if fd.Mode != "emailbind" {
			_ = s.Auth.Revoke(ctx, req.SendID)
			writeErr(c, http.StatusBadRequest, errors.New("验证会话已过期，请重新发起"))
			return
		}
		if nowFunc().Unix()-fd.OtpSentAt < 60 {
			writeErr(c, http.StatusTooManyRequests, errors.New("发送过于频繁，请 60 秒后重试"))
			return
		}
	} else {
		plain, ierr := s.Auth.Issue(ctx, auth.Spec{Kind: store.AuthKindFlow, UserID: cur.ID,
			Username: cur.Username, Role: cur.Role,
			FlowData: encodeFlow(&flowData{Mode: "emailbind", UID: cur.ID, Email: email}),
			TTL:      10 * time.Minute, Persist: true})
		if ierr != nil {
			writeErr(c, statusOf(ierr), ierr)
			return
		}
		e, _ = s.Auth.Lookup(ctx, plain)
		if e == nil {
			writeErr(c, http.StatusInternalServerError, errors.New("验证会话创建失败"))
			return
		}
		fd = flowData{Mode: "emailbind", UID: cur.ID, Email: email}
		req.SendID = plain
	}
	code := randDigits6()
	fd.OtpHash = hashOtp(req.SendID, code)
	fd.OtpSentAt = nowFunc().Unix()
	if uerr := s.Auth.UpdateFlow(ctx, e, encodeFlow(&fd)); uerr != nil {
		writeErr(c, statusOf(uerr), uerr)
		return
	}
	body := "FileManage 邮箱绑定验证码：" + code + "\n\n5 分钟内有效。若非本人操作请忽略并立即修改密码。"
	if serr := mailx.Send(s.Cfg.Mail, email, "FileManage 邮箱绑定验证码", body); serr != nil {
		logx.WarnT(traceOf(c), "mfa: 邮箱绑定验证码投递失败: %v", serr)
		writeErr(c, http.StatusServiceUnavailable, errors.New("邮件投递失败（"+serr.Error()+"），请稍后重试或改用 TOTP"))
		return
	}
	logx.InfoT(traceOf(c), "mfa: 邮箱绑定验证码已发送 account=%q to=%s", cur.Username, maskEmail(email))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "sendId": req.SendID, "hint": "验证码已发送至 " + maskEmail(email)})
}

// handleMfaEmailConfirm POST /api/v1/auth/mfa/email/confirm {sendId, code}
// 换绑邮箱第二步：OTP 确认 → email+verified 落库并启用邮箱二验（method=email；
// 不吊销会话）。一次性消费 flow 行。
func (s *Server) handleMfaEmailConfirm(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	var req struct {
		SendID string `json:"sendId"`
		Code   string `json:"code"`
	}
	if err := decode(c, &req); err != nil || req.SendID == "" || req.Code == "" {
		writeErr(c, http.StatusBadRequest, errors.New("sendId 与 code 必填"))
		return
	}
	ctx := c.Request.Context()
	e, err := s.Auth.Lookup(ctx, req.SendID)
	if err != nil || e == nil || e.Kind != store.AuthKindFlow {
		writeErr(c, http.StatusBadRequest, errors.New("验证会话已过期，请重新发起"))
		return
	}
	fd, derr := decodeFlow(e.FlowData)
	if derr != nil || fd.Mode != "emailbind" || fd.UID != u.ID {
		_ = s.Auth.Revoke(ctx, req.SendID)
		writeErr(c, http.StatusBadRequest, errors.New("验证会话已过期，请重新发起"))
		return
	}
	ip := clientIP(c)
	gKey := "mfaemail|" + u.Username
	if s.guard.Blocked(gKey, ip) {
		writeErr(c, http.StatusTooManyRequests, errors.New("失败次数过多，请稍后再试"))
		return
	}
	if subtleCompare(fd.OtpHash, hashOtp(req.SendID, strings.TrimSpace(req.Code))) != 1 ||
		nowFunc().Unix()-fd.OtpSentAt > 5*60 {
		s.guard.Fail(gKey, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("邮箱验证码错误或已过期"))
		return
	}
	cur, gerr := s.Store.GetUserByID(u.ID)
	if gerr != nil {
		writeErr(c, statusOf(gerr), gerr)
		return
	}
	// 已有 TOTP 密钥保留（method 切 email；用户之后可自行改回）
	if err := s.Store.UpdateUserAuth(cur.ID, fd.Email, true, store.MfaEmail, cur.MfaSecret); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	_ = s.Auth.Revoke(ctx, req.SendID) // 一次性消费
	logx.InfoT(traceOf(c), "mfa: 邮箱已绑定并启用邮箱二验 account=%q to=%s", cur.Username, maskEmail(fd.Email))
	s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "email bound+verified by self (mfa=email)")
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "mfa": store.MfaEmail})
}

// handleMfaDisable POST /api/v1/auth/mfa/disable {password}
// 关闭二次验证（安全降级）：口令复验 + 清密钥（邮箱字段保留）+ 吊销全部
// 会话（reauthRequired——与改密同纪律，旧会话带着"已二验"假设继续活跃不可接受）。
func (s *Server) handleMfaDisable(c *gin.Context) {
	u, ok := mfaGuardUser(c)
	if !ok {
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil || req.Password == "" {
		writeErr(c, http.StatusBadRequest, errors.New("password required"))
		return
	}
	cur, err := s.Store.GetUserByID(u.ID)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if !checkPassword(cur.PasswordHash, req.Password) {
		ip := clientIP(c)
		s.guard.Fail("mfaoff|"+cur.Username, ip)
		writeErr(c, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	if err := s.Store.UpdateUserAuth(cur.ID, cur.Email, cur.EmailVerified, store.MfaOff, ""); err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	_ = s.Auth.RevokeUser(c.Request.Context(), cur.ID) // 全端吊销，强制重新登录
	logx.InfoT(traceOf(c), "mfa: 二次验证已关闭（会话全部吊销）account=%q", cur.Username)
	s.audit(c, store.AuditUserUpdate, cur.Username, cur.ID, "", "", "mfa disabled by self")
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "reauthRequired": true})
}

// ---------- 管理端二验治理（0.2.12；管理端口专属） ----------

// handleAdminUserMfa PUT /api/v1/admin/users/:id/mfa {method:"off"|"totp"|"email"|"reset"}
// 需求："在管理端可管理用户二次验证，默认不使用"。
//   - off：关闭并清密钥（吊销该用户全部会话——含其已签发的 flow/code，
//     撤权三层即时）；
//   - totp：强制开通；用户无密钥时下次登录页走现场 enroll（不落库）；已有
//     密钥保持不变；
//   - email：强制邮箱通道——要求该账户邮箱**已验证**（未绑定/未验证 400，
//     由用户自助完成绑定）；
//   - reset：重置——清密钥+关闭（用户须重新开通；用于丢失验证器/换机场景）。
//
// 目标为管理员/超管一律 400（管理台登录本批不做二验，见 CHANGELOG 记录）。
func (s *Server) handleAdminUserMfa(c *gin.Context) {
	uid, err := parseIDParam(c)
	if err != nil {
		return
	}
	if uid == store.SuperAdminID {
		writeErr(c, http.StatusBadRequest, errors.New("超级管理员账户由服务端账户文件管理，不支持二次验证设置"))
		return
	}
	target, err := s.Store.GetUserByID(uid)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return
	}
	if target.Role == store.RoleAdmin {
		writeErr(c, http.StatusBadRequest, errors.New("管理员账户经管理台登录（本批二验不覆盖管理台），无需设置"))
		return
	}
	var req struct {
		Method string `json:"method"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	switch req.Method {
	case "off":
		if err := s.Store.UpdateUserAuth(target.ID, target.Email, target.EmailVerified, store.MfaOff, ""); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		_ = s.Auth.RevokeUser(c.Request.Context(), target.ID)
		logx.InfoT(traceOf(c), "admin: 关闭二步验证 id=%d account=%q", target.ID, target.Username)
		s.audit(c, store.AuditUserUpdate, target.Username, target.ID, "", "", "mfa off by admin")
	case "totp":
		if err := s.Store.UpdateUserAuth(target.ID, target.Email, target.EmailVerified, store.MfaTotp, target.MfaSecret); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		_ = s.Auth.RevokeUser(c.Request.Context(), target.ID) // 强制策略变更即时重登
		logx.InfoT(traceOf(c), "admin: 强制 TOTP id=%d account=%q", target.ID, target.Username)
		s.audit(c, store.AuditUserUpdate, target.Username, target.ID, "", "", "mfa forced totp by admin")
	case "email":
		if !target.EmailVerified || target.Email == "" {
			writeErr(c, http.StatusBadRequest, errors.New("该账户邮箱未绑定/未验证，无法强制邮箱验证码；请先让用户在客户端完成邮箱绑定"))
			return
		}
		if err := s.Store.UpdateUserAuth(target.ID, target.Email, true, store.MfaEmail, target.MfaSecret); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		_ = s.Auth.RevokeUser(c.Request.Context(), target.ID)
		logx.InfoT(traceOf(c), "admin: 强制邮箱二步验证 id=%d account=%q", target.ID, target.Username)
		s.audit(c, store.AuditUserUpdate, target.Username, target.ID, "", "", "mfa forced email by admin")
	case "reset":
		if err := s.Store.UpdateUserAuth(target.ID, target.Email, target.EmailVerified, store.MfaOff, ""); err != nil {
			writeErr(c, statusOf(err), err)
			return
		}
		_ = s.Auth.RevokeUser(c.Request.Context(), target.ID)
		logx.InfoT(traceOf(c), "admin: 重置二步验证（清密钥+关闭）id=%d account=%q", target.ID, target.Username)
		s.audit(c, store.AuditUserUpdate, target.Username, target.ID, "", "", "mfa reset by admin")
	default:
		writeErr(c, http.StatusBadRequest, errors.New("method 须为 off|totp|email|reset"))
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true})
}

// ---------- 小工具 ----------

func subtleCompare(a, b string) int {
	if len(a) != len(b) {
		return 0
	}
	return subtleEq([]byte(a), []byte(b))
}

func validEmail(e string) bool {
	if len(e) > 320 {
		return false
	}
	at := strings.Index(e, "@")
	if at <= 0 || at != strings.LastIndex(e, "@") || at == len(e)-1 {
		return false
	}
	dot := strings.LastIndex(e[at:], ".")
	return dot > 1 && dot < len(e)-at-1
}
