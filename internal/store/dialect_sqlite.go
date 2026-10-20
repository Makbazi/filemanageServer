// dialect_sqlite.go：sqlite3 方言 DDL。
// 语句与原 filemanageDb 0.2.0 完全逐字一致（modernc.org/sqlite 纯 Go 驱动），
// 保证既有 .db 文件无需重建即可继续使用。改动本文件即破坏磁盘兼容，须走迁移评审。
package store

import (
	"fmt"

	"gorm.io/gorm"
)

func sqliteDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			nickname TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user',
			allowed_roots TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS file_metas (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			name TEXT NOT NULL, is_dir INTEGER NOT NULL, size INTEGER NOT NULL,
			mtime DATETIME NOT NULL, kind TEXT NOT NULL, content_hash TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL,
			UNIQUE(user_id, root_id, path))`,
		`CREATE TABLE IF NOT EXISTS ai_descriptions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			name TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT '', embedding BLOB,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			UNIQUE(user_id, root_id, path, kind))`,
		`CREATE TABLE IF NOT EXISTS trash_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trash_id TEXT NOT NULL UNIQUE, user_id INTEGER NOT NULL, root_id TEXT NOT NULL,
			rel_path TEXT NOT NULL, name TEXT NOT NULL, is_dir INTEGER NOT NULL,
			size INTEGER NOT NULL, deleted_at DATETIME NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_desc_text ON ai_descriptions(user_id)`,
		// ---- 新增表（幂等，旧库自动升级） ----
		`CREATE TABLE IF NOT EXISTS groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			members TEXT NOT NULL DEFAULT '[]',
			created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS folder_acls (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			root_id TEXT NOT NULL, path TEXT NOT NULL DEFAULT '',
			subject_type TEXT NOT NULL, subject_id INTEGER NOT NULL DEFAULT 0,
			can_view INTEGER NOT NULL DEFAULT 0, can_download INTEGER NOT NULL DEFAULT 0,
			can_upload INTEGER NOT NULL DEFAULT 0, can_delete INTEGER NOT NULL DEFAULT 0,
			can_share INTEGER NOT NULL DEFAULT 0,
			created_by INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL,
			UNIQUE(root_id, path, subject_type, subject_id))`,
		`CREATE TABLE IF NOT EXISTS share_links (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			token TEXT NOT NULL UNIQUE, root_id TEXT NOT NULL, path TEXT NOT NULL,
			is_dir INTEGER NOT NULL DEFAULT 0, password_hash TEXT NOT NULL DEFAULT '',
			expires_at DATETIME NULL, disabled INTEGER NOT NULL DEFAULT 0,
			created_by INTEGER NOT NULL DEFAULT 0, created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS file_versions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			root_id TEXT NOT NULL, path TEXT NOT NULL, version_no INTEGER NOT NULL,
			size INTEGER NOT NULL DEFAULT 0, hash TEXT NOT NULL DEFAULT '',
			store_id TEXT NOT NULL, created_by INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			UNIQUE(root_id, path, version_no))`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL, token_hash TEXT NOT NULL UNIQUE,
			expires_at DATETIME NOT NULL, last_used_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS upload_sessions (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL, root_id TEXT NOT NULL, path TEXT NOT NULL,
			size INTEGER NOT NULL DEFAULT 0, chunk_size INTEGER NOT NULL DEFAULT 0,
			content_hash TEXT NOT NULL DEFAULT '', base_hash TEXT NOT NULL DEFAULT '',
			uploaded_chunks TEXT NOT NULL DEFAULT '[]',
			status TEXT NOT NULL DEFAULT 'uploading',
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL)`,
		// ---- 新增表（幂等，旧库自动升级；只增不改已发布表） ----
		`CREATE TABLE IF NOT EXISTS user_roots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL, root_id TEXT NOT NULL,
			sub_path TEXT NOT NULL DEFAULT '', alias TEXT NOT NULL DEFAULT '',
			UNIQUE(user_id, root_id, sub_path))`,
		`CREATE TABLE IF NOT EXISTS server_meta (mkey TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at DATETIME NOT NULL, user_id INTEGER NOT NULL DEFAULT 0,
			username TEXT NOT NULL DEFAULT '', action TEXT NOT NULL,
			root_id TEXT NOT NULL DEFAULT '', path TEXT NOT NULL DEFAULT '',
			detail TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_events(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_upload_active ON upload_sessions(root_id, path, status)`,
		`CREATE INDEX IF NOT EXISTS idx_versions_path ON file_versions(root_id, path)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_exp ON sessions(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_trash_deleted ON trash_items(deleted_at)`,
		// ---- 0.2.3 auth_sessions（三层认证图持久层；幂等，旧库自动升级；只增不改已发布表） ----
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL DEFAULT 'user', user_id INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '',
			super INTEGER NOT NULL DEFAULT 0, gen INTEGER NOT NULL DEFAULT 0,
			ref TEXT NOT NULL DEFAULT '', client_id TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL UNIQUE,
			expires_at DATETIME NOT NULL, last_used_at DATETIME NOT NULL,
			created_at DATETIME NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_authsess_exp ON auth_sessions(expires_at)`,
	}
}

// sqliteEnsureExtra 存量库加列守卫（0.2.4 auth_sessions.client_id、
// 0.2.11 users.nickname；只增不改，属幂等 DDL 清单的组成部分——sqlite 无
// ADD COLUMN IF NOT EXISTS，先查 pragma_table_info 再 ALTER；既有列/表绝不
// 触碰，磁盘字节兼容不受影响）。
func sqliteEnsureExtra(db *gorm.DB) error {
	var cnt int64
	err := db.Raw("SELECT COUNT(*) FROM pragma_table_info('auth_sessions') WHERE name = 'client_id'").
		Scan(&cnt).Error
	if err != nil {
		return fmt.Errorf("migrate: probe client_id: %w", err)
	}
	if cnt == 0 {
		if err := db.Exec("ALTER TABLE auth_sessions ADD COLUMN client_id TEXT NOT NULL DEFAULT ''").Error; err != nil {
			return fmt.Errorf("migrate: add client_id: %w", err)
		}
	}
	cnt = 0
	err = db.Raw("SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'nickname'").
		Scan(&cnt).Error
	if err != nil {
		return fmt.Errorf("migrate: probe users.nickname: %w", err)
	}
	if cnt == 0 {
		if err := db.Exec("ALTER TABLE users ADD COLUMN nickname TEXT NOT NULL DEFAULT ''").Error; err != nil {
			return fmt.Errorf("migrate: add users.nickname: %w", err)
		}
	}
	return nil
}
