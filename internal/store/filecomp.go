// filecomp.go：文件存储快照压缩（0.1.1）——回收墓碑记录真正释放磁盘空间
// （审计 90 天清理/回收站清理产生的删除只追加墓碑，压缩才物理回收）。
//
// 流程（全程持写锁，读请求短暂等待——10 万行量级约几十 ms，单实例可接受）：
//  1. 同步并关闭旧段（其中记录已全部反映在内存索引）；
//  2. 写快照 snapshot-N.log（头帧 opMaxID 水位 + 全量活行 Put 帧，按主键序，tmp→fsync→rename）；
//  3. 开新空段 seg-M.log；
//  4. 原子换 MANIFEST = {snapshot-N, segments:[seg-M]}；
//  5. 删除一切未被引用的旧段/旧快照/孤儿 tmp。
package store

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
)

func (f *fileStore) maybeCompactLocked() {
	if f.appends > compactAppends ||
		(f.tombs > compactTombMin && f.tombs*4 > f.idx.liveRows()) {
		if err := f.compactLocked(); err != nil {
			log.Printf("filestore: 压缩失败（下次触发重试）: %v", err)
		}
	}
}

func (f *fileStore) compactLocked() error {
	// 1) 关旧段
	if err := f.syncNowLocked(); err != nil {
		return err
	}
	if err := f.seg.Close(); err != nil {
		return err
	}
	f.seg, f.segW = nil, nil

	// 2) 写快照
	snapName := fsFileName("snapshot", f.man.Seq)
	f.man.Seq++
	if err := f.writeSnapshotLocked(filepath.Join(f.dir, snapName+".tmp"), snapName); err != nil {
		os.Remove(filepath.Join(f.dir, snapName+".tmp"))
		return err
	}

	// 3) 新活动段
	segName := fsFileName("seg", f.man.Seq)
	f.man.Seq++

	// 4) 换清单（旧段/旧快照自此不再被引用）
	man := fsManifest{Snapshot: snapName, Segments: []string{segName}, Seq: f.man.Seq}
	f.man = man
	if err := writeManifestFile(f.dir, man); err != nil {
		return err
	}
	if err := f.openSegmentLocked(segName); err != nil {
		return err
	}

	// 5) 清理未引用文件
	keep := map[string]bool{snapName: true, segName: true, "MANIFEST": true, "MANIFEST.old": true, "LOCK": true}
	if entries, err := os.ReadDir(f.dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if keep[name] {
				continue
			}
			if matched, _ := filepath.Match("*.log*", name); matched {
				_ = os.Remove(filepath.Join(f.dir, name))
			}
		}
	} else {
		log.Printf("filestore: 压缩清理目录枚举失败: %v", err)
	}
	f.appends, f.tombs, f.dirty = 0, 0, false
	log.Printf("filestore: 压缩完成 → %s（活行 %d）", snapName, f.idx.liveRows())
	return nil
}

// writeSnapshotLocked 把全量活行写为快照帧文件（tmp 名传入，fsync+rename 到正式名）。
func (f *fileStore) writeSnapshotLocked(tmpPath, finalName string) error {
	fl, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(fl, 256<<10)
	if err = writeFileHeader(w, fkSnapshot); err != nil {
		fl.Close()
		return err
	}
	mx, err := json.Marshal(f.nextID)
	if err != nil {
		fl.Close()
		return err
	}
	if _, err = w.Write(encodeFrame(opMaxID, mx)); err != nil {
		fl.Close()
		return err
	}
	write := func(t byte, row any) error {
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		_, err = w.Write(encodeFrame(opPut(t), b))
		return err
	}
	// 各表按主键升序（文本 PK 表按字典序 = copy keyset 语义）
	if err = eachSorted(f.idx.users, func(r userRow) error { return write(tUsers, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.userRoots, func(r userRootRow) error { return write(tUserRoots, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.fileMetas, func(r fileMetaRow) error { return write(tFileMetas, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.aiDescs, func(r aiDescRow) error { return write(tAIDescs, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.trash, func(r trashRow) error { return write(tTrash, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.groups, func(r groupRow) error { return write(tGroups, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.acls, func(r aclRow) error { return write(tFolderACLs, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.shares, func(r shareRow) error { return write(tShareLinks, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.versions, func(r versionRow) error { return write(tFileVersions, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.sessions, func(r sessionRow) error { return write(tSessions, r) }); err != nil {
		fl.Close()
		return err
	}
	var upKeys []string
	for k := range f.idx.uploads {
		upKeys = append(upKeys, k)
	}
	sort.Strings(upKeys)
	for _, k := range upKeys {
		if err = write(tUploadSessions, *f.idx.uploads[k]); err != nil {
			fl.Close()
			return err
		}
	}
	var mKeys []string
	for k := range f.idx.metas {
		mKeys = append(mKeys, k)
	}
	sort.Strings(mKeys)
	for _, k := range mKeys {
		if err = write(tServerMeta, metaKVRow{Mkey: k, Value: f.idx.metas[k]}); err != nil {
			fl.Close()
			return err
		}
	}
	if err = eachSorted(f.idx.audits, func(r auditRow) error { return write(tAuditEvents, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = eachSorted(f.idx.authSessions, func(r authSessionRow) error { return write(tAuthSessions, r) }); err != nil {
		fl.Close()
		return err
	}
	if err = w.Flush(); err != nil {
		fl.Close()
		return err
	}
	if err = fl.Sync(); err != nil {
		fl.Close()
		return err
	}
	if err = fl.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpPath, filepath.Join(f.dir, finalName)); err != nil {
		return err
	}
	return fsyncDir(f.dir)
}

// eachSorted 按主键升序对 map 值执行 fn。
func eachSorted[T any](m map[int64]*T, fn func(T) error) error {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		if err := fn(*m[k]); err != nil {
			return err
		}
	}
	return nil
}

// fsCleanupOrphans 启动时清理未引用的 .log 文件（压缩/滚动中途崩溃的孤儿）。
func fsCleanupOrphans(dir string, man fsManifest) {
	referenced := map[string]bool{"MANIFEST": true, "MANIFEST.old": true, "MANIFEST.tmp": true, "LOCK": true}
	if man.Snapshot != "" {
		referenced[man.Snapshot] = true
	}
	for _, s := range man.Segments {
		referenced[s] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if referenced[name] {
			continue
		}
		if matched, _ := filepath.Match("snapshot-*.log", name); matched {
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		if matched, _ := filepath.Match("seg-*.log", name); matched {
			// 未引用的旧段（新清单生效后不应存在）——保守跳过当前活动段（调用方已确认引用）
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
	}
}
