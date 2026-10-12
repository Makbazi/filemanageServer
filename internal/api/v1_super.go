package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/auth"
	store "filemanageServer/internal/store"
)

// v1_super.go：超级管理员账户文件的管理端点（0.1.0 需求32/34）。
// 仅注册在管理端口 lite 组（authCtx+requireAdmin，组外 requireDB），且进一步
// 收紧为**仅超管本人**可访问——其他管理员不得触达超管（需求33）。
// 明文可见性遵循需求34：账户口令在使用前（或文件重置后再使用前）为明文可回显，
// 一经成功登录即转 bcrypt 密文，此后 GET 不再返回明文。

// requireSuperAdmin 仅超管本人放行（管理面分层，需求33）。
func (s *Server) requireSuperAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isSuperAdmin(c) {
			writeErr(c, http.StatusForbidden, errors.New("仅超级管理员可维护超级管理员账户"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// handleAdminGetSuper GET /api/v1/admin/super → 超管账户概览（永不含密文原文）。
func (s *Server) handleAdminGetSuper(c *gin.Context) {
	name, err := s.Super.Username()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	gen, err := s.Super.Generation()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	pw, plaintext, err := s.Super.Plaintext()
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{
		"username":   name,
		"generation": gen,
		// 需求34：仅未使用（明文）时回显口令；已转密文则只报状态。
		"password":  pw,
		"plaintext": plaintext,
		"passwordHint": map[bool]string{true: "当前为明文（首次登录成功后自动转密文）",
			false: "已是密文（bcrypt），不可回显；如需重置请编辑账户文件或配置 adminPassword"}[plaintext],
	})
}

// handleAdminPutSuper PUT /api/v1/admin/super {username?,password?} → 改账户名/口令。
// 任一变更都推进凭据纪元（吊销全部已发超管令牌，含本次调用者），故成功时
// 直接重发一套超管令牌回给管理台换用（否则改密后当前会话立即失效，无法继续操作）。
func (s *Server) handleAdminPutSuper(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if req.Username == "" && req.Password == "" {
		writeErr(c, http.StatusBadRequest, errors.New("username or password required"))
		return
	}
	acct, err := s.Super.SetCredentials(req.Username, req.Password)
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	// 纪元已变 → 三层显式清空全部超管凭证（0.2.3：authCtx 的逐请求纪元核对仍是
	// 权威兜底，此处主动清理让旧令牌即刻、跨实例消失而非等 memTTL/自然过期），
	// 再以新纪元签发新凭证回给管理台换用（否则改密后当前会话立即失效无法继续操作）。
	_ = s.Auth.RevokeUser(c.Request.Context(), store.SuperAdminID)
	persist := s.dbReady()
	at, err := s.Auth.Issue(c.Request.Context(), auth.Spec{
		Kind: store.AuthKindUser, UserID: store.SuperAdminID, Username: acct.Username,
		Role: store.RoleAdmin, Super: true, Gen: acct.Generation,
		TTL: s.Cfg.AccessTTLDuration(), Persist: persist,
	})
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	who := "username/password"
	if req.Password == "" {
		who = "username"
	} else if req.Username == "" {
		who = "password"
	}
	s.audit(c, store.AuditSuperUpdate, acct.Username, store.SuperAdminID, "", "", "superadmin account updated: "+who)
	// 0.2.4 响应收敛：accessToken/expiresIn 别名删除（管理台只读 token/username，
	// 见 app.js saveSuper），保留 user 概要之外的管理面自有字段。
	writeJSON(c, http.StatusOK, map[string]any{
		"ok":        true,
		"username":  acct.Username,
		"plaintext": !isHashedPassword(acct.Password),
		"token":     at,
		"updatedAt": acct.UpdatedAt.Format(time.RFC3339),
		"notice":    "口令已以明文写入账户文件，首次成功登录后自动转密文",
	})
}

// isHashedPassword bcrypt 摘要前缀判定（与 superadmin 内部一致，供响应标注明文态）。
func isHashedPassword(s string) bool {
	return len(s) >= 3 && s[0] == '$' && s[1] == '2'
}
