// dialect_mysql.go：MySQL/MariaDB 方言 DDL。
//
// 与 sqlite 语义等价的表结构，差异点：
//   - mysql 无 CREATE INDEX IF NOT EXISTS，二级索引全部折叠进 CREATE TABLE 内联 KEY；
//   - groups 为 8.x 保留字，须反引号；
//   - utf8mb4 下 InnoDB 索引键上限 3072 字节，因此对各 VARCHAR 列做了长度收束
//     （root_id 32 / path 700 / kind·subject_type·status 16 / hash 80 / token·id 64·128），
//     所有复合 UNIQUE/KEY 均 <= 3000 字节；超过 path 长度的路径写入会报错（README 已注明）；
//   - 时间列 DATETIME(3)，DSN 自动 parseTime=true（dialect.normalizeDSN）。
//
// 已知行为差异（记录于 CHANGELOG，不做静默修正）：默认 collation utf8mb4_0900_ai_ci
// 大小写不敏感（username/token 等 UNIQUE 判定与 sqlite/pg 不同）。
package store

import (
	"fmt"

	"gorm.io/gorm"
)

const mysqlTail = " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"

func mysqlDDL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			username VARCHAR(64) NOT NULL UNIQUE,
			password_hash VARCHAR(100) NOT NULL,
			role VARCHAR(16) NOT NULL DEFAULT 'user',
			allowed_roots TEXT NOT NULL,
			created_at DATETIME(3) NOT NULL)` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS file_metas (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL,
			name VARCHAR(255) NOT NULL, is_dir TINYINT NOT NULL, size BIGINT NOT NULL,
			mtime DATETIME(3) NOT NULL, kind VARCHAR(16) NOT NULL, content_hash VARCHAR(80) NOT NULL DEFAULT '',
			updated_at DATETIME(3) NOT NULL,
			UNIQUE KEY uk_meta (user_id, root_id, path))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS ai_descriptions (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL,
			name VARCHAR(255) NOT NULL, kind VARCHAR(16) NOT NULL, text MEDIUMTEXT NOT NULL,
			model VARCHAR(64) NOT NULL DEFAULT '', embedding LONGBLOB,
			created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			UNIQUE KEY uk_desc (user_id, root_id, path, kind),
			KEY idx_desc_text (user_id))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS trash_items (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			trash_id VARCHAR(64) NOT NULL UNIQUE, user_id BIGINT NOT NULL, root_id VARCHAR(32) NOT NULL,
			rel_path VARCHAR(700) NOT NULL, name VARCHAR(255) NOT NULL, is_dir TINYINT NOT NULL,
			size BIGINT NOT NULL, deleted_at DATETIME(3) NOT NULL,
			KEY idx_trash_deleted (deleted_at))` + mysqlTail,
		"CREATE TABLE IF NOT EXISTS `groups` (" + `
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			name VARCHAR(64) NOT NULL UNIQUE,
			members TEXT NOT NULL,
			created_at DATETIME(3) NOT NULL)` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS folder_acls (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL DEFAULT '',
			subject_type VARCHAR(16) NOT NULL, subject_id BIGINT NOT NULL DEFAULT 0,
			can_view TINYINT NOT NULL DEFAULT 0, can_download TINYINT NOT NULL DEFAULT 0,
			can_upload TINYINT NOT NULL DEFAULT 0, can_delete TINYINT NOT NULL DEFAULT 0,
			can_share TINYINT NOT NULL DEFAULT 0,
			created_by BIGINT NOT NULL DEFAULT 0, created_at DATETIME(3) NOT NULL,
			UNIQUE KEY uk_acl (root_id, path, subject_type, subject_id))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS share_links (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			token VARCHAR(64) NOT NULL UNIQUE, root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL,
			is_dir TINYINT NOT NULL DEFAULT 0, password_hash VARCHAR(100) NOT NULL DEFAULT '',
			expires_at DATETIME(3) NULL, disabled TINYINT NOT NULL DEFAULT 0,
			created_by BIGINT NOT NULL DEFAULT 0, created_at DATETIME(3) NOT NULL)` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS file_versions (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL, version_no BIGINT NOT NULL,
			size BIGINT NOT NULL DEFAULT 0, hash VARCHAR(80) NOT NULL DEFAULT '',
			store_id VARCHAR(64) NOT NULL, created_by BIGINT NOT NULL DEFAULT 0,
			created_at DATETIME(3) NOT NULL,
			UNIQUE KEY uk_ver (root_id, path, version_no),
			KEY idx_versions_path (root_id, path))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS sessions (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id BIGINT NOT NULL, token_hash VARCHAR(128) NOT NULL UNIQUE,
			expires_at DATETIME(3) NOT NULL, last_used_at DATETIME(3) NOT NULL,
			created_at DATETIME(3) NOT NULL,
			KEY idx_sessions_exp (expires_at))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS upload_sessions (
			id VARCHAR(64) NOT NULL PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id VARCHAR(32) NOT NULL, path VARCHAR(700) NOT NULL,
			size BIGINT NOT NULL DEFAULT 0, chunk_size BIGINT NOT NULL DEFAULT 0,
			content_hash VARCHAR(80) NOT NULL DEFAULT '', base_hash VARCHAR(80) NOT NULL DEFAULT '',
			uploaded_chunks TEXT NOT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'uploading',
			created_at DATETIME(3) NOT NULL, updated_at DATETIME(3) NOT NULL,
			KEY idx_upload_active (root_id, path, status))` + mysqlTail,
		// ---- 新增表 ----
		`CREATE TABLE IF NOT EXISTS user_roots (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			user_id BIGINT NOT NULL, root_id VARCHAR(32) NOT NULL,
			sub_path VARCHAR(700) NOT NULL DEFAULT '', alias VARCHAR(64) NOT NULL DEFAULT '',
			UNIQUE KEY uk_userroot (user_id, root_id, sub_path(200)))` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS server_meta (
			mkey VARCHAR(64) NOT NULL PRIMARY KEY, value TEXT NOT NULL)` + mysqlTail,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			created_at DATETIME(3) NOT NULL, user_id BIGINT NOT NULL DEFAULT 0,
			username VARCHAR(64) NOT NULL DEFAULT '', action VARCHAR(32) NOT NULL,
			root_id VARCHAR(32) NOT NULL DEFAULT '', path VARCHAR(700) NOT NULL DEFAULT '',
			detail VARCHAR(500) NOT NULL DEFAULT '', ip VARCHAR(64) NOT NULL DEFAULT '',
			KEY idx_audit_created (created_at))` + mysqlTail,
		// ---- 0.2.3 auth_sessions（三层认证图持久层） ----
		`CREATE TABLE IF NOT EXISTS auth_sessions (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			kind VARCHAR(16) NOT NULL DEFAULT 'user', user_id BIGINT NOT NULL,
			username VARCHAR(64) NOT NULL DEFAULT '', role VARCHAR(16) NOT NULL DEFAULT '',
			super TINYINT NOT NULL DEFAULT 0, gen BIGINT NOT NULL DEFAULT 0,
			ref VARCHAR(128) NOT NULL DEFAULT '', client_id VARCHAR(64) NOT NULL DEFAULT '',
			token_hash VARCHAR(128) NOT NULL UNIQUE,
			expires_at DATETIME(3) NOT NULL, last_used_at DATETIME(3) NOT NULL,
			created_at DATETIME(3) NOT NULL,
			KEY idx_authsess_exp (expires_at))` + mysqlTail,
	}
}

// mysqlEnsureExtra 存量库加列守卫（0.2.4 auth_sessions.client_id；只增不改，
// 属幂等 DDL 清单的组成部分——MySQL 不支持 ADD COLUMN IF NOT EXISTS，先查
// information_schema 再 ALTER）。
func mysqlEnsureExtra(db *gorm.DB) error {
	var cnt int64
	err := db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'auth_sessions' AND column_name = 'client_id'").
		Scan(&cnt).Error
	if err != nil {
		return fmt.Errorf("migrate: probe client_id: %w", err)
	}
	if cnt == 0 {
		if err := db.Exec("ALTER TABLE auth_sessions ADD COLUMN client_id VARCHAR(64) NOT NULL DEFAULT ''").Error; err != nil {
			return fmt.Errorf("migrate: add client_id: %w", err)
		}
	}
	return nil
}
