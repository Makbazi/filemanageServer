// filequery.go：文件存储 Store 方法（回收站/用户组/ACL/分享/版本/会话/上传会话/审计）。
// 语义与 sqlstore.go 逐方法对齐（排序、ErrNotFound/ErrExists、批量删除静默、SET 列清单）。
package store

import (
	"encoding/json"
	"sort"
	"time"
)

func aclPtrList(rows []*aclRow) []*FolderACL {
	out := make([]*FolderACL, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out
}

// ---------- 回收站 ----------

func (f *fileStore) AddTrashItem(t *TrashItem) error {
	if t.DeletedAt.IsZero() {
		t.DeletedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := trashRow{}.fromTrash(t)
	r.ID = f.allocID(tTrash)
	if err := f.putLocked(tTrash, r); err != nil {
		return err
	}
	t.ID = r.ID
	return nil
}

func (f *fileStore) ListTrashItems(userID int64, rootID string) ([]*TrashItem, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*trashRow
	for _, r := range f.idx.trash {
		if r.UserID == userID && (rootID == "" || r.RootID == rootID) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].DeletedAt.Equal(rows[j].DeletedAt) {
			return rows[i].DeletedAt.After(rows[j].DeletedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	out := make([]*TrashItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

// TakeTrashItem 原子取删（一次写锁内完成，杜绝并发双还原）。
func (f *fileStore) TakeTrashItem(userID int64, trashID string) (*TrashItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, closedErr()
	}
	r, ok := f.idx.trashByID[trashID]
	if !ok || r.UserID != userID {
		return nil, ErrNotFound
	}
	item := r.to()
	if err := f.delLocked(tTrash, r.ID); err != nil {
		return nil, err
	}
	return item, nil
}

func (f *fileStore) RemoveTrashItem(userID int64, trashID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.trashByID[trashID]
	if !ok || r.UserID != userID {
		return ErrNotFound
	}
	return f.delLocked(tTrash, r.ID)
}

func (f *fileStore) ClearTrash(userID int64, rootID string) ([]*TrashItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, closedErr()
	}
	var rows []*trashRow
	for _, r := range f.idx.trash {
		if r.UserID == userID && (rootID == "" || r.RootID == rootID) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].DeletedAt.Equal(rows[j].DeletedAt) {
			return rows[i].DeletedAt.After(rows[j].DeletedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	out := make([]*TrashItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	for _, r := range rows {
		if err := f.delLocked(tTrash, r.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (f *fileStore) ListTrashBefore(before time.Time) ([]*TrashItem, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*trashRow
	for _, r := range f.idx.trash {
		if r.DeletedAt.Before(before) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].DeletedAt.Equal(rows[j].DeletedAt) {
			return rows[i].DeletedAt.Before(rows[j].DeletedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	out := make([]*TrashItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

// ---------- 用户组 ----------

func (f *fileStore) CreateGroup(g *Group) error {
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := groupRow{}.fromGroup(g)
	r.ID = f.allocID(tGroups)
	if err := f.putLocked(tGroups, r); err != nil {
		return err
	}
	g.ID = r.ID
	return nil
}

func (f *fileStore) GetGroupByID(id int64) (*Group, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.groups[id]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) GetGroupByName(name string) (*Group, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.groupsByName[name]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListGroups() ([]*Group, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	ids := make([]int64, 0, len(f.idx.groups))
	for id := range f.idx.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*Group, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.idx.groups[id].to())
	}
	return out, nil
}

// UpdateGroup 改名会移动二级键（先摘旧名，重名撞其他行 → ErrExists）。
func (f *fileStore) UpdateGroup(g *Group) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.groups[g.ID]
	if !ok {
		return ErrNotFound
	}
	nr := groupRow{}.fromGroup(g)
	nr.ID = r.ID
	nr.CreatedAt = r.CreatedAt
	if e, dup := f.idx.groupsByName[g.Name]; dup && e.ID != r.ID {
		return dupErr("groups.name")
	}
	if f.idx.groupsByName[r.Name] == r {
		delete(f.idx.groupsByName, r.Name)
	}
	*r = nr
	f.idx.groups[r.ID] = r
	f.idx.groupsByName[r.Name] = r
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err = f.appendLocked(opPut(tGroups), b); err != nil {
		return err
	}
	f.appends++
	return nil
}

func (f *fileStore) DeleteGroup(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.groups[id]; !ok {
		return ErrNotFound
	}
	return f.delLocked(tGroups, id)
}

// ---------- 目录权限（ACL） ----------

func (f *fileStore) SetFolderACL(a *FolderACL) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := aclRow{}.fromACL(a)
	k := aclKeyOf(r.RootID, r.Path, r.SubjectType, r.SubjectID)
	if e, ok := f.idx.aclsBy[k]; ok {
		r.ID, r.CreatedAt = e.ID, e.CreatedAt // ON CONFLICT：SET 不含 created_at/id
	} else {
		r.ID = f.allocID(tFolderACLs)
	}
	if err := f.putLocked(tFolderACLs, r); err != nil {
		return err
	}
	if a.ID == 0 {
		a.ID = r.ID
	}
	return nil
}

func (f *fileStore) GetFolderACLs(rootID, path string) ([]*FolderACL, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*aclRow
	for _, r := range f.idx.acls {
		if r.RootID == rootID && r.Path == path {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return aclPtrList(rows), nil
}

func (f *fileStore) ListRootACLs(rootID string) ([]*FolderACL, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*aclRow
	for _, r := range f.idx.acls {
		if rootID == "" || r.RootID == rootID {
			rows = append(rows, r)
		}
	}
	sortACLs(rows)
	return aclPtrList(rows), nil
}

func sortACLs(rows []*aclRow) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.RootID != b.RootID {
			return a.RootID < b.RootID
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.ID < b.ID
	})
}

func (f *fileStore) ListPublicViewACLs() ([]*FolderACL, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*aclRow
	for _, r := range f.idx.acls {
		if r.SubjectType == SubjectPublic && r.CanView == 1 {
			rows = append(rows, r)
		}
	}
	sortACLs(rows)
	return aclPtrList(rows), nil
}

func (f *fileStore) DeleteFolderACL(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.acls[id]; !ok {
		return ErrNotFound
	}
	return f.delLocked(tFolderACLs, id)
}

func (f *fileStore) DeleteFolderACLsBySubject(subjectType string, subjectID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var ids []int64
	for _, r := range f.idx.acls {
		if r.SubjectType == subjectType && r.SubjectID == subjectID {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		if err := f.delLocked(tFolderACLs, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 分享链接 ----------

func (f *fileStore) CreateShareLink(l *ShareLink) error {
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	l.HasPassword = l.PasswordHash != ""
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := shareRow{}.fromShare(l)
	r.ID = f.allocID(tShareLinks)
	if err := f.putLocked(tShareLinks, r); err != nil {
		return err
	}
	l.ID = r.ID
	return nil
}

func (f *fileStore) GetShareLinkByToken(token string) (*ShareLink, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.sharesByToken[token]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListShareLinks(createdBy int64) ([]*ShareLink, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*shareRow
	for _, r := range f.idx.shares {
		if createdBy <= 0 || r.CreatedBy == createdBy {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	out := make([]*ShareLink, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func (f *fileStore) UpdateShareLink(l *ShareLink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.shares[l.ID]
	if !ok {
		return ErrNotFound
	}
	r.PasswordHash = l.PasswordHash
	r.ExpiresAt = l.ExpiresAt
	r.Disabled = b2i(l.Disabled)
	return f.putLocked(tShareLinks, *r)
}

func (f *fileStore) DeleteShareLink(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.shares[id]; !ok {
		return ErrNotFound
	}
	return f.delLocked(tShareLinks, id)
}

// ---------- 文件版本 ----------

// AddFileVersion 写锁内分配 max(version_no)+1（对齐 SQL 事务）。
func (f *fileStore) AddFileVersion(v *FileVersion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now()
	}
	if v.VersionNo == 0 {
		var maxNo int64
		for _, r := range f.idx.versions {
			if r.RootID == v.RootID && r.Path == v.Path && r.VersionNo > maxNo {
				maxNo = r.VersionNo
			}
		}
		v.VersionNo = maxNo + 1
	}
	if _, dup := f.idx.versionsBy[versionKeyOf(v.RootID, v.Path, v.VersionNo)]; dup {
		return dupErr("file_versions(root_id,path,version_no)")
	}
	r := versionRow{}.fromVersion(v)
	r.ID = f.allocID(tFileVersions)
	if err := f.putLocked(tFileVersions, r); err != nil {
		return err
	}
	v.ID = r.ID
	return nil
}

func (f *fileStore) GetFileVersion(id int64) (*FileVersion, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.versions[id]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) ListFileVersions(rootID, path string) ([]*FileVersion, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*versionRow
	for _, r := range f.idx.versions {
		if r.RootID == rootID && r.Path == path {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].VersionNo > rows[j].VersionNo })
	out := make([]*FileVersion, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func (f *fileStore) DeleteFileVersion(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.versions[id]; !ok {
		return ErrNotFound
	}
	return f.delLocked(tFileVersions, id)
}

func (f *fileStore) DeleteFileVersions(rootID, path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var ids []int64
	for _, r := range f.idx.versions {
		if r.RootID == rootID && pathInSubtree(r.Path, path) {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		if err := f.delLocked(tFileVersions, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 登录会话 ----------

func (f *fileStore) CreateSession(sess *Session) error {
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = now
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := sessionRow{}.fromSession(sess)
	r.ID = f.allocID(tSessions)
	if err := f.putLocked(tSessions, r); err != nil {
		return err
	}
	sess.ID = r.ID
	return f.syncNowLocked() // 吊销/发放须持久
}

func (f *fileStore) GetSessionByTokenHash(tokenHash string) (*Session, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.sessByHash[tokenHash]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) RenewSession(id int64, expiresAt, lastUsedAt time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.sessions[id]
	if !ok {
		return ErrNotFound
	}
	r.ExpiresAt, r.LastUsedAt = expiresAt, lastUsedAt
	return f.putLocked(tSessions, *r)
}

func (f *fileStore) DeleteSession(id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.sessions[id]; !ok {
		return ErrNotFound
	}
	if err := f.delLocked(tSessions, id); err != nil {
		return err
	}
	return f.syncNowLocked()
}

func (f *fileStore) DeleteSessionsByUser(userID int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var ids []int64
	for _, r := range f.idx.sessions {
		if r.UserID == userID {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		if err := f.delLocked(tSessions, id); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		return f.syncNowLocked() // 会话吊销（改密/登出）须持久
	}
	return nil
}

func (f *fileStore) DeleteExpiredSessions(now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var ids []int64
	for _, r := range f.idx.sessions {
		if r.ExpiresAt.Before(now) {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		if err := f.delLocked(tSessions, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 分片上传会话 ----------

func (f *fileStore) CreateUploadSession(u *UploadSession) error {
	now := time.Now()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	if u.Status == "" {
		u.Status = UploadStatusUploading
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, dup := f.idx.uploads[u.ID]; dup {
		return dupErr("upload_sessions.id")
	}
	return f.putLocked(tUploadSessions, uploadRow{}.fromUpload(u))
}

func (f *fileStore) GetUploadSession(id string) (*UploadSession, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.idx.uploads[id]; ok {
		return r.to(), nil
	}
	return nil, ErrNotFound
}

func (f *fileStore) GetActiveUploadSession(rootID, path string) (*UploadSession, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var best *uploadRow
	for _, r := range f.idx.uploads {
		if r.RootID == rootID && r.Path == path && r.Status == UploadStatusUploading {
			if best == nil || r.CreatedAt.After(best.CreatedAt) {
				best = r
			}
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best.to(), nil
}

func (f *fileStore) ListUploadSessions(userID int64) ([]*UploadSession, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	var rows []*uploadRow
	for _, r := range f.idx.uploads {
		if userID <= 0 || r.UserID == userID {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].CreatedAt.After(rows[j].CreatedAt)
		}
		return rows[i].ID > rows[j].ID
	})
	out := make([]*UploadSession, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.to())
	}
	return out, nil
}

func (f *fileStore) UpdateUploadSession(u *UploadSession) error {
	u.UpdatedAt = time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r, ok := f.idx.uploads[u.ID]
	if !ok {
		return ErrNotFound
	}
	r.Size, r.ContentHash, r.BaseHash = u.Size, u.ContentHash, u.BaseHash
	r.UploadedChunks = uploadRow{}.fromUpload(u).UploadedChunks
	r.Status, r.UpdatedAt = u.Status, u.UpdatedAt
	return f.putLocked(tUploadSessions, *r)
}

func (f *fileStore) DeleteUploadSession(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	if _, ok := f.idx.uploads[id]; !ok {
		return ErrNotFound
	}
	return f.delKeyLocked(tUploadSessions, []byte(id))
}

func (f *fileStore) DeleteUploadSessionsBefore(before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var keys []string
	for _, r := range f.idx.uploads {
		if r.CreatedAt.Before(before) && r.Status != UploadStatusUploading {
			keys = append(keys, r.ID)
		}
	}
	for _, k := range keys {
		if err := f.delKeyLocked(tUploadSessions, []byte(k)); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 审计日志 ----------

func (f *fileStore) AddAuditEvent(e *AuditEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	r := auditRow{}.fromAudit(e)
	r.ID = f.allocID(tAuditEvents)
	if err := f.putLocked(tAuditEvents, r); err != nil {
		return err
	}
	e.ID = r.ID
	return nil
}

func (f *fileStore) ListAuditEvents(limit int, userID int64) ([]*AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	ids := make([]int64, 0, len(f.idx.audits))
	for id, r := range f.idx.audits {
		if userID <= 0 || r.UserID == userID {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] })
	if len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]*AuditEvent, 0, len(ids))
	for _, id := range ids {
		out = append(out, f.idx.audits[id].to())
	}
	return out, nil
}

func (f *fileStore) DeleteAuditBefore(before time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return closedErr()
	}
	var ids []int64
	for _, r := range f.idx.audits {
		if r.CreatedAt.Before(before) {
			ids = append(ids, r.ID)
		}
	}
	for _, id := range ids {
		if err := f.delLocked(tAuditEvents, id); err != nil {
			return err
		}
	}
	return nil
}

