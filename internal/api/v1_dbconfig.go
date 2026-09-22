// v1_dbconfig.go：管理台数据库配置面（0.7.0）——驱动选型、状态查看、
// 测试连接、连接/切换（含可选跨库数据迁移），全部仅存在于 adminListen。
//
// 与 /api/v1/admin/*（整组挂 requireDB）不同，本组注册在 requireDB 之外：
// 降级模式（无库）下管理台必须可达才能配库。鉴权（dbConfigGuard，0.1.0 需求32/33
// 收敛为单一态）：文件超管或有库 DB 管理员的标准登录态（authCtx+requireAdmin）
// ——超管令牌不依赖元数据库，无库时同样放行；0.7.0 的 firstBoot 开放与
// adminPassword 解锁令牌流程已随超管文件账户模型移除。
//
// connect 为热切换（store.Proxy）：开新库→建表→（可选）全表搬迁→原子换入→
// FlushAll（redis）→改写配置文件 db 段（.bak 备份）→关旧库。旧库会话随库失效，
// 需要重新登录；切换仅作用于本进程（多实例逐台执行或重启，见 README）。
package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/auth"
	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// registerAdminDBAPI 挂在 AdminHandler 上、requireDB 组之外的数据库配置路由。
func (s *Server) registerAdminDBAPI(e *gin.Engine) {
	g := e.Group("/api/v1/admin/db")
	g.GET("/status", s.handleAdminDBStatus)   // 软判定：无权限时只回最小信息
	g.GET("/drivers", s.handleAdminDBDrivers) // 静态目录，开放
	g.POST("/test", s.dbConfigGuard(), s.handleAdminDBTest)
	g.POST("/connect", s.dbConfigGuard(), s.handleAdminDBConnect)
}

// dbConfigGuard 管理员登录态（超管或 DB 管理员；两态判定都在 authCtx 内完成，
// 超管分支零 Store 调用故降级模式可用）。仅注册于管理端口引擎（adminScope 保证）。
func (s *Server) dbConfigGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !c.GetBool(adminModeKey) {
			// 防御（正常不可能发生）：用户端口不存在本组路由
			writeErr(c, http.StatusNotFound, errors.New("not found"))
			c.Abort()
			return
		}
		s.authCtx()(c)
		if c.IsAborted() {
			return
		}
		s.requireAdmin()(c)
		if c.IsAborted() {
			return
		}
		c.Set(dbConfigAuthKey, "admin")
		c.Next()
	}
}

const dbConfigAuthKey = "fm.dbcfg"

// dbStatusAuthed status 的软授权判定（不写响应）：超管令牌（零 Store 调用，
// 降级模式可用）或有效 DB 管理员 Bearer（需要有库）。
func (s *Server) dbStatusAuthed(c *gin.Context) bool {
	h := c.Request.Header.Get("Authorization")
	ts, ok := cutBearer(h)
	if !ok {
		return false
	}
	cl, err := auth.ParseAccessClaims(s.Cfg.JWTSecret, ts)
	if err != nil {
		return false
	}
	if cl.Super {
		if s.Super == nil {
			return false
		}
		gen, gerr := s.Super.Generation()
		return gerr == nil && gen == cl.Gen
	}
	if !s.dbReady() {
		return false
	}
	u, err := s.Store.GetUserByID(cl.UID)
	return err == nil && u != nil && u.Role == store.RoleAdmin
}

func cutBearer(h string) (string, bool) {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):], true
	}
	return "", false
}

// handleAdminDBStatus GET /admin/db/status —— 状态查看（登录页探测/setup 模式
// 判定也调它，故永远 200；无权限时仅回最小字段，绝不回 DSN/表统计）。
func (s *Server) handleAdminDBStatus(c *gin.Context) {
	out := map[string]any{
		"serverVersion": version.Version,
		"schemaVersion": s.Cfg.SchemaVersion,
		"degraded":      !s.dbReady(),
		"configured":    s.dbReady(),
		"dataDir":       s.Cfg.DataDir,
		"usersDir":      s.Cfg.UsersDir(),
		// 0.1.0 需求32/33：降级模式登录台改用超管文件账户（0.7.x 的
		// firstBoot/needsUnlock 键已移除），登录页据此展示超管引导文案。
		"superAdmin": s.superAdminName(),
	}
	// hasUsers 始终上报（仅"有无账户"一位事实，登录页据此提示"注册初始管理员"）
	if s.dbReady() {
		if n, err := s.Store.CountUsers(); err == nil {
			out["hasUsers"] = n > 0
		}
	}
	if !s.dbStatusAuthed(c) {
		out["access"] = false
		writeJSON(c, http.StatusOK, out)
		return
	}
	out["access"] = true
	if !s.dbReady() {
		writeJSON(c, http.StatusOK, out)
		return
	}
	out["driver"] = s.Store.Driver()
	out["dsn"] = redactDSN(s.Cfg.DB.DSN)
	out["connected"] = store.Ping(s.Store) == nil
	if n, err := s.Store.CountUsers(); err == nil {
		out["hasUsers"] = n > 0
	}
	if tc, err := store.TableCounts(s.DBProxy.Current()); err == nil {
		out["tables"] = tc
	}
	writeJSON(c, http.StatusOK, out)
}

// handleAdminDBDrivers GET /admin/db/drivers —— 支持的驱动与 DSN 模板（静态）。
func (s *Server) handleAdminDBDrivers(c *gin.Context) {
	writeJSON(c, http.StatusOK, gin.H{"drivers": []map[string]string{
		{"id": "sqlite3", "name": "SQLite（单文件，免安装）",
			"dsnHint": "文件路径，如 /var/lib/filemanage/fm.db（相对路径按进程工作目录解析）"},
		{"id": "mysql", "name": "MySQL / MariaDB",
			"dsnHint": "user:password@tcp(127.0.0.1:3306)/filemanage"},
		{"id": "postgresql", "name": "PostgreSQL",
			"dsnHint": "postgres://user:password@127.0.0.1:5432/filemanage"},
		{"id": "none", "name": "不配置数据库（降级模式：仅匿名只读浏览）", "dsnHint": ""},
	}})
}

// handleAdminDBTest POST /admin/db/test {driver,dsn} —— 打开新连接、PING、即关；
// 不建表、不改状态、不落盘（注意：sqlite 打开即创建空文件，属预期副作用）。
func (s *Server) handleAdminDBTest(c *gin.Context) {
	var req struct {
		Driver string `json:"driver"`
		DSN    string `json:"dsn"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	driver, err := store.NormalizeDriver(req.Driver)
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if driver == "" {
		writeErr(c, http.StatusBadRequest, errors.New(`driver "none" cannot be tested (degraded mode)`))
		return
	}
	st, err := store.Open(store.Options{Driver: driver, DSN: req.DSN})
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	defer st.Close()
	if err := store.Ping(st); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "driver": driver})
}

// handleAdminDBConnect POST /admin/db/connect {driver,dsn,migrate?,force?} ——
// 连接/切换/断开（driver=none）数据库并热生效。序列见文件头注释；dbSwapMu 串行化。
func (s *Server) handleAdminDBConnect(c *gin.Context) {
	var req struct {
		Driver  string `json:"driver"`
		DSN     string `json:"dsn"`
		Migrate bool   `json:"migrate"`
		Force   bool   `json:"force"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	driver, err := store.NormalizeDriver(req.Driver)
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	s.dbSwapMu.Lock()
	defer s.dbSwapMu.Unlock()

	var counts map[string]int64
	newStore := store.Store(nil)
	if driver != "" {
		if req.DSN == "" {
			writeErr(c, http.StatusBadRequest, errors.New("dsn required"))
			return
		}
		st2, err := store.Open(store.Options{Driver: driver, DSN: req.DSN})
		if err != nil {
			writeErr(c, http.StatusBadRequest, err)
			return
		}
		if err := st2.Migrate(); err != nil { // 幂等建表（失败即拒绝切换）
			st2.Close()
			writeErr(c, http.StatusInternalServerError, err)
			return
		}
		if req.Migrate && s.dbReady() {
			if !req.Force {
				if nonEmpty, e := store.TableNonEmpty(st2); e != nil {
					st2.Close()
					writeErr(c, http.StatusInternalServerError, e)
					return
				} else if len(nonEmpty) > 0 {
					st2.Close()
					writeErr(c, http.StatusConflict, fmt.Errorf(
						"target database is not empty (tables: %s); retry with force=true to migrate into it anyway",
						strings.Join(nonEmpty, ",")))
					return
				}
			}
			if counts, err = store.CopyAllTables(st2, s.DBProxy.Current()); err != nil {
				st2.Close()
				writeErr(c, http.StatusInternalServerError, err)
				return
			}
		}
		newStore = st2
	}

	old := s.DBProxy.Set(newStore)
	if s.Cache != nil {
		if ferr := s.Cache.FlushAll(); ferr != nil {
			// 缓存失效失败只告警：键有 TTL，且已切换为新库为准
			log.Printf("db connect: cache flush: %v", ferr)
		}
	}
	// 家目录存量补齐（幂等；空新库无用户也直接落标记，见 EnsureUserHomes）
	if s.dbReady() {
		if herr := fileops.EnsureUserHomes(s.DBProxy, s.Cfg.UsersDir()); herr != nil {
			log.Printf("db connect: ensure homes: %v", herr)
		}
	}
	if old != nil {
		_ = old.Close() // 在途个别请求可能失败一次，可接受（冒烟口径）
	}
	// 配置持久化（写规范化驱动名；断开=none）。绝不整体序列化 Config（会落 jwtSecret）。
	persistDriver, persistDSN := driver, req.DSN
	if persistDriver == "" {
		persistDriver = "none"
		persistDSN = ""
	}
	warning := ""
	if s.CfgPath != "" {
		if err := config.PersistDBEdit(s.CfgPath, persistDriver, persistDSN); err != nil {
			warning = "数据库已切换生效，但配置文件写入失败：" + err.Error() + "（请手工更新 server.json 的 db 段）"
		}
	} else {
		warning = "未持有配置文件路径（内存配置启动）：切换仅本次进程有效"
	}
	// 运行期内存配置同步（health/admin info 立即反映新值）
	s.Cfg.DB.Driver, s.Cfg.DB.DSN = persistDriver, persistDSN
	auditAction := "db_switch"
	if driver == "" {
		auditAction = "db_disconnect"
	}
	// 审计写新库（s.Store 经代理已指向新委托；断开后降级态自动静默）
	s.audit(c, auditAction, "", 0, "", "", "driver="+persistDriver)
	out := map[string]any{"ok": true, "driver": driver, "degraded": !s.dbReady()}
	if counts != nil {
		out["migrated"] = counts
	}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(c, http.StatusOK, out)
}

// passwordKVRe pg key=value 风格 DSN 的密码键。
var passwordKVRe = regexp.MustCompile(`(?i)(password\s*=\s*)(\S+)`)

// redactDSN 掩码 DSN 中的密码（URL scheme://u:pw@host、mysql user:pw@tcp(...)、
// pg key=value 三路；启发式尽力而为，状态接口永不回显原始 DSN——敏感信息约定）。
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		scheme, rest := dsn[:i+3], dsn[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			userinfo := rest[:at]
			if c := strings.Index(userinfo, ":"); c >= 0 {
				return scheme + userinfo[:c] + ":•••" + rest[at:]
			}
		}
	}
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		userinfo := dsn[:at]
		if c := strings.Index(userinfo, ":"); c >= 0 {
			return userinfo[:c] + ":•••" + dsn[at:]
		}
	}
	return passwordKVRe.ReplaceAllString(dsn, "${1}•••")
}
