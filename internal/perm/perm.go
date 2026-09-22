// Package perm 实现目录级权限引擎（需求6/8/9）：
//   - 管理员拥有一切权限；
//   - 登录用户默认全权限，除非路径链上存在与其（用户/所属用户组）匹配的 ACL；
//   - 匿名访客默认只读浏览（anonymous.enabled 控制），下载/上传/删除/分享一律拒绝，
//     目录可通过 public 主体 ACL 显式放开或收紧"查看"；
//   - 0.6.0：public 条目同样匹配登录主体（仅查看——公开目录对他用户可见不可写）；
//     主体自己的分配目录（user_roots）短路全权，不受任何 ACL 条目影响；
//   - ACL 继承：取离目标路径最近的、含匹配主体的祖先目录配置，命中即止（OR 合并同层多条）。
package perm

import (
	"strings"

	store "filemanageServer/internal/store"
)

// Decision 一次权限判定的结果。
type Decision struct {
	View     bool `json:"view"`
	Download bool `json:"download"`
	Upload   bool `json:"upload"`
	Delete   bool `json:"delete"`
	Share    bool `json:"share"`
}

// All 返回全权限。
func All() Decision {
	return Decision{View: true, Download: true, Upload: true, Delete: true, Share: true}
}

// ViewOnly 只读（匿名默认）。
func ViewOnly() Decision { return Decision{View: true} }

// DenyAll 全拒绝。
func DenyAll() Decision { return Decision{} }

// Engine 权限引擎。
type Engine struct {
	Store            store.Store
	AnonymousEnabled bool
}

// For 判定主体 (u 为 nil 表示匿名) 对 rootID/relPath 的有效权限。
func (e *Engine) For(u *store.User, rootID, relPath string) Decision {
	return e.BatchFor(u, rootID)(relPath)
}

// BatchFor 返回一个对同 root 内多条路径批量判定的闭包：ACL 与组归属只取一次，
// 供目录列表/搜索逐条目过滤权限使用（避免每条目重复查询造成 N+1，0.3.1）。
func (e *Engine) BatchFor(u *store.User, rootID string) func(relPath string) Decision {
	if !store.Degraded(e.Store) && u != nil {
		// 0.6.0（需求13）：登录主体对"自己被授予的分配目录"（user_roots）天然全权，
		// 不查 ACL——否则他人在自己目录上设置的 public 条目会把属主降级为仅查看。
		if own := e.ownSubs(rootID, u.ID); len(own) > 0 {
			inner := e.batchForLocked(u, rootID)
			return func(relPath string) Decision {
				if underAny(strings.Trim(relPath, "/"), own) {
					return All()
				}
				return inner(relPath)
			}
		}
	}
	return e.batchForLocked(u, rootID)
}

// batchForLocked 原 BatchFor 主体（拆分以复用自有分配短路）。
func (e *Engine) batchForLocked(u *store.User, rootID string) func(relPath string) Decision {
	if store.Degraded(e.Store) {
		// 降级模式（无元数据库）：无用户体系，匿名按配置放开只读浏览，
		// 下载/上传/修改/删除一律禁止（0.5.0 D1 收紧：与有库模式同语义，
		// 修复 0.3.x"匿名可下载"与需求6 的冲突）。登录端点已被 requireDB 拦为 503。
		d := DenyAll()
		if e.AnonymousEnabled {
			d = ViewOnly()
		}
		return func(string) Decision { return d }
	}
	if u != nil && u.Role == store.RoleAdmin {
		return func(string) Decision { return All() }
	}
	entries, err := e.Store.ListRootACLs(rootID)
	if err != nil {
		// 存储异常时退化为安全默认：登录用户全权限（与 0.1 行为一致），匿名只读
		fallback := e.anonDefault()
		if u != nil {
			fallback = All()
		}
		return func(string) Decision { return fallback }
	}
	var groupIDs map[int64]bool
	if u != nil {
		groupIDs = e.userGroups(u.ID)
	}
	return func(relPath string) Decision { return e.decide(u, entries, groupIDs, relPath) }
}

// ownSubs 用户在指定物理根上被授予的分配目录子路径（user_roots）。
// 查询失败返回 nil（保守退化为按 ACL 判定，不影响 jail 基座层拦截）。
func (e *Engine) ownSubs(rootID string, userID int64) []string {
	urs, err := e.Store.ListUserRoots(userID)
	if err != nil {
		return nil
	}
	var out []string
	for _, r := range urs {
		if r.RootID == rootID {
			out = append(out, strings.Trim(r.SubPath, "/"))
		}
	}
	return out
}

// underAny rel 是否等于或以任一 base 为祖先目录（base ""=整根覆盖一切）。
func underAny(rel string, bases []string) bool {
	for _, b := range bases {
		if b == "" || rel == b || strings.HasPrefix(rel, b+"/") {
			return true
		}
	}
	return false
}

// decide 权限判定纯逻辑：自目标路径最近祖先向根回溯，命中含匹配主体的层即止
// （同层多条 OR 合并）。
func (e *Engine) decide(u *store.User, entries []*store.FolderACL, groupIDs map[int64]bool, relPath string) Decision {
	relPath = strings.Trim(relPath, "/")
	for level := relPath; ; {
		var merged Decision
		matched := false
		for _, a := range entries {
			if a.Path != level {
				continue
			}
			if !matchSubject(a, u, groupIDs) {
				continue
			}
			matched = true
			merged.View = merged.View || a.Perms.View
			// 0.6.0：public 条目对登录主体也匹配（需求13 公开目录可见），但只放开"查看"；
			// 下载/上传/删除/分享永不经 public 条目授予（匿名同语义）。
			if a.SubjectType == store.SubjectPublic {
				continue
			}
			merged.Download = merged.Download || a.Perms.Download
			merged.Upload = merged.Upload || a.Perms.Upload
			merged.Delete = merged.Delete || a.Perms.Delete
			merged.Share = merged.Share || a.Perms.Share
		}
		if matched {
			if u == nil {
				// 匿名主体即使 ACL 放开下载/上传/删除也不生效（需求6：登录后才可下载/修改/删除）
				merged.Download, merged.Upload, merged.Delete = false, false, false
			}
			return merged
		}
		if level == "" {
			break
		}
		i := strings.LastIndex(level, "/")
		if i < 0 {
			level = ""
		} else {
			level = level[:i]
		}
	}
	// 无任何匹配 ACL：默认策略
	if u != nil {
		return All()
	}
	return e.anonDefault()
}

func (e *Engine) anonDefault() Decision {
	if e.AnonymousEnabled {
		return ViewOnly()
	}
	return DenyAll()
}

func matchSubject(a *store.FolderACL, u *store.User, groupIDs map[int64]bool) bool {
	switch a.SubjectType {
	case store.SubjectPublic:
		return true // 0.6.0（需求13）：public 条目对登录主体也匹配（仅 View，见 decide）
	case store.SubjectUser:
		return u != nil && u.ID == a.SubjectID
	case store.SubjectGroup:
		return u != nil && groupIDs[a.SubjectID]
	}
	return false
}

// userGroups 返回用户所属组 ID 集合。
func (e *Engine) userGroups(userID int64) map[int64]bool {
	out := map[int64]bool{}
	groups, err := e.Store.ListGroups()
	if err != nil {
		return out
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if m == userID {
				out[g.ID] = true
			}
		}
	}
	return out
}
