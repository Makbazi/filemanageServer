// Package api 的 gin 中间件（0.3.0：由 net/http 包装器迁移而来，判定语义与响应
// 信封逐字保持不变）。用户态注入从 request context 改为 gin 键值（c.Set/c.Get）。
package api

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/auth"
	store "filemanageServer/internal/store"
)

// userKey gin 上下文键：当前登录用户（*store.User）。
const userKey = "fm.user"

// errNoDB 降级模式统一错误（0.3.0：未配置元数据库时元数据端点返回 503）。
var errNoDB = errors.New("metadata database not configured")

// requireDB 元数据端点守卫：未配置数据库 → 503。
// 路由规约（CLAUDE.md）：所有依赖 Store 的端点/分组必须挂本中间件。
func (s *Server) requireDB() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.dbReady() {
			writeErr(c, http.StatusServiceUnavailable, errNoDB)
			c.Abort()
			return
		}
		c.Next()
	}
}

// maintenanceGate 0.1.1 维护暂停屏障：切换（含迁移）/导出窗口与手动暂停期间，
// 冻结全部业务/管理写读端点（503），保证源/目标存储在该窗口内无并发变更。
// 豁免：/api/v1/health（负载均衡探测）与 /api 版本发现；管理台登录与 /admin/db
// 配置面不经本守卫（挂在各自分组之外），操作员永远可解除暂停。
func (s *Server) maintenanceGate() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.Paused() && c.Request.URL.Path != "/api/v1/health" {
			writeErr(c, http.StatusServiceUnavailable,
				errors.New("服务维护中（存储切换/数据导出进行或已手动暂停），请稍后重试"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// authCtx 可选认证中间件（需求6）：
//   - 携带有效 Bearer token → 注入登录用户；
//   - 未携带 token → 匿名主体（由权限引擎判定可浏览范围）；
//   - 携带无效/过期 token → 401（提示客户端走 refresh 流程）；
//   - 降级模式（无元数据库）下携带**普通用户** token → 503（无法校验用户，拒绝冒充）。
//
// 0.1.0 需求32/33/36 重排：先解析 JWT 声明再分流——超管令牌（claims.Super）
// 校验凭据纪元后注入合成用户，全程零 Store 调用（降级模式同样可用，且天然
// 绕开 redis 用户缓存）；普通令牌才落库查用户。两分支共用管理员端口隔离
// 规则：admin 身份（含超管）在用户端口一律 403。
func (s *Server) authCtx() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Request.Header.Get("Authorization")
		ts, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || ts == "" {
			c.Next()
			return
		}
		cl, err := auth.ParseAccessClaims(s.Cfg.JWTSecret, ts)
		if err != nil {
			writeErr(c, http.StatusUnauthorized, errors.New("token invalid or expired"))
			c.Abort()
			return
		}
		if cl.Super {
			if s.Super == nil { // 防御：未装配超管（仅测试构造可为 nil）
				writeErr(c, http.StatusUnauthorized, errors.New("superadmin not configured"))
				c.Abort()
				return
			}
			if !c.GetBool(adminModeKey) {
				writeErr(c, http.StatusForbidden, errors.New("admin accounts cannot be used on the client API; use the admin console"))
				c.Abort()
				return
			}
			gen, serr := s.Super.Generation()
			if serr != nil || gen != cl.Gen {
				writeErr(c, http.StatusUnauthorized, errors.New("superadmin credential changed (password rotated or account file reset), please login again"))
				c.Abort()
				return
			}
			c.Set(userKey, &store.User{ID: store.SuperAdminID, Username: cl.Username, Role: store.RoleAdmin})
			c.Next()
			return
		}
		if !s.dbReady() {
			writeErr(c, http.StatusServiceUnavailable, errNoDB)
			c.Abort()
			return
		}
		u, err := s.Store.GetUserByID(cl.UID)
		if err != nil {
			writeErr(c, http.StatusUnauthorized, errors.New("user not found"))
			c.Abort()
			return
		}
		// 0.6.0 决策D3（需求11/12）：管理员身份仅限服务端管理台（adminListen 独立端口），
		// 用户端口对 admin 的一切请求（含升级前已发 token）一律 403。
		if u.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
			writeErr(c, http.StatusForbidden, errors.New("admin accounts cannot be used on the client API; use the admin console"))
			c.Abort()
			return
		}
		c.Set(userKey, u)
		c.Next()
	}
}

// requireLogin 必须登录（匿名 → 401）。须与 authCtx 搭配（路由分组已保证顺序）。
func (s *Server) requireLogin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if currentUser(c) == nil {
			writeErr(c, http.StatusUnauthorized, errors.New("login required"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireAdmin 必须管理员（登录 + admin 角色）。
func (s *Server) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		u := currentUser(c)
		if u == nil {
			writeErr(c, http.StatusUnauthorized, errors.New("login required"))
			c.Abort()
			return
		}
		if u.Role != store.RoleAdmin {
			writeErr(c, http.StatusForbidden, errors.New("admin privilege required"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// lanFilter 默认只允许局域网（RFC1918 / ULA / loopback）客户端访问。
// 与 0.2.x 一致：始终以 RemoteAddr 判定（不信任 X-Forwarded-For）。
func (s *Server) lanFilter() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.Cfg.LANOnly {
			c.Next()
			return
		}
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			host = c.Request.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !isLANIP(ip) {
			writeErr(c, http.StatusForbidden, fmt.Errorf("access denied: server only allows LAN clients"))
			c.Abort()
			return
		}
		c.Next()
	}
}

func isLANIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// adminIPFilter 管理端口 IP 准入（0.1.0 需求38）：替代管理引擎上的 lanFilter，
// 按 cfg.adminAccess 判定（local/lan/any/custom）。与 lanFilter 同口径：
// 始终以 RemoteAddr 判定、不信任 X-Forwarded-For。注册在引擎级中间件，
// 故 /admin/ 静态台、admin API、配库 API、/api 发现端点一并受控。
func (s *Server) adminIPFilter() gin.HandlerFunc {
	return func(c *gin.Context) {
		host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
		if err != nil {
			host = c.Request.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !s.Cfg.AdminAllows(ip) {
			writeErr(c, http.StatusForbidden, fmt.Errorf("access denied: admin console policy (adminAccess=%q) does not allow this address", s.Cfg.AdminAccess.Mode))
			c.Abort()
			return
		}
		c.Next()
	}
}

// loggerGin 请求日志（格式与原 net/http logger 一致）。
func loggerGin() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Printf("%s %s %s (%s)", c.Request.Method, c.Request.URL.Path, c.Request.RemoteAddr,
			time.Since(start).Round(time.Millisecond))
	}
}

// ---------- 请求速率与跨域（0.5.1 安全迭代） ----------

// qpsWindow 单 IP 的秒级固定窗口计数。
type qpsWindow struct {
	sec   int64
	count int
}

type qpsLimiter struct {
	mu      sync.Mutex
	windows map[string]*qpsWindow
}

func newQPSLimiter() *qpsLimiter { return &qpsLimiter{windows: map[string]*qpsWindow{}} }

func (l *qpsLimiter) allow(ip string, limit int) bool {
	sec := time.Now().Unix()
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.windows[ip]
	if w == nil || w.sec != sec {
		w = &qpsWindow{sec: sec}
		l.windows[ip] = w
	}
	w.count++
	if len(l.windows) > 4096 { // 只保留当前秒活跃项，防内存膨胀
		for k, v := range l.windows {
			if v.sec != sec {
				delete(l.windows, k)
			}
		}
	}
	return w.count <= limit
}

// qpsLimit 每 IP 每秒请求数上限（rateLimit.qps，0=关闭；登录/分享爆破与扫描器缓解）。
func (s *Server) qpsLimit() gin.HandlerFunc {
	if s.Cfg.RateLimit.QPS <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	lim := newQPSLimiter()
	limit := s.Cfg.RateLimit.QPS
	return func(c *gin.Context) {
		if !lim.allow(clientIP(c), limit) {
			writeErr(c, http.StatusTooManyRequests, errors.New("too many requests"))
			c.Abort()
			return
		}
		c.Next()
	}
}

// corsGin 显式 CORS 白名单（corsAllowOrigins，默认空 = 不下发任何 CORS 头，
// 与 0.3.x 行为一致）。命中白名单回显具体 Origin 并允许预检；不使用通配符
// 回显（凭证虽关闭，仍按最小面处理）。
func (s *Server) corsGin() gin.HandlerFunc {
	allowed := s.Cfg.CORSAllowOrigins
	return func(c *gin.Context) {
		origin := c.Request.Header.Get("Origin")
		if len(allowed) == 0 || origin == "" {
			c.Next()
			return
		}
		ok := false
		for _, a := range allowed {
			if a == "*" || strings.EqualFold(a, origin) {
				ok = true
				break
			}
		}
		if !ok {
			c.Next() // 未授权来源：不加头，浏览器侧跨域读取自然被拒
			return
		}
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
		h.Set("Access-Control-Allow-Credentials", "false") // 认证走 Authorization 头，不依赖 Cookie
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Max-Age", "600")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// ---------- 分享链接密码防爆破（需求7） ----------

// bruteGuard 简单的"token+IP → 失败次数"限流：10 次失败锁定 15 分钟。
type bruteGuard struct {
	mu       sync.Mutex
	failures map[string]*failRecord
}

type failRecord struct {
	count    int
	lockUtil time.Time
	lastFail time.Time
}

func newBruteGuard() *bruteGuard {
	return &bruteGuard{failures: map[string]*failRecord{}}
}

func (g *bruteGuard) key(token, ip string) string { return token + "|" + ip }

// Blocked 是否处于锁定期。
func (g *bruteGuard) Blocked(token, ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.failures[g.key(token, ip)]
	return rec != nil && time.Now().Before(rec.lockUtil)
}

// Fail 记录一次失败；达到阈值则锁定。
func (g *bruteGuard) Fail(token, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := g.key(token, ip)
	rec := g.failures[k]
	now := time.Now()
	if rec == nil || now.Sub(rec.lastFail) > 30*time.Minute {
		rec = &failRecord{}
		g.failures[k] = rec
	}
	rec.count++
	rec.lastFail = now
	if rec.count >= 10 {
		rec.lockUtil = now.Add(15 * time.Minute)
		rec.count = 0
	}
}

// Reset 成功后清除失败计数。
func (g *bruteGuard) Reset(token, ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.failures, g.key(token, ip))
}
