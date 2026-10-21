// homes.go：用户主目录分配（需求3；0.2.11 账户/昵称/用户 id 三分离 + 回收站并级）。
//
// 规则：任何用户创建路径都必须建立 <dataDir>/Users/user_<账户>_<用户id> 家目录
// 并自动分配内部合成根 (_users, 该目录名)；用户由此 jail 到自己家目录（需求5：
// 用户端口只能访问被分配目录）。**目录名含自增用户 id（永不复用）——删除账户
// 后同名重建必落在新目录，同名复用旧目录的串号问题从根上消除**（0.2.11 需求4）。
// 回收站实体在**并级** <dir>_trash/（0.2.11 需求5；旧版内嵌 .fmtrash 由一次性
// 迁移 MigrateUserHomeLayout 搬移，过渡期解析保留旧位置候选）。
// 账户（User.Username）唯一且不可改，可安全作目录名片段；昵称（Nickname）仅
// 展示、可改可重复，绝不进任何路径/键。
// 家目录根不进配置文件（不进 cfg.Roots），由 api.Server 注入内存 Roots 映射。
// 用户端口虚拟根 id 对用户 id 的遮蔽由 api/scope.go 负责（基座名 ⇄ 账户名
// 双向映射，HomeAccountOf 为其提供解析）。
package fileops

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
)

// HomeTrashName 旧版（≤0.2.10）家目录内嵌回收站目录名（保留名）。
// 0.2.11 起新实体在并级 <dir>_trash；本名继续用于：存量迁移、迁移中间态候选
// 解析、以及对未迁移孤儿目录的保留名拦截（scope.go）。
const HomeTrashName = ".fmtrash"

// homesMetaKey EnsureUserHomes 幂等标记（server_meta 表，模式同 jailmodel.v5）。
const homesMetaKey = "userhomes.v7"

// homeLayoutMetaKey 0.2.11 一次性布局迁移幂等标记：家目录改名（含用户 id）+
// 回收站从内嵌 .fmtrash 搬至并级 _trash + 存量物理键行改写。
const homeLayoutMetaKey = "userhomes.v8"

// 家目录/回收站目录命名（0.2.11）：user_<账户>_<用户id> 与 <该目录名>_trash。
const (
	HomeDirPrefix   = "user_"
	HomeTrashSuffix = "_trash"
)

// usernameDirRe 账户名→目录名片段的安全字符集：字母数字开头，总长 1..64，
// 允许字母数字 . _ -（不得含路径分隔符/':'/空白；'.' 开头被首字符规则排除，
// 故 "." 与 ".." 天然不可能出现）。0.2.11 起作用于**账户名**（昵称不受此限）。
var usernameDirRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// windowsReserved Windows 保留设备名（部署到 wsl/共享盘等场景防坑）。
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
}

// UsernameAsDir 校验账户名可直接作为目录名片段使用（建号入口共用）。
func UsernameAsDir(name string) error {
	if !usernameDirRe.MatchString(name) {
		return fmt.Errorf("account %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (used as home directory name)", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("account %q must not end with '.' or '-'", name)
	}
	base := strings.ToLower(name)
	if windowsReserved[base] {
		return fmt.Errorf("account %q is a reserved name", name)
	}
	if strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt") {
		rest := base[3:]
		if len(rest) == 1 && rest[0] >= '1' && rest[0] <= '9' {
			return fmt.Errorf("account %q is a reserved name", name)
		}
	}
	return nil
}

// HomeDirName 家目录名：user_<账户>_<用户id>。id 为自增主键、永不复用。
func HomeDirName(username string, userID int64) string {
	return HomeDirPrefix + username + "_" + strconv.FormatInt(userID, 10)
}

// HomeAccountOf 从家目录名右锚定解析账户名（"user_<账户>_<digits>" → "<账户>"；
// 账户名允许含 '_'，故按最后一个下划线切分，尾部必须为纯数字）。
// 解析失败（旧布局遗留名/非本函数生成的目录）返回 ""。
func HomeAccountOf(dirName string) string {
	if !strings.HasPrefix(dirName, HomeDirPrefix) || len(dirName) <= len(HomeDirPrefix) {
		return ""
	}
	rest := dirName[len(HomeDirPrefix):]
	i := strings.LastIndexByte(rest, '_')
	if i <= 0 {
		return ""
	}
	tail := rest[i+1:]
	if tail == "" {
		return ""
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return rest[:i]
}

// UserTrashDirName 并级回收站目录名。
func UserTrashDirName(home string) string { return home + HomeTrashSuffix }

// HomeDirPath 家目录绝对路径（name 传 HomeDirName 结果或旧布局名）。
func HomeDirPath(usersDir, name string) string {
	return filepath.Join(usersDir, name)
}

// UserTrashDir 并级回收站目录绝对路径。
func UserTrashDir(usersDir, home string) string {
	return filepath.Join(usersDir, UserTrashDirName(home))
}

// UserTrashPath 并级回收站实体路径。
func UserTrashPath(usersDir, home, trashID string) string {
	return filepath.Join(usersDir, UserTrashDirName(home), trashID)
}

// HomeTrashDir 旧版家目录内嵌回收站目录（迁移/兼容候选）。
func HomeTrashDir(usersDir, name string) string {
	return filepath.Join(usersDir, name, HomeTrashName)
}

// HomeTrashPath 旧版家目录内嵌回收站实体路径（迁移/兼容候选）。
func HomeTrashPath(usersDir, name, trashID string) string {
	return filepath.Join(usersDir, name, HomeTrashName, trashID)
}

// TrashBlobCandidates 回收站实体的有序候选路径（新→旧）：并级 _trash、
// 迁移中间态（目录已改名、实体仍在 .fmtrash）、旧布局名内嵌 .fmtrash。
// 存在性判定/彻底删除逐候选尝试，保证迁移中断窗口内条目仍可还原/清理。
func TrashBlobCandidates(usersDir, home string, legacyName *string, trashID string) []string {
	out := []string{UserTrashPath(usersDir, home, trashID), HomeTrashPath(usersDir, home, trashID)}
	if legacyName != nil && *legacyName != "" && *legacyName != home {
		out = append(out, HomeTrashPath(usersDir, *legacyName, trashID))
	}
	return out
}

// EnsureHomeFor 为单个用户建家目录+并级回收站目录并追加 _users 分配
// （幂等：已有分配直接返回；SetUserRoots 为全量替换语义，先读既有分配再追加，
// 绝不冲掉管理员的其他授权）。
func EnsureHomeFor(st store.Store, usersDir string, u *store.User) error {
	if err := UsernameAsDir(u.Username); err != nil {
		return err
	}
	home := HomeDirName(u.Username, u.ID)
	if err := os.MkdirAll(HomeDirPath(usersDir, home), 0o700); err != nil {
		return fmt.Errorf("mkdir home: %w", err)
	}
	if err := os.MkdirAll(UserTrashDir(usersDir, home), 0o700); err != nil {
		return fmt.Errorf("mkdir trash: %w", err)
	}
	urs, err := st.ListUserRoots(u.ID)
	if err != nil {
		return err
	}
	for _, r := range urs {
		if r.RootID == store.HomeRootID {
			return nil // 已分配
		}
	}
	urs = append(urs, &store.UserRoot{UserID: u.ID, RootID: store.HomeRootID, SubPath: home, Alias: "Home"})
	return st.SetUserRoots(u.ID, urs)
}

// EnsureUserHomes 启动一次性存量补齐：为所有尚无家目录分配的存量用户补齐
// （建目录+追加分配，保留既有任何其他分配）。server_meta 标记幂等；
// 不安全账户名（早期无校验时代的遗留）跳过并告警，不阻塞启动。
func EnsureUserHomes(st store.Store, usersDir string) error {
	done, err := st.GetMeta(homesMetaKey)
	if err != nil {
		return err
	}
	if done == "1" {
		return nil
	}
	users, err := st.ListUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := UsernameAsDir(u.Username); err != nil {
			logx.Warn("homes: skip user %q: %v（账户名不能作目录名片段，请管理员处理）", u.Username, err)
			continue
		}
		if err := EnsureHomeFor(st, usersDir, u); err != nil {
			return fmt.Errorf("ensure home for %q: %w", u.Username, err)
		}
	}
	log.Printf("homes: 已为 %d 个存量用户确认家目录（%s）", len(users), usersDir)
	return st.SetMeta(homesMetaKey, "1")
}

// MigrateUserHomeLayout 0.2.11 一次性布局迁移（标记 userhomes.v8，模式同 v7）：
//  1. 家目录 Users/<旧名> → Users/user_<账户>_<id>（os.Rename，先磁盘后库——
//     崩溃安全锚点；三态可重入：旧名在→改名；仅新名+行旧→补写库；库已新→跳过）；
//  2. 目录内嵌 .fmtrash/* 逐条目搬入并级 <新名>_trash/（条目级幂等），删空壳；
//  3. 库内物理键改写：Store.RewritePhysicalPath(_users, 旧名, 新名)（七表）+
//     user_roots 行 SubPath 更新（保留其余分配）。
//
// 两代目录名同时存在=运维手工布置，大声拒绝并保留标记待下次启动重试。
// 全部用户成功才置标记；任何失败返回错误（不置标记，下次启动重跑）。
// usersDir 不存在（全新装且尚无用户）直接置标记返回。
func MigrateUserHomeLayout(st store.Store, usersDir string) error {
	done, err := st.GetMeta(homeLayoutMetaKey)
	if err != nil {
		return err
	}
	if done == "1" {
		return nil
	}
	users, err := st.ListUsers()
	if err != nil {
		return err
	}
	migrated := 0
	for _, u := range users {
		home := HomeDirName(u.Username, u.ID)
		urs, err := st.ListUserRoots(u.ID)
		if err != nil {
			return fmt.Errorf("migrate home %q: list roots: %w", u.Username, err)
		}
		homeIdx, sub := -1, ""
		for i, r := range urs {
			if r.RootID == store.HomeRootID {
				homeIdx, sub = i, CleanRel(r.SubPath)
				break
			}
		}
		if homeIdx < 0 {
			// 无家目录分配（异常遗留/不安全账户名被 v7 跳过）：按现名补齐分配
			// ——不安全名保持 v7 的跳过策略，仅告警。
			if err := UsernameAsDir(u.Username); err != nil {
				logx.Warn("migrate homes: skip %q: %v", u.Username, err)
				continue
			}
			if err := EnsureHomeFor(st, usersDir, u); err != nil {
				return fmt.Errorf("migrate home %q: ensure: %w", u.Username, err)
			}
			continue
		}
		if sub == "" {
			// 病态遗留：家目录行为空基座（理论上不可达——EnsureHomeFor/编辑器
			// 都写目录名）。跳过并告警（策略同 v7 对不安全名），绝不拿 UsersDir
			// 本身当旧名迁移。
			logx.Warn("migrate homes: skip %q: empty home sub_path（请管理员修正分配）", u.Username)
			continue
		}
		if sub == home {
			// 库已新：确保两目录实体齐备（修崩溃丢 mkdir），继续下一用户。
			if err := os.MkdirAll(HomeDirPath(usersDir, home), 0o700); err != nil {
				return fmt.Errorf("migrate home %q: mkdir: %w", home, err)
			}
			if err := os.MkdirAll(UserTrashDir(usersDir, home), 0o700); err != nil {
				return fmt.Errorf("migrate home %q: mkdir trash: %w", home, err)
			}
			continue
		}
		if err := UsernameAsDir(u.Username); err != nil {
			logx.Warn("migrate homes: skip %q: %v（账户名不能作目录名片段，保持旧布局分配）", u.Username, err)
			continue
		}
		oldDir := filepath.Join(usersDir, sub)
		newDir := filepath.Join(usersDir, home)
		oldExists := dirExists(oldDir)
		newExists := dirExists(newDir)
		switch {
		case oldExists && newExists:
			return fmt.Errorf("migrate home %q: both %q and %q exist（运维手工布置，请二选一后重启）", u.Username, sub, home)
		case oldExists:
			if err := os.MkdirAll(usersDir, 0o700); err != nil {
				return fmt.Errorf("migrate home %q: mkdir users: %w", home, err)
			}
			if err := os.Rename(oldDir, newDir); err != nil {
				return fmt.Errorf("migrate home %q: rename %s→%s: %w", home, sub, home, err)
			}
		default:
			// 仅新名在（上次崩溃于 rename 后）或两者都不在：确保存在。
			if err := os.MkdirAll(newDir, 0o700); err != nil {
				return fmt.Errorf("migrate home %q: mkdir: %w", home, err)
			}
		}
		// 回收站搬移：新目录内 .fmtrash → 并级 _trash（条目级幂等）。
		if err := absorbLegacyTrash(usersDir, home); err != nil {
			return fmt.Errorf("migrate home %q: trash: %w", home, err)
		}
		if err := os.MkdirAll(UserTrashDir(usersDir, home), 0o700); err != nil {
			return fmt.Errorf("migrate home %q: mkdir trash: %w", home, err)
		}
		// 库内物理键改写（幂等，二次跑 0 行）+ user_roots SubPath 更新。
		n, err := st.RewritePhysicalPath(store.HomeRootID, sub, home)
		if err != nil {
			return fmt.Errorf("migrate home %q: rewrite paths: %w", home, err)
		}
		urs[homeIdx].SubPath = home
		if err := st.SetUserRoots(u.ID, urs); err != nil {
			return fmt.Errorf("migrate home %q: set roots: %w", home, err)
		}
		migrated++
		logx.Info("migrate homes: %q: %s → %s（物理键行改写 %d 条）", u.Username, sub, home, n)
	}
	logx.Info("homes: 0.2.11 布局迁移完成（%d 个用户改道，目录 %s）", migrated, usersDir)
	return st.SetMeta(homeLayoutMetaKey, "1")
}

// absorbLegacyTrash 把家目录内嵌 .fmtrash 的条目逐项搬入并级 _trash，
// 全部搬空后删除 .fmtrash 空壳；仍有残留则报错（保留标记下次重试）。
func absorbLegacyTrash(usersDir, home string) error {
	legacyDir := filepath.Join(usersDir, home, HomeTrashName)
	entries, err := os.ReadDir(legacyDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dstDir := UserTrashDir(usersDir, home)
	for _, e := range entries {
		src := filepath.Join(legacyDir, e.Name())
		dst := filepath.Join(dstDir, e.Name())
		if dirExists(dst) || fileExists(dst) {
			continue // 条目级幂等（上次中断半途）
		}
		if err := os.MkdirAll(dstDir, 0o700); err != nil {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("move trash entry %s: %w", e.Name(), err)
		}
	}
	rest, err := os.ReadDir(legacyDir)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("legacy %s not empty after absorb", HomeTrashName)
	}
	return os.Remove(legacyDir)
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
