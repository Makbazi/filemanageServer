// 元数据模型定义（由独立工程 filemanageDb 并入本服务端 internal/store）。
// 敏感字段三件套 PasswordHash/TokenHash 均为 json:"-"（防 API 泄露）；
// redis 缓存层持久化这些模型时必须使用影子结构保留敏感字段（见 internal/cache）。
package store

import "time"

const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// SuperAdminID 超级管理员的合成用户 ID（0.1.0 需求32/33）：超管只存
// <dataDir>/admin-account.json（internal/superadmin），不进 users 表；
// 会话表以 user_id=0 为超管哨兵（users 自增从 1 起、无外键，天然不冲突）。
const SuperAdminID int64 = 0

// User 服务端账户，区分用户并限定可见根目录。
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	AllowedRoots []string  `json:"allowedRoots"` // 空表示允许全部根目录
	CreatedAt    time.Time `json:"createdAt"`
}

// FileMeta 文件基础元数据（归属、日期等）。
type FileMeta struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"userId"`
	RootID      string    `json:"rootId"`
	Path        string    `json:"path"` // 相对根目录的正斜杠路径
	Name        string    `json:"name"`
	IsDir       bool      `json:"isDir"`
	Size        int64     `json:"size"`
	Mtime       time.Time `json:"mtime"`
	Kind        string    `json:"kind"` // image/video/audio/document/archive/code/other
	ContentHash string    `json:"contentHash"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// AIDescription AI 生成的文件描述。Embedding 为向量检索预留字段。
type AIDescription struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"userId"`
	RootID    string    `json:"rootId"`
	Path      string    `json:"path"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text"`
	Model     string    `json:"model"`
	Embedding []byte    `json:"embedding,omitempty"` // 预留：向量检索
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TrashItem 回收站条目。
type TrashItem struct {
	ID        int64     `json:"id"`
	TrashID   string    `json:"trashId"`
	UserID    int64     `json:"userId"`
	RootID    string    `json:"rootId"`
	RelPath   string    `json:"relPath"` // 相对根目录的原始路径
	Name      string    `json:"name"`
	IsDir     bool      `json:"isDir"`
	Size      int64     `json:"size"`
	DeletedAt time.Time `json:"deletedAt"`
}

// SearchHit 按描述检索的命中结果，Score 为预留相似度分值。
type SearchHit struct {
	AIDescription
	Score float64 `json:"score"`
}

// ---------- 用户根分配（jail 模型：管理员分配目录，用户只见所分配目录） ----------

// UserRoot 管理员为用户分配的一个虚拟根：物理根 root_id 下的子目录 sub_path。
// sub_path 为 "" 表示整个物理根（老配置的等价迁移）。用户在用户端口
// 只能访问自己被授予的虚拟根；物理配置根本身对用户不可见。
// 旧字段 User.AllowedRoots 仅作为迁移输入保留，运行时判定一律以本表为准。
type UserRoot struct {
	ID      int64  `json:"id"`
	UserID  int64  `json:"userId"`
	RootID  string `json:"rootId"`
	SubPath string `json:"subPath"`
	Alias   string `json:"alias"`
}

// ---------- 用户组（需求9：文件夹分享给用户组） ----------

// Group 用户组，Members 为成员用户 ID 列表。
type Group struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Members   []int64   `json:"members"`
	CreatedAt time.Time `json:"createdAt"`
}

// ---------- 目录权限（需求6/8：匿名只读 + 文件夹权限） ----------

// ACL 主体类型。
const (
	SubjectUser   = "user"
	SubjectGroup  = "group"
	SubjectPublic = "public" // 未登录访客
)

// PermSet 目录操作权限位。
type PermSet struct {
	View     bool `json:"view"`
	Download bool `json:"download"`
	Upload   bool `json:"upload"`
	Delete   bool `json:"delete"`
	Share    bool `json:"share"`
}

// FolderACL 某目录对某主体（用户/用户组/public）的授权条目。
// Path 为相对根目录路径，"" 表示根目录本身；子目录未配置时继承最近祖先。
type FolderACL struct {
	ID          int64     `json:"id"`
	RootID      string    `json:"rootId"`
	Path        string    `json:"path"`
	SubjectType string    `json:"subjectType"` // user | group | public
	SubjectID   int64     `json:"subjectId"`   // public 时为 0
	Perms       PermSet   `json:"perms"`
	CreatedBy   int64     `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

// ---------- 临时分享链接（需求7） ----------

// ShareLink 临时访问链接。PasswordHash 为空表示免密；ExpiresAt 为 nil 表示永久。
// PasswordHash 不出现在 JSON（API 响应）中，仅 HasPassword 对外可见。
type ShareLink struct {
	ID           int64      `json:"id"`
	Token        string     `json:"token"`
	RootID       string     `json:"rootId"`
	Path         string     `json:"path"`
	IsDir        bool       `json:"isDir"`
	PasswordHash string     `json:"-"`
	HasPassword  bool       `json:"hasPassword"`
	ExpiresAt    *time.Time `json:"expiresAt"` // nil = 永久有效
	Disabled     bool       `json:"disabled"`
	CreatedBy    int64      `json:"createdBy"`
	CreatedAt    time.Time  `json:"createdAt"`
}

// ---------- 文件版本（需求13） ----------

// FileVersion 文件历史版本。StoreID 为版本内容在版本库目录中的相对文件名。
type FileVersion struct {
	ID        int64     `json:"id"`
	RootID    string    `json:"rootId"`
	Path      string    `json:"path"`
	VersionNo int64     `json:"versionNo"`
	Size      int64     `json:"size"`
	Hash      string    `json:"hash"`
	StoreID   string    `json:"storeId"`
	CreatedBy int64     `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
}

// ---------- 登录会话（需求22：3 周滑动过期） ----------

// Session refresh token 会话。TokenHash 为 refresh token 的 SHA-256 hex，
// 不出现在 JSON 中。
type Session struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"userId"`
	TokenHash  string    `json:"-"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ---------- 认证会话（0.2.3：三层认证图，去 JWT） ----------

// 认证会话种类（Kind 必须持久——重启 Warm 恢复后仍须区分身份会话与分享会话：
// share 行绝不可被当作登录身份注入）：
//   - user：登录会话（客户端用户/管理台超管/DB 管理员）；有效期策略按持久化
//     Role 判定——Role=admin（含超管）为绝对 session.accessTTL 且不滑动（保持
//     管理面每小时重登节奏），普通用户滑动 session.idleDays（每次认证请求续期）；
//   - share：分享链接解锁会话，绝对 2h，Ref 绑定链接 token。
const (
	AuthKindUser  = "user"
	AuthKindShare = "share"
)

// AuthShareUserID share 行的 user_id 保留值——与超管哨兵 SuperAdminID(0) 区分，
// 按用户撤权（RevokeUser）绝不触碰 share 行，share 校验也绝不落入用户身份注入。
const AuthShareUserID int64 = -1

// AuthSession 一条认证凭据记录（认证图条目的持久形态）。凭证本身为随机 64-hex
// 字符串（不编码任何身份），表与各级缓存只存其 SHA-256（TokenHash，json:"-"，
// 永不出 API——敏感哈希纪律同 PasswordHash）。真实身份（UserID/Username/Role/
// Super/Gen）仅存服务端，校验时由认证图解析。
type AuthSession struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	UserID     int64     `json:"userId"` // 0=超管哨兵；-1=share 保留
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	Super      bool      `json:"super"`
	Gen        int64     `json:"gen"` // 超管凭据纪元（逐请求核对，需求32/34）
	Ref        string    `json:"ref"` // share：绑定的链接 token（share_links.token）
	TokenHash  string    `json:"-"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

// ---------- 审计日志（安全可追溯迭代） ----------

// AuditEvent 关键安全事件（登录/账户与权限变更/删除/分享/回滚）。
// 保留 90 天由后台 Cleaner 自动清理；仅管理端口可查询。
type AuditEvent struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UserID    int64     `json:"userId"`   // 0 = 匿名/未命中用户
	Username  string    `json:"username"` // 含登录失败时的尝试用户名
	Action    string    `json:"action"`
	RootID    string    `json:"rootId,omitempty"`
	Path      string    `json:"path,omitempty"` // 物理路径（相对物理根）
	Detail    string    `json:"detail,omitempty"`
	IP        string    `json:"ip,omitempty"`
}

// 审计动作常量。
const (
	AuditLogin       = "login"
	AuditLoginFail   = "login_fail"
	AuditRegister    = "register"
	AuditUserCreate  = "user_create"
	AuditUserUpdate  = "user_update"
	AuditUserDelete  = "user_delete"
	AuditSuperUpdate = "super_update" // 0.1.0 需求32：超管账户名/口令变更（文件回写）
	AuditRootsAssign = "roots_assign"
	AuditACLSet      = "acl_set"
	AuditACLDelete   = "acl_delete"
	AuditShareCreate = "share_create"
	AuditShareDelete = "share_delete"
	AuditFileDelete  = "file_delete"
	AuditRollback    = "rollback"
)

// ---------- 分片上传会话（需求3：分片/断点续传/并发写保护） ----------

// 上传会话状态。
const (
	UploadStatusUploading = "uploading"
	UploadStatusCompleted = "completed"
	UploadStatusAborted   = "aborted"
)

// UploadSession 分片上传会话。元数据入库以支持跨实例共享与重启续传；
// BaseHash 为乐观并发锁：创建会话时目标文件的现有内容哈希（新文件为空），
// 完成合并前服务端复核，不一致则报冲突。
type UploadSession struct {
	ID             string    `json:"id"` // 随机 hex/uuid
	UserID         int64     `json:"userId"`
	RootID         string    `json:"rootId"`
	Path           string    `json:"path"`
	Size           int64     `json:"size"`
	ChunkSize      int64     `json:"chunkSize"`
	ContentHash    string    `json:"contentHash"` // 期望的整文件 SHA-256（可空=不校验）
	BaseHash       string    `json:"baseHash"`    // 乐观锁基准哈希（可空）
	UploadedChunks []int64   `json:"uploadedChunks"`
	Status         string    `json:"status"` // uploading | completed | aborted
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}
