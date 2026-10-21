// sqlstore.go：Store 接口的唯一 SQL 实现（sqlite3/mysql/postgresql 共用）。
// 需求2：数据操作一律经 gorm 框架（行对象映射见 rows.go）；
// 建表/迁移仍走 dialect_*.go 的幂等 DDL（需求25：sqlite 既有 .db 文件字节兼容，
// 只增不改，不使用 gorm AutoMigrate）。upsert 经 clause.OnConflict 由 gorm 按
// 方言渲染（sqlite/pg ON CONFLICT、mysql ON DUPLICATE KEY UPDATE），各方言语义
// 一致。唯一约束冲突经 TranslateError/文本兜底统一映射为
// ErrExists，记录不存在统一 ErrNotFound。
package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type sqlStore struct {
	db *gorm.DB
	d  *dialect
}

func newSQLStore(driver, dsn string) (Store, error) {
	d, err := newDialect(driver)
	if err != nil {
		return nil, err
	}
	fullDSN := d.normalizeDSN(dsn)
	var dialector gorm.Dialector
	switch d.name {
	case DriverSQLite:
		dialector = glebarezsqlite.Open(fullDSN) // 纯 Go（modernc 同源），无 CGO
	case DriverMySQL:
		dialector = mysql.Open(fullDSN)
	case DriverPg:
		dialector = postgres.Open(fullDSN)
	default:
		return nil, fmt.Errorf("store: no gorm dialector for driver %q", d.name)
	}
	db, err := gorm.Open(dialector, &gorm.Config{
		TranslateError: true, // 唯一键/未找到 → gorm.ErrDuplicatedKey/ErrRecordNotFound
		Logger:         logger.Default.LogMode(logger.Warn),
	})
	if err != nil { // 启动即暴露连接/DSN 错误，不留到首个请求
		return nil, fmt.Errorf("store: connect %s: %w", d.name, err)
	}
	if d.name == DriverSQLite {
		if raw, err := db.DB(); err == nil {
			raw.SetMaxOpenConns(1) // sqlite 单写者，避免锁冲突
		}
	}
	return &sqlStore{db: db, d: d}, nil
}

func (s *sqlStore) Driver() string { return s.d.name }

// Ping 连通性探测（管理台数据库状态卡；实现 store.pinger）。
func (s *sqlStore) Ping() error {
	raw, err := s.db.DB()
	if err != nil {
		return err
	}
	return raw.Ping()
}

func (s *sqlStore) Close() error {
	raw, err := s.db.DB()
	if err != nil {
		return nil
	}
	return raw.Close()
}

// Migrate 执行幂等建表 DDL（不 AutoMigrate，保持已发布表字节兼容），随后执行
// 方言加列守卫（0.2.4 auth_sessions.client_id，见 dialect.extra 注释——只增
// 不改，存量 0.2.3 库自动补列，新库 CREATE 清单已含）。
func (s *sqlStore) Migrate() error {
	for _, q := range s.d.ddl {
		if err := s.db.Exec(q).Error; err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if s.d.extra != nil {
		return s.d.extra(s.db)
	}
	return nil
}

// ---------- 错误/结果映射 ----------

// mapErr 把 gorm 哨兵错误翻译为 Store 契约错误；唯一键冲突额外做文本兜底
// （TranslateError 未覆盖的驱动路径也保持统一的 ErrExists 语义）。
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return ErrNotFound
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return ErrExists
	case isUniqueText(err):
		return ErrExists
	default:
		return err
	}
}

// isUniqueText 三驱动唯一约束报错文本兜底（mysql 1062 / sqlite UNIQUE constraint
// failed / postgres duplicate key value）。
func isUniqueText(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return mysqlUnique(err) ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value")
}

// affectedG 更新/删除零行 → ErrNotFound（零行即视为记录不存在）。
func affectedG(res *gorm.DB) error {
	if res.Error != nil {
		return mapErr(res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// upsert 构造按冲突列覆盖指定列的 OnConflict 子句。
func upsert(conflict, set []string) clause.OnConflict {
	cols := make([]clause.Column, 0, len(conflict))
	for _, c := range conflict {
		cols = append(cols, clause.Column{Name: c})
	}
	return clause.OnConflict{Columns: cols, DoUpdates: clause.AssignmentColumns(set)}
}

// ---------- users ----------

func (s *sqlStore) CreateUser(u *User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	r := userRow{}.fromUser(u)
	r.ID = 0
	if err := s.db.Create(&r).Error; err != nil {
		return mapErr(err)
	}
	u.ID = r.ID
	return nil
}

func (s *sqlStore) GetUserByName(username string) (*User, error) {
	var r userRow
	if err := s.db.Where("username = ?", username).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.toUser(), nil
}

func (s *sqlStore) GetUserByID(id int64) (*User, error) {
	var r userRow
	if err := s.db.Where("id = ?", id).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.toUser(), nil
}

func (s *sqlStore) ListUsers() ([]*User, error) {
	var rows []userRow
	if err := s.db.Order("id").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*User, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].toUser())
	}
	return out, nil
}

func (s *sqlStore) UpdateUserPassword(id int64, hash string) error {
	return affectedG(s.db.Model(&userRow{}).Where("id = ?", id).Update("password_hash", hash))
}

func (s *sqlStore) UpdateUserRoots(id int64, roots []string) error {
	r := userRow{}.fromUser(&User{AllowedRoots: roots})
	return affectedG(s.db.Model(&userRow{}).Where("id = ?", id).Update("allowed_roots", r.AllowedRoots))
}

func (s *sqlStore) UpdateUserNickname(id int64, nickname string) error {
	return affectedG(s.db.Model(&userRow{}).Where("id = ?", id).Update("nickname", nickname))
}

// UpdateUserAuth（0.2.12）：邮箱+二次验证四列全量写入（map 式 Updates——
// 空串/零值列不被跳过；password_hash 绝不触碰）。
func (s *sqlStore) UpdateUserAuth(id int64, email string, emailVerified bool, mfaMethod, mfaSecret string) error {
	return affectedG(s.db.Model(&userRow{}).Where("id = ?", id).
		Updates(map[string]any{"email": email, "email_verified": b2i(emailVerified),
			"mfa_method": mfaMethod, "mfa_secret": mfaSecret}))
}

func (s *sqlStore) UpdateUserRole(id int64, role string) error {
	return affectedG(s.db.Model(&userRow{}).Where("id = ?", id).Update("role", role))
}

// rewriteSub 前缀改写命中判据（Go 侧过滤，避免方言间 LIKE 转义/大小写规则差异；
// 0.2.11 布局迁移为启动一次性操作，按根取行规模可控）。
func rewriteSub(path, oldPrefix string) bool {
	return path == oldPrefix || strings.HasPrefix(path, oldPrefix+"/")
}

// RewritePhysicalPath 见接口注释（0.2.11）。七表逐一"取 id+路径列→Go 判前缀→
// 按主键回写"，单事务；不触磁盘（实体搬移由 fileops 迁移器负责）。
func (s *sqlStore) RewritePhysicalPath(rootID, oldPrefix, newPrefix string) (int64, error) {
	if oldPrefix == "" || oldPrefix == newPrefix {
		return 0, nil
	}
	var total int64
	err := s.db.Transaction(func(tx *gorm.DB) error {
		{ // file_metas
			var hits []struct {
				ID   int64
				Path string
			}
			if err := tx.Model(&fileMetaRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&fileMetaRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // ai_descriptions
			var hits []struct {
				ID   int64
				Path string
			}
			if err := tx.Model(&aiDescRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&aiDescRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // trash_items（rel_path 列）
			var hits []struct {
				ID      int64
				RelPath string
			}
			if err := tx.Model(&trashRow{}).Where("root_id = ?", rootID).Select("id", "rel_path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.RelPath, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.RelPath, oldPrefix)
				if err := tx.Model(&trashRow{}).Where("id = ?", h.ID).Update("rel_path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // folder_acls
			var hits []struct {
				ID   int64
				Path string
			}
			if err := tx.Model(&aclRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&aclRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // share_links
			var hits []struct {
				ID   int64
				Path string
			}
			if err := tx.Model(&shareRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&shareRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // file_versions
			var hits []struct {
				ID   int64
				Path string
			}
			if err := tx.Model(&versionRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&versionRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		{ // upload_sessions（TEXT 主键）
			var hits []struct {
				ID   string
				Path string
			}
			if err := tx.Model(&uploadRow{}).Where("root_id = ?", rootID).Select("id", "path").Find(&hits).Error; err != nil {
				return err
			}
			for _, h := range hits {
				if !rewriteSub(h.Path, oldPrefix) {
					continue
				}
				np := newPrefix + strings.TrimPrefix(h.Path, oldPrefix)
				if err := tx.Model(&uploadRow{}).Where("id = ?", h.ID).Update("path", np).Error; err != nil {
					return err
				}
				total++
			}
		}
		return nil
	})
	return total, mapErr(err)
}

// DeleteUser 级联清理用户的全部关联数据（单事务，按固定顺序级联删除）。
func (s *sqlStore) DeleteUser(id int64) error {
	return mapErr(s.db.Transaction(func(tx *gorm.DB) error {
		byUser := []any{&fileMetaRow{}, &aiDescRow{}, &trashRow{}, &sessionRow{}, &uploadRow{}, &userRootRow{},
			&authSessionRow{}} // 0.2.3：认证图随删号级联（share 行 user_id=-1 天然不命中）
		for _, m := range byUser {
			if err := tx.Where("user_id = ?", id).Delete(m).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("subject_type = ? AND subject_id = ?", SubjectUser, id).Delete(&aclRow{}).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", id).Delete(&userRow{}).Error
	}))
}

func (s *sqlStore) CountUsers() (int64, error) {
	var n int64
	err := s.db.Model(&userRow{}).Count(&n).Error
	return n, mapErr(err)
}

// ---------- user roots（jail 模型） ----------

func (s *sqlStore) ListUserRoots(userID int64) ([]*UserRoot, error) {
	var rows []userRootRow
	if err := s.db.Where("user_id = ?", userID).Order("root_id, sub_path").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*UserRoot, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) ListAllUserRoots() ([]*UserRoot, error) {
	var rows []userRootRow
	if err := s.db.Order("user_id, root_id, sub_path").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*UserRoot, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

// SetUserRoots 全量替换某用户的分配（事务内先删后插，跨驱动一致）。
func (s *sqlStore) SetUserRoots(userID int64, list []*UserRoot) error {
	return mapErr(s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Delete(&userRootRow{}).Error; err != nil {
			return err
		}
		for _, r := range list {
			if r.RootID == "" {
				continue
			}
			row := userRootRow{UserID: userID, RootID: r.RootID, SubPath: r.SubPath, Alias: r.Alias}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	}))
}

// ---------- server_meta ----------

func (s *sqlStore) GetMeta(key string) (string, error) {
	var r metaKVRow
	err := s.db.Where("mkey = ?", key).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", mapErr(err)
	}
	return r.Value, nil
}

func (s *sqlStore) SetMeta(key, value string) error {
	err := s.db.Clauses(upsert([]string{"mkey"}, []string{"value"})).
		Create(&metaKVRow{Mkey: key, Value: value}).Error
	return mapErr(err)
}

// ---------- file metas ----------

// PutFileMeta upsert 元数据。content_hash 保留语义（0.2.1，四方言一致）：
// 内容哈希只在"上传/回滚等真正产生新字节"的调用方携带；handleList 之类的
// 例行刷行不得把已存哈希刷空——incoming hash=="" 且 size/mtime（秒精度）与
// 旧行相同 → 保留旧 content_hash；否则以 incoming 覆盖（含清除）。
// 读-改-写非原子：多实例并写同一行竞态存在，哈希仅作缓存性加速判据，
// 竞态最坏结果是退化为 unknown（客户端复查），不影响正确性。
func (s *sqlStore) PutFileMeta(m *FileMeta) error {
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now()
	}
	set := []string{"name", "is_dir", "size", "mtime", "kind", "content_hash", "updated_at"}
	if m.ContentHash == "" {
		var old fileMetaRow
		err := s.db.Where("user_id = ? AND root_id = ? AND path = ?", m.UserID, m.RootID, m.Path).
			Take(&old).Error
		if err == nil && old.Size == m.Size &&
			old.Mtime.Unix() == m.Mtime.Unix() && old.ContentHash != "" {
			set = []string{"name", "is_dir", "size", "mtime", "kind", "updated_at"}
		}
	}
	row := fileMetaRow{}.fromMeta(m)
	err := s.db.Clauses(upsert([]string{"user_id", "root_id", "path"}, set)).
		Create(&row).Error
	return mapErr(err)
}

func (s *sqlStore) GetFileMeta(userID int64, rootID, path string) (*FileMeta, error) {
	var r fileMetaRow
	err := s.db.Where("user_id = ? AND root_id = ? AND path = ?", userID, rootID, path).Take(&r).Error
	if err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListFileMetas(userID int64, rootID string) ([]*FileMeta, error) {
	q := s.db.Where("user_id = ?", userID)
	if rootID != "" {
		q = q.Where("root_id = ?", rootID)
	}
	var rows []fileMetaRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*FileMeta, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) DeleteFileMeta(userID int64, rootID, path string) error {
	return affectedG(s.db.Where("user_id = ? AND root_id = ? AND path = ?", userID, rootID, path).
		Delete(&fileMetaRow{}))
}

func (s *sqlStore) RenameFileMeta(userID int64, rootID, from, to string) error {
	return affectedG(s.db.Model(&fileMetaRow{}).
		Where("user_id = ? AND root_id = ? AND (path = ? OR path LIKE ?)", userID, rootID, from, from+"/%").
		Updates(map[string]any{"path": to, "updated_at": time.Now()}))
}

// ---------- ai descriptions ----------

func (s *sqlStore) SaveAIDescription(d *AIDescription) error {
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	row := aiDescRow{}.fromDesc(d)
	row.ID = 0
	err := s.db.Clauses(upsert([]string{"user_id", "root_id", "path", "kind"},
		[]string{"name", "text", "model", "embedding", "updated_at"})).
		Create(&row).Error
	if err != nil {
		return mapErr(err)
	}
	d.ID = row.ID
	return nil
}

func (s *sqlStore) GetAIDescription(userID int64, rootID, path, kind string) (*AIDescription, error) {
	var r aiDescRow
	err := s.db.Where("user_id = ? AND root_id = ? AND path = ? AND kind = ?", userID, rootID, path, kind).
		Take(&r).Error
	if err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListAIDescriptions(userID int64, rootID string) ([]*AIDescription, error) {
	q := s.db.Where("user_id = ?", userID)
	if rootID != "" {
		q = q.Where("root_id = ?", rootID)
	}
	var rows []aiDescRow
	if err := q.Order("updated_at DESC").Limit(1000).Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*AIDescription, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) DeleteAIDescription(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&aiDescRow{}))
}

func (s *sqlStore) SearchDescriptions(userID int64, text string, limit int) ([]*SearchHit, error) {
	if limit <= 0 {
		limit = 50
	}
	// ESCAPE '/' 三驱动通用（原 ESCAPE '\' 在 mysql 下为非法转义）；'_' 一并转义。
	like := "%" + escapeLike(text) + "%"
	var rows []aiDescRow
	err := s.db.Where("user_id = ?", userID).
		Where("text LIKE ? ESCAPE '/' OR name LIKE ? ESCAPE '/'", like, like).
		Order("updated_at DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]*SearchHit, 0, len(rows))
	for i := range rows {
		out = append(out, &SearchHit{AIDescription: *rows[i].to(), Score: 1})
	}
	return out, nil
}

// ---------- trash ----------

func (s *sqlStore) AddTrashItem(t *TrashItem) error {
	if t.DeletedAt.IsZero() {
		t.DeletedAt = time.Now()
	}
	row := trashRow{}.fromTrash(t)
	row.ID = 0
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	t.ID = row.ID
	return nil
}

func (s *sqlStore) ListTrashItems(userID int64, rootID string) ([]*TrashItem, error) {
	q := s.db.Where("user_id = ?", userID)
	if rootID != "" {
		q = q.Where("root_id = ?", rootID)
	}
	var rows []trashRow
	if err := q.Order("deleted_at DESC").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*TrashItem, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

// TakeTrashItem 原子地取出并删除条目（事务内 SELECT+DELETE，防止并发双还原）。
func (s *sqlStore) TakeTrashItem(userID int64, trashID string) (*TrashItem, error) {
	var t *TrashItem
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var r trashRow
		if err := tx.Where("user_id = ? AND trash_id = ?", userID, trashID).Take(&r).Error; err != nil {
			return err
		}
		if err := tx.Where("id = ?", r.ID).Delete(&trashRow{}).Error; err != nil {
			return err
		}
		t = r.to()
		return nil
	})
	if err != nil {
		return nil, mapErr(err)
	}
	return t, nil
}

func (s *sqlStore) RemoveTrashItem(userID int64, trashID string) error {
	return affectedG(s.db.Where("user_id = ? AND trash_id = ?", userID, trashID).Delete(&trashRow{}))
}

func (s *sqlStore) ClearTrash(userID int64, rootID string) ([]*TrashItem, error) {
	items, err := s.ListTrashItems(userID, rootID)
	if err != nil {
		return nil, err
	}
	q := s.db.Where("user_id = ?", userID)
	if rootID != "" {
		q = q.Where("root_id = ?", rootID)
	}
	if err := q.Delete(&trashRow{}).Error; err != nil {
		return nil, mapErr(err)
	}
	return items, nil
}

func (s *sqlStore) ListTrashBefore(before time.Time) ([]*TrashItem, error) {
	var rows []trashRow
	if err := s.db.Where("deleted_at < ?", before).Order("deleted_at").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*TrashItem, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

// ---------- groups（表名 gorm 按方言自动引用） ----------

func (s *sqlStore) CreateGroup(g *Group) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	row := groupRow{}.fromGroup(g)
	row.ID = 0
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	g.ID = row.ID
	return nil
}

func (s *sqlStore) GetGroupByID(id int64) (*Group, error) {
	var r groupRow
	if err := s.db.Where("id = ?", id).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) GetGroupByName(name string) (*Group, error) {
	var r groupRow
	if err := s.db.Where("name = ?", name).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListGroups() ([]*Group, error) {
	var rows []groupRow
	if err := s.db.Order("id").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*Group, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) UpdateGroup(g *Group) error {
	row := groupRow{}.fromGroup(g)
	return affectedG(s.db.Model(&groupRow{}).Where("id = ?", g.ID).
		Updates(map[string]any{"name": g.Name, "members": row.Members}))
}

func (s *sqlStore) DeleteGroup(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&groupRow{}))
}

// ---------- 目录权限（ACL） ----------

func (s *sqlStore) SetFolderACL(a *FolderACL) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	row := aclRow{}.fromACL(a)
	row.ID = 0
	err := s.db.Clauses(upsert([]string{"root_id", "path", "subject_type", "subject_id"},
		[]string{"can_view", "can_download", "can_upload", "can_delete", "can_share", "created_by"})).
		Create(&row).Error
	if err != nil {
		return mapErr(err)
	}
	if a.ID == 0 {
		a.ID = row.ID
	}
	return nil
}

func (s *sqlStore) GetFolderACLs(rootID, path string) ([]*FolderACL, error) {
	var rows []aclRow
	if err := s.db.Where("root_id = ? AND path = ?", rootID, path).Order("id").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	return aclList(rows), nil
}

func (s *sqlStore) ListRootACLs(rootID string) ([]*FolderACL, error) {
	q := s.db.Model(&aclRow{})
	if rootID != "" {
		q = q.Where("root_id = ?", rootID)
	}
	var rows []aclRow
	if err := q.Order("root_id, path, id").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	return aclList(rows), nil
}

func aclList(rows []aclRow) []*FolderACL {
	out := make([]*FolderACL, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out
}

// ListPublicViewACLs public 主体且 view=1 的条目（公开共享目录清单）。
func (s *sqlStore) ListPublicViewACLs() ([]*FolderACL, error) {
	var rows []aclRow
	err := s.db.Where("subject_type = ? AND can_view = 1", SubjectPublic).
		Order("root_id, path, id").Find(&rows).Error
	if err != nil {
		return nil, mapErr(err)
	}
	return aclList(rows), nil
}

func (s *sqlStore) DeleteFolderACL(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&aclRow{}))
}

func (s *sqlStore) DeleteFolderACLsBySubject(subjectType string, subjectID int64) error {
	return mapErr(s.db.Where("subject_type = ? AND subject_id = ?", subjectType, subjectID).
		Delete(&aclRow{}).Error)
}

// ---------- 分享链接 ----------

func (s *sqlStore) CreateShareLink(l *ShareLink) error {
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	l.HasPassword = l.PasswordHash != ""
	row := shareRow{}.fromShare(l)
	row.ID = 0
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	l.ID = row.ID
	return nil
}

func (s *sqlStore) GetShareLinkByToken(token string) (*ShareLink, error) {
	var r shareRow
	if err := s.db.Where("token = ?", token).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListShareLinks(createdBy int64) ([]*ShareLink, error) {
	q := s.db.Model(&shareRow{})
	if createdBy > 0 {
		q = q.Where("created_by = ?", createdBy)
	}
	var rows []shareRow
	if err := q.Order("id DESC").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*ShareLink, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) UpdateShareLink(l *ShareLink) error {
	return affectedG(s.db.Model(&shareRow{}).Where("id = ?", l.ID).
		Updates(map[string]any{"password_hash": l.PasswordHash, "expires_at": l.ExpiresAt,
			"disabled": b2i(l.Disabled)}))
}

func (s *sqlStore) DeleteShareLink(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&shareRow{}))
}

// ---------- 文件版本 ----------

// AddFileVersion 在事务内自动分配版本号（该路径 max+1），避免并发重复。
func (s *sqlStore) AddFileVersion(v *FileVersion) error {
	return mapErr(s.db.Transaction(func(tx *gorm.DB) error {
		if v.CreatedAt.IsZero() {
			v.CreatedAt = time.Now()
		}
		if v.VersionNo == 0 {
			var maxNo int64
			if err := tx.Model(&versionRow{}).
				Where("root_id = ? AND path = ?", v.RootID, v.Path).
				Select("COALESCE(MAX(version_no), 0)").Scan(&maxNo).Error; err != nil {
				return err
			}
			v.VersionNo = maxNo + 1
		}
		row := versionRow{}.fromVersion(v)
		row.ID = 0
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		v.ID = row.ID
		return nil
	}))
}

func (s *sqlStore) GetFileVersion(id int64) (*FileVersion, error) {
	var r versionRow
	if err := s.db.Where("id = ?", id).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListFileVersions(rootID, path string) ([]*FileVersion, error) {
	var rows []versionRow
	err := s.db.Where("root_id = ? AND path = ?", rootID, path).Order("version_no DESC").Find(&rows).Error
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]*FileVersion, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) DeleteFileVersion(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&versionRow{}))
}

// DeleteFileVersions 删除该路径（含子路径）的全部版本记录。
func (s *sqlStore) DeleteFileVersions(rootID, path string) error {
	return mapErr(s.db.Where("root_id = ? AND (path = ? OR path LIKE ?)", rootID, path, path+"/%").
		Delete(&versionRow{}).Error)
}

// ---------- 登录会话 ----------

func (s *sqlStore) CreateSession(sess *Session) error {
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = now
	}
	row := sessionRow{}.fromSession(sess)
	row.ID = 0
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	sess.ID = row.ID
	return nil
}

func (s *sqlStore) GetSessionByTokenHash(tokenHash string) (*Session, error) {
	var r sessionRow
	if err := s.db.Where("token_hash = ?", tokenHash).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) RenewSession(id int64, expiresAt, lastUsedAt time.Time) error {
	return affectedG(s.db.Model(&sessionRow{}).Where("id = ?", id).
		Updates(map[string]any{"expires_at": expiresAt, "last_used_at": lastUsedAt}))
}

func (s *sqlStore) DeleteSession(id int64) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&sessionRow{}))
}

func (s *sqlStore) DeleteSessionsByUser(userID int64) error {
	return mapErr(s.db.Where("user_id = ?", userID).Delete(&sessionRow{}).Error)
}

func (s *sqlStore) DeleteExpiredSessions(now time.Time) error {
	return mapErr(s.db.Where("expires_at < ?", now).Delete(&sessionRow{}).Error)
}

// ---------- 认证会话（0.2.3 三层认证图持久层） ----------

func (s *sqlStore) CreateAuthSession(a *AuthSession) error {
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.LastUsedAt.IsZero() {
		a.LastUsedAt = now
	}
	row := authSessionRow{}.fromAuthSession(a)
	row.ID = 0
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	a.ID = row.ID
	return nil
}

func (s *sqlStore) GetAuthSessionByTokenHash(tokenHash string) (*AuthSession, error) {
	var r authSessionRow
	if err := s.db.Where("token_hash = ?", tokenHash).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

// RenewAuthSession 按主键续期；RowsAffected=0 → ErrNotFound（行已被撤权/清扫——
// 认证中心据此摘除上层缓存，绝不续住已吊销条目）。map 式 Updates（零值列不被跳过）。
func (s *sqlStore) RenewAuthSession(id int64, expiresAt, lastUsedAt time.Time) error {
	return affectedG(s.db.Model(&authSessionRow{}).Where("id = ?", id).
		Updates(map[string]any{"expires_at": expiresAt, "last_used_at": lastUsedAt}))
}

// UpdateAuthSessionFlow（0.2.12）：按主键改写协议状态 JSON；RowsAffected=0 →
// ErrNotFound（条目已被消费/吊销，调用方据此中止流程——与 Renew 同竞态判据）。
func (s *sqlStore) UpdateAuthSessionFlow(id int64, flowData string) error {
	return affectedG(s.db.Model(&authSessionRow{}).Where("id = ?", id).
		Updates(map[string]any{"flow_data": flowData}))
}

func (s *sqlStore) DeleteAuthSessionByTokenHash(tokenHash string) error {
	return mapErr(s.db.Where("token_hash = ?", tokenHash).Delete(&authSessionRow{}).Error)
}

func (s *sqlStore) DeleteAuthSessionsByUser(userID int64) error {
	return mapErr(s.db.Where("user_id = ?", userID).Delete(&authSessionRow{}).Error)
}

func (s *sqlStore) DeleteExpiredAuthSessions(now time.Time) error {
	return mapErr(s.db.Where("expires_at < ?", now).Delete(&authSessionRow{}).Error)
}

// ListActiveAuthSessions 未过期条目（token_hash 升序窗口，供认证中心重启预热；
// 窗口按哈希稳定采样，冷凭证仍经读路径惰性恢复；上限由调用方传入）。
func (s *sqlStore) ListActiveAuthSessions(limit int) ([]*AuthSession, error) {
	var rows []authSessionRow
	q := s.db.Where("expires_at >= ?", time.Now()).Order("token_hash")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*AuthSession, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

// ---------- 分片上传会话 ----------

func (s *sqlStore) CreateUploadSession(u *UploadSession) error {
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if u.Status == "" {
		u.Status = UploadStatusUploading
	}
	row := uploadRow{}.fromUpload(u)
	if err := s.db.Create(&row).Error; err != nil {
		return mapErr(err)
	}
	return nil
}

func (s *sqlStore) GetUploadSession(id string) (*UploadSession, error) {
	var r uploadRow
	if err := s.db.Where("id = ?", id).Take(&r).Error; err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) GetActiveUploadSession(rootID, path string) (*UploadSession, error) {
	var r uploadRow
	err := s.db.Where("root_id = ? AND path = ? AND status = ?", rootID, path, UploadStatusUploading).
		Order("created_at DESC").Take(&r).Error
	if err != nil {
		return nil, mapErr(err)
	}
	return r.to(), nil
}

func (s *sqlStore) ListUploadSessions(userID int64) ([]*UploadSession, error) {
	q := s.db.Model(&uploadRow{})
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	var rows []uploadRow
	if err := q.Order("created_at DESC").Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*UploadSession, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) UpdateUploadSession(u *UploadSession) error {
	u.UpdatedAt = time.Now()
	row := uploadRow{}.fromUpload(u)
	return affectedG(s.db.Model(&uploadRow{}).Where("id = ?", u.ID).
		Updates(map[string]any{"size": u.Size, "content_hash": u.ContentHash, "base_hash": u.BaseHash,
			"uploaded_chunks": row.UploadedChunks, "status": u.Status, "updated_at": u.UpdatedAt}))
}

func (s *sqlStore) DeleteUploadSession(id string) error {
	return affectedG(s.db.Where("id = ?", id).Delete(&uploadRow{}))
}

func (s *sqlStore) DeleteUploadSessionsBefore(before time.Time) error {
	return mapErr(s.db.Where("created_at < ? AND status != ?", before, UploadStatusUploading).
		Delete(&uploadRow{}).Error)
}

// ---------- 审计日志 ----------

func (s *sqlStore) AddAuditEvent(e *AuditEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	row := auditRow{}.fromAudit(e)
	row.ID = 0
	return mapErr(s.db.Create(&row).Error)
}

func (s *sqlStore) ListAuditEvents(limit int, userID int64) ([]*AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := s.db.Model(&auditRow{})
	if userID > 0 {
		q = q.Where("user_id = ?", userID)
	}
	var rows []auditRow
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, mapErr(err)
	}
	out := make([]*AuditEvent, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].to())
	}
	return out, nil
}

func (s *sqlStore) DeleteAuditBefore(before time.Time) error {
	return mapErr(s.db.Where("created_at < ?", before).Delete(&auditRow{}).Error)
}

// ---------- 0.1.1 全表迁移通道（rowSource/rowSink，与 filecopy.go 对端） ----------

// sqlScanInt/sqlScanStr：keyset 分批读（与原 copyInt/copyStr 逐字同语义）。
func sqlScanInt[T any](s *sqlStore, keyOf func(*T) int64, emit func([]T) error) error {
	var last int64
	for {
		var batch []T
		if err := s.db.Model(new(T)).Where("id > ?", last).
			Order("id").Limit(copyBatchSize).Find(&batch).Error; err != nil {
			return fmt.Errorf("copy read: %w", mapErr(err))
		}
		if len(batch) == 0 {
			return nil
		}
		if err := emit(batch); err != nil {
			return err
		}
		last = keyOf(&batch[len(batch)-1])
		if len(batch) < copyBatchSize {
			return nil
		}
	}
}

func sqlScanStr[T any](s *sqlStore, pkCol string, keyOf func(*T) string, emit func([]T) error) error {
	last := ""
	for {
		var batch []T
		if err := s.db.Model(new(T)).Where(pkCol+" > ?", last).
			Order(pkCol).Limit(copyBatchSize).Find(&batch).Error; err != nil {
			return fmt.Errorf("copy read: %w", mapErr(err))
		}
		if len(batch) == 0 {
			return nil
		}
		if err := emit(batch); err != nil {
			return err
		}
		last = keyOf(&batch[len(batch)-1])
		if len(batch) < copyBatchSize {
			return nil
		}
	}
}

func (s *sqlStore) copyScan(table string, fn func(batch any) error) error {
	emit := func(b any) error { return fn(b) }
	switch table {
	case "users":
		return sqlScanInt(s, func(r *userRow) int64 { return r.ID }, func(b []userRow) error { return emit(b) })
	case "user_roots":
		return sqlScanInt(s, func(r *userRootRow) int64 { return r.ID }, func(b []userRootRow) error { return emit(b) })
	case "file_metas":
		return sqlScanInt(s, func(r *fileMetaRow) int64 { return r.ID }, func(b []fileMetaRow) error { return emit(b) })
	case "ai_descriptions":
		return sqlScanInt(s, func(r *aiDescRow) int64 { return r.ID }, func(b []aiDescRow) error { return emit(b) })
	case "trash_items":
		return sqlScanInt(s, func(r *trashRow) int64 { return r.ID }, func(b []trashRow) error { return emit(b) })
	case "groups":
		return sqlScanInt(s, func(r *groupRow) int64 { return r.ID }, func(b []groupRow) error { return emit(b) })
	case "folder_acls":
		return sqlScanInt(s, func(r *aclRow) int64 { return r.ID }, func(b []aclRow) error { return emit(b) })
	case "share_links":
		return sqlScanInt(s, func(r *shareRow) int64 { return r.ID }, func(b []shareRow) error { return emit(b) })
	case "file_versions":
		return sqlScanInt(s, func(r *versionRow) int64 { return r.ID }, func(b []versionRow) error { return emit(b) })
	case "sessions":
		return sqlScanInt(s, func(r *sessionRow) int64 { return r.ID }, func(b []sessionRow) error { return emit(b) })
	case "upload_sessions":
		return sqlScanStr(s, "id", func(r *uploadRow) string { return r.ID }, func(b []uploadRow) error { return emit(b) })
	case "server_meta":
		return sqlScanStr(s, "mkey", func(r *metaKVRow) string { return r.Mkey }, func(b []metaKVRow) error { return emit(b) })
	case "audit_events":
		return sqlScanInt(s, func(r *auditRow) int64 { return r.ID }, func(b []auditRow) error { return emit(b) })
	case "auth_sessions":
		return sqlScanInt(s, func(r *authSessionRow) int64 { return r.ID }, func(b []authSessionRow) error { return emit(b) })
	}
	return fmt.Errorf("copy: unknown table %q", table)
}

func (s *sqlStore) copyWrite(table string, batch any) error {
	var err error
	switch b := batch.(type) {
	case []userRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []userRootRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []fileMetaRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []aiDescRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []trashRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []groupRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []aclRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []shareRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []versionRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []sessionRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []uploadRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []metaKVRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []auditRow:
		err = s.db.CreateInBatches(b, 500).Error
	case []authSessionRow:
		err = s.db.CreateInBatches(b, 500).Error
	default:
		return fmt.Errorf("copy: batch type mismatch for table %q", table)
	}
	if err != nil {
		return fmt.Errorf("copy write: %w", mapErr(err))
	}
	return nil
}

// copyFinish postgresql 序列对齐（显式主键插入不推进 SERIAL；sqlite no-op）。
func (s *sqlStore) copyFinish() error {
	for _, q := range s.d.seqReset {
		if err := s.db.Exec(q).Error; err != nil {
			return fmt.Errorf("reset sequences: %w", mapErr(err))
		}
	}
	return nil
}

var (
	_ rowSource = (*sqlStore)(nil)
	_ rowSink   = (*sqlStore)(nil)
)

// 编译期断言：sqlStore 完整实现 Store 接口。
var _ Store = (*sqlStore)(nil)
