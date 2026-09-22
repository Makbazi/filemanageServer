// Package cache 提供可选的 redis 元数据缓存（0.3.0，非必须组件）。
//
// 接入方式：main.go 以 Wrap(store, Cache) 装饰 internal/store.Store —— 读穿透 +
// 写失效集中在装饰器一处，全部 handler/perm/transfer 零改动。
// cache.enabled=false（默认）时为 Noop，行为与 0.2.x 完全一致。
// 启动时 PING 失败直接报错退出（fail-fast，配置了就要工作）；运行期 redis
// 故障按"响亮失败"处理（返回 error → 500），不静默旁路。
package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"filemanageServer/internal/config"
	store "filemanageServer/internal/store"
)

// Cache 类型化窄接口（非通用 Get/Set，使失效逻辑显式可控）。
// 只缓存每请求级高频读：按 ID 用户（authCtx）、按根 ACL（perm 引擎）、
// 用户组列表（组归属判定）。
// 刻意不缓存会话（GetSessionByTokenHash 仅出现在低频 /auth/refresh，且
// "改密踢下线"(DeleteSessionsByUser) 无法枚举失效缓存键，缓存会造成
// TTL 内旧 refresh 令牌的越权窗口 —— 直接不缓存即无回归）。
// 键约定：fm:u:{id} 用户、fm:acl:{rootID}:v{ver} 目录 ACL（全局版本号，
// 任何 ACL 写即 bump，免精确失效）、fm:grp 用户组列表。
type Cache interface {
	Kind() string // "noop" | "redis"
	Ping(ctx context.Context) error
	Close() error

	GetUser(id int64) (*store.User, bool, error)
	PutUser(u *store.User) error
	DropUser(id int64) error

	GetACLs(rootID string) ([]*store.FolderACL, bool, error)
	PutACLs(rootID string, acls []*store.FolderACL) error
	BumpACLVersion() error

	GetGroups() ([]*store.Group, bool, error)
	PutGroups(gs []*store.Group) error
	DropGroups() error

	// FlushAll 清空本缓存的全部 fm:* 键（0.7.0 数据库热切换后调用：
	// 新旧库的用户 ID/ACL 可能互相冲突，残留缓存会遮蔽新库数据）。
	FlushAll() error
}

// New 按配置构造缓存；未启用返回 Noop；启用时 ping 失败即报错（fail-fast）。
func New(cfg config.CacheCfg) (Cache, error) {
	if !cfg.Enabled {
		return Noop{}, nil
	}
	rc := &Redis{
		cli: redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		}),
		userTTL: time.Duration(cfg.Redis.UserTTLSeconds) * time.Second,
		aclTTL:  time.Duration(cfg.Redis.ACLTTLSeconds) * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rc.cli.Ping(ctx).Err(); err != nil {
		rc.cli.Close()
		return nil, errors.New("cache: redis unreachable at " + cfg.Redis.Addr + ": " + err.Error())
	}
	return rc, nil
}
