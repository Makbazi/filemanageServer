// homes.go：用户主目录分配（需求3）。
//
// 规则：任何用户创建路径（管理台建号/引导注册/恢复管理员）都必须为用户
// 建立 <dataDir>/Users/<用户名> 家目录并自动分配内部合成根 (_users,<用户名>)，
// 用户由此 jail 到自己家目录（需求5：用户端口只能访问被分配目录）；
// 回收站实体内嵌家目录 .fmtrash（HomeTrashName，对 API 不可见/不可写，见 api/scope.go）。
// 家目录根不进配置文件（不进 cfg.Roots），由 api.Server 注入内存 Roots 映射。
package fileops

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	store "filemanageServer/internal/store"
)

// HomeTrashName 家目录内嵌回收站目录名（保留名）。
const HomeTrashName = ".fmtrash"

// homesMetaKey EnsureUserHomes 幂等标记（server_meta 表，模式同 jailmodel.v5）。
const homesMetaKey = "userhomes.v7"

// usernameDirRe 用户名→目录名的安全字符集：字母数字开头，总长 1..64，
// 允许字母数字 . _ -（不得含路径分隔符/':'/空白；'.' 开头被首字符规则排除，
// 故 "." 与 ".." 天然不可能出现）。
var usernameDirRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// windowsReserved Windows 保留设备名（部署到 wsl/共享盘等场景防坑）。
var windowsReserved = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
}

// UsernameAsDir 校验用户名可直接作为目录名使用（管理台建号/注册入口共用）。
func UsernameAsDir(name string) error {
	if !usernameDirRe.MatchString(name) {
		return fmt.Errorf("username %q must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ (used as home directory name)", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("username %q must not end with '.' or '-'", name)
	}
	base := strings.ToLower(name)
	if windowsReserved[base] {
		return fmt.Errorf("username %q is a reserved name", name)
	}
	if strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt") {
		rest := base[3:]
		if len(rest) == 1 && rest[0] >= '1' && rest[0] <= '9' {
			return fmt.Errorf("username %q is a reserved name", name)
		}
	}
	return nil
}

// HomeDirPath 家目录绝对路径。
func HomeDirPath(usersDir, username string) string {
	return filepath.Join(usersDir, username)
}

// HomeTrashDir 家目录内回收站目录。
func HomeTrashDir(usersDir, username string) string {
	return filepath.Join(usersDir, username, HomeTrashName)
}

// HomeTrashPath 家目录内回收站实体路径。
func HomeTrashPath(usersDir, username, trashID string) string {
	return filepath.Join(usersDir, username, HomeTrashName, trashID)
}

// EnsureHomeFor 为单个用户建家目录并追加 _users 分配（幂等：已有分配直接返回；
// SetUserRoots 为全量替换语义，先读既有分配再追加，绝不冲掉管理员的其他授权）。
func EnsureHomeFor(st store.Store, usersDir string, u *store.User) error {
	if err := UsernameAsDir(u.Username); err != nil {
		return err
	}
	if err := os.MkdirAll(HomeDirPath(usersDir, u.Username), 0o700); err != nil {
		return fmt.Errorf("mkdir home: %w", err)
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
	urs = append(urs, &store.UserRoot{UserID: u.ID, RootID: store.HomeRootID, SubPath: u.Username, Alias: "Home"})
	return st.SetUserRoots(u.ID, urs)
}

// EnsureUserHomes 启动一次性存量迁移：为所有尚无家目录分配的存量用户补齐
// （建目录+追加分配，保留既有任何其他分配）。server_meta 标记幂等；
// 不安全用户名（早期无校验时代的遗留）跳过并告警，不阻塞启动。
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
			log.Printf("homes: skip user %q: %v（用户名不能作目录名，请管理员处理）", u.Username, err)
			continue
		}
		if err := EnsureHomeFor(st, usersDir, u); err != nil {
			return fmt.Errorf("ensure home for %q: %w", u.Username, err)
		}
	}
	log.Printf("homes: 已为 %d 个存量用户确认家目录（%s）", len(users), usersDir)
	return st.SetMeta(homesMetaKey, "1")
}
