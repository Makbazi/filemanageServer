// filestore.go：文件元数据存储驱动（0.1.1）——生命周期、写路径与用户/分配/meta/
// 文件元数据/AI 描述各表。帧格式与恢复见 filelog.go；索引见 fileindex.go；
// 其余表方法在 filequery.go；压缩在 filecomp.go。
//
// 并发模型：单一 sync.RWMutex——读 RLock（纯内存索引），写 Lock（改索引 + 追加帧）。
// 多记录事务（DeleteUser 级联、SetUserRoots 替换、RenameFileMeta）在**一次写锁**内
// 完成全部追加：flusher 也在写锁内刷缓冲，故任何一批要么整体落盘、要么整体不在，
// 对齐 SQL 事务语义。刷盘策略：后台 50ms 批量写穿页缓存；安全关键操作（账户/会话/
// SetMeta）返回前同步 fsync；Close 必 fsync。段 8MiB 滚动，追加/墓碑超阈值触发快照压缩。
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type fileStore struct {
	mu     sync.RWMutex
	dir    string
	lockf  *os.File // flock 持有者（进程生命周期）
	idx    *fileIndex
	nextID [tCount + 1]int64

	man      fsManifest
	seg      *os.File
	segW     *bufio.Writer
	segBytes int64
	appends  int64 // 快照以来 Put/Del 帧数
	tombs    int64 // 快照以来 Del 帧数
	dirty    bool

	stopCh chan struct{}
	doneCh chan struct{}
	closed bool
}

var _ Store = (*fileStore)(nil)

// errClosed 存储已关闭（热切换后旧实例拒写）。
var errClosed = errors.New("filestore: store closed")

func newFileStore(dir string) (Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("store: driver %q requires a dsn (filestore directory)", DriverFile)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filestore: mkdir %s: %w", dir, err)
	}
	lockf, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("filestore: open lock: %w", err)
	}
	if err = lockStoreFile(lockf.Fd()); err != nil {
		lockf.Close()
		return nil, fmt.Errorf("filestore: 目录 %s 已被另一实例锁定（文件存储仅支持单实例；"+
			"多实例部署请配置 sqlite3/mysql/postgresql）: %w", dir, err)
	}
	idx, nextID, man, err := fsReplay(dir)
	if err != nil {
		lockStoreUnlock(lockf)
		return nil, err
	}
	fsCleanupOrphans(dir, man) // 压缩/滚动中途崩溃的未引用文件
	f := &fileStore{
		dir: dir, lockf: lockf, idx: idx, nextID: nextID, man: man,
		stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
	// 活动段：清单末项；全新存储则开首段并落清单
	if len(f.man.Segments) == 0 {
		name := fsFileName("seg", f.man.Seq)
		f.man.Seq++
		f.man.Segments = []string{name}
	}
	if err = f.openSegmentLocked(f.man.Segments[len(f.man.Segments)-1]); err != nil {
		lockStoreUnlock(lockf)
		return nil, err
	}
	if err = writeManifestFile(dir, f.man); err != nil {
		f.seg.Close()
		lockStoreUnlock(lockf)
		return nil, err
	}
	go f.flushLoop()
	return f, nil
}

func lockStoreUnlock(f *os.File) {
	unlockStoreFile(f.Fd())
	f.Close()
}

// FileStoreInfo 文件存储对外身份视图（管理端点判断"当前/目标是否文件存储、同目录"）。
type FileStoreInfo interface {
	Dir() string
}

// IsFileStore 解包 Proxy 判断委托是否文件存储（0.1.1 管理端点用；
// 调用方传入的恒为 DBProxy.Current()——cache 装饰器包在 Proxy 之上，不经此处）。
func IsFileStore(st Store) (FileStoreInfo, bool) {
	if p, ok := st.(*Proxy); ok {
		st = p.Current()
	}
	f, ok := st.(*fileStore)
	if !ok {
		return nil, false
	}
	return f, true
}

func (f *fileStore) Dir() string { return f.dir }

// ClearFileStore 删除目录内全部数据文件（MANIFEST*/段/快照；LOCK 保留）。
// 0.1.1 导出 force 覆盖前的清空（调用方须保证该目录无活动实例持有 flock）。
func ClearFileStore(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "LOCK" {
			continue
		}
		if matched, _ := filepath.Match("*.log", name); matched {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(name, "MANIFEST") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------- 生命周期 ----------

func (f *fileStore) Driver() string { return DriverFile }

func (f *fileStore) Ping() error {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return errClosed
	}
	return nil
}

// Migrate 幂等：目录/清单已在 Open 建立，无 schema 概念。
func (f *fileStore) Migrate() error { return nil }

func (f *fileStore) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()

	close(f.stopCh)
	<-f.doneCh // flusher 已退出（其循环持写锁），可独占写锁

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seg == nil {
		return nil
	}
	if b, err := json.Marshal(f.nextID); err == nil { // 自增水位尾帧（永不回退）
		_ = f.appendLocked(opMaxID, b)
	}
	serr := f.syncNowLocked()
	f.man.Closed = true
	if werr := writeManifestFile(f.dir, f.man); werr != nil && serr == nil {
		serr = werr
	}
	cerr := f.seg.Close()
	f.seg = nil
	lockStoreUnlock(f.lockf)
	if serr == nil {
		serr = cerr
	}
	return serr
}

// ---------- 写路径 ----------

// openSegmentLocked 打开/新建活动段（含文件头）。
func (f *fileStore) openSegmentLocked(name string) error {
	p := filepath.Join(f.dir, name)
	existed := true
	if _, err := os.Stat(p); err != nil {
		existed = false
	}
	fl, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.seg = fl
	f.segW = bufio.NewWriterSize(fl, 64<<10)
	f.segBytes = 0
	if !existed {
		if err = writeFileHeader(f.segW, fkSegment); err != nil {
			return err
		}
		f.segBytes = fileHeaderSize
	} else {
		st, serr := fl.Stat()
		if serr == nil {
			f.segBytes = st.Size()
		}
	}
	return nil
}

// appendLocked 追加一帧（写锁内）。段达阈值滚动新段。
func (f *fileStore) appendLocked(op byte, payload []byte) error {
	if f.closed || f.segW == nil {
		return errClosed
	}
	if len(payload) > maxFramePayload {
		return fmt.Errorf("filestore: record too large (%d bytes)", len(payload))
	}
	frame := encodeFrame(op, payload)
	n, err := f.segW.Write(frame)
	f.segBytes += int64(n)
	f.dirty = true
	if err != nil {
		log.Printf("filestore: 段写入失败（存储可能已满/被拔盘）: %v", err)
		return err
	}
	if f.segBytes >= segRollBytes {
		return f.rollSegmentLocked()
	}
	return nil
}

func (f *fileStore) syncNowLocked() error {
	if f.segW == nil {
		return nil
	}
	if err := f.segW.Flush(); err != nil {
		return err
	}
	f.dirty = false
	return f.seg.Sync()
}

// rollSegmentLocked 滚动活动段（同步旧段、开新段、换清单）。
func (f *fileStore) rollSegmentLocked() error {
	if err := f.syncNowLocked(); err != nil {
		return err
	}
	if err := f.seg.Close(); err != nil {
		return err
	}
	name := fsFileName("seg", f.man.Seq)
	f.man.Seq++
	f.man.Segments = append(f.man.Segments, name)
	if err := writeManifestFile(f.dir, f.man); err != nil {
		return err
	}
	f.seg = nil
	f.segW = nil
	return f.openSegmentLocked(name)
}

// putLocked/delLocked：索引 + 日志原语（写锁内）。索引维护与 replay/copy 共用
// fileIndex.put/del（单一事实源）。先查冲突（put 内）再写日志；日志写失败会打错误日志。
func (f *fileStore) putLocked(t byte, row any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if err = f.idx.put(t, b, &f.nextID); err != nil {
		return err
	}
	if err = f.appendLocked(opPut(t), b); err != nil {
		return err
	}
	f.appends++
	return nil
}

func (f *fileStore) delLocked(t byte, id int64) error {
	return f.delKeyLocked(t, encodeKey(id))
}

func (f *fileStore) delKeyLocked(t byte, key []byte) error {
	if err := f.idx.del(t, key); err != nil {
		return err
	}
	if err := f.appendLocked(opDel(t), key); err != nil {
		return err
	}
	f.tombs++
	return nil
}

func (f *fileStore) allocID(t byte) int64 {
	id := f.nextID[t]
	if id < 1 {
		id = 1 // users 等整型主键从 1 起（SuperAdminID=0 哨兵不占用表空间）
	}
	f.nextID[t] = id + 1
	return id
}

// flushLoop 后台刷盘 + 压缩触发（需求：文件刷写策略）。
func (f *fileStore) flushLoop() {
	defer close(f.doneCh)
	tick := time.NewTicker(time.Duration(flushEveryMs) * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-tick.C:
			f.mu.Lock()
			if !f.closed && f.dirty && f.segW != nil {
				if err := f.segW.Flush(); err != nil {
					log.Printf("filestore: 刷盘失败: %v", err)
				} else {
					f.dirty = false
				}
			}
			if !f.closed {
				f.maybeCompactLocked()
			}
			f.mu.Unlock()
		}
	}
}

// ---------- 通用小工具（本文件与 filequery.go 共用） ----------

func closedErr() error { return errClosed }

// pathInSubtree SQL path LIKE from+"/%" 的内存等价（含自身）。
func pathInSubtree(path, from string) bool {
	return path == from || strings.HasPrefix(path, from+"/")
}

// ---------- users ----------

func (f *fileStore) CreateUser(u *User) error {
	if u.CreatedAt.IsZero() {
		u.CreatedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := userRow{}.fromUser(u)
	r.ID = f.allocID(tUsers)
	if err := f.putLocked(tUsers, r); err != nil {
		return err
	}
	u.ID = r.ID
	return f.syncNowLocked() // 关键：账户落盘后才返回
}

func (f *fileStore) GetUserByName(username string) (*User, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.usersByName[username]; ok {
		return r.toUser(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) GetUserByID(id int64) (*User, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.users[id]; ok {
		return r.toUser(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListUsers() ([]*User, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ids := make([]int64, 0, len(f.idx.users))
	for id := range f.idx.users {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*User, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.idx.users[id].toUser())
	}
	return out, nil
}

func (f *fileStore) UpdateUserPassword(id int64, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.users[id]
	if !ok {
		return ErrNotFound
	}
	r.PasswordHash = hash
	if err := f.putLocked(tUsers, *r); err != nil {
		return err
	}
	return f.syncNowLocked()
}

func (f *fileStore) UpdateUserRoots(id int64, roots []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.users[id]
	if !ok {
		return ErrNotFound
	}
	r.AllowedRoots = userRow{}.fromUser(&User{AllowedRoots: roots}).AllowedRoots
	return f.putLocked(tUsers, *r)
}

func (f *fileStore) UpdateUserRole(id int64, role string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.users[id]
	if !ok {
		return ErrNotFound
	}
	r.Role = role
	if err := f.putLocked(tUsers, *r); err != nil {
		return err
	}
	return f.syncNowLocked()
}

// DeleteUser 级联清理（与 sqlStore 相同顺序，一次写锁内全部追加 = 原子批次；
// 目标用户不存在时静默成功——SQL 批量 DELETE 语义）。
func (f *fileStore) DeleteUser(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var metaIDs, descIDs, trashIDs, sessIDs, rootIDs, aclIDs, authIDs []int64
	var uploadIDs []string
	for _, r := range f.idx.fileMetas {
		if r.UserID == id {
			metaIDs = append(metaIDs, r.ID)
		}
	}
	for _, r := range f.idx.aiDescs {
		if r.UserID == id {
			descIDs = append(descIDs, r.ID)
		}
	}
	for _, r := range f.idx.trash {
		if r.UserID == id {
			trashIDs = append(trashIDs, r.ID)
		}
	}
	for _, r := range f.idx.sessions {
		if r.UserID == id {
			sessIDs = append(sessIDs, r.ID)
		}
	}
	for _, r := range f.idx.uploads {
		if r.UserID == id {
			uploadIDs = append(uploadIDs, r.ID)
		}
	}
	for _, r := range f.idx.userRoots {
		if r.UserID == id {
			rootIDs = append(rootIDs, r.ID)
		}
	}
	for _, r := range f.idx.acls {
		if r.SubjectType == SubjectUser && r.SubjectID == id {
			aclIDs = append(aclIDs, r.ID)
		}
	}
	for _, r := range f.idx.authSessions {
		if r.UserID == id {
			authIDs = append(authIDs, r.ID)
		}
	}
	for _, x := range metaIDs {
		if err := f.delLocked(tFileMetas, x); err != nil {
			return err
		}
	}
	for _, x := range descIDs {
		if err := f.delLocked(tAIDescs, x); err != nil {
			return err
		}
	}
	for _, x := range trashIDs {
		if err := f.delLocked(tTrash, x); err != nil {
			return err
		}
	}
	for _, x := range sessIDs {
		if err := f.delLocked(tSessions, x); err != nil {
			return err
		}
	}
	for _, x := range authIDs {
		if err := f.delLocked(tAuthSessions, x); err != nil {
			return err
		}
	}
	for _, x := range uploadIDs {
		if err := f.delKeyLocked(tUploadSessions, []byte(x)); err != nil {
			return err
		}
	}
	for _, x := range rootIDs {
		if err := f.delLocked(tUserRoots, x); err != nil {
			return err
		}
	}
	for _, x := range aclIDs {
		if err := f.delLocked(tFolderACLs, x); err != nil {
			return err
		}
	}
	if _, ok := f.idx.users[id]; ok {
		if err := f.delLocked(tUsers, id); err != nil {
			return err
		}
	}
	return f.syncNowLocked()
}

func (f *fileStore) CountUsers() (int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return int64(len(f.idx.users)), nil
}

// ---------- user roots ----------

func (f *fileStore) ListUserRoots(userID int64) ([]*UserRoot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*userRootRow
	for _, r := range f.idx.userRoots {
		if r.UserID == userID {
			rows = append(rows, r)
		}
	}
	sortUserRoots(rows)
	out := make([]*UserRoot, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func (f *fileStore) ListAllUserRoots() ([]*UserRoot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	rows := make([]*userRootRow, 0, len(f.idx.userRoots))
	for _, r := range f.idx.userRoots {
		rows = append(rows, r)
	}
	sortUserRoots(rows)
	out := make([]*UserRoot, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func sortUserRoots(rows []*userRootRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.RootID != b.RootID {
			return a.RootID < b.RootID
		}
		return a.SubPath < b.SubPath
	})
}

// SetUserRoots 全量替换（一次写锁 = 单事务；输入内 (root,sub) 重复 → ErrExists）。
func (f *fileStore) SetUserRoots(userID int64, list []*UserRoot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var old []int64
	seen := map[string]bool{}
	var news []*userRootRow
	for _, r := range list {
		if r.RootID == "" {
			continue
		}
		k := nsep(strconv.FormatInt(userID, 10), r.RootID, r.SubPath)
		if seen[k] {
			return dupErr("user_roots(user_id,root_id,sub_path)")
		}
		seen[k] = true
		news = append(news, &userRootRow{UserID: userID, RootID: r.RootID, SubPath: r.SubPath, Alias: r.Alias})
	}
	for _, r := range f.idx.userRoots {
		if r.UserID == userID {
			old = append(old, r.ID)
		}
	}
	for _, x := range old {
		if err := f.delLocked(tUserRoots, x); err != nil {
			return err
		}
	}
	for _, r := range news {
		r.ID = f.allocID(tUserRoots)
		if err := f.putLocked(tUserRoots, *r); err != nil {
			return err
		}
	}
	return nil
}

// ---------- server_meta ----------

func (f *fileStore) GetMeta(key string) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.idx.metas[key], nil
}

func (f *fileStore) SetMeta(key, value string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if err := f.putLocked(tServerMeta, metaKVRow{Mkey: key, Value: value}); err != nil {
		return err
	}
	return f.syncNowLocked() // 一次性迁移标记必须持久
}

// ---------- file metas ----------

// PutFileMeta upsert 元数据。content_hash 保留语义与 SQL 侧完全一致（0.2.1，
// 见 sqlstore.go 同名方法注释）：incoming 哈希为空且旧行 size/mtime（秒）未变
// → 保留旧 content_hash，例行刷行不得清空已存哈希。
func (f *fileStore) PutFileMeta(m *FileMeta) error {
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := fileMetaRow{}.fromMeta(m)
	// upsert：唯一键命中 → 保留旧行 ID（SQL ON CONFLICT 语义，m.ID 不回填）
	if e, ok := f.idx.fileMetasBy[metaKeyOf(r.UserID, r.RootID, r.Path)]; ok {
		r.ID = e.ID
		if r.ContentHash == "" && e.ContentHash != "" &&
			e.Size == r.Size && e.Mtime.Unix() == r.Mtime.Unix() {
			r.ContentHash = e.ContentHash
		}
	} else {
		r.ID = f.allocID(tFileMetas)
	}
	return f.putLocked(tFileMetas, r)
}

func (f *fileStore) GetFileMeta(userID int64, rootID, path string) (*FileMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.fileMetasBy[metaKeyOf(userID, rootID, path)]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListFileMetas(userID int64, rootID string) ([]*FileMeta, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	rows := make([]*fileMetaRow, 0)
	for _, r := range f.idx.fileMetas {
		if r.UserID == userID && (rootID == "" || r.RootID == rootID) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	out := make([]*FileMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func (f *fileStore) DeleteFileMeta(userID int64, rootID, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.fileMetasBy[metaKeyOf(userID, rootID, path)]
	if !ok {
		return ErrNotFound
	}
	return f.delLocked(tFileMetas, r.ID)
}

// RenameFileMeta 与 SQL 同语义：命中 path==from OR path LIKE from+"/%" 的行，
// 全部置 path=to（多子行同目标 = SQL 必撞唯一键 → ErrExists）。
func (f *fileStore) RenameFileMeta(userID int64, rootID, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var hits []*fileMetaRow
	for _, r := range f.idx.fileMetas {
		if r.UserID == userID && r.RootID == rootID && pathInSubtree(r.Path, from) {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return ErrNotFound
	}
	if len(hits) > 1 {
		return dupErr("file_metas(user_id,root_id,path)")
	}
	target := metaKeyOf(userID, rootID, to)
	if e, ok := f.idx.fileMetasBy[target]; ok && e.ID != hits[0].ID {
		return dupErr("file_metas(user_id,root_id,path)")
	}
	r := hits[0]
	oldKey := metaKeyOf(r.UserID, r.RootID, r.Path)
	newPath, now := to, time.Now()
	// 先摘旧二级键（字段可变更新不走通用 put 的"旧键"路径），改字段后重挂
	if f.idx.fileMetasBy[oldKey] == r {
		delete(f.idx.fileMetasBy, oldKey)
	}
	r.Path = newPath
	r.UpdatedAt = now
	f.idx.fileMetasBy[target] = r
	b, err := json.Marshal(*r)
	if err != nil {
		return err
	}
	if err = f.appendLocked(opPut(tFileMetas), b); err != nil {
		return err
	}
	f.appends++
	return nil
}

// ---------- AI 描述 ----------

func (f *fileStore) SaveAIDescription(d *AIDescription) error {
	now := time.Now()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := aiDescRow{}.fromDesc(d)
	k := descKeyOf(r.UserID, r.RootID, r.Path, r.Kind)
	if e, ok := f.idx.aiDescsBy[k]; ok {
		r.ID, r.CreatedAt = e.ID, e.CreatedAt // ON CONFLICT：保留原 id/created_at
	} else {
		r.ID = f.allocID(tAIDescs)
	}
	if err := f.putLocked(tAIDescs, r); err != nil {
		return err
	}
	d.ID = r.ID
	return nil
}

func (f *fileStore) GetAIDescription(userID int64, rootID, path, kind string) (*AIDescription, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.aiDescsBy[descKeyOf(userID, rootID, path, kind)]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListAIDescriptions(userID int64, rootID string) ([]*AIDescription, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*aiDescRow
	for _, r := range f.idx.aiDescs {
		if r.UserID == userID && (rootID == "" || r.RootID == rootID) {
			rows = append(rows, r)
		}
	}
	sortDescsByUpdated(rows)
	if len(rows) > 1000 {
		rows = rows[:1000]
	}
	out := make([]*AIDescription, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func sortDescsByUpdated(rows []*aiDescRow) {
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
			return rows[i].UpdatedAt.After(rows[j].UpdatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
}

func (f *fileStore) DeleteAIDescription(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.aiDescs[id]; !ok {
		return ErrNotFound
	}
	return f.delLocked(tAIDescs, id)
}

func (f *fileStore) SearchDescriptions(userID int64, text string, limit int) ([]*SearchHit, error) {
	if limit <= 0 {
		limit = 50
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*aiDescRow
	for _, r := range f.idx.aiDescs {
		if r.UserID == userID && (strings.Contains(r.Text, text) || strings.Contains(r.Name, text)) {
			rows = append(rows, r)
		}
	}
	sortDescsByUpdated(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]*SearchHit, 0, len(rows))
	for _, r := range rows {
		out = append(out, &SearchHit{AIDescription: *r.to(), Score: 1})
	}
	return out, nil
}
