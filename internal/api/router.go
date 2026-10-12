// Package api 的路由装配（gin 路由器分组，需求17 统一 /api/v1）。
// 外部契约（路径/方法/参数/错误信封/状态码）与 0.2.x 的 net/http 版本逐一对应，
// 客户端无需感知服务端框架切换。
package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/adminui"
	"filemanageServer/internal/version"
)

// Handler 用户/客户端端口路由（不含任何管理路由：/admin/ 与
// /api/v1/admin/* 只在管理端口 AdminHandler 上可达，两端口完全独立）。
// 返回 *gin.Engine（实现 http.Handler），main.go 以 http.Serve/ServeTLS 承载。
// 将来新增 API 版本时：新建 registerV2(e) 并追加到 APIVersions 即可，
// 旧版本路由保持不变（版本迭代的 API 管理）。
func (s *Server) Handler() *gin.Engine {
	e := s.newEngine(s.lanFilter())
	s.registerV1(e)
	return e
}

// AdminHandler 管理面端口（cfg.AdminListen，默认仅 bind 127.0.0.1）路由：
// 内嵌管理台 /admin/ + 全部管理 API + 同源复用业务端点（登录/ACL/分享总览等）。
// adminScope 标记使路径解析按物理根全量进行（管理台直接维护物理路径，无 jail 限制）。
// 网络准入走 adminIPFilter（0.1.0 需求38 adminAccess，替代该引擎上的 lanOnly 判定）。
func (s *Server) AdminHandler() *gin.Engine {
	e := s.newEngine(s.adminIPFilter(), s.adminScope())
	s.registerV1(e)
	// 数据库配置面注册在 requireDB 之外（降级模式可达）；0.1.0 需求33
	// 起闸门收敛为单一态（超管/DB 管理员登录态，超管无库可用），见 v1_dbconfig.go
	s.registerAdminDBAPI(e)
	// 0.1.0 需求33：超管直接登录使降级模式管理台可完整工作——info/super 注册在
	// requireDB 之外（两者零 Store 依赖或 proxy 安全）；其余管理 API 仍需元数据库。
	lite := e.Group("/api/v1/admin", s.authCtx(), s.requireAdmin())
	lite.GET("/info", s.handleAdminInfo)
	// 需求33：超管账户页进一步收紧为仅超管本人（DB 管理员不可触达）
	lite.GET("/super", s.requireSuperAdmin(), s.handleAdminGetSuper)
	lite.PUT("/super", s.requireSuperAdmin(), s.handleAdminPutSuper)
	// 0.1.1：业务管理组同样受维护暂停屏障（迁移/导出窗口内管理写入同样破坏一致性）；
	// lite 组（info/super）与 /admin/db 配置面不受限，操作员始终可解除暂停。
	admin := e.Group("/api/v1/admin", s.requireDB(), s.authCtx(), s.requireAdmin(), s.maintenanceGate())
	s.registerAdminAPI(admin)
	adminui.Mount(e)
	return e
}

// adminScope 管理端口标记中间件（见 scope.go computeBases）。
func (s *Server) adminScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(adminModeKey, true)
		c.Next()
	}
}

// newEngine 构建 gin 引擎。netGuard 为该端口的网络准入中间件
// （0.1.0 需求38：用户端口=lanFilter，管理端口=adminIPFilter，互不叠加——
// lanOnly 只管客户端面，管理面由 adminAccess 独立决定）。
func (s *Server) newEngine(netGuard gin.HandlerFunc, extra ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.RedirectTrailingSlash = false // ServeMux 无自动重定向，保持路由语义一致
	e.HandleMethodNotAllowed = true
	e.NoRoute(func(c *gin.Context) {
		writeErr(c, http.StatusNotFound, fmt.Errorf("not found: %s", c.Request.URL.Path))
	})
	e.NoMethod(func(c *gin.Context) {
		writeErr(c, http.StatusMethodNotAllowed, errors.New("method not allowed"))
	})
	// 全局中间件顺序：logger 最外 → 网络准入 → QPS → CORS →（管理端口）adminScope → 业务
	e.Use(loggerGin(), netGuard, s.qpsLimit(), s.corsGin())
	for _, h := range extra {
		e.Use(h)
	}
	e.GET("/api", s.handleAPIVersions)
	return e
}

// handleAPIVersions GET /api → 支持的 API 版本列表。
// adminUI 字段仅在管理端口出现（管理面与用户端口物理隔离）。
func (s *Server) handleAPIVersions(c *gin.Context) {
	out := map[string]any{
		"server":      version.Version,
		"versions":    version.APIVersions,
		"current":     version.CurrentAPI,
		"baseUrl":     "/api/" + version.CurrentAPI,
		"instanceId":  s.Cfg.InstanceID,
		"health":      "/api/v1/health",
		"deprecation": "旧版本 API 至少保留一个大版本周期",
	}
	if c.GetBool(adminModeKey) {
		out["adminUI"] = "/admin/"
	}
	writeJSON(c, http.StatusOK, out)
}

// registerV1 以 gin 分组注册 /api/v1 全部端点。
// 分组即权限矩阵（CLAUDE.md 规约：依赖元数据的端点必须落在 requireDB 之内）：
//   - public  ：健康检查、登录注册刷新（后三者需要元数据库）；
//   - anon    ：可选认证，降级模式（无库）下仍可用的匿名只读端点；
//   - login   ：requireDB + 登录；
//   - admin   ：requireDB + 管理员；
//   - share   ：分享链接匿名访问（token+密码授权，需要元数据库）。
func (s *Server) registerV1(e *gin.Engine) {
	v1 := e.Group("/api/v1", s.maintenanceGate()) // 0.1.1：组内统一维护暂停屏障（health 豁免）

	// ---- 公开 ----
	v1.GET("/health", s.handleHealth)
	// 0.1.0 需求32/33：login 摘掉路由级 requireDB——超管账户文件不依赖数据库，
	// 降级模式管理台必须能登录；普通账户在 handler 内判无库 → 503（errNoDB）。
	// 引导注册端点随文件超管模型移除（管理员由超管在管理台创建，需求33）。
	v1.POST("/auth/login", s.handleLogin)
	// 0.2.4：/auth/refresh 移除——滑动续期由 authCtx→Touch 服务端无感完成，
	// 凭证只在重新登录时更换（旧客户端 refresh 退化=401 重登，兼容矩阵已明示）。

	// ---- 分享链接匿名访问（需求7，凭 token+密码，无需登录）----
	shr := e.Group("/s/:token", s.maintenanceGate(), s.requireDB())
	shr.GET("", s.handleShareInfo)
	shr.POST("/verify", s.handleShareVerify)
	shr.GET("/files", s.handleShareFiles)
	shr.GET("/download", s.handleShareDownload)

	// ---- 可匿名浏览（需求6：未登录按 public ACL 只读；降级模式亦走此组）----
	// download/preview 在此组但内部按模式分支：有库时 requirePerm(Download) 强制
	// 登录（与 0.2.x 契约一致，匿名 401），无库时放开匿名下载（用户决策）。
	anon := v1.Group("", s.authCtx())
	anon.GET("/roots", s.handleRoots)
	anon.GET("/files", s.handleList)
	anon.GET("/stat", s.handleStat)
	anon.POST("/search", s.handleSearch)
	anon.GET("/download", s.handleDownload)
	anon.GET("/preview", s.handlePreview)
	anon.GET("/thumb", s.handleThumb) // 0.2.1 服务端缩略图（加性；守卫/匿名语义同 /preview）

	// ---- 需登录（+元数据库）----
	auth := v1.Group("", s.requireDB(), s.authCtx(), s.requireLogin())
	auth.POST("/auth/logout", s.handleLogout)
	auth.GET("/auth/me", s.handleMe)
	// 需求31：登录用户自助改密（校验旧密码；改后全部会话作废、强制重新登录）
	auth.POST("/auth/change-password", s.handleChangePassword)

	auth.POST("/mkdir", s.handleMkdir)
	auth.POST("/mkdirs", s.handleMkdirs) // 需求41：目录上传批量建目录（加性端点）
	auth.POST("/check", s.handleCheck)   // 0.2.1：目录重传批量校验（相同跳过/不同询问的预检）
	auth.POST("/rename", s.handleRename)
	auth.POST("/move", s.handleMove)
	auth.POST("/copy", s.handleCopy)
	auth.POST("/delete", s.handleDelete)

	auth.POST("/upload", s.handleUpload) // 兼容旧客户端的整文件上传

	// 分片上传/断点续传（需求3）
	auth.POST("/upload/session", s.handleUploadSessionCreate)
	auth.GET("/upload/session", s.handleUploadSessionStatus)
	auth.PUT("/upload/chunk", s.handleUploadChunk)
	auth.POST("/upload/complete", s.handleUploadComplete)
	auth.DELETE("/upload/session", s.handleUploadSessionAbort)

	auth.GET("/trash", s.handleTrashList)
	auth.POST("/trash/restore", s.handleTrashRestore)
	auth.POST("/trash/remove", s.handleTrashRemove)
	auth.POST("/trash/clear", s.handleTrashClear)

	// 文件版本（需求13）
	auth.GET("/versions", s.handleVersionList)
	auth.GET("/versions/content", s.handleVersionContent)
	auth.POST("/versions/rollback", s.handleVersionRollback)
	auth.DELETE("/versions/:id", s.handleVersionDelete)

	// 分享链接管理（需求7）
	auth.GET("/share/links", s.handleShareList)
	auth.POST("/share/links", s.handleShareCreate)
	auth.PUT("/share/links/:token", s.handleShareUpdate)
	auth.DELETE("/share/links/:token", s.handleShareDelete)

	// 目录权限（需求8/9）
	auth.GET("/acl", s.handleACLGet)
	auth.PUT("/acl", s.handleACLPut)
	auth.DELETE("/acl/:id", s.handleACLDelete)
	auth.GET("/acl/subjects", s.handleACLSubjects)

	// AI
	auth.POST("/ai/describe", s.handleAIDescribe)
	auth.GET("/ai/description", s.handleAIGetDescription)
	auth.POST("/ai/search", s.handleAISearch)
}

// registerAdminAPI 管理员组（仅注册在管理端口 AdminHandler 上，
// 用户端口不存在这些路由——404；0.1.0：info/super 已移至 AdminHandler 的
// lite 组（组外 requireDB），本组全部端点仍需元数据库）。
func (s *Server) registerAdminAPI(admin *gin.RouterGroup) {
	admin.GET("/users", s.handleAdminListUsers)
	admin.POST("/users", s.handleAdminCreateUser)
	admin.PUT("/users/:id", s.handleAdminUpdateUser)
	admin.DELETE("/users/:id", s.handleAdminDeleteUser)
	// 用户根分配（jail 模型：管理员分配目录，一人可多个）
	admin.GET("/userroots", s.handleAdminListAllUserRoots)
	admin.GET("/users/:id/roots", s.handleAdminGetUserRoots)
	admin.PUT("/users/:id/roots", s.handleAdminSetUserRoots)
	// 物理根只读目录浏览（分配目录时的选择器；仅管理端口可用）
	admin.GET("/fs", s.handleAdminBrowseFS)
	admin.GET("/groups", s.handleAdminListGroups)
	admin.POST("/groups", s.handleAdminCreateGroup)
	admin.PUT("/groups/:id", s.handleAdminUpdateGroup)
	admin.DELETE("/groups/:id", s.handleAdminDeleteGroup)
	admin.GET("/acls", s.handleAdminListACLs)
	admin.GET("/links", s.handleAdminListLinks)
	admin.GET("/audits", s.handleAdminListAudits) // 审计日志
}
