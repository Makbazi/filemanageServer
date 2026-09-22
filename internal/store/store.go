// Package store 提供统一的元数据存储抽象（0.3.0 起由独立工程 filemanageDb 并入）。
// 上层只依赖 Store 接口与模型类型，禁止绕过接口直接操作数据库。
//
// 0.3.0 变更：json/memory 驱动移除；改为 SQL 三驱动 sqlite3|mysql|postgresql，
// 单一 sqlStore 实现 + 方言层（dialect.go）。未配置驱动（"none"）时 Open 返回
// (nil, nil)，服务端以降级模式启动（仅匿名只读，元数据功能返回 503）。
package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("store: not found")

// ErrExists 表示记录已存在。
var ErrExists = errors.New("store: already exists")

// 规范驱动名（Open 返回的 Store.Driver() 即取其一）。
const (
	DriverSQLite = "sqlite3"
	DriverMySQL  = "mysql"
	DriverPg     = "postgresql"
)

// 用户主目录内部合成根（0.7.0 需求3）：路径 <dataDir>/Users 由配置派生，
// 不写入配置文件 roots；id 以 '_' 开头（config 校验已禁止用户配置 '_' 前缀 id，
// 且 ':' 为虚拟根分隔符，天然无冲突）。用户创建时自动分配 (_users, <用户名>)，
// 即 jail 到 <dataDir>/Users/<用户名>；回收站内嵌于家目录 .fmtrash。
const (
	HomeRootID   = "_users"
	HomeRootName = "个人主目录"
)

// Store 是唯一的数据库访问接口，上层（服务端）只依赖它。
// 语义约定（跨驱动一致）：记录不存在返回 ErrNotFound，唯一约束冲突返回 ErrExists，
// 上层用 errors.Is 判断；排序、级联删除行为三驱动保持一致。
type Store interface {
	Driver() string
	Close() error
	Migrate() error

	// 用户
	CreateUser(u *User) error
	GetUserByName(username string) (*User, error)
	GetUserByID(id int64) (*User, error)
	ListUsers() ([]*User, error)
	UpdateUserPassword(id int64, hash string) error
	UpdateUserRoots(id int64, roots []string) error // 旧 0.3.x 字段；0.5.0 起仅作迁移输入
	UpdateUserRole(id int64, role string) error
	DeleteUser(id int64) error
	CountUsers() (int64, error)

	// 用户根分配（0.5.0 jail 模型，见 UserRoot 注释）
	ListUserRoots(userID int64) ([]*UserRoot, error)
	SetUserRoots(userID int64, list []*UserRoot) error // 全量替换（事务内先删后插）
	ListAllUserRoots() ([]*UserRoot, error)            // 管理台总览

	// server_meta：一次性数据迁移标记等键值（不存在返回 ""）
	GetMeta(key string) (string, error)
	SetMeta(key, value string) error

	// 文件元数据
	PutFileMeta(m *FileMeta) error
	GetFileMeta(userID int64, rootID, path string) (*FileMeta, error)
	ListFileMetas(userID int64, rootID string) ([]*FileMeta, error)
	DeleteFileMeta(userID int64, rootID, path string) error
	RenameFileMeta(userID int64, rootID, from, to string) error

	// AI 描述
	SaveAIDescription(d *AIDescription) error
	GetAIDescription(userID int64, rootID, path, kind string) (*AIDescription, error)
	ListAIDescriptions(userID int64, rootID string) ([]*AIDescription, error)
	DeleteAIDescription(id int64) error
	// SearchDescriptions 关键词检索（预留：接入 embedding 后升级为向量相似度检索）
	SearchDescriptions(userID int64, text string, limit int) ([]*SearchHit, error)

	// 回收站
	AddTrashItem(t *TrashItem) error
	ListTrashItems(userID int64, rootID string) ([]*TrashItem, error)
	TakeTrashItem(userID int64, trashID string) (*TrashItem, error)
	RemoveTrashItem(userID int64, trashID string) error
	ClearTrash(userID int64, rootID string) ([]*TrashItem, error)
	// ListTrashBefore 返回 deletedAt 早于 before 的条目（回收站 30 天自动清理用）。
	ListTrashBefore(before time.Time) ([]*TrashItem, error)

	// 用户组
	CreateGroup(g *Group) error
	GetGroupByID(id int64) (*Group, error)
	GetGroupByName(name string) (*Group, error)
	ListGroups() ([]*Group, error)
	UpdateGroup(g *Group) error // 按 ID 更新 Name/Members
	DeleteGroup(id int64) error

	// 目录权限（ACL）
	SetFolderACL(a *FolderACL) error // 按 (rootID,path,subjectType,subjectID) upsert
	GetFolderACLs(rootID, path string) ([]*FolderACL, error)
	ListRootACLs(rootID string) ([]*FolderACL, error) // rootID=="" 返回全部（管理员用）
	// ListPublicViewACLs 主体为 public 且 view=1 的条目（匿名"公开共享目录"清单，0.5.0）。
	ListPublicViewACLs() ([]*FolderACL, error)
	DeleteFolderACL(id int64) error
	DeleteFolderACLsBySubject(subjectType string, subjectID int64) error

	// 临时分享链接
	CreateShareLink(l *ShareLink) error
	GetShareLinkByToken(token string) (*ShareLink, error) // 返回值含 PasswordHash（仅服务端内部使用）
	ListShareLinks(createdBy int64) ([]*ShareLink, error) // createdBy<=0 返回全部（管理员用）
	UpdateShareLink(l *ShareLink) error                   // 按 ID 更新 PasswordHash/ExpiresAt/Disabled
	DeleteShareLink(id int64) error

	// 文件版本
	AddFileVersion(v *FileVersion) error // VersionNo==0 时自动取该路径最大版本号+1
	GetFileVersion(id int64) (*FileVersion, error)
	ListFileVersions(rootID, path string) ([]*FileVersion, error) // 按 VersionNo 降序
	DeleteFileVersion(id int64) error
	DeleteFileVersions(rootID, path string) error

	// 登录会话（refresh token，滑动过期）
	CreateSession(sess *Session) error
	GetSessionByTokenHash(tokenHash string) (*Session, error)
	RenewSession(id int64, expiresAt, lastUsedAt time.Time) error
	DeleteSession(id int64) error
	DeleteSessionsByUser(userID int64) error
	DeleteExpiredSessions(now time.Time) error

	// 分片上传会话（断点续传，跨实例共享）
	CreateUploadSession(u *UploadSession) error
	GetUploadSession(id string) (*UploadSession, error)
	GetActiveUploadSession(rootID, path string) (*UploadSession, error) // 同路径仅允许一个活动会话（并发写保护）
	ListUploadSessions(userID int64) ([]*UploadSession, error)          // userID<=0 返回全部
	UpdateUploadSession(u *UploadSession) error                         // 按 ID 全量更新
	DeleteUploadSession(id string) error
	DeleteUploadSessionsBefore(before time.Time) error

	// 审计日志（0.5.1）
	AddAuditEvent(e *AuditEvent) error
	ListAuditEvents(limit int, userID int64) ([]*AuditEvent, error) // userID<=0 全部
	DeleteAuditBefore(before time.Time) error
}

// Options 打开参数。
type Options struct {
	Driver string // sqlite3 | mysql | postgresql（别名见 normalizeDriver）| none/""（降级模式）
	DSN    string // sqlite3 文件路径 / mysql DSN / postgres DSN
}

// NormalizeDriver 将用户配置的驱动名规范为 DriverSQLite/DriverMySQL/DriverPg，
// 或 ""（降级模式，无数据库）。json/memory 驱动已在 0.3.0 移除。
// 配置校验（internal/config）与 Open 共用本函数。
func NormalizeDriver(s string) (string, error) {
	switch s {
	case "", "none":
		return "", nil
	case "sqlite", "sqlite3":
		return DriverSQLite, nil
	case "mysql", "mariadb":
		return DriverMySQL, nil
	case "postgres", "postgresql", "pg":
		return DriverPg, nil
	case "json", "memory":
		return "", fmt.Errorf("store: driver %q was removed in 0.3.0 (configure sqlite3|mysql|postgresql, or \"none\" for degraded mode)", s)
	default:
		return "", fmt.Errorf("store: unknown driver %q (want sqlite3|mysql|postgresql|none)", s)
	}
}

// ValidDriver 报告驱动名是否可被 Open 接受（供配置校验使用）。
func ValidDriver(s string) bool {
	_, err := NormalizeDriver(s)
	return err == nil
}

// Open 按驱动创建 Store。
// Driver 为 ""/none 时返回 (nil, nil)：服务端以降级模式运行（见包注释）。
func Open(opt Options) (Store, error) {
	driver, err := NormalizeDriver(opt.Driver)
	if err != nil {
		return nil, err
	}
	if driver == "" {
		return nil, nil // 降级模式：无元数据库
	}
	if opt.DSN == "" {
		return nil, fmt.Errorf("store: driver %q requires a dsn", driver)
	}
	switch driver {
	case DriverSQLite, DriverMySQL, DriverPg:
		return newSQLStore(driver, opt.DSN)
	default:
		return nil, fmt.Errorf("store: unknown driver %q", opt.Driver)
	}
}
