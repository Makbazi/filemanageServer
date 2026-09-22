// dialect_sqlite.go：sqlite3 方言 DDL。
// 语句与原 filemanageDb 0.2.0 完全逐字一致（modernc.org/sqlite 纯 Go 驱动），
// 保证既有 .db 文件无需重建即可继续使用。改动本文件即破坏磁盘兼容，须走迁移评审。
package store

func sqliteDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
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
		// ---- v0.2.0 新增表（幂等，旧库自动升级） ----
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
		// ---- v0.5.0 新增表（幂等，旧库自动升级；只增不改已发布表） ----
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
	}
}
