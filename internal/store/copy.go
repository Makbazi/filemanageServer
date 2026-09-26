// copy.go：跨驱动/跨库全表元数据搬迁（0.7.0 管理台热切换可选"迁移现有数据"；
// 0.1.1 起两端可为文件存储——file→db 迁移与 db→file 导出共用本通道）。
//
// 方式：按表 keyset 分批（按主键升序，每批 ≤1000）从源端读出（复用 rows.go 的 13 个
// 行模型为传输载体，字段编码由驱动层负责），原样保留主键写入目标端；目标须为**空**
// （connect/export 前置检查 TableNonEmpty，非空且未 force 时 API 层返回 409，本层不做强判）。
// postgresql 目标额外重置各 serial 序列（显式主键插入不推进 SERIAL）；sqlite/mysql
// 自增随插入自动前移；file 目标把 nextID 抬升到 max(已见 PK)+1（id 永不复用）。
//
// 0.1.1 泛化：CopyAllTables/TableNonEmpty/TableCounts 经内部 rowSource/rowSink 接口
// 分派 *sqlStore 与 *fileStore 两端（file↔db/db↔db/file→file 全覆盖）。
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

// asFileStore 解包到 *fileStore（Proxy 取其当前委托）。
func asFileStore(st Store) (*fileStore, bool) {
	if p, ok := st.(*Proxy); ok {
		st = p.Current()
	}
	s, ok := st.(*fileStore)
	return s, ok
}

// rowSource/rowSink：13 行模型的批量读/写通道（两端各自实现，见 sqlstore.go 尾部
// 与 filecopy.go）。
type rowSource interface {
	copyScan(table string, fn func(batch any) error) error
}

type rowSink interface {
	copyWrite(table string, batch any) error
	copyFinish() error // 序列/水位收尾（pg seqReset / file nextID 已在 copyWrite 抬升）
}

func asRowSource(st Store) (rowSource, bool) {
	if s, ok := asSQLStore(st); ok {
		return s, true
	}
	if f, ok := asFileStore(st); ok {
		return f, true
	}
	return nil, false
}

func asRowSink(st Store) (rowSink, bool) {
	if s, ok := asSQLStore(st); ok {
		return s, true
	}
	if f, ok := asFileStore(st); ok {
		return f, true
	}
	return nil, false
}

// CopyAllTables 把 src（可为 Proxy，取其当前委托）的全部表数据复制到 dst，
// 返回每表行数。两端支持 sqlStore（三驱动）与 fileStore（0.1.1）任意组合。
func CopyAllTables(dst, src Store) (map[string]int64, error) {
	s, ok1 := asRowSource(src)
	d, ok2 := asRowSink(dst)
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("store: copy requires sqlStore/fileStore endpoints")
	}
	out := map[string]int64{}
	for _, t := range tableNames {
		var n int64
		err := s.copyScan(t, func(batch any) error {
			if err := d.copyWrite(t, batch); err != nil {
				return err
			}
			n += batchSizeOf(batch)
			return nil
		})
		if err != nil {
			return out, fmt.Errorf("%s: %w", t, err)
		}
		out[t] = n
	}
	if err := d.copyFinish(); err != nil {
		return out, fmt.Errorf("finish: %w", err)
	}
	return out, nil
}

// batchSizeOf 批次行数（any 载体回推长度）。
func batchSizeOf(batch any) int64 {
	switch b := batch.(type) {
	case []userRow:
		return int64(len(b))
	case []userRootRow:
		return int64(len(b))
	case []fileMetaRow:
		return int64(len(b))
	case []aiDescRow:
		return int64(len(b))
	case []trashRow:
		return int64(len(b))
	case []groupRow:
		return int64(len(b))
	case []aclRow:
		return int64(len(b))
	case []shareRow:
		return int64(len(b))
	case []versionRow:
		return int64(len(b))
	case []sessionRow:
		return int64(len(b))
	case []uploadRow:
		return int64(len(b))
	case []metaKVRow:
		return int64(len(b))
	case []auditRow:
		return int64(len(b))
	}
	return 0
}

// tableNames 全表清单（非空检查与状态展示共用；序 1..13 = filelog 表序）。
var tableNames = []string{"users", "user_roots", "file_metas", "ai_descriptions",
	"trash_items", "groups", "folder_acls", "share_links", "file_versions",
	"sessions", "upload_sessions", "server_meta", "audit_events"}

// TableNonEmpty 返回 st 中已有数据的表名（迁移前置检查）。
func TableNonEmpty(st Store) ([]string, error) {
	c, err := TableCounts(st)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tableNames {
		if c[t] > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// TableCounts 返回各表行数（状态卡展示用；0.1.1 起支持文件存储）。
func TableCounts(st Store) (map[string]int64, error) {
	if p, ok := st.(*Proxy); ok {
		st = p.Current()
	}
	switch v := st.(type) {
	case *sqlStore:
		out := map[string]int64{}
		for _, t := range tableNames {
			var c int64
			if err := v.db.Table(t).Count(&c).Error; err != nil {
				return out, fmt.Errorf("count %s: %w", t, mapErr(err))
			}
			out[t] = c
		}
		return out, nil
	case *fileStore:
		v.mu.RLock()
		defer v.mu.RUnlock()
		return v.idx.counts(), nil
	}
	return nil, fmt.Errorf("store: TableCounts requires sqlStore or fileStore")
}
