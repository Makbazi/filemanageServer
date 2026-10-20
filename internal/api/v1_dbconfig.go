// v1_dbconfig.go：管理台数据库配置面——驱动选型、状态查看、测试连接、
// 连接/切换（含可选数据迁移）、导出、维护暂停，全部仅存在于 adminListen。
//
// 与 /api/v1/admin/*（整组挂 requireDB）不同，本组注册在 requireDB 与
// maintenanceGate 之外：配库与解除暂停在任何状态下必须可达。鉴权（dbConfigGuard，
// 0.1.0 需求32/33 收敛为单一态）：文件超管或 DB 管理员的标准登录态
// （authCtx+requireAdmin）——超管令牌不依赖元数据库，任何存储形态下同样放行。
//
// 0.1.1 三独立操作（切换与迁移解耦，维护者语义澄清）：
//   - connect：切换引擎；勾选 migrate 才在同窗口全表复制（file↔db 双向，保留主键）。
//     复制→换入→FlushAll 全程包在 withAutoPause 屏障内（用户端口/业务管理端点 503），
//     杜绝迁移/切换窗口内源端并发写造成不一致。
//   - export：纯导出——当前库全表复制到文件存储目录，**不换引擎、不写配置**；
//     全程同屏障；目标非空 409（force 清空重建）。
//   - pause：手动"中断按钮"——维护暂停开关（/status 的 paused 字段驱动 UI 按钮态）。
//
// 旧 connect 的 driver=none"断开回降级模式"臂已废除：""/none 归一为 file（文件存储）。
package api

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/version"
)

// registerAdminDBAPI 挂在 AdminHandler 上、requireDB 组之外的数据库配置路由。
// 0.1.1 三独立操作：connect（切换，可选携数据迁移，file↔db 双向）、
// export（纯导出当前库数据到文件存储目录、不换引擎）、pause（手动维护屏障开关）。
// 本组不经 maintenanceGate——暂停期间操作员必须仍能触达配置面。
func (s *Server) registerAdminDBAPI(e *gin.Engine) {
	g := e.Group("/api/v1/admin/db")
	g.GET("/status", s.handleAdminDBStatus)   // 软判定：无权限时只回最小信息
	g.GET("/drivers", s.handleAdminDBDrivers) // 静态目录，开放
	g.POST("/test", s.dbConfigGuard(), s.handleAdminDBTest)
	g.POST("/connect", s.dbConfigGuard(), s.handleAdminDBConnect)
	g.POST("/export", s.dbConfigGuard(), s.handleAdminDBExport)
	g.POST("/pause", s.dbConfigGuard(), s.handleAdminDBPause)
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
	e, err := s.Auth.Lookup(c.Request.Context(), ts)
	if err != nil || e.Kind != store.AuthKindUser {
		return false
	}
	if e.Super {
		if s.Super == nil {
			return false
		}
		gen, gerr := s.Super.Generation()
		return gerr == nil && gen == e.Gen
	}
	if !s.dbReady() {
		return false
	}
	return e.Role == store.RoleAdmin
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
		// 0.1.0 需求32/33：降级模式登录台改用超管文件账户（此前的
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
	out["paused"] = s.Paused() // 0.1.1 维护暂停状态（UI 按钮/徽章据此渲染）
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
// 0.1.1：file = 内置文件存储（无库默认；旧 "none" 目录项废除——NormalizeDriver
// 已把 ""/none 归一到 file）。
func (s *Server) handleAdminDBDrivers(c *gin.Context) {
	writeJSON(c, http.StatusOK, gin.H{"drivers": []map[string]string{
		{"id": "file", "name": "本地文件存储（无数据库默认，全功能；仅单实例）",
			"dsnHint": "存储目录，留空=<dataDir>/filestore"},
		{"id": "sqlite3", "name": "SQLite（单文件，免安装）",
			"dsnHint": "文件路径，如 /var/lib/filemanage/fm.db（相对路径按进程工作目录解析）"},
		{"id": "mysql", "name": "MySQL / MariaDB",
			"dsnHint": "user:password@tcp(127.0.0.1:3306)/filemanage"},
		{"id": "postgresql", "name": "PostgreSQL",
			"dsnHint": "postgres://user:password@127.0.0.1:5432/filemanage"},
	}})
}

// filestoreDir 解析文件存储目录（dsn 空 = <dataDir>/filestore，与 config.finalize 同规则）。
func (s *Server) filestoreDir(dsn string) string {
	if dsn != "" {
		return dsn
	}
	return filepath.Join(s.Cfg.DataDir, "filestore")
}

// handleAdminDBTest POST /admin/db/test {driver,dsn} —— 打开新连接、PING、即关；
// 不建表、不改状态、不落盘（注意：sqlite 打开即创建空文件，属预期副作用）。
// 0.1.1：file 驱动拒绝测连（Open 即建目录并拿 flock，副作用不归"测试"管）。
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
	if driver == store.DriverFile {
		writeErr(c, http.StatusBadRequest, errors.New("文件存储无需测连（目录在切换/导出时创建并校验）"))
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
// 切换存储引擎并热生效（0.1.1：driver 支持 file=切回/切换到文件存储；""/none 归一为 file）。
// migrate=true 时在同一次操作内先全表复制（file↔db 双向，切换与迁移解耦：
// 不勾选 = 纯切换目标引擎既有内容）。复制→换入全程置于维护屏障（withAutoPause）：
// 期间用户端口与业务管理端点 503，杜绝源端并发写导致的不一致。dbSwapMu 串行化。
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
	dsn := req.DSN
	if driver == store.DriverFile {
		dsn = s.filestoreDir(dsn)
		// 已在用同目录文件存储 → 无意义切换（flock 会自撞，提前给出人话错误）
		if cur, ok := store.IsFileStore(s.DBProxy.Current()); ok && cur.Dir() == dsn {
			writeErr(c, http.StatusBadRequest, errors.New("当前已在使用该文件存储目录，无需切换"))
			return
		}
	} else if dsn == "" {
		writeErr(c, http.StatusBadRequest, errors.New("dsn required"))
		return
	}
	s.dbSwapMu.Lock()
	defer s.dbSwapMu.Unlock()

	var counts map[string]int64
	st2, err := store.Open(store.Options{Driver: driver, DSN: dsn})
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
		// ★ 维护屏障：非空检查→全表复制→换入→冲刷缓存 全程冻结客户端
		if perr := s.withAutoPause(func() error {
			if !req.Force {
				if nonEmpty, e := store.TableNonEmpty(st2); e != nil {
					return e
				} else if len(nonEmpty) > 0 {
					return errTargetNonEmpty{nonEmpty}
				}
			}
			var cerr error
			counts, cerr = store.CopyAllTables(st2, s.DBProxy.Current())
			return cerr
		}); perr != nil {
			st2.Close()
			var ne errTargetNonEmpty
			if errors.As(perr, &ne) {
				writeErr(c, http.StatusConflict, fmt.Errorf(
					"target storage is not empty (tables: %s); retry with force=true to migrate into it anyway",
					strings.Join(ne.tables, ",")))
				return
			}
			writeErr(c, http.StatusInternalServerError, perr)
			return
		}
	}

	old := s.DBProxy.Set(st2)
	if s.Cache != nil {
		if ferr := s.Cache.FlushAll(); ferr != nil {
			// 缓存失效失败只告警：键有 TTL，且已切换为新库为准
			// （0.2.7：走 logx.Warn——默认等级 warn 下真实故障必须可见）
			logx.WarnT(traceOf(c), "db connect: cache flush: %v", ferr)
		}
	}
	// 家目录存量补齐（幂等；空新库无用户也直接落标记，见 EnsureUserHomes）；
	// 0.2.11：随后跑 v8 布局迁移（改名+回收站并级+物理键行改写，同序幂等）。
	if s.dbReady() {
		if herr := fileops.EnsureUserHomes(s.DBProxy, s.Cfg.UsersDir()); herr != nil {
			logx.WarnT(traceOf(c), "db connect: ensure homes: %v", herr)
		}
		if herr := fileops.MigrateUserHomeLayout(s.DBProxy, s.Cfg.UsersDir()); herr != nil {
			logx.WarnT(traceOf(c), "db connect: home layout v8: %v", herr)
		}
	}
	if old != nil {
		_ = old.Close() // 在途个别请求可能失败一次，可接受（冒烟口径）
	}
	// 配置持久化（写规范化驱动名与解析后的 file 目录）。绝不整体序列化 Config（会落 jwtSecret）。
	warning := ""
	if s.CfgPath != "" {
		if err := config.PersistDBEdit(s.CfgPath, driver, dsn); err != nil {
			warning = "数据库已切换生效，但配置文件写入失败：" + err.Error() + "（请手工更新 server.json 的 db 段）"
		}
	} else {
		warning = "未持有配置文件路径（内存配置启动）：切换仅本次进程有效"
	}
	// 运行期内存配置同步（health/admin info 立即反映新值）
	s.Cfg.DB.Driver, s.Cfg.DB.DSN = driver, dsn
	// 审计写新库（s.Store 经代理已指向新委托）
	s.audit(c, "db_switch", "", 0, "", "", fmt.Sprintf("driver=%s dsn=%s migrate=%t", driver, dsn, req.Migrate))
	out := map[string]any{"ok": true, "driver": driver, "degraded": !s.dbReady()}
	if counts != nil {
		out["migrated"] = counts
	}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(c, http.StatusOK, out)
}

// errTargetNonEmpty 目标非空哨兵（映射 409，与旧行为一致）。
type errTargetNonEmpty struct{ tables []string }

func (e errTargetNonEmpty) Error() string { return "target storage is not empty" }

// handleAdminDBExport POST /admin/db/export {dsn?,force?} —— 0.1.1 纯导出：
// 把当前活动存储的全表数据复制到文件存储目录，**不切换引擎**（服务端继续以当前库
// 运行）。导出全程置于维护屏障；目标非空须 force（清空后重建，避免半旧半新）。
func (s *Server) handleAdminDBExport(c *gin.Context) {
	var req struct {
		DSN   string `json:"dsn"`
		Force bool   `json:"force"`
	}
	if err := decode(c, &req); err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if cur, ok := store.IsFileStore(s.DBProxy.Current()); ok {
		writeErr(c, http.StatusBadRequest, fmt.Errorf("当前已是文件存储（%s），导出到文件无意义；换目录请用切换", cur.Dir()))
		return
	}
	dir := s.filestoreDir(req.DSN)
	s.dbSwapMu.Lock()
	defer s.dbSwapMu.Unlock()

	var counts map[string]int64
	err := s.withAutoPause(func() error {
		st2, err := store.Open(store.Options{Driver: store.DriverFile, DSN: dir})
		if err != nil {
			return err
		}
		nonEmpty, e := store.TableNonEmpty(st2)
		if e != nil {
			st2.Close()
			return e
		}
		if len(nonEmpty) > 0 {
			if !req.Force {
				st2.Close()
				return errTargetNonEmpty{nonEmpty}
			}
			// force = 清空目标目录内既有数据文件后重建（同一屏障内完成，客户端不可见中间态）
			if e := st2.Close(); e != nil {
				return e
			}
			if e := store.ClearFileStore(dir); e != nil {
				return e
			}
			if st2, e = store.Open(store.Options{Driver: store.DriverFile, DSN: dir}); e != nil {
				return e
			}
		}
		if e := st2.Migrate(); e != nil {
			st2.Close()
			return e
		}
		counts, e = store.CopyAllTables(st2, s.DBProxy.Current())
		if e != nil {
			st2.Close()
			return e
		}
		return st2.Close() // Close 含最终 fsync + 清单落盘；flock 随之释放
	})
	if err != nil {
		var ne errTargetNonEmpty
		if errors.As(err, &ne) {
			writeErr(c, http.StatusConflict, fmt.Errorf(
				"目标文件存储非空（tables: %s）；force=true 将清空并覆盖", strings.Join(ne.tables, ",")))
			return
		}
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	s.audit(c, "db_export", "", 0, "", "", fmt.Sprintf("to=%s rows=%d", dir, sumCounts(counts)))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "dir": dir, "exported": counts,
		"note": "已复制到文件存储；当前引擎未切换，配置不变"})
}

func sumCounts(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
}

// handleAdminDBPause POST /admin/db/pause {paused} —— 0.1.1 手动维护开关（可选的
// "中断按钮"）：置位后用户端口一切操作与业务管理端点 503（health/配置面豁免），
// 用于切换/导出前后的自定义一致性窗口。
func (s *Server) handleAdminDBPause(c *gin.Context) {
	var req struct {
		Paused *bool `json:"paused"`
	}
	if err := decode(c, &req); err != nil || req.Paused == nil {
		writeErr(c, http.StatusBadRequest, errors.New("paused (true/false) required"))
		return
	}
	state := s.SetManualPause(*req.Paused)
	action := "db_pause"
	if !*req.Paused {
		action = "db_resume"
	}
	s.audit(c, action, "", 0, "", "", "manual="+fmt.Sprint(*req.Paused))
	writeJSON(c, http.StatusOK, map[string]any{"ok": true, "paused": state})
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
