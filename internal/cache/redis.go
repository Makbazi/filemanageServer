// redis.go：Noop 与 redis(v9) 两种实现。
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	store "filemanageServer/internal/store"
)

var ctxTimeout = 2 * time.Second

func cctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), ctxTimeout)
}

// ---------- Noop（cache.enabled=false） ----------

type Noop struct{}

func (Noop) Kind() string                                     { return "noop" }
func (Noop) Ping(context.Context) error                       { return nil }
func (Noop) Close() error                                     { return nil }
func (Noop) GetUser(int64) (*store.User, bool, error)         { return nil, false, nil }
func (Noop) PutUser(*store.User) error                        { return nil }
func (Noop) DropUser(int64) error                             { return nil }
func (Noop) GetACLs(string) ([]*store.FolderACL, bool, error) { return nil, false, nil }
func (Noop) PutACLs(string, []*store.FolderACL) error         { return nil }
func (Noop) BumpACLVersion() error                            { return nil }
func (Noop) GetGroups() ([]*store.Group, bool, error)         { return nil, false, nil }
func (Noop) PutGroups([]*store.Group) error                   { return nil }
func (Noop) DropGroups() error                                { return nil }
func (Noop) FlushAll() error                                  { return nil }

// ---------- Redis ----------

// userWire 为影子结构：User.PasswordHash 是 json:"-"（防 API 泄露），
// 缓存必须完整往返，故单独建镜像类型持久化（redis 值不出服务端进程，等同入库级别）。
type userWire struct {
	U            store.User `json:"u"`
	PasswordHash string     `json:"ph"`
}

type Redis struct {
	cli     *redis.Client
	userTTL time.Duration
	aclTTL  time.Duration
}

func (r *Redis) Kind() string { return "redis" }
func (r *Redis) Close() error { return r.cli.Close() }

func (r *Redis) Ping(ctx context.Context) error { return r.cli.Ping(ctx).Err() }

func keyMissing(err error) bool { return errors.Is(err, redis.Nil) }

func (r *Redis) getJSON(key string, v any) (bool, error) {
	ctx, cancel := cctx()
	defer cancel()
	b, err := r.cli.Get(ctx, key).Bytes()
	if keyMissing(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Redis) setJSON(key string, v any, ttl time.Duration) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := cctx()
	defer cancel()
	return r.cli.Set(ctx, key, b, ttl).Err()
}

func (r *Redis) del(key string) error {
	ctx, cancel := cctx()
	defer cancel()
	return r.cli.Del(ctx, key).Err()
}

func (r *Redis) GetUser(id int64) (*store.User, bool, error) {
	var w userWire
	ok, err := r.getJSON("fm:u:"+strconv.FormatInt(id, 10), &w)
	if !ok || err != nil {
		return nil, ok, err
	}
	u := w.U
	u.PasswordHash = w.PasswordHash
	return &u, true, nil
}

func (r *Redis) PutUser(u *store.User) error {
	return r.setJSON("fm:u:"+strconv.FormatInt(u.ID, 10), userWire{U: *u, PasswordHash: u.PasswordHash}, r.userTTL)
}

func (r *Redis) DropUser(id int64) error {
	return r.del("fm:u:" + strconv.FormatInt(id, 10))
}

const aclVerKey = "fm:acl:ver"

func (r *Redis) aclVersion() (int64, error) {
	ctx, cancel := cctx()
	defer cancel()
	v, err := r.cli.Get(ctx, aclVerKey).Int64()
	if keyMissing(err) {
		return 0, nil
	}
	return v, err
}

func (r *Redis) BumpACLVersion() error {
	ctx, cancel := cctx()
	defer cancel()
	return r.cli.Incr(ctx, aclVerKey).Err()
}

func (r *Redis) GetACLs(rootID string) ([]*store.FolderACL, bool, error) {
	ver, err := r.aclVersion()
	if err != nil {
		return nil, false, err
	}
	var out []*store.FolderACL
	ok, err := r.getJSON("fm:acl:"+rootID+":v"+strconv.FormatInt(ver, 10), &out)
	return out, ok, err
}

func (r *Redis) PutACLs(rootID string, acls []*store.FolderACL) error {
	ver, err := r.aclVersion()
	if err != nil {
		return err
	}
	return r.setJSON("fm:acl:"+rootID+":v"+strconv.FormatInt(ver, 10), acls, r.aclTTL)
}

func (r *Redis) GetGroups() ([]*store.Group, bool, error) {
	var out []*store.Group
	ok, err := r.getJSON("fm:grp", &out)
	return out, ok, err
}

func (r *Redis) PutGroups(gs []*store.Group) error { return r.setJSON("fm:grp", gs, r.aclTTL) }
func (r *Redis) DropGroups() error                 { return r.del("fm:grp") }

// FlushAll SCAN 清删全部 fm:* 键（用户/ACL 各版本/全局 ACL 版本/用户组）。
// 只动本应用前缀，不用 FLUSHDB（redis 可能与其他服务共享实例）。
func (r *Redis) FlushAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var cursor uint64
	for {
		keys, next, err := r.cli.Scan(ctx, cursor, "fm:*", 500).Result()
		if err != nil {
			return err
		}
		if len(keys) > 0 {
			if err := r.cli.Del(ctx, keys...).Err(); err != nil {
				return err
			}
		}
		if cursor = next; cursor == 0 {
			return nil
		}
	}
}
