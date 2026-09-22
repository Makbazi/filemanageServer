// copy.go：跨驱动/跨库全表元数据搬迁（0.7.0 管理台热切换可选"迁移现有数据"）。
//
// 方式：按表 keyset 分批（WHERE pk > ? ORDER BY pk LIMIT n）从旧库读出（复用
// rows.go 的 gorm 行模型，字段编码由驱动层负责，不手写 SELECT/INSERT 语句），
// 原样保留主键批量写入新库；新库须为**空库**（connect 前置检查 TableNonEmpty，
// 非空且未 force 时 API 层返回 409，本层不做强判）。
// postgresql 目标库额外重置各 serial 序列（显式主键插入不推进 SERIAL，
// 不重置则切换后首个 CreateUser 必撞唯一键）；sqlite/mysql 自增随插入自动前移。
// 迁移语义：users/…/server_meta 全部表逐行复制，两库表结构均由幂等 DDL 保证一致。
package store

import "fmt"

// copyBatchSize keyset 分批大小（行/批）。
const copyBatchSize = 1000

// asSQLStore 解包到 *sqlStore（Proxy 取其当前委托）。
func asSQLStore(st Store) (*sqlStore, bool) {
	if p, ok := st.(*Proxy); ok {
		st = p.Current()
	}
	s, ok := st.(*sqlStore)
	return s, ok
}

// copyInt copies one autoincrement-`id` table. gen 提供行模型与主键取值。
func copyInt[T any](dst, src *sqlStore, keyOf func(*T) int64) (int64, error) {
	var total, last int64
	for {
		var batch []T
		if err := src.db.Model(new(T)).Where("id > ?", last).
			Order("id").Limit(copyBatchSize).Find(&batch).Error; err != nil {
			return total, fmt.Errorf("copy read: %w", mapErr(err))
		}
		if len(batch) == 0 {
			return total, nil
		}
		if err := dst.db.CreateInBatches(batch, 500).Error; err != nil {
			return total, fmt.Errorf("copy write: %w", mapErr(err))
		}
		total += int64(len(batch))
		last = keyOf(&batch[len(batch)-1])
		if len(batch) < copyBatchSize {
			return total, nil
		}
	}
}

// copyStr copies one TEXT-primary-key table (server_meta.mkey / upload_sessions.id).
func copyStr[T any](dst, src *sqlStore, pkCol string, keyOf func(*T) string) (int64, error) {
	var total int64
	last := ""
	for {
		var batch []T
		if err := src.db.Model(new(T)).Where(pkCol+" > ?", last).
			Order(pkCol).Limit(copyBatchSize).Find(&batch).Error; err != nil {
			return total, fmt.Errorf("copy read: %w", mapErr(err))
		}
		if len(batch) == 0 {
			return total, nil
		}
		if err := dst.db.CreateInBatches(batch, 500).Error; err != nil {
			return total, fmt.Errorf("copy write: %w", mapErr(err))
		}
		total += int64(len(batch))
		last = keyOf(&batch[len(batch)-1])
		if len(batch) < copyBatchSize {
			return total, nil
		}
	}
}

// CopyAllTables 把 src（可为 Proxy，取其当前委托）的全部表数据复制到 dst，
// 返回每表行数。dst/src 都必须是本包 sqlStore（三驱动之一）。
func CopyAllTables(dst, src Store) (map[string]int64, error) {
	d, ok1 := asSQLStore(dst)
	s, ok2 := asSQLStore(src)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("store: copy requires sqlStore endpoints")
	}
	out := map[string]int64{}
	var n int64
	var err error
	if n, err = copyInt(d, s, func(r *userRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("users: %w", err)
	}
	out["users"] = n
	if n, err = copyInt(d, s, func(r *userRootRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("user_roots: %w", err)
	}
	out["user_roots"] = n
	if n, err = copyInt(d, s, func(r *fileMetaRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("file_metas: %w", err)
	}
	out["file_metas"] = n
	if n, err = copyInt(d, s, func(r *aiDescRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("ai_descriptions: %w", err)
	}
	out["ai_descriptions"] = n
	if n, err = copyInt(d, s, func(r *trashRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("trash_items: %w", err)
	}
	out["trash_items"] = n
	if n, err = copyInt(d, s, func(r *groupRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("groups: %w", err)
	}
	out["groups"] = n
	if n, err = copyInt(d, s, func(r *aclRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("folder_acls: %w", err)
	}
	out["folder_acls"] = n
	if n, err = copyInt(d, s, func(r *shareRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("share_links: %w", err)
	}
	out["share_links"] = n
	if n, err = copyInt(d, s, func(r *versionRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("file_versions: %w", err)
	}
	out["file_versions"] = n
	if n, err = copyInt(d, s, func(r *sessionRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("sessions: %w", err)
	}
	out["sessions"] = n
	if n, err = copyStr(d, s, "id", func(r *uploadRow) string { return r.ID }); err != nil {
		return out, fmt.Errorf("upload_sessions: %w", err)
	}
	out["upload_sessions"] = n
	if n, err = copyStr(d, s, "mkey", func(r *metaKVRow) string { return r.Mkey }); err != nil {
		return out, fmt.Errorf("server_meta: %w", err)
	}
	out["server_meta"] = n
	if n, err = copyInt(d, s, func(r *auditRow) int64 { return r.ID }); err != nil {
		return out, fmt.Errorf("audit_events: %w", err)
	}
	out["audit_events"] = n

	// 方言级序列对齐（见 dialect.go：pg 显式主键插入不推进 SERIAL；sqlite/mysql no-op）
	for _, q := range d.d.seqReset {
		if err := d.db.Exec(q).Error; err != nil {
			return out, fmt.Errorf("reset sequences: %w", mapErr(err))
		}
	}
	return out, nil
}

// tableNames 全表清单（非空检查与状态展示共用）。
var tableNames = []string{"users", "user_roots", "file_metas", "ai_descriptions",
	"trash_items", "groups", "folder_acls", "share_links", "file_versions",
	"sessions", "upload_sessions", "server_meta", "audit_events"}

// TableNonEmpty 返回 st 中已有数据的表名（迁移前置检查）。
func TableNonEmpty(st Store) ([]string, error) {
	s, ok := asSQLStore(st)
	if !ok {
		return nil, fmt.Errorf("store: TableNonEmpty requires sqlStore")
	}
	var out []string
	for _, t := range tableNames {
		var c int64
		if err := s.db.Table(t).Count(&c).Error; err != nil {
			return out, fmt.Errorf("count %s: %w", t, mapErr(err))
		}
		if c > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// TableCounts 返回各表行数（状态卡展示用）。
func TableCounts(st Store) (map[string]int64, error) {
	s, ok := asSQLStore(st)
	if !ok {
		return nil, fmt.Errorf("store: TableCounts requires sqlStore")
	}
	out := map[string]int64{}
	for _, t := range tableNames {
		var c int64
		if err := s.db.Table(t).Count(&c).Error; err != nil {
			return out, fmt.Errorf("count %s: %w", t, mapErr(err))
		}
		out[t] = c
	}
	return out, nil
}
