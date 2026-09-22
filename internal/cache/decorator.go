// decorator.go：store.Store 的缓存装饰器。main.go 单点接入（Wrap），
// 未启用 redis 时原样返回 base，零开销。
//
// 约定（CLAUDE.md）：任何绕过 Wrap 直接写库的路径都不会触发失效——
// 服务端所有元数据读写必须经由注入的 Store 实例。
package cache

import (
	store "filemanageServer/internal/store"
)

type cached struct {
	store.Store
	c Cache
}

// Degraded 实现 store 侧 availability 透传（0.7.0）：装饰链的降级状态
// 跟随底层（Proxy），供 requireDB/store.Degraded 统一判定。
func (s *cached) Degraded() bool { return store.Degraded(s.Store) }

// Wrap 用缓存装饰 base；Noop 缓存时直接返回 base。
func Wrap(base store.Store, c Cache) store.Store {
	if base == nil || c == nil || c.Kind() == "noop" {
		return base
	}
	return &cached{Store: base, c: c}
}

// ---------- 读穿透 ----------

func (s *cached) GetUserByID(id int64) (*store.User, error) {
	u, ok, err := s.c.GetUser(id)
	if err != nil {
		return nil, err // 运行期 redis 故障：响亮失败
	}
	if ok {
		return u, nil
	}
	u, err = s.Store.GetUserByID(id)
	if err != nil {
		return nil, err
	}
	if err := s.c.PutUser(u); err != nil {
		return nil, err
	}
	return u, nil
}

// ListRootACLs 缓存每根的全量 ACL（perm 引擎每请求读取）；
// rootID=="" 为管理员全表视图，不缓存。
func (s *cached) ListRootACLs(rootID string) ([]*store.FolderACL, error) {
	if rootID == "" {
		return s.Store.ListRootACLs(rootID)
	}
	acls, ok, err := s.c.GetACLs(rootID)
	if err != nil {
		return nil, err
	}
	if ok {
		return acls, nil
	}
	acls, err = s.Store.ListRootACLs(rootID)
	if err != nil {
		return nil, err
	}
	if err := s.c.PutACLs(rootID, acls); err != nil {
		return nil, err
	}
	return acls, nil
}

func (s *cached) ListGroups() ([]*store.Group, error) {
	gs, ok, err := s.c.GetGroups()
	if err != nil {
		return nil, err
	}
	if ok {
		return gs, nil
	}
	gs, err = s.Store.ListGroups()
	if err != nil {
		return nil, err
	}
	if err := s.c.PutGroups(gs); err != nil {
		return nil, err
	}
	return gs, nil
}

// ---------- 写失效（先落库成功，后失效；失效失败即报错） ----------

func (s *cached) UpdateUserPassword(id int64, hash string) error {
	if err := s.Store.UpdateUserPassword(id, hash); err != nil {
		return err
	}
	return s.c.DropUser(id)
}

func (s *cached) UpdateUserRoots(id int64, roots []string) error {
	if err := s.Store.UpdateUserRoots(id, roots); err != nil {
		return err
	}
	return s.c.DropUser(id)
}

func (s *cached) UpdateUserRole(id int64, role string) error {
	if err := s.Store.UpdateUserRole(id, role); err != nil {
		return err
	}
	return s.c.DropUser(id)
}

func (s *cached) DeleteUser(id int64) error {
	if err := s.Store.DeleteUser(id); err != nil {
		return err
	}
	return s.c.DropUser(id)
}

func (s *cached) SetFolderACL(a *store.FolderACL) error {
	if err := s.Store.SetFolderACL(a); err != nil {
		return err
	}
	return s.c.BumpACLVersion()
}

func (s *cached) DeleteFolderACL(id int64) error {
	if err := s.Store.DeleteFolderACL(id); err != nil {
		return err
	}
	return s.c.BumpACLVersion()
}

func (s *cached) DeleteFolderACLsBySubject(subjectType string, subjectID int64) error {
	if err := s.Store.DeleteFolderACLsBySubject(subjectType, subjectID); err != nil {
		return err
	}
	return s.c.BumpACLVersion()
}

func (s *cached) CreateGroup(g *store.Group) error {
	if err := s.Store.CreateGroup(g); err != nil {
		return err
	}
	return s.c.DropGroups()
}

func (s *cached) UpdateGroup(g *store.Group) error {
	if err := s.Store.UpdateGroup(g); err != nil {
		return err
	}
	return s.c.DropGroups()
}

func (s *cached) DeleteGroup(id int64) error {
	if err := s.Store.DeleteGroup(id); err != nil {
		return err
	}
	return s.c.DropGroups()
}
