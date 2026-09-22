// proxy.go：可在运行期原子替换底层实现的 Store 代理（0.7.0 管理台配库热切换）。
//
// 背景：0.6.0 及之前 Store 是启动期捕获的单例（api.Server/perm/transfer/
// fileops/cleaner/cache 装饰器共享同一实例），无法在不重启进程的情况下换库。
// 0.7.0 起装配顺序为 store.Open → NewProxy → cache.Wrap(proxy)（可选）→
// NewServer/Cleaner，各消费方持有的是 Proxy（或其装饰器），管理台 connect
// 成功后 Set() 原子换入新库即可全局生效（旧委托由调用方负责 Close）。
//
// 降级模式表示：Proxy 委托为 nil 即"无元数据库"。上层判定统一用
// store.Degraded(st)（穿透 cache 装饰器），替代散落的 `Store == nil` 比较。
// 已声明 requireDB() 的路由理论上不会在降级态调到方法；这里仍对所有方法做
// 委托前判空（返回 ErrNoDB）兜底，绝不 panic。
package store

import (
	"errors"
	"sync/atomic"
	"time"
)

// ErrNoDB 降级模式（未配置元数据库）下调用存储方法的哨兵错误。
var ErrNoDB = errors.New("store: database not configured (degraded)")

// availability 可选能力接口：报告（可能被装饰的）Store 链是否代表"无库"。
type availability interface {
	Degraded() bool
}

// Degraded 判定一条 Store 链是否处于降级模式：nil、或委托 nil 的 Proxy、
// 或实现了 availability 且其自报为降级（cache 装饰器透传）。
func Degraded(st Store) bool {
	if st == nil {
		return true
	}
	if a, ok := st.(availability); ok {
		return a.Degraded()
	}
	return false
}

// pinger 轻量连通性探测（管理台状态卡用；非 Store 接口成员）。
type pinger interface {
	Ping() error
}

// Ping 探测当前委托是否可用；降级模式返回 ErrNoDB。
func Ping(st Store) error {
	if Degraded(st) {
		return ErrNoDB
	}
	if p, ok := st.(pinger); ok {
		return p.Ping()
	}
	return nil
}

// Proxy Store 的原子可替换代理，实现完整 Store 接口。
type Proxy struct {
	v atomic.Pointer[Store]
}

var _ Store = (*Proxy)(nil)

// NewProxy 构造代理；s 可为 nil（降级模式启动）。
func NewProxy(s Store) *Proxy {
	p := &Proxy{}
	if s != nil {
		p.v.Store(&s)
	} else {
		// atomic.Pointer 存指针本身：nil 委托用"指向 nil 接口的指针"表达。
		var zero Store
		p.v.Store(&zero)
	}
	return p
}

// Current 返回当前委托（nil = 降级模式）。
func (p *Proxy) Current() Store {
	if sp := p.v.Load(); sp != nil {
		return *sp
	}
	return nil
}

// Set 原子换入新委托并返回旧委托（旧委托由调用方决定何时 Close）。s 可为 nil（回退降级）。
func (p *Proxy) Set(s Store) (old Store) {
	if prev := p.v.Swap(&s); prev != nil {
		old = *prev
	}
	return old
}

// Degraded 实现 availability：委托为 nil 即降级。
func (p *Proxy) Degraded() bool { return p.Current() == nil }

// Driver 返回当前委托驱动名；降级模式返回 ""。
func (p *Proxy) Driver() string {
	if s := p.Current(); s != nil {
		return s.Driver()
	}
	return ""
}

// Close 关闭当前委托（进程退出清理用；代理本身不阻塞后续 Set）。
func (p *Proxy) Close() error {
	if s := p.Current(); s != nil {
		return s.Close()
	}
	return nil
}

// Ping 实现 pinger（供 store.Ping 穿透）。
func (p *Proxy) Ping() error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	if pg, ok := s.(pinger); ok {
		return pg.Ping()
	}
	return nil
}

func (p *Proxy) Migrate() error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.Migrate()
}

// ---------- 以下全部为机械委托 ----------

func (p *Proxy) CreateUser(u *User) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.CreateUser(u)
}

func (p *Proxy) GetUserByName(username string) (*User, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetUserByName(username)
}

func (p *Proxy) GetUserByID(id int64) (*User, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetUserByID(id)
}

func (p *Proxy) ListUsers() ([]*User, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListUsers()
}

func (p *Proxy) UpdateUserPassword(id int64, hash string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateUserPassword(id, hash)
}

func (p *Proxy) UpdateUserRoots(id int64, roots []string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateUserRoots(id, roots)
}

func (p *Proxy) UpdateUserRole(id int64, role string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateUserRole(id, role)
}

func (p *Proxy) DeleteUser(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteUser(id)
}

func (p *Proxy) CountUsers() (int64, error) {
	s := p.Current()
	if s == nil {
		return 0, ErrNoDB
	}
	return s.CountUsers()
}

func (p *Proxy) ListUserRoots(userID int64) ([]*UserRoot, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListUserRoots(userID)
}

func (p *Proxy) SetUserRoots(userID int64, list []*UserRoot) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.SetUserRoots(userID, list)
}

func (p *Proxy) ListAllUserRoots() ([]*UserRoot, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListAllUserRoots()
}

func (p *Proxy) GetMeta(key string) (string, error) {
	s := p.Current()
	if s == nil {
		return "", ErrNoDB
	}
	return s.GetMeta(key)
}

func (p *Proxy) SetMeta(key, value string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.SetMeta(key, value)
}

func (p *Proxy) PutFileMeta(m *FileMeta) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.PutFileMeta(m)
}

func (p *Proxy) GetFileMeta(userID int64, rootID, path string) (*FileMeta, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetFileMeta(userID, rootID, path)
}

func (p *Proxy) ListFileMetas(userID int64, rootID string) ([]*FileMeta, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListFileMetas(userID, rootID)
}

func (p *Proxy) DeleteFileMeta(userID int64, rootID, path string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteFileMeta(userID, rootID, path)
}

func (p *Proxy) RenameFileMeta(userID int64, rootID, from, to string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.RenameFileMeta(userID, rootID, from, to)
}

func (p *Proxy) SaveAIDescription(d *AIDescription) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.SaveAIDescription(d)
}

func (p *Proxy) GetAIDescription(userID int64, rootID, path, kind string) (*AIDescription, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetAIDescription(userID, rootID, path, kind)
}

func (p *Proxy) ListAIDescriptions(userID int64, rootID string) ([]*AIDescription, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListAIDescriptions(userID, rootID)
}

func (p *Proxy) DeleteAIDescription(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteAIDescription(id)
}

func (p *Proxy) SearchDescriptions(userID int64, text string, limit int) ([]*SearchHit, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.SearchDescriptions(userID, text, limit)
}

func (p *Proxy) AddTrashItem(t *TrashItem) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.AddTrashItem(t)
}

func (p *Proxy) ListTrashItems(userID int64, rootID string) ([]*TrashItem, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListTrashItems(userID, rootID)
}

func (p *Proxy) TakeTrashItem(userID int64, trashID string) (*TrashItem, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.TakeTrashItem(userID, trashID)
}

func (p *Proxy) RemoveTrashItem(userID int64, trashID string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.RemoveTrashItem(userID, trashID)
}

func (p *Proxy) ClearTrash(userID int64, rootID string) ([]*TrashItem, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ClearTrash(userID, rootID)
}

func (p *Proxy) ListTrashBefore(before time.Time) ([]*TrashItem, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListTrashBefore(before)
}

func (p *Proxy) CreateGroup(g *Group) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.CreateGroup(g)
}

func (p *Proxy) GetGroupByID(id int64) (*Group, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetGroupByID(id)
}

func (p *Proxy) GetGroupByName(name string) (*Group, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetGroupByName(name)
}

func (p *Proxy) ListGroups() ([]*Group, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListGroups()
}

func (p *Proxy) UpdateGroup(g *Group) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateGroup(g)
}

func (p *Proxy) DeleteGroup(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteGroup(id)
}

func (p *Proxy) SetFolderACL(a *FolderACL) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.SetFolderACL(a)
}

func (p *Proxy) GetFolderACLs(rootID, path string) ([]*FolderACL, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetFolderACLs(rootID, path)
}

func (p *Proxy) ListRootACLs(rootID string) ([]*FolderACL, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListRootACLs(rootID)
}

func (p *Proxy) ListPublicViewACLs() ([]*FolderACL, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListPublicViewACLs()
}

func (p *Proxy) DeleteFolderACL(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteFolderACL(id)
}

func (p *Proxy) DeleteFolderACLsBySubject(subjectType string, subjectID int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteFolderACLsBySubject(subjectType, subjectID)
}

func (p *Proxy) CreateShareLink(l *ShareLink) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.CreateShareLink(l)
}

func (p *Proxy) GetShareLinkByToken(token string) (*ShareLink, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetShareLinkByToken(token)
}

func (p *Proxy) ListShareLinks(createdBy int64) ([]*ShareLink, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListShareLinks(createdBy)
}

func (p *Proxy) UpdateShareLink(l *ShareLink) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateShareLink(l)
}

func (p *Proxy) DeleteShareLink(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteShareLink(id)
}

func (p *Proxy) AddFileVersion(v *FileVersion) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.AddFileVersion(v)
}

func (p *Proxy) GetFileVersion(id int64) (*FileVersion, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetFileVersion(id)
}

func (p *Proxy) ListFileVersions(rootID, path string) ([]*FileVersion, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListFileVersions(rootID, path)
}

func (p *Proxy) DeleteFileVersion(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteFileVersion(id)
}

func (p *Proxy) DeleteFileVersions(rootID, path string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteFileVersions(rootID, path)
}

func (p *Proxy) CreateSession(sess *Session) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.CreateSession(sess)
}

func (p *Proxy) GetSessionByTokenHash(tokenHash string) (*Session, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetSessionByTokenHash(tokenHash)
}

func (p *Proxy) RenewSession(id int64, expiresAt, lastUsedAt time.Time) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.RenewSession(id, expiresAt, lastUsedAt)
}

func (p *Proxy) DeleteSession(id int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteSession(id)
}

func (p *Proxy) DeleteSessionsByUser(userID int64) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteSessionsByUser(userID)
}

func (p *Proxy) DeleteExpiredSessions(now time.Time) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteExpiredSessions(now)
}

func (p *Proxy) CreateUploadSession(u *UploadSession) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.CreateUploadSession(u)
}

func (p *Proxy) GetUploadSession(id string) (*UploadSession, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetUploadSession(id)
}

func (p *Proxy) GetActiveUploadSession(rootID, path string) (*UploadSession, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.GetActiveUploadSession(rootID, path)
}

func (p *Proxy) ListUploadSessions(userID int64) ([]*UploadSession, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListUploadSessions(userID)
}

func (p *Proxy) UpdateUploadSession(u *UploadSession) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.UpdateUploadSession(u)
}

func (p *Proxy) DeleteUploadSession(id string) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteUploadSession(id)
}

func (p *Proxy) DeleteUploadSessionsBefore(before time.Time) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteUploadSessionsBefore(before)
}

func (p *Proxy) AddAuditEvent(e *AuditEvent) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.AddAuditEvent(e)
}

func (p *Proxy) ListAuditEvents(limit int, userID int64) ([]*AuditEvent, error) {
	s := p.Current()
	if s == nil {
		return nil, ErrNoDB
	}
	return s.ListAuditEvents(limit, userID)
}

func (p *Proxy) DeleteAuditBefore(before time.Time) error {
	s := p.Current()
	if s == nil {
		return ErrNoDB
	}
	return s.DeleteAuditBefore(before)
}
