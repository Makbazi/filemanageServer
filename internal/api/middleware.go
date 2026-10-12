// Package api 的 gin 中间件（由 net/http 包装器迁移而来，判定语义与响应
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

// errNoDB 降级模式统一错误（未配置元数据库时元数据端点返回 503）。
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

// authCtx 可选认证中间件（需求6；0.2.3 去 JWT 重构）：
//   - 携带有效 Bearer 凭证 → 经三层认证图（internal/auth.Center）解析身份并注入；
//   - 未携带 token → 匿名主体（由权限引擎判定可浏览范围）；
//   - 凭证无效/过期/已撤 → 401（提示客户端走 refresh 流程）；
//   - 分享解锁凭证（kind=share）不是登录身份 → 一律 401；
//   - 降级异常路径下携带**普通用户**凭证 → 503（拒绝冒充，requireDB 纵深防御沿用）。
//
// 0.1.0 需求32/33/36 语义保留：先解析声明再分流——超管凭证（e.Super）核对
// 凭据纪元后注入合成用户，全程零 Store 调用（降级模式同样可用，天然绕开
// redis 用户缓存）；普通凭证的身份直接来自认证图条目（**不再逐请求 GetUserByID**）。
// 两分支共用管理员端口隔离规则：admin 身份（含超管）在用户端口一律 403。
// 滑动续期：普通用户条目每次认证 Touch（管理台条目绝对 TTL，不续期——
// 保持"控制台每小时重登"节奏）。撤权即时性：L3/L2 立删全实例可见；
// 其他实例 L1 陈旧窗口 ≤ auth.memTTL（条目超龄强制回源，明示延迟上界）。
func (s *Server) authCtx() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Request.Header.Get("Authorization")
		ts, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || ts == "" {
			c.Next()
			return
		}
		e, err := s.Auth.Lookup(c.Request.Context(), ts)
		if err != nil {
			if errors.Is(err, auth.ErrBadCredential) {
				writeErr(c, http.StatusUnauthorized, errors.New("token invalid or expired"))
			} else {
				writeErr(c, http.StatusServiceUnavailable, err) // 存储故障=服务不可用，不是坏凭证
			}
			c.Abort()
			return
		}
		if e.Kind != store.AuthKindUser { // share 凭证走的是 /s/ 链路专用校验，永不代表身份
			writeErr(c, http.StatusUnauthorized, errors.New("token invalid or expired"))
			c.Abort()
			return
		}
		// 0.2.4 客户端绑定核对：登录时携带 clientId 签发的凭证与该标识绑定，
		// 逐请求 X-Client-Id 必须等值——缺失/不符=401（标识变了或是被冒用，
		// 一律重新登录取新凭证）。未绑定会话（旧客户端/移动端/管理台，
		// ClientID=""）不看头，行为同前；多客户端同账号各持己凭证互不挤占。
		if e.ClientID != "" && c.Request.Header.Get("X-Client-Id") != e.ClientID {
			writeErr(c, http.StatusUnauthorized, errors.New("client identity mismatch, please login again"))
			c.Abort()
			return
		}
		if e.Super {
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
			if serr != nil || gen != e.Gen {
				writeErr(c, http.StatusUnauthorized, errors.New("superadmin credential changed (password rotated or account file reset), please login again"))
				c.Abort()
				return
			}
			c.Set(userKey, &store.User{ID: store.SuperAdminID, Username: e.Username, Role: store.RoleAdmin})
			c.Next()
			return
		}
		if !s.dbReady() {
			writeErr(c, http.StatusServiceUnavailable, errNoDB)
			c.Abort()
			return
		}
		// 端口隔离判据取自认证图条目（登录时刻的角色快照）：管理员角色变更即
		// 按用户撤权（见 v1_admin），条目角色与库内角色不会出现可被利用的分歧。
		if e.Role == store.RoleAdmin && !c.GetBool(adminModeKey) {
			writeErr(c, http.StatusForbidden, errors.New("admin accounts cannot be used on the client API; use the admin console"))
			c.Abort()
			return
		}
		if e.Role != store.RoleAdmin { // 普通用户会话：滑动续期（闲置窗口内活跃即有效）
			s.Auth.Touch(e, s.Cfg.IdleDuration())
		}
		c.Set(userKey, &store.User{ID: e.UserID, Username: e.Username, Role: e.Role})
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

// ---------- 请求速率与跨域（安全迭代） ----------

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
// 与既有行为一致）。命中白名单回显具体 Origin 并允许预检；不使用通配符
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
