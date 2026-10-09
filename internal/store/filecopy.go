// filecopy.go：文件存储的全表迁移通道（0.1.1，实现 copy.go 的 rowSource/rowSink）。
// file→db（迁移）与 db→file（导出/切换携带）共用；载体同为 rows.go 的 14 个行模型，
// 主键原样保留，目标端 nextID 抬升到 max(已见 PK)+1（id 永不复用）。
package store

import (
	"encoding/json"
	"fmt"
	"sort"
)

// tableOrdinal 表名 → 帧内表序（1..14，与 tableNames/copy.go 对齐）。
func tableOrdinal(name string) (byte, error) {
	for i, t := range tableNames {
		if t == name {
			return byte(i + 1), nil
		}
	}
	return 0, fmt.Errorf("copy: unknown table %q", name)
}

func batched[T any](vals []T, emit func([]T) error) error {
	for i := 0; i < len(vals); i += copyBatchSize {
		e := i + copyBatchSize
		if e > len(vals) {
			e = len(vals)
		}
		if err := emit(vals[i:e]); err != nil {
			return err
		}
	}
	return nil
}

func sortedValsInt[T any](m map[int64]*T) []T {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, *m[id])
	}
	return out
}

func sortedValsStr[T any](m map[string]*T) []T {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]T, 0, len(keys))
	for _, k := range keys {
		out = append(out, *m[k])
	}
	return out
}

func (f *fileStore) copyScan(table string, fn func(batch any) error) error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	switch table {
	case "users":
		return batched(sortedValsInt(f.idx.users), func(b []userRow) error { return fn(b) })
	case "user_roots":
		return batched(sortedValsInt(f.idx.userRoots), func(b []userRootRow) error { return fn(b) })
	case "file_metas":
		return batched(sortedValsInt(f.idx.fileMetas), func(b []fileMetaRow) error { return fn(b) })
	case "ai_descriptions":
		return batched(sortedValsInt(f.idx.aiDescs), func(b []aiDescRow) error { return fn(b) })
	case "trash_items":
		return batched(sortedValsInt(f.idx.trash), func(b []trashRow) error { return fn(b) })
	case "groups":
		return batched(sortedValsInt(f.idx.groups), func(b []groupRow) error { return fn(b) })
	case "folder_acls":
		return batched(sortedValsInt(f.idx.acls), func(b []aclRow) error { return fn(b) })
	case "share_links":
		return batched(sortedValsInt(f.idx.shares), func(b []shareRow) error { return fn(b) })
	case "file_versions":
		return batched(sortedValsInt(f.idx.versions), func(b []versionRow) error { return fn(b) })
	case "sessions":
		return batched(sortedValsInt(f.idx.sessions), func(b []sessionRow) error { return fn(b) })
	case "upload_sessions":
		return batched(sortedValsStr(f.idx.uploads), func(b []uploadRow) error { return fn(b) })
	case "server_meta":
		mvs := make(map[string]*metaKVRow, len(f.idx.metas))
		for k, v := range f.idx.metas {
			mvs[k] = &metaKVRow{Mkey: k, Value: v}
		}
		return batched(sortedValsStr(mvs), func(b []metaKVRow) error { return fn(b) })
	case "audit_events":
		return batched(sortedValsInt(f.idx.audits), func(b []auditRow) error { return fn(b) })
	case "auth_sessions":
		return batched(sortedValsInt(f.idx.authSessions), func(b []authSessionRow) error { return fn(b) })
	}
	return fmt.Errorf("copy: unknown table %q", table)
}

func (f *fileStore) copyWrite(table string, batch any) error {
	ord, err := tableOrdinal(table)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	return copyEach(batch, func(row any) error {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		// 主键与唯一键校验/水位抬升复用索引单一事实源（撞键 = 目标非空，上层 409 已拦截，
		// 真撞到时 ErrExists 中止迁移，旧引擎不受影响）
		if err := f.idx.put(ord, b, &f.nextID); err != nil {
			return err
		}
		if err := f.appendLocked(opPut(ord), b); err != nil {
			return err
		}
		f.appends++
		return nil
	})
}

func (f *fileStore) copyFinish() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	return f.syncNowLocked() // 迁移批次整体落盘后才宣告完成
}

// copyEach 行模型批次逐行回调（类型分发与 sqlStore.copyWrite 对齐）。
func copyEach(batch any, fn func(any) error) error {
	switch b := batch.(type) {
	case []userRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []userRootRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []fileMetaRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []aiDescRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []trashRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []groupRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []aclRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []shareRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []versionRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []sessionRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []uploadRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []metaKVRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []auditRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	case []authSessionRow:
		for i := range b {
			if err := fn(b[i]); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("copy: batch type mismatch")
	}
	return nil
}

var (
	_ rowSource = (*fileStore)(nil)
	_ rowSink   = (*fileStore)(nil)
)
