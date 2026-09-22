package transfer

import "sync"

// LockRegistry 按 key（rootID + NUL + relPath）提供互斥锁，
// 用于同一实例内对同一文件写操作（上传合并/回滚/覆盖）的串行化（需求3：
// 多客户端修改相同文件保护）。跨实例保护由分片上传会话的唯一性
// （同路径仅一个活动会话）+ BaseHash 乐观并发校验承担。
type LockRegistry struct {
	mu    sync.Mutex
	locks map[string]*refCounted
}

type refCounted struct {
	mu   sync.Mutex
	refs int
}

// NewLockRegistry 创建锁注册表。
func NewLockRegistry() *LockRegistry {
	return &LockRegistry{locks: map[string]*refCounted{}}
}

// Lock 阻塞获取 key 对应的锁，返回解锁函数（幂等，只应调用一次）。
func (r *LockRegistry) Lock(key string) func() {
	r.mu.Lock()
	l := r.locks[key]
	if l == nil {
		l = &refCounted{}
		r.locks[key] = l
	}
	l.refs++
	r.mu.Unlock()

	l.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			r.mu.Lock()
			l.refs--
			if l.refs == 0 {
				delete(r.locks, key)
			}
			r.mu.Unlock()
		})
	}
}

// Key 生成路径锁 key。
func Key(rootID, relPath string) string {
	return rootID + "\x00" + relPath
}
