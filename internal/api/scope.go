// scope.go 虚拟根（jail 模型）解析——0.5.0 权限模型的唯一翻译层。
//
// 规则（用户决策）：
//   - 管理员在管理端口为用户分配 (物理根, 子目录)，即 user_roots；用户在
//     用户端口只能访问被授予的虚拟根，物理配置根本身对任何用户主体不可见。
//   - 匿名主体只能访问被 ACL 明确放开 public-view 的目录（"公开共享目录"清单）。
//   - ACL 分享给 user/group 且本人有 view 的目录，同样构成虚拟根（"分享给我"）。
//   - 请求 rootId 形式："physID"（base=""）或 "physID:jail"；存储层键
//     （ACL/版本/回收站/上传会话/file_meta/分享链接）一律用 (physID, 物理路径)，
//     客户端只见相对所命中 base 的显示路径。
//   - 管理端口（adminScope 中间件）按物理根全量工作，base=""，管理台直接维护物理路径。
//   - 降级模式（无元数据库）保持 0.3.x 形态：plain 配置根（匿名按 D1 仅浏览不可下载）。
package api

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/gin-gonic/gin"

	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
)

// adminModeKey 管理端口标记（仅 AdminHandler 的引擎设置）。
const adminModeKey = "fm.adminMode"

// basesKey 请求级授权基座缓存键。
const basesKey = "fm.bases"

var errNoRootAccess = errors.New("root not assigned to this principal")

// errReservedName 家目录内保留目录（.fmtrash，0.7.0 内嵌回收站）不可经文件 API 访问。
var errReservedName = errors.New("reserved directory not accessible")

// hasSegment 路径 p（slash 相对路径）的任一段是否等于 seg。
func hasSegment(p, seg string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == seg {
			return true
		}
	}
	return false
}

// scopeInfo 一次请求解析出的路径与权限上下文。
type scopeInfo struct {
	PhysID  string          // 物理根 id（存储键）
	Base    string          // 命中的授权基座（jail/公开路径/分享路径；""=整根）
	VRoot   string          // 客户端可见 root id
	Root    *config.RootCfg // 物理根配置
	BaseAbs string          // base 的磁盘绝对路径
	Phys    string          // 物理路径（相对物理根）
	Disp    string          // 显示路径（相对 base）
	Abs     string          // 磁盘绝对路径
}

// physOf 显示路径 → 存储物理路径。
func (sc *scopeInfo) physOf(disp string) string { return joinSlash(sc.Base, fileops.CleanRel(disp)) }

// entry 以显示路径（相对 base）生成客户端条目。
func (sc *scopeInfo) entry(abs string, info fs.FileInfo) FileEntry {
	rel := fileops.RelOf(sc.BaseAbs, abs)
	return FileEntry{
		Name: info.Name(), Path: rel, IsDir: info.IsDir(),
		Size: info.Size(), Mtime: info.ModTime(), Kind: fileops.KindOf(rel, info.IsDir()),
	}
}

func joinSlash(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "/" + b
	}
}

// hasPathPrefix base 是 cand 的祖先（或相等）；base=="" 覆盖一切。
func hasPathPrefix(cand, base string) bool {
	if base == "" {
		return true
	}
	return cand == base || strings.HasPrefix(cand, base+"/")
}

// parseVRootID 拆分 "physID[:jail]"。
func parseVRootID(id string) (physID, jail string) {
	if i := strings.IndexByte(id, ':'); i >= 0 {
		return id[:i], fileops.CleanRel(id[i+1:])
	}
	return id, ""
}

func vrootID(physID, base string) string {
	if base == "" {
		return physID
	}
	return physID + ":" + base
}

// basesMap 当前主体对每个物理根的授权基座集合（请求级缓存）。
// 返回 map[physID][]string；根缺席或列表为空 = 无任何访问面。
func (s *Server) basesMap(c *gin.Context) (map[string][]string, error) {
	if v, ok := c.Get(basesKey); ok {
		e := v.(basesCache)
		return e.m, e.err
	}
	m, err := s.computeBases(c)
	c.Set(basesKey, basesCache{m: m, err: err})
	return m, err
}

type basesCache struct {
	m   map[string][]string
	err error
}

func (s *Server) computeBases(c *gin.Context) (map[string][]string, error) {
	out := map[string][]string{}
	for id := range s.Roots {
		out[id] = []string{}
	}
	if c.GetBool(adminModeKey) {
		// 管理端口：整根即基座（含内部家目录根 _users，管理台可见）
		for id := range s.Roots {
			out[id] = []string{""}
		}
		return out, nil
	}
	if !s.dbReady() {
		// 降级模式：plain 配置根整根基座（0.3.x 形态）；内部家目录根**不暴露**
		// （无用户体系时整根基座等于让任意主体浏览全部用户家目录，需求3/6 拒绝）
		for id := range s.Roots {
			if id == store.HomeRootID {
				continue
			}
			out[id] = []string{""}
		}
		return out, nil
	}
	u := currentUser(c)
	if u == nil {
		// 匿名：public-view 目录清单
		acls, err := s.Store.ListPublicViewACLs()
		if err != nil {
			return nil, err
		}
		for _, a := range acls {
			if _, ok := s.Roots[a.RootID]; ok {
				out[a.RootID] = append(out[a.RootID], fileops.CleanRel(a.Path))
			}
		}
		return out, nil
	}
	bases, err := s.basesForUser(u)
	if err != nil {
		return nil, err
	}
	// 0.6.0（需求13）：他人的 public-view 目录对登录主体同样可见（只读基座，
	// perm 引擎对命中 public 条目的登录主体强制仅 View；属主走自有分配短路不受影响）。
	// 注意：只加在请求基座，不加入 basesForUser——后者被分享断链校验共用，
	// 保持"创建者分配被回收 → 链接立即 410"的 0.5.0 语义。
	acls, err := s.Store.ListPublicViewACLs()
	if err != nil {
		return nil, err
	}
	for _, a := range acls {
		if _, ok := s.Roots[a.RootID]; !ok {
			continue
		}
		p := fileops.CleanRel(a.Path)
		dup := false
		for _, b := range bases[a.RootID] {
			if hasPathPrefix(p, b) {
				dup = true
				break
			}
		}
		if !dup {
			bases[a.RootID] = append(bases[a.RootID], p)
		}
	}
	return bases, nil
}

// basesForUser 登录主体的授权基座集合：分配目录（user_roots）+ ACL 分享给我的
// 目录（user 精确 / group 归属且 view；已处于自有 base 之下的跳过）。
// computeBases 与分享链接"权限回收即断链"校验共用此逻辑（0.5.0）。
func (s *Server) basesForUser(u *store.User) (map[string][]string, error) {
	out := map[string][]string{}
	for id := range s.Roots {
		out[id] = []string{}
	}
	urs, err := s.Store.ListUserRoots(u.ID)
	if err != nil {
		return nil, err
	}
	for _, r := range urs {
		if _, ok := s.Roots[r.RootID]; ok {
			out[r.RootID] = append(out[r.RootID], fileops.CleanRel(r.SubPath))
		}
	}
	groupIDs, err := s.userGroupIDs(u.ID)
	if err != nil {
		return nil, err
	}
	for rootID := range s.Roots {
		acls, err := s.Store.ListRootACLs(rootID)
		if err != nil {
			return nil, err
		}
		for _, a := range acls {
			if !a.Perms.View || a.SubjectType == store.SubjectPublic {
				continue
			}
			mine := (a.SubjectType == store.SubjectUser && a.SubjectID == u.ID) ||
				(a.SubjectType == store.SubjectGroup && groupIDs[a.SubjectID])
			if !mine {
				continue
			}
			p := fileops.CleanRel(a.Path)
			dup := false
			for _, b := range out[rootID] {
				if hasPathPrefix(p, b) {
					dup = true
					break
				}
			}
			if !dup {
				out[rootID] = append(out[rootID], p)
			}
		}
	}
	return out, nil
}

// pathInUserScope (physID, physPath) 是否落在该登录主体的任一授权基座内。
func (s *Server) pathInUserScope(u *store.User, physID, physPath string) bool {
	bases, err := s.basesForUser(u)
	if err != nil {
		return false
	}
	physPath = fileops.CleanRel(physPath)
	for _, b := range bases[physID] {
		if hasPathPrefix(physPath, b) {
			return true
		}
	}
	return false
}

func (s *Server) userGroupIDs(userID int64) (map[int64]bool, error) {
	groups, err := s.Store.ListGroups()
	if err != nil {
		return nil, err
	}
	out := map[int64]bool{}
	for _, g := range groups {
		for _, m := range g.Members {
			if m == userID {
				out[g.ID] = true
			}
		}
	}
	return out, nil
}

// scope 把请求键 (rootID, rel) 解析为磁盘路径与权限上下文；越出授权基座即拒绝。
func (s *Server) scope(c *gin.Context, rootID, rel string) (*scopeInfo, error) {
	rel = fileops.CleanRel(rel)
	physID, jail := parseVRootID(rootID)
	root := s.Roots[physID]
	if root == nil {
		return nil, fmt.Errorf("unknown root %q", physID)
	}
	bases, err := s.basesMap(c)
	if err != nil {
		return nil, err
	}
	// jail 本身必须命中 base（防伪造 "phys:其他用户目录"）
	baseOK := false
	for _, b := range bases[physID] {
		if hasPathPrefix(jail, b) {
			baseOK = true
			break
		}
	}
	if !baseOK {
		return nil, fmt.Errorf("%w: %q", errNoRootAccess, physID)
	}
	cand := joinSlash(jail, rel)
	// 0.7.0 需求3：家目录内 .fmtrash 为服务端回收站内核——列表不可见（handleList
	// 过滤）、读写一律 403（在此集中拦截：list/upload/mkdir/move/copy/delete/stat）；
	// 回收站实体只经 v1_trash.go/cleaner.go 的内部路径解析操作，不走本翻译层。
	if physID == store.HomeRootID && hasSegment(cand, fileops.HomeTrashName) {
		return nil, errReservedName
	}
	// 取最长匹配 base 作为显示基准
	best, found := "", false
	for _, b := range bases[physID] {
		if hasPathPrefix(cand, b) && (!found || len(b) > len(best)) {
			best, found = b, true
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %q", errNoRootAccess, physID)
	}
	baseAbs, err := fileops.JoinRoot(root.Path, best)
	if err != nil {
		return nil, err
	}
	disp := strings.TrimPrefix(cand, best)
	disp = strings.TrimPrefix(disp, "/")
	abs, err := fileops.JoinRoot(baseAbs, disp)
	if err != nil {
		return nil, err
	}
	// 0.5.1：root 内预存符号链接不得成为逃逸通道（集中在此一处防护）
	if err := fileops.EnsureNoEscape(baseAbs, abs); err != nil {
		return nil, err
	}
	return &scopeInfo{
		PhysID: physID, Base: best, VRoot: vrootID(physID, best),
		Root: root, BaseAbs: baseAbs, Phys: cand, Disp: disp, Abs: abs,
	}, nil
}

// scopePhys 从物理存储键 (rootID, physPath) 反查授权范围——供上传会话完成、
// 版本回滚、回收站还原等"存储键回流入 API"的路径使用（0.6.0 修复：0.5.0 直接
// 以物理根 id 调 scope 会把 jailed 用户误判为越权 403，仅整根分配/管理员可用）。
// 语义等价于 scope(c, vrootID(physID, phys), "")：phys 必须落在调用者任一授权
// 基座内（分配回收/ACL 收回在此拒绝），命中最长匹配 base。
func (s *Server) scopePhys(c *gin.Context, physID, physPath string) (*scopeInfo, error) {
	return s.scope(c, vrootID(physID, fileops.CleanRel(physPath)), "")
}

// displayOf 存储键 (physID, physPath) 反查客户端视图（rootId, 显示路径）。
// 命中不了任何 base 时返回 ok=false（调用方应跳过该行）。
func (s *Server) displayOf(c *gin.Context, physID, physPath string) (string, string, bool) {
	bases, err := s.basesMap(c)
	if err != nil {
		return "", "", false
	}
	physPath = fileops.CleanRel(physPath)
	best, found := "", false
	for _, b := range bases[physID] {
		if hasPathPrefix(physPath, b) && (!found || len(b) > len(best)) {
			best, found = b, true
		}
	}
	if !found {
		return "", "", false
	}
	disp := strings.TrimPrefix(physPath, best)
	disp = strings.TrimPrefix(disp, "/")
	return vrootID(physID, best), disp, true
}

// baseName base 的展示名（整根回退根名，否则取路径末段）。
func baseName(root *config.RootCfg, base string) string {
	if base == "" {
		return root.Name
	}
	parts := strings.Split(base, "/")
	return parts[len(parts)-1]
}
