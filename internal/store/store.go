// Package store 提供统一的元数据存储抽象（由独立工程 filemanageDb 并入）。
// 上层只依赖 Store 接口与模型类型，禁止绕过接口直接操作数据库。
//
// json/memory 驱动已移除；改为 SQL 三驱动 sqlite3|mysql|postgresql，
// 单一 sqlStore 实现 + 方言层（dialect.go）。
//
// 0.1.1 变更：新增 "file" 驱动（filestore.go 等）——未配置数据库时的**默认**元数据
// 存储：二进制追加日志（op+length+data+crc 帧）+ 内存索引 + 快照压缩，全功能可用；
// 旧配置 driver ""/"none" 一律映射到 file，**降级模式（503）自此取消**
// （store.Degraded/requireDB 保留作打开失败等异常路径的纵深防御）。
// file 驱动为单实例引擎：目录 flock 排他，两实例同目录并写会被拒绝启动。
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
	DriverFile   = "file" // 0.1.1：无数据库默认（DSN=存储目录）
)

// 用户主目录内部合成根（需求3）：路径 <dataDir>/Users 由配置派生，
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
	UpdateUserNickname(id int64, nickname string) error // 0.2.11：昵称可改可重复，仅展示用
	// UpdateUserAuth（0.2.12 认证批）：邮箱与二次验证四字段全量写入
	// （email/verified 来自绑定确认；mfaMethod ∈ ""|totp|email；
	// mfaSecret=TOTP 密钥或 ""——只增不改列、绝不触 password_hash）。
	UpdateUserAuth(id int64, email string, emailVerified bool, mfaMethod, mfaSecret string) error
	UpdateUserRoots(id int64, roots []string) error // 旧字段；仅作迁移输入
	UpdateUserRole(id int64, role string) error
	DeleteUser(id int64) error
	CountUsers() (int64, error)

	// 用户根分配（jail 模型，见 UserRoot 注释）
	ListUserRoots(userID int64) ([]*UserRoot, error)
	SetUserRoots(userID int64, list []*UserRoot) error // 全量替换（事务内先删后插）
	ListAllUserRoots() ([]*UserRoot, error)            // 管理台总览

	// server_meta：一次性数据迁移标记等键值（不存在返回 ""）
	GetMeta(key string) (string, error)
	SetMeta(key, value string) error

	// RewritePhysicalPath 将指定物理根下**一切按路径键控**的存量行的路径前缀
	// 由 oldPrefix 改写为 newPrefix（命中 path==old 或 path 以 old+"/" 开头；
	// 覆盖 file_metas/ai_descriptions/trash_items/folder_acls/share_links/
	// file_versions/upload_sessions）。0.2.11 家目录布局迁移（userhomes.v8）
	// 专用；单事务/单写锁内完成，幂等（二次调用返回 0）。返回改写行数。
	RewritePhysicalPath(rootID, oldPrefix, newPrefix string) (int64, error)

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
	// ListPublicViewACLs 主体为 public 且 view=1 的条目（匿名"公开共享目录"清单）。
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

	// 登录会话（refresh token，滑动过期）——0.2.3 起认证走 auth_sessions（下方），
	// 本组方法与 sessions 表保留不再读写（磁盘兼容，不做 ALTER/DROP）
	CreateSession(sess *Session) error
	GetSessionByTokenHash(tokenHash string) (*Session, error)
	RenewSession(id int64, expiresAt, lastUsedAt time.Time) error
	DeleteSession(id int64) error
	DeleteSessionsByUser(userID int64) error
	DeleteExpiredSessions(now time.Time) error

	// 认证会话（0.2.3：三层认证图持久层，不透明随机凭证；internal/auth.Center）
	CreateAuthSession(a *AuthSession) error
	GetAuthSessionByTokenHash(tokenHash string) (*AuthSession, error)
	RenewAuthSession(id int64, expiresAt, lastUsedAt time.Time) error // 无行→ErrNotFound（撤权竞态判据）
	// UpdateAuthSessionFlow（0.2.12）：按主键改写 FlowData（登录中转/设备码批准
	// 等协议状态推进）。无行→ErrNotFound（条目已被消费/吊销——调用方据此中止）。
	UpdateAuthSessionFlow(id int64, flowData string) error
	DeleteAuthSessionByTokenHash(tokenHash string) error
	DeleteAuthSessionsByUser(userID int64) error // userID<0 非法（share 保留 -1，调用方守门）
	DeleteExpiredAuthSessions(now time.Time) error
	ListActiveAuthSessions(limit int) ([]*AuthSession, error) // 未过期，token_hash 升序窗口（重启预热）

	// 分片上传会话（断点续传，跨实例共享）
	CreateUploadSession(u *UploadSession) error
	GetUploadSession(id string) (*UploadSession, error)
	GetActiveUploadSession(rootID, path string) (*UploadSession, error) // 同路径仅允许一个活动会话（并发写保护）
	ListUploadSessions(userID int64) ([]*UploadSession, error)          // userID<=0 返回全部
	UpdateUploadSession(u *UploadSession) error                         // 按 ID 全量更新
	DeleteUploadSession(id string) error
	DeleteUploadSessionsBefore(before time.Time) error

	// 审计日志
	AddAuditEvent(e *AuditEvent) error
	ListAuditEvents(limit int, userID int64) ([]*AuditEvent, error) // userID<=0 全部
	DeleteAuditBefore(before time.Time) error
}

// Options 打开参数。
type Options struct {
	Driver string // sqlite3 | mysql | postgresql（别名见 normalizeDriver）| none/""（降级模式）
	DSN    string // sqlite3 文件路径 / mysql DSN / postgres DSN
}

// NormalizeDriver 将用户配置的驱动名规范为 DriverFile/DriverSQLite/DriverMySQL/DriverPg。
// 0.1.1 起 ""/"none" 是 "file" 的别名（无数据库 = 文件存储，全功能可用；旧的
// "返回空串进入降级模式"语义废止）。json/memory 驱动已移除。
// 配置校验（internal/config）与 Open 共用本函数。
func NormalizeDriver(s string) (string, error) {
	switch s {
	case "", "none", "file":
		return DriverFile, nil
	case "sqlite", "sqlite3":
		return DriverSQLite, nil
	case "mysql", "mariadb":
		return DriverMySQL, nil
	case "postgres", "postgresql", "pg":
		return DriverPg, nil
	case "json", "memory":
		return "", fmt.Errorf("store: driver %q was removed in a prior version (configure sqlite3|mysql|postgresql, or \"file\"/\"none\" for the built-in file store)", s)
	default:
		return "", fmt.Errorf("store: unknown driver %q (want file|sqlite3|mysql|postgresql)", s)
	}
}

// ValidDriver 报告驱动名是否可被 Open 接受（供配置校验使用）。
func ValidDriver(s string) bool {
	_, err := NormalizeDriver(s)
	return err == nil
}

// Open 按驱动创建 Store。0.1.1 起不再有 (nil, nil) 降级返回：
// ""/none/file → 文件存储（DSN=目录，config.finalize 默认 <dataDir>/filestore）。
func Open(opt Options) (Store, error) {
	driver, err := NormalizeDriver(opt.Driver)
	if err != nil {
		return nil, err
	}
	if opt.DSN == "" {
		return nil, fmt.Errorf("store: driver %q requires a dsn", driver)
	}
	switch driver {
	case DriverFile:
		return newFileStore(opt.DSN)
	case DriverSQLite, DriverMySQL, DriverPg:
		return newSQLStore(driver, opt.DSN)
	default:
		return nil, fmt.Errorf("store: unknown driver %q", opt.Driver)
	}
}
