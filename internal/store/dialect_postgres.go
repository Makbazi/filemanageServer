// dialect_postgres.go：PostgreSQL 方言 DDL（lib/pq）。
//
// 与 sqlite 语义等价，差异点：
//   - BIGSERIAL 自增主键；无长度 TEXT；时间 TIMESTAMP；布尔列用 INTEGER
//     （Go 侧 b2i 传 int64，与 sqlite 一致）；embedding BYTEA；
//   - groups 为保留字，须双引号；
//   - 二级索引独立 CREATE INDEX IF NOT EXISTS。
//
// 已知行为差异（记录于 CHANGELOG，不做静默修正）：pg 的 LIKE 大小写敏感，
// sqlite 对 ASCII 大小写不敏感（SearchDescriptions 前缀匹配在 pg 下更严格）。
package store

import (
	"fmt"

	"gorm.io/gorm"
)

func pgDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			allowed_roots TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS file_metas (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			name TEXT NOT NULL, is_dir INTEGER NOT NULL, size BIGINT NOT NULL,
			mtime TIMESTAMP NOT NULL, kind TEXT NOT NULL, content_hash TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL,
			UNIQUE(user_id, root_id, path))`,
		`CREATE TABLE IF NOT EXISTS ai_descriptions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			name TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', embedding BYTEA,
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL,
			UNIQUE(user_id, root_id, path, kind))`,
		`CREATE TABLE IF NOT EXISTS trash_items (
			id BIGSERIAL PRIMARY KEY,
			trash_id TEXT NOT NULL UNIQUE, user_id BIGINT NOT NULL, root_id TEXT NOT NULL,
			rel_path TEXT NOT NULL, name TEXT NOT NULL, is_dir INTEGER NOT NULL,
			size BIGINT NOT NULL, deleted_at TIMESTAMP NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_desc_text ON ai_descriptions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_trash_deleted ON trash_items(deleted_at)`,
		`CREATE TABLE IF NOT EXISTS "groups" (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			members TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS folder_acls (
			id BIGSERIAL PRIMARY KEY,
			root_id TEXT NOT NULL, path TEXT NOT NULL DEFAULT '',
			subject_type TEXT NOT NULL, subject_id BIGINT NOT NULL DEFAULT 0,
			can_view INTEGER NOT NULL DEFAULT 0, can_download INTEGER NOT NULL DEFAULT 0,
			can_upload INTEGER NOT NULL DEFAULT 0, can_delete INTEGER NOT NULL DEFAULT 0,
			can_share INTEGER NOT NULL DEFAULT 0,
			created_by BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL,
			UNIQUE(root_id, path, subject_type, subject_id))`,
		`CREATE TABLE IF NOT EXISTS share_links (
			id BIGSERIAL PRIMARY KEY,
			token TEXT NOT NULL UNIQUE, root_id TEXT NOT NULL, path TEXT NOT NULL,
			is_dir INTEGER NOT NULL DEFAULT 0, password_hash TEXT NOT NULL DEFAULT '',
			expires_at TIMESTAMP NULL, disabled INTEGER NOT NULL DEFAULT 0,
			created_by BIGINT NOT NULL DEFAULT 0, created_at TIMESTAMP NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS file_versions (
			id BIGSERIAL PRIMARY KEY,
			root_id TEXT NOT NULL, path TEXT NOT NULL, version_no BIGINT NOT NULL,
			size BIGINT NOT NULL DEFAULT 0, hash TEXT NOT NULL DEFAULT '',
			store_id TEXT NOT NULL, created_by BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL,
			UNIQUE(root_id, path, version_no))`,
		`CREATE INDEX IF NOT EXISTS idx_versions_path ON file_versions(root_id, path)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_exp ON sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS upload_sessions (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			size BIGINT NOT NULL DEFAULT 0, chunk_size BIGINT NOT NULL DEFAULT 0,
			content_hash TEXT NOT NULL DEFAULT '', base_hash TEXT NOT NULL DEFAULT '',
			uploaded_chunks TEXT NOT NULL DEFAULT '[]',
			status TEXT NOT NULL DEFAULT 'uploading',
			created_at TIMESTAMP NOT NULL, updated_at TIMESTAMP NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_active ON upload_sessions(root_id, path, status)`,
		// ---- 新增表 ----
		`CREATE TABLE IF NOT EXISTS user_roots (
			id BIGSERIAL PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id TEXT NOT NULL,
			sub_path TEXT NOT NULL DEFAULT '', alias TEXT NOT NULL DEFAULT '',
			UNIQUE(user_id, root_id, sub_path))`,
		`CREATE TABLE IF NOT EXISTS server_meta (mkey TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id BIGSERIAL PRIMARY KEY,
			created_at TIMESTAMP NOT NULL, user_id BIGINT NOT NULL DEFAULT 0,
			username TEXT NOT NULL DEFAULT '', action TEXT NOT NULL,
			root_id TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_events(created_at)`,
		// ---- 0.2.3 auth_sessions（三层认证图持久层） ----
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id BIGSERIAL PRIMARY KEY,
			kind TEXT NOT NULL DEFAULT 'user', user_id BIGINT NOT NULL,
			username TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '',
			super INTEGER NOT NULL DEFAULT 0, gen BIGINT NOT NULL DEFAULT 0,
			ref TEXT NOT NULL DEFAULT '', client_id TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL UNIQUE,
			expires_at TIMESTAMP NOT NULL, last_used_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_authsess_exp ON auth_sessions(expires_at)`,
	}
}

// pgEnsureExtra 存量库加列守卫（0.2.4 auth_sessions.client_id；只增不改，
// 属幂等 DDL 清单的组成部分——pg 原生支持 ADD COLUMN IF NOT EXISTS）。
func pgEnsureExtra(db *gorm.DB) error {
	if err := db.Exec("ALTER TABLE auth_sessions ADD COLUMN IF NOT EXISTS client_id TEXT NOT NULL DEFAULT ''").Error; err != nil {
		return fmt.Errorf("migrate: add client_id: %w", err)
	}
	return nil
}

// pgSeqReset 跨库复制后的 BIGSERIAL 序列对齐（仅 copy.go 使用）：
// 显式主键插入不推进序列，不重置则切换后首个 INSERT 撞唯一键。
// setval(seq, max+1, false) 使下一个 nextval 恰为 max+1；空表落到 1。
func pgSeqReset() []string {
	intTables := []string{"users", "user_roots", "file_metas", "ai_descriptions",
		"trash_items", "groups", "folder_acls", "share_links", "file_versions",
		"sessions", "audit_events", "auth_sessions"}
	out := make([]string, 0, len(intTables))
	for _, t := range intTables {
		out = append(out, fmt.Sprintf(
			`SELECT setval(pg_get_serial_sequence('%s', 'id'), COALESCE(MAX(id),0)+1, false) FROM %s`, t, t))
	}
	return out
}
