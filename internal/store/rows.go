// rows.go：gorm 行对象（0.6.0，需求2：SQL 操作一律走框架）。
//
// 列名/列类型与已发布 DDL（dialect_*.go）逐字对齐，仅作为 SQL 映射层存在：
//   - 布尔列存 INTEGER/SMALLINT 0/1（保持既有 sqlite .db 文件字节兼容，
//     模型层 bool 字段与行层 int64 之间经 b2i/i2b 显式转换）；
//   - JSON 列（allowed_roots/members/uploaded_chunks）存 TEXT，读写语义与原
//     json.Marshal/Unmarshal 完全一致（含 nil 切片的行为差异：users 允许 "null"，
//     groups/upload_sessions 规范化为 "[]"）；
//   - 可空时间（share_links.expires_at）用 *time.Time；embedding 用 BLOB。
//
// 上层 API 只见 model.go 的业务模型，行对象不出包。
package store

import (
	"encoding/json"
	"time"
)

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func i2b(n int64) bool { return n != 0 }

// ---------- users ----------

type userRow struct {
	ID           int64     `gorm:"column:id;primaryKey;autoIncrement"`
	Username     string    `gorm:"column:username"`
	PasswordHash string    `gorm:"column:password_hash"`
	Role         string    `gorm:"column:role"`
	AllowedRoots string    `gorm:"column:allowed_roots"` // JSON 文本
	CreatedAt    time.Time `gorm:"column:created_at"`
}

func (userRow) TableName() string { return "users" }

func (r userRow) fromUser(u *User) userRow {
	b, _ := json.Marshal(u.AllowedRoots)
	return userRow{ID: u.ID, Username: u.Username, PasswordHash: u.PasswordHash,
		Role: u.Role, AllowedRoots: string(b), CreatedAt: u.CreatedAt}
}

func (r userRow) toUser() *User {
	var u User
	_ = json.Unmarshal([]byte(r.AllowedRoots), &u.AllowedRoots)
	u.ID, u.Username, u.PasswordHash, u.Role, u.CreatedAt = r.ID, r.Username, r.PasswordHash, r.Role, r.CreatedAt
	return &u
}

// ---------- user_roots ----------

type userRootRow struct {
	ID      int64  `gorm:"column:id;primaryKey;autoIncrement"`
	UserID  int64  `gorm:"column:user_id"`
	RootID  string `gorm:"column:root_id"`
	SubPath string `gorm:"column:sub_path"`
	Alias   string `gorm:"column:alias"`
}

func (userRootRow) TableName() string { return "user_roots" }

func (r userRootRow) to() *UserRoot {
	return &UserRoot{ID: r.ID, UserID: r.UserID, RootID: r.RootID, SubPath: r.SubPath, Alias: r.Alias}
}

// ---------- server_meta ----------

type metaKVRow struct {
	Mkey  string `gorm:"column:mkey;primaryKey"`
	Value string `gorm:"column:value"`
}

func (metaKVRow) TableName() string { return "server_meta" }

// ---------- file_metas ----------

type fileMetaRow struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID      int64     `gorm:"column:user_id"`
	RootID      string    `gorm:"column:root_id"`
	Path        string    `gorm:"column:path"`
	Name        string    `gorm:"column:name"`
	IsDir       int64     `gorm:"column:is_dir"`
	Size        int64     `gorm:"column:size"`
	Mtime       time.Time `gorm:"column:mtime"`
	Kind        string    `gorm:"column:kind"`
	ContentHash string    `gorm:"column:content_hash"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
}

func (fileMetaRow) TableName() string { return "file_metas" }

func (r fileMetaRow) fromMeta(m *FileMeta) fileMetaRow {
	return fileMetaRow{ID: m.ID, UserID: m.UserID, RootID: m.RootID, Path: m.Path, Name: m.Name,
		IsDir: b2i(m.IsDir), Size: m.Size, Mtime: m.Mtime, Kind: m.Kind,
		ContentHash: m.ContentHash, UpdatedAt: m.UpdatedAt}
}

func (r fileMetaRow) to() *FileMeta {
	return &FileMeta{ID: r.ID, UserID: r.UserID, RootID: r.RootID, Path: r.Path, Name: r.Name,
		IsDir: i2b(r.IsDir), Size: r.Size, Mtime: r.Mtime, Kind: r.Kind,
		ContentHash: r.ContentHash, UpdatedAt: r.UpdatedAt}
}

// ---------- ai_descriptions ----------

type aiDescRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID    int64     `gorm:"column:user_id"`
	RootID    string    `gorm:"column:root_id"`
	Path      string    `gorm:"column:path"`
	Name      string    `gorm:"column:name"`
	Kind      string    `gorm:"column:kind"`
	Text      string    `gorm:"column:text"`
	Model     string    `gorm:"column:model"`
	Embedding []byte    `gorm:"column:embedding"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (aiDescRow) TableName() string { return "ai_descriptions" }

func (r aiDescRow) fromDesc(d *AIDescription) aiDescRow {
	return aiDescRow{ID: d.ID, UserID: d.UserID, RootID: d.RootID, Path: d.Path, Name: d.Name,
		Kind: d.Kind, Text: d.Text, Model: d.Model, Embedding: d.Embedding,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}

func (r aiDescRow) to() *AIDescription {
	return &AIDescription{ID: r.ID, UserID: r.UserID, RootID: r.RootID, Path: r.Path, Name: r.Name,
		Kind: r.Kind, Text: r.Text, Model: r.Model, Embedding: r.Embedding,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ---------- trash_items ----------

type trashRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	TrashID   string    `gorm:"column:trash_id"`
	UserID    int64     `gorm:"column:user_id"`
	RootID    string    `gorm:"column:root_id"`
	RelPath   string    `gorm:"column:rel_path"`
	Name      string    `gorm:"column:name"`
	IsDir     int64     `gorm:"column:is_dir"`
	Size      int64     `gorm:"column:size"`
	DeletedAt time.Time `gorm:"column:deleted_at"`
}

func (trashRow) TableName() string { return "trash_items" }

func (r trashRow) fromTrash(t *TrashItem) trashRow {
	return trashRow{ID: t.ID, TrashID: t.TrashID, UserID: t.UserID, RootID: t.RootID, RelPath: t.RelPath,
		Name: t.Name, IsDir: b2i(t.IsDir), Size: t.Size, DeletedAt: t.DeletedAt}
}

func (r trashRow) to() *TrashItem {
	return &TrashItem{ID: r.ID, TrashID: r.TrashID, UserID: r.UserID, RootID: r.RootID, RelPath: r.RelPath,
		Name: r.Name, IsDir: i2b(r.IsDir), Size: r.Size, DeletedAt: r.DeletedAt}
}

// ---------- groups ----------

type groupRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	Name      string    `gorm:"column:name"`
	Members   string    `gorm:"column:members"` // JSON 文本（nil 规范化为 "[]"）
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (groupRow) TableName() string { return "groups" }

func (r groupRow) fromGroup(g *Group) groupRow {
	b, _ := json.Marshal(orEmptyIDs(g.Members))
	return groupRow{ID: g.ID, Name: g.Name, Members: string(b), CreatedAt: g.CreatedAt}
}

func (r groupRow) to() *Group {
	var members []int64
	_ = json.Unmarshal([]byte(r.Members), &members)
	if members == nil {
		members = []int64{}
	}
	return &Group{ID: r.ID, Name: r.Name, Members: members, CreatedAt: r.CreatedAt}
}

func orEmptyIDs(in []int64) []int64 {
	if in == nil {
		return []int64{}
	}
	return in
}

// ---------- folder_acls ----------

type aclRow struct {
	ID          int64     `gorm:"column:id;primaryKey;autoIncrement"`
	RootID      string    `gorm:"column:root_id"`
	Path        string    `gorm:"column:path"`
	SubjectType string    `gorm:"column:subject_type"`
	SubjectID   int64     `gorm:"column:subject_id"`
	CanView     int64     `gorm:"column:can_view"`
	CanDownload int64     `gorm:"column:can_download"`
	CanUpload   int64     `gorm:"column:can_upload"`
	CanDelete   int64     `gorm:"column:can_delete"`
	CanShare    int64     `gorm:"column:can_share"`
	CreatedBy   int64     `gorm:"column:created_by"`
	CreatedAt   time.Time `gorm:"column:created_at"`
}

func (aclRow) TableName() string { return "folder_acls" }

func (r aclRow) fromACL(a *FolderACL) aclRow {
	return aclRow{ID: a.ID, RootID: a.RootID, Path: a.Path, SubjectType: a.SubjectType, SubjectID: a.SubjectID,
		CanView: b2i(a.Perms.View), CanDownload: b2i(a.Perms.Download), CanUpload: b2i(a.Perms.Upload),
		CanDelete: b2i(a.Perms.Delete), CanShare: b2i(a.Perms.Share), CreatedBy: a.CreatedBy, CreatedAt: a.CreatedAt}
}

func (r aclRow) to() *FolderACL {
	return &FolderACL{ID: r.ID, RootID: r.RootID, Path: r.Path, SubjectType: r.SubjectType, SubjectID: r.SubjectID,
		Perms: PermSet{View: i2b(r.CanView), Download: i2b(r.CanDownload), Upload: i2b(r.CanUpload),
			Delete: i2b(r.CanDelete), Share: i2b(r.CanShare)},
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
}

// ---------- share_links ----------

type shareRow struct {
	ID           int64      `gorm:"column:id;primaryKey;autoIncrement"`
	Token        string     `gorm:"column:token"`
	RootID       string     `gorm:"column:root_id"`
	Path         string     `gorm:"column:path"`
	IsDir        int64      `gorm:"column:is_dir"`
	PasswordHash string     `gorm:"column:password_hash"`
	ExpiresAt    *time.Time `gorm:"column:expires_at"`
	Disabled     int64      `gorm:"column:disabled"`
	CreatedBy    int64      `gorm:"column:created_by"`
	CreatedAt    time.Time  `gorm:"column:created_at"`
}

func (shareRow) TableName() string { return "share_links" }

func (r shareRow) fromShare(l *ShareLink) shareRow {
	return shareRow{ID: l.ID, Token: l.Token, RootID: l.RootID, Path: l.Path, IsDir: b2i(l.IsDir),
		PasswordHash: l.PasswordHash, ExpiresAt: l.ExpiresAt, Disabled: b2i(l.Disabled),
		CreatedBy: l.CreatedBy, CreatedAt: l.CreatedAt}
}

func (r shareRow) to() *ShareLink {
	l := &ShareLink{ID: r.ID, Token: r.Token, RootID: r.RootID, Path: r.Path, IsDir: i2b(r.IsDir),
		PasswordHash: r.PasswordHash, ExpiresAt: r.ExpiresAt, Disabled: i2b(r.Disabled),
		CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
	l.HasPassword = r.PasswordHash != ""
	return l
}

// ---------- file_versions ----------

type versionRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	RootID    string    `gorm:"column:root_id"`
	Path      string    `gorm:"column:path"`
	VersionNo int64     `gorm:"column:version_no"`
	Size      int64     `gorm:"column:size"`
	Hash      string    `gorm:"column:hash"`
	StoreID   string    `gorm:"column:store_id"`
	CreatedBy int64     `gorm:"column:created_by"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

func (versionRow) TableName() string { return "file_versions" }

func (r versionRow) fromVersion(v *FileVersion) versionRow {
	return versionRow{ID: v.ID, RootID: v.RootID, Path: v.Path, VersionNo: v.VersionNo, Size: v.Size,
		Hash: v.Hash, StoreID: v.StoreID, CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt}
}

func (r versionRow) to() *FileVersion {
	return &FileVersion{ID: r.ID, RootID: r.RootID, Path: r.Path, VersionNo: r.VersionNo, Size: r.Size,
		Hash: r.Hash, StoreID: r.StoreID, CreatedBy: r.CreatedBy, CreatedAt: r.CreatedAt}
}

// ---------- sessions ----------

type sessionRow struct {
	ID         int64     `gorm:"column:id;primaryKey;autoIncrement"`
	UserID     int64     `gorm:"column:user_id"`
	TokenHash  string    `gorm:"column:token_hash"`
	ExpiresAt  time.Time `gorm:"column:expires_at"`
	LastUsedAt time.Time `gorm:"column:last_used_at"`
	CreatedAt  time.Time `gorm:"column:created_at"`
}

func (sessionRow) TableName() string { return "sessions" }

func (r sessionRow) fromSession(s *Session) sessionRow {
	return sessionRow{ID: s.ID, UserID: s.UserID, TokenHash: s.TokenHash,
		ExpiresAt: s.ExpiresAt, LastUsedAt: s.LastUsedAt, CreatedAt: s.CreatedAt}
}

func (r sessionRow) to() *Session {
	return &Session{ID: r.ID, UserID: r.UserID, TokenHash: r.TokenHash,
		ExpiresAt: r.ExpiresAt, LastUsedAt: r.LastUsedAt, CreatedAt: r.CreatedAt}
}

// ---------- upload_sessions ----------

type uploadRow struct {
	ID             string    `gorm:"column:id;primaryKey"` // 随机 hex，非自增
	UserID         int64     `gorm:"column:user_id"`
	RootID         string    `gorm:"column:root_id"`
	Path           string    `gorm:"column:path"`
	Size           int64     `gorm:"column:size"`
	ChunkSize      int64     `gorm:"column:chunk_size"`
	ContentHash    string    `gorm:"column:content_hash"`
	BaseHash       string    `gorm:"column:base_hash"`
	UploadedChunks string    `gorm:"column:uploaded_chunks"` // JSON 文本（nil 规范化为 "[]"）
	Status         string    `gorm:"column:status"`
	CreatedAt      time.Time `gorm:"column:created_at"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (uploadRow) TableName() string { return "upload_sessions" }

func (r uploadRow) fromUpload(u *UploadSession) uploadRow {
	b, _ := json.Marshal(orEmptyIDs(u.UploadedChunks))
	return uploadRow{ID: u.ID, UserID: u.UserID, RootID: u.RootID, Path: u.Path, Size: u.Size,
		ChunkSize: u.ChunkSize, ContentHash: u.ContentHash, BaseHash: u.BaseHash, UploadedChunks: string(b),
		Status: u.Status, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt}
}

func (r uploadRow) to() *UploadSession {
	var chunks []int64
	_ = json.Unmarshal([]byte(r.UploadedChunks), &chunks)
	if chunks == nil {
		chunks = []int64{}
	}
	return &UploadSession{ID: r.ID, UserID: r.UserID, RootID: r.RootID, Path: r.Path, Size: r.Size,
		ChunkSize: r.ChunkSize, ContentHash: r.ContentHash, BaseHash: r.BaseHash, UploadedChunks: chunks,
		Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// ---------- audit_events ----------

type auditRow struct {
	ID        int64     `gorm:"column:id;primaryKey;autoIncrement"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UserID    int64     `gorm:"column:user_id"`
	Username  string    `gorm:"column:username"`
	Action    string    `gorm:"column:action"`
	RootID    string    `gorm:"column:root_id"`
	Path      string    `gorm:"column:path"`
	Detail    string    `gorm:"column:detail"`
	IP        string    `gorm:"column:ip"`
}

func (auditRow) TableName() string { return "audit_events" }

func (r auditRow) fromAudit(e *AuditEvent) auditRow {
	return auditRow{ID: e.ID, CreatedAt: e.CreatedAt, UserID: e.UserID, Username: e.Username,
		Action: e.Action, RootID: e.RootID, Path: e.Path, Detail: e.Detail, IP: e.IP}
}

func (r auditRow) to() *AuditEvent {
	return &AuditEvent{ID: r.ID, CreatedAt: r.CreatedAt, UserID: r.UserID, Username: r.Username,
		Action: r.Action, RootID: r.RootID, Path: r.Path, Detail: r.Detail, IP: r.IP}
}
