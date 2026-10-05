// Package api 实现统一版本化的 HTTP API（需求17：ip:port/api/v1）。
// Server 聚合全部依赖，handler 按其领域拆分到 v1_*.go。
package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/ai"
	"filemanageServer/internal/cache"
	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	"filemanageServer/internal/perm"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/superadmin"
	"filemanageServer/internal/transfer"
)

// Server 聚合依赖：配置、元数据存储（仅通过 internal/store 接口）、AI、权限引擎、
// 分片上传管理器、版本库、路径锁。
type Server struct {
	Cfg      *config.Config
	Store    store.Store // 0.7.0：恒为 store.Proxy（或其 cache 装饰器），支持运行期热切换
	AI       ai.Provider
	Roots    map[string]*config.RootCfg
	Perms    *perm.Engine
	Uploads  *transfer.Manager
	Versions *fileops.VersionStore
	Locks    *transfer.LockRegistry
	guard    *bruteGuard // 分享链接密码防爆破

	// 0.7.0 管理台配库（v1_dbconfig.go）
	DBProxy  *store.Proxy // 裸代理（未装饰），connect 换入新库用；状态/表统计也走它
	Cache    cache.Cache  // 可为 nil（未启用 redis）；切换后 FlushAll
	CfgPath  string       // 配置文件路径（PersistDBEdit 回写；空=内存配置，不回写）
	dbSwapMu sync.Mutex   // connect 串行化

	// Super 超级管理员账户（0.1.0 需求32：文件承载、不进数据库、无库降级模式同样可用）
	Super *superadmin.Store

	// 0.1.1 维护暂停（需求：切换/迁移/导出前后数据一致性）：
	// manualPause = 管理台"暂停客户端"开关；autoPause = connect(含迁移)/export
	// 复制窗口内自动屏障（defer 释放）。任一置位 → 用户端口与业务管理组 503，
	// health / 版本发现 / /admin/db 配置面 / 超管页始终可用（操作员不被锁死）。
	manualPause atomic.Bool
	autoPause   atomic.Bool

	startedAt time.Time
}

// NewServer 构造 Server 并初始化各子模块。
// 0.7.0：st 传入 store.Proxy（或其 cache 装饰器）——热切换数据库对
// 全部消费方透明，无需重建 Server；proxy 为 st 之下的裸代理。
// 0.1.0：super 为文件承载超管账户（需求32；nil 时超管路径整体关闭，仅测试构造用）。
func NewServer(cfg *config.Config, st store.Store, proxy *store.Proxy, cch cache.Cache, cfgPath string, super *superadmin.Store) (*Server, error) {
	locks := transfer.NewLockRegistry()
	uploads, err := transfer.NewManager(st, cfg.UploadStagingDir, locks)
	if err != nil {
		return nil, fmt.Errorf("upload manager: %w", err)
	}
	s := &Server{
		Cfg:     cfg,
		Store:   st,
		AI:      ai.NewProvider(cfg.AI),
		Roots:   map[string]*config.RootCfg{},
		Perms:   &perm.Engine{Store: st, AnonymousEnabled: cfg.Anonymous.Enabled},
		Uploads: uploads,
		Versions: &fileops.VersionStore{
			Store: st, Dir: cfg.Versions.Dir,
			MaxVersions: cfg.Versions.MaxVersions, Enabled: cfg.Versions.Enabled,
		},
		Locks:     locks,
		guard:     newBruteGuard(),
		DBProxy:   proxy,
		Cache:     cch,
		CfgPath:   cfgPath,
		Super:     super,
		startedAt: time.Now(),
	}
	for i := range cfg.Roots {
		s.Roots[cfg.Roots[i].ID] = &cfg.Roots[i]
	}
	// 0.7.0 需求3：内部合成根 <dataDir>/Users（用户家目录），只进内存 Roots 映射、
	// 不进配置文件 roots（config 已禁止 "_" 前缀用户根 id）；无库时 computeBases
	// 不暴露它，有库时用户经自动分配 (_users,<用户名>) jail 到家目录。
	s.Roots[store.HomeRootID] = &config.RootCfg{
		ID: store.HomeRootID, Name: store.HomeRootName, Path: cfg.UsersDir(),
	}
	return s, nil
}

// ---------- 主体（登录用户 / 匿名访客） ----------

// currentUser 返回登录用户；匿名访客返回 nil（需求6）。
func currentUser(c *gin.Context) *store.User {
	v, _ := c.Get(userKey)
	u, _ := v.(*store.User)
	return u
}

// superAdminName 超管账户名（信息展示用；文件不可读/未初始化时返回空串）。
func (s *Server) superAdminName() string {
	if s.Super == nil {
		return ""
	}
	name, err := s.Super.Username()
	if err != nil {
		return ""
	}
	return name
}

// isSuperAdmin 当前登录主体是否超级管理员（0.1.0 需求33：管理面分层判定——
// 其他管理员不得触达超管、不得增删改管理员账户）。超管为合成用户（ID=0），
// authCtx 超管分支注入，永不经数据库。
func isSuperAdmin(c *gin.Context) bool {
	u := currentUser(c)
	return u != nil && u.ID == store.SuperAdminID
}

// resolve 把 (rootID, relPath) 映射到磁盘绝对路径。0.5.0 起为 scope 的薄封装：
// 除 chroot 防逃逸外，还校验请求路径命中主体的授权基座（分配/分享/公开，见 scope.go）。
func (s *Server) resolve(c *gin.Context, rootID, rel string) (*config.RootCfg, string, error) {
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		return nil, "", err
	}
	return sc.Root, sc.Abs, nil
}

// permsFor 返回当前主体对 (rootID, rel) 的有效权限。
// rootID 为虚拟根 id（或内部存储用的物理键），rel 为对应的显示/物理路径——
// 二者经 scope 统一翻译为物理路径后交权限引擎判定。
func (s *Server) permsFor(c *gin.Context, rootID, rel string) perm.Decision {
	sc, err := s.scope(c, rootID, rel)
	if err != nil {
		return perm.DenyAll()
	}
	return s.Perms.For(currentUser(c), sc.PhysID, sc.Phys)
}

// permsBatch 返回当前主体在 rootID 内按显示路径逐条目判定的闭包（ACL 只取一次）。
// 目录列表/搜索等按条目过滤权限的场景使用，避免 N+1 查询（0.3.1/0.5.0）。
func (s *Server) permsBatch(c *gin.Context, rootID string) (func(disp string) perm.Decision, error) {
	sc, err := s.scope(c, rootID, "")
	if err != nil {
		return nil, err
	}
	dec := s.Perms.BatchFor(currentUser(c), sc.PhysID)
	return func(disp string) perm.Decision { return dec(sc.physOf(disp)) }, nil
}

// requireLoginUser 断言已登录并返回用户，否则写 401 并返回 nil。
func (s *Server) requireLoginUser(c *gin.Context) *store.User {
	u := currentUser(c)
	if u == nil {
		writeErr(c, http.StatusUnauthorized, errors.New("login required"))
		return nil
	}
	return u
}

// dbReady 报告元数据库是否可用（0.7.0：经 store.Proxy 动态判定，
// 管理台热切换后自动跟随；替代旧 `Store == nil` 启动期判定）。
func (s *Server) dbReady() bool { return !store.Degraded(s.Store) }

// Paused 报告维护暂停是否生效（手动开关 或 迁移/导出自动屏障，0.1.1）。
func (s *Server) Paused() bool { return s.manualPause.Load() || s.autoPause.Load() }

// SetManualPause 管理台"暂停客户端操作"开关（/admin/db/pause）。
func (s *Server) SetManualPause(v bool) bool {
	s.manualPause.Store(v)
	return s.Paused()
}

// withAutoPause 在屏障内执行 fn（复制→换入/导出全程冻结客户端），返回 fn 的结果；
// 手动暂停标志与本次屏障无关（defer 只恢复 autoPause）。
func (s *Server) withAutoPause(fn func() error) error {
	s.autoPause.Store(true)
	defer s.autoPause.Store(false)
	return fn()
}

// audit 写审计日志（0.5.1：登录/账户/权限/删除/分享等关键事件）。
// 降级模式（无库）静默跳过；任何写入失败只记日志，绝不影响主流程。
// path 传显示路径即可，内部经 scope 换算物理键；换算失败时按原样记录。
func (s *Server) audit(c *gin.Context, action, username string, userID int64, rootID, path, detail string) {
	if !s.dbReady() {
		return
	}
	e := &store.AuditEvent{
		CreatedAt: time.Now(), UserID: userID, Username: username,
		Action: action, RootID: rootID, Path: fileops.CleanRel(path),
		Detail: detail, IP: clientIP(c),
	}
	if rootID != "" {
		if rid, disp, ok := s.displayOf(c, rootID, path); ok {
			e.RootID, e.Path = rid, disp
		} else if sc, err := s.scopePhys(c, rootID, path); err == nil {
			e.RootID, e.Path = sc.PhysID, sc.Phys
		}
	}
	if u := currentUser(c); u != nil && userID == 0 {
		e.UserID, e.Username = u.ID, u.Username
	}
	if err := s.Store.AddAuditEvent(e); err != nil {
		log.Printf("audit: %v", err)
	}
}

// requirePerm 断言登录且具备指定权限，失败时已写响应并返回 false。
func (s *Server) requirePerm(c *gin.Context, rootID, rel string, need func(perm.Decision) bool, what string) bool {
	if s.requireLoginUser(c) == nil {
		return false
	}
	d := s.permsFor(c, rootID, rel)
	if !need(d) {
		writeErr(c, http.StatusForbidden, fmt.Errorf("permission denied: %s on %q", what, rel))
		return false
	}
	return true
}

// requirePermPhys 同 requirePerm，但键是存储层物理键 (rootID, physPath)——
// 供版本/回收站等"库内记录回流入 API"的路径使用（0.6.0：jailed 用户不再被误判越权）。
func (s *Server) requirePermPhys(c *gin.Context, physRoot, physPath string, need func(perm.Decision) bool, what string) bool {
	if s.requireLoginUser(c) == nil {
		return false
	}
	sc, err := s.scopePhys(c, physRoot, physPath)
	if err != nil {
		writeErr(c, statusOf(err), err)
		return false
	}
	if d := s.Perms.For(currentUser(c), sc.PhysID, sc.Phys); !need(d) {
		writeErr(c, http.StatusForbidden, fmt.Errorf("permission denied: %s on %q", what, physPath))
		return false
	}
	return true
}

// degradedView 匿名只读端点的统一权限闸门（0.3.0）：
// 配置了元数据库时行为与 0.2.x 完全一致（requirePerm，匿名 401）；
// 降级模式（无库）下跳过登录要求，按 perm 引擎的匿名决策放行。
// 0.5.0 收紧（D1 决策）：降级模式匿名仅可浏览（元数据视图），下载/预览一律 403。
func (s *Server) degradedView(c *gin.Context, rootID, rel string, need func(perm.Decision) bool, what string) bool {
	if s.dbReady() {
		return s.requirePerm(c, rootID, rel, need, what)
	}
	if d := s.permsFor(c, rootID, rel); !need(d) {
		writeErr(c, http.StatusForbidden, fmt.Errorf("permission denied: %s on %q", what, rel))
		return false
	}
	return true
}

// ---------- 通用工具 ----------

// FileEntry 前端展示用的文件条目，Path 为相对根目录的 slash 路径。
type FileEntry struct {
	Name  string    `json:"name"`
	Path  string    `json:"path"`
	IsDir bool      `json:"isDir"`
	Size  int64     `json:"size"`
	Mtime time.Time `json:"mtime"`
	Kind  string    `json:"kind"`
	Hash  string    `json:"hash,omitempty"` // 仅上传/校验响应中携带
}

func sortEntries(entries []FileEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

// recordMeta 把文件元数据（归属、日期等）记入 Store，供 AI 描述关联。
// 存储键统一用物理根 + 物理路径（0.5.0，见 scope.go）。
// e.Hash 非空（上传/整传/回滚等真正写入字节的调用方现算）时携带 content_hash；
// 为例行刷行（handleList）所留空时，Store 侧按 size/mtime 保留旧哈希（0.2.1），
// 供 POST /check 重复上传校验做廉价比对。
func (s *Server) recordMeta(u *store.User, sc *scopeInfo, e FileEntry) {
	if u == nil {
		return
	}
	_ = s.Store.PutFileMeta(&store.FileMeta{
		UserID: u.ID, RootID: sc.PhysID, Path: sc.physOf(e.Path), Name: e.Name,
		IsDir: e.IsDir, Size: e.Size, Mtime: e.Mtime, Kind: e.Kind,
		ContentHash: e.Hash, UpdatedAt: time.Now(),
	})
}
