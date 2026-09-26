// fileindex.go：文件存储的内存索引（0.1.1）。
//
// 全量活行常驻内存（每表 PK map + 与 SQL 唯一键一一对应的二级 map），所有查询走
// 内存扫描/排序——这正是需求的"索引加速"：等值查找 O(1)、列表读时排序、无磁盘扫描。
// Put/Del 的索引维护逻辑为回放（replayPut/replayDel）、运行期方法、copy 导入三条
// 路径共用（单一事实源）；唯一键冲突返回 ErrExists，与 sqlStore 契约一致。
//
// 规模参考：10 万条 file_metas 约 30–50MB 常驻；更大规模应配置数据库（管理台在线切换）。
package store

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type fileIndex struct {
	users       map[int64]*userRow
	usersByName map[string]*userRow

	userRoots map[int64]*userRootRow

	fileMetas   map[int64]*fileMetaRow
	fileMetasBy map[string]*fileMetaRow // user\x00root\x00path

	aiDescs   map[int64]*aiDescRow
	aiDescsBy map[string]*aiDescRow // user\x00root\x00path\x00kind

	trash     map[int64]*trashRow
	trashByID map[string]*trashRow // trash_id 全局唯一

	groups       map[int64]*groupRow
	groupsByName map[string]*groupRow

	acls   map[int64]*aclRow
	aclsBy map[string]*aclRow // root\x00path\x00subjectType\x00subjectID

	shares        map[int64]*shareRow
	sharesByToken map[string]*shareRow

	versions   map[int64]*versionRow
	versionsBy map[string]*versionRow // root\x00path\x00versionNo

	sessions   map[int64]*sessionRow
	sessByHash map[string]*sessionRow

	uploads map[string]*uploadRow

	metas map[string]string // server_meta

	audits map[int64]*auditRow
}

func newFileIndex() *fileIndex {
	return &fileIndex{
		users: map[int64]*userRow{}, usersByName: map[string]*userRow{},
		userRoots:   map[int64]*userRootRow{},
		fileMetas:   map[int64]*fileMetaRow{}, fileMetasBy: map[string]*fileMetaRow{},
		aiDescs:     map[int64]*aiDescRow{}, aiDescsBy: map[string]*aiDescRow{},
		trash:       map[int64]*trashRow{}, trashByID: map[string]*trashRow{},
		groups:      map[int64]*groupRow{}, groupsByName: map[string]*groupRow{},
		acls:        map[int64]*aclRow{}, aclsBy: map[string]*aclRow{},
		shares:      map[int64]*shareRow{}, sharesByToken: map[string]*shareRow{},
		versions:    map[int64]*versionRow{}, versionsBy: map[string]*versionRow{},
		sessions:    map[int64]*sessionRow{}, sessByHash: map[string]*sessionRow{},
		uploads:     map[string]*uploadRow{},
		metas:       map[string]string{},
		audits:      map[int64]*auditRow{},
	}
}

// ---------- 键构造 ----------

func nsep(parts ...string) string { return strings.Join(parts, "\x00") }

func metaKeyOf(userID int64, rootID, path string) string {
	return nsep(strconv.FormatInt(userID, 10), rootID, path)
}
func descKeyOf(userID int64, rootID, path, kind string) string {
	return nsep(strconv.FormatInt(userID, 10), rootID, path, kind)
}
func aclKeyOf(rootID, path, subjectType string, subjectID int64) string {
	return nsep(rootID, path, subjectType, strconv.FormatInt(subjectID, 10))
}
func versionKeyOf(rootID, path string, vno int64) string {
	return nsep(rootID, path, strconv.FormatInt(vno, 10))
}

func dupErr(what string) error {
	return fmt.Errorf("store: unique violation on %s: %w", what, ErrExists)
}

// bumpID 抬升自增水位（含已被墓碑删除的行——id 永不复用）。
func bumpID(nextID *[tCount + 1]int64, t byte, id int64) {
	if nextID != nil && id+1 > nextID[t] {
		nextID[t] = id + 1
	}
}

// put 通用入口（replay 与运行期共用）：同 PK 覆盖（先摘旧二级键），
// 二级唯一键被其他 PK 占用 → ErrExists。
func (x *fileIndex) put(t byte, payload []byte, nextID *[tCount + 1]int64) error {
	switch t {
	case tUsers:
		var r userRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if old := x.users[r.ID]; old != nil && x.usersByName[old.Username] == old {
			delete(x.usersByName, old.Username)
		}
		if e, ok := x.usersByName[r.Username]; ok && e.ID != r.ID {
			return dupErr("users.username")
		}
		x.users[r.ID] = &r
		x.usersByName[r.Username] = &r
		bumpID(nextID, t, r.ID)
	case tUserRoots:
		var r userRootRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		x.userRoots[r.ID] = &r
		bumpID(nextID, t, r.ID)
	case tFileMetas:
		var r fileMetaRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		k := metaKeyOf(r.UserID, r.RootID, r.Path)
		if old := x.fileMetas[r.ID]; old != nil {
			ok := metaKeyOf(old.UserID, old.RootID, old.Path)
			if x.fileMetasBy[ok] == old {
				delete(x.fileMetasBy, ok)
			}
		}
		if e, ok := x.fileMetasBy[k]; ok && e.ID != r.ID {
			return dupErr("file_metas(user_id,root_id,path)")
		}
		x.fileMetas[r.ID] = &r
		x.fileMetasBy[k] = &r
		bumpID(nextID, t, r.ID)
	case tAIDescs:
		var r aiDescRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		k := descKeyOf(r.UserID, r.RootID, r.Path, r.Kind)
		if old := x.aiDescs[r.ID]; old != nil {
			ok := descKeyOf(old.UserID, old.RootID, old.Path, old.Kind)
			if x.aiDescsBy[ok] == old {
				delete(x.aiDescsBy, ok)
			}
		}
		if e, ok := x.aiDescsBy[k]; ok && e.ID != r.ID {
			return dupErr("ai_descriptions(user_id,root_id,path,kind)")
		}
		x.aiDescs[r.ID] = &r
		x.aiDescsBy[k] = &r
		bumpID(nextID, t, r.ID)
	case tTrash:
		var r trashRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if old := x.trash[r.ID]; old != nil && x.trashByID[old.TrashID] == old {
			delete(x.trashByID, old.TrashID)
		}
		if e, ok := x.trashByID[r.TrashID]; ok && e.ID != r.ID {
			return dupErr("trash_items.trash_id")
		}
		x.trash[r.ID] = &r
		x.trashByID[r.TrashID] = &r
		bumpID(nextID, t, r.ID)
	case tGroups:
		var r groupRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if old := x.groups[r.ID]; old != nil && x.groupsByName[old.Name] == old {
			delete(x.groupsByName, old.Name)
		}
		if e, ok := x.groupsByName[r.Name]; ok && e.ID != r.ID {
			return dupErr("groups.name")
		}
		x.groups[r.ID] = &r
		x.groupsByName[r.Name] = &r
		bumpID(nextID, t, r.ID)
	case tFolderACLs:
		var r aclRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		k := aclKeyOf(r.RootID, r.Path, r.SubjectType, r.SubjectID)
		if old := x.acls[r.ID]; old != nil {
			ok := aclKeyOf(old.RootID, old.Path, old.SubjectType, old.SubjectID)
			if x.aclsBy[ok] == old {
				delete(x.aclsBy, ok)
			}
		}
		if e, ok := x.aclsBy[k]; ok && e.ID != r.ID {
			return dupErr("folder_acls unique")
		}
		x.acls[r.ID] = &r
		x.aclsBy[k] = &r
		bumpID(nextID, t, r.ID)
	case tShareLinks:
		var r shareRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if old := x.shares[r.ID]; old != nil && x.sharesByToken[old.Token] == old {
			delete(x.sharesByToken, old.Token)
		}
		if e, ok := x.sharesByToken[r.Token]; ok && e.ID != r.ID {
			return dupErr("share_links.token")
		}
		x.shares[r.ID] = &r
		x.sharesByToken[r.Token] = &r
		bumpID(nextID, t, r.ID)
	case tFileVersions:
		var r versionRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		k := versionKeyOf(r.RootID, r.Path, r.VersionNo)
		if old := x.versions[r.ID]; old != nil {
			ok := versionKeyOf(old.RootID, old.Path, old.VersionNo)
			if x.versionsBy[ok] == old {
				delete(x.versionsBy, ok)
			}
		}
		if e, ok := x.versionsBy[k]; ok && e.ID != r.ID {
			return dupErr("file_versions(root_id,path,version_no)")
		}
		x.versions[r.ID] = &r
		x.versionsBy[k] = &r
		bumpID(nextID, t, r.ID)
	case tSessions:
		var r sessionRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		if old := x.sessions[r.ID]; old != nil && x.sessByHash[old.TokenHash] == old {
			delete(x.sessByHash, old.TokenHash)
		}
		if e, ok := x.sessByHash[r.TokenHash]; ok && e.ID != r.ID {
			return dupErr("sessions.token_hash")
		}
		x.sessions[r.ID] = &r
		x.sessByHash[r.TokenHash] = &r
		bumpID(nextID, t, r.ID)
	case tUploadSessions:
		var r uploadRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		x.uploads[r.ID] = &r // TEXT PK，无二级唯一键、无自增水位
	case tServerMeta:
		var r metaKVRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		x.metas[r.Mkey] = r.Value
	case tAuditEvents:
		var r auditRow
		if err := json.Unmarshal(payload, &r); err != nil {
			return err
		}
		x.audits[r.ID] = &r
		bumpID(nextID, t, r.ID)
	default:
		return fmt.Errorf("filestore: put unknown table %d", t)
	}
	return nil
}

// del 按 PK 删除（含二级键摘除）。absent 静默（回放对截断前缀天然幂等）。
func (x *fileIndex) del(t byte, payload []byte) error {
	switch t {
	case tServerMeta:
		delete(x.metas, string(payload))
		return nil
	case tUploadSessions:
		delete(x.uploads, string(payload))
		return nil
	}
	id, ok := decodeIntKey(payload)
	if !ok {
		return errCorrupt
	}
	switch t {
	case tUsers:
		if r := x.users[id]; r != nil {
			if x.usersByName[r.Username] == r {
				delete(x.usersByName, r.Username)
			}
			delete(x.users, id)
		}
	case tUserRoots:
		delete(x.userRoots, id)
	case tFileMetas:
		if r := x.fileMetas[id]; r != nil {
			k := metaKeyOf(r.UserID, r.RootID, r.Path)
			if x.fileMetasBy[k] == r {
				delete(x.fileMetasBy, k)
			}
			delete(x.fileMetas, id)
		}
	case tAIDescs:
		if r := x.aiDescs[id]; r != nil {
			k := descKeyOf(r.UserID, r.RootID, r.Path, r.Kind)
			if x.aiDescsBy[k] == r {
				delete(x.aiDescsBy, k)
			}
			delete(x.aiDescs, id)
		}
	case tTrash:
		if r := x.trash[id]; r != nil {
			if x.trashByID[r.TrashID] == r {
				delete(x.trashByID, r.TrashID)
			}
			delete(x.trash, id)
		}
	case tGroups:
		if r := x.groups[id]; r != nil {
			if x.groupsByName[r.Name] == r {
				delete(x.groupsByName, r.Name)
			}
			delete(x.groups, id)
		}
	case tFolderACLs:
		if r := x.acls[id]; r != nil {
			k := aclKeyOf(r.RootID, r.Path, r.SubjectType, r.SubjectID)
			if x.aclsBy[k] == r {
				delete(x.aclsBy, k)
			}
			delete(x.acls, id)
		}
	case tShareLinks:
		if r := x.shares[id]; r != nil {
			if x.sharesByToken[r.Token] == r {
				delete(x.sharesByToken, r.Token)
			}
			delete(x.shares, id)
		}
	case tFileVersions:
		if r := x.versions[id]; r != nil {
			k := versionKeyOf(r.RootID, r.Path, r.VersionNo)
			if x.versionsBy[k] == r {
				delete(x.versionsBy, k)
			}
			delete(x.versions, id)
		}
	case tSessions:
		if r := x.sessions[id]; r != nil {
			if x.sessByHash[r.TokenHash] == r {
				delete(x.sessByHash, r.TokenHash)
			}
			delete(x.sessions, id)
		}
	case tAuditEvents:
		delete(x.audits, id)
	default:
		return fmt.Errorf("filestore: del unknown table %d", t)
	}
	return nil
}

// ---------- 回放入口 ----------

func (x *fileIndex) replayPut(t byte, payload []byte, nextID *[tCount + 1]int64) error {
	return x.put(t, payload, nextID)
}

func (x *fileIndex) replayDel(t byte, payload []byte) error {
	return x.del(t, payload)
}

// ---------- 计数（TableCounts / 压缩触发 / 非空检查） ----------

func (x *fileIndex) counts() map[string]int64 {
	return map[string]int64{
		"users": int64(len(x.users)), "user_roots": int64(len(x.userRoots)),
		"file_metas": int64(len(x.fileMetas)), "ai_descriptions": int64(len(x.aiDescs)),
		"trash_items": int64(len(x.trash)), "groups": int64(len(x.groups)),
		"folder_acls": int64(len(x.acls)), "share_links": int64(len(x.shares)),
		"file_versions": int64(len(x.versions)), "sessions": int64(len(x.sessions)),
		"upload_sessions": int64(len(x.uploads)), "server_meta": int64(len(x.metas)),
		"audit_events": int64(len(x.audits)),
	}
}

func (x *fileIndex) liveRows() int64 {
	var n int64
	for _, c := range x.counts() {
		n += c
	}
	return n
}
