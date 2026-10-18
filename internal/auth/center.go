// center.go：三层认证图（0.2.3）——进程内 LRU(L1) + 可选 redis(L2) + 持久化 Store(L3)。
//
// 结构纪律：
//   - L3（auth_sessions 表，file/sqlite3/mysql/postgresql 四方言）是唯一事实源：
//     发放/吊销同步落库（file 驱动 fsync 关键操作纪律沿用）；L1/L2 只是缓存，
//     任何一层的数据都只存凭证哈希（SHA-256），明文凭证只在 Issue 返回值出现一次。
//   - 读路径 L1→L2→L3 逐级回填：重启恢复 = WarmStart 预热 + 冷凭证惰性回源；
//     L2 读故障一律落到 L3（fail-open——所有撤权路径先删 L3，L3 说没有才算没有）。
//   - 滑动续期（仅普通用户 kind=user 且 Role!=admin）：L1 立即更新 + 置脏，
//     L2/L3 由清扫器按 TouchWriteEvery 节流批量写回；崩溃丢 ≤ 一个刷写窗口的
//     续期进度 = 凭证提前到期（安全方向），不逐请求 fsync。
//   - 多实例撤权：L3+L2 共享层立删即全实例可见；各实例 L1 陈旧窗口 ≤ MemTTL
//     （条目超龄强制回源），这是明示的一致性延迟上界。
//   - L2 撤销失败进 pendingRevoke：本地校验兜底拒绝 + 清扫器重试直至成功。
package auth

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"filemanageServer/internal/logx"
	store "filemanageServer/internal/store"
)

// ErrBadCredential 凭证无效/过期/已吊销/已被 pendingRevoke 兜底拒绝。
// 中间件统一映射 401（不是服务故障，绝不 503）。
var ErrBadCredential = errors.New("auth: invalid or expired credential")

// RedisCfg L2 连接参数（main 从 cache.redis 配置组装；Addr="" = 不接 L2）。
type RedisCfg struct {
	Addr     string
	Password string
	DB       int
}

// Options 组装参数（0 值取默认；config.finalize 已给全默认，此处为纵深兜底）。
type Options struct {
	Store           store.Store
	Redis           RedisCfg
	MemMax          int           // L1 条目上限（需求：默认 200000，LRU 驱逐）
	MemTTL          time.Duration // L1 条目新鲜期（默认 5m，超龄回源——多实例撤权延迟上界）
	RedisMax        int64         // L2 条目上限（需求：默认 5000000，ZSET score 近似 LRU 裁剪）
	SweepInterval   time.Duration // 清扫/写回周期（默认 10m）
	TouchWriteEvery time.Duration // 单条目续期写回最小间隔（默认 10m）
}

// Spec 签发参数。
type Spec struct {
	Kind     string // store.AuthKindUser | store.AuthKindShare
	UserID   int64  // 0=超管哨兵；store.AuthShareUserID=-1=share
	Username string
	Role     string
	Super    bool
	Gen      int64  // 超管凭据纪元（authCtx 逐请求核对）
	Ref      string // share：绑定的链接 token
	ClientID string // 0.2.4：客户端唯一标识（kind=user 且非空=绑定，逐请求核对
	// X-Client-Id 头；空=未绑定——旧客户端/移动端/管理台会话）
	TTL     time.Duration
	Persist bool // false=仅 L1(+L2)：降级模式超管登录（对齐旧行为，重启即失效）
}

// Center 认证中心。对外方法均并发安全；锁只护 L1 纯内存操作，所有 I/O 在锁外。
type Center struct {
	st  store.Store
	rdb *redis.Client // nil = L2 未启用
	opt Options

	mu            sync.Mutex
	l1            map[string]*l1Node
	lru           *list.List // front=最近使用；element.Value=*l1Node
	byUser        map[int64]map[string]struct{}
	pendingRevoke map[string]struct{}
	sweeps        int
}

type l1Node struct {
	elem      *list.Element
	e         *store.AuthSession // 权威快照副本（Expires/LastUsed 可已被内存滑动前推）
	loadedAt  time.Time
	dirty     bool
	lastFlush time.Time
}

const (
	keyPrefix      = "fm:auth:"
	idxKey         = "fm:auth:idx" // ZSET：hash → lastUsed(unix)，作 L2 上限裁剪的近似 LRU 序
	userPrefix     = "fm:auth:u:"  // SET：userId → hashes，按用户撤权的 L2 反向索引
	redisOpTimeout = 2 * time.Second
)

// wireEntry redis 影子结构（需求8 纪律：敏感字段不落缓存原值——这里连哈希都不存，
// 哈希即键名；AuthSession 的 json tag 面向 API 且 TokenHash json:"-"，不可直接复用）。
type wireEntry struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	UserID     int64     `json:"userId"`
	Username   string    `json:"username"`
	Role       string    `json:"role"`
	Super      bool      `json:"super"`
	Gen        int64     `json:"gen"`
	Ref        string    `json:"ref"`
	ClientID   string    `json:"clientId"`
	ExpiresAt  time.Time `json:"expiresAt"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

func toWire(e *store.AuthSession) wireEntry {
	return wireEntry{ID: e.ID, Kind: e.Kind, UserID: e.UserID, Username: e.Username,
		Role: e.Role, Super: e.Super, Gen: e.Gen, Ref: e.Ref, ClientID: e.ClientID,
		ExpiresAt: e.ExpiresAt, LastUsedAt: e.LastUsedAt, CreatedAt: e.CreatedAt}
}

func (w wireEntry) session(hash string) *store.AuthSession {
	return &store.AuthSession{ID: w.ID, Kind: w.Kind, UserID: w.UserID, Username: w.Username,
		Role: w.Role, Super: w.Super, Gen: w.Gen, Ref: w.Ref, ClientID: w.ClientID, TokenHash: hash,
		ExpiresAt: w.ExpiresAt, LastUsedAt: w.LastUsedAt, CreatedAt: w.CreatedAt}
}

// New 构造认证中心。redis Ping 失败仅告警不 fail-fast（区别于 cache.New：认证
// 可降级为 L1+L3 继续工作，L2 恢复后由读路径/清扫器自然重灌）。
func New(opt Options) (*Center, error) {
	if opt.Store == nil {
		return nil, errors.New("auth: center requires a store")
	}
	if opt.MemMax <= 0 {
		opt.MemMax = 200000
	}
	if opt.MemTTL <= 0 {
		opt.MemTTL = 5 * time.Minute
	}
	if opt.RedisMax <= 0 {
		opt.RedisMax = 5000000
	}
	if opt.SweepInterval <= 0 {
		opt.SweepInterval = 10 * time.Minute
	}
	if opt.TouchWriteEvery <= 0 {
		opt.TouchWriteEvery = 10 * time.Minute
	}
	c := &Center{
		st:            opt.Store,
		opt:           opt,
		l1:            map[string]*l1Node{},
		lru:           list.New(),
		byUser:        map[int64]map[string]struct{}{},
		pendingRevoke: map[string]struct{}{},
	}
	if opt.Redis.Addr != "" {
		c.rdb = redis.NewClient(&redis.Options{Addr: opt.Redis.Addr, Password: opt.Redis.Password, DB: opt.Redis.DB})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.rdb.Ping(ctx).Err(); err != nil {
			// 0.2.7：真实运行故障走 logx.Warn（默认等级 warn 下可见；不含任何凭证值）
			logx.Warn("auth: warn: L2 redis 启动不可达（%v）——先以 L1+持久层工作，读写路径自动重试", err)
		}
	}
	return c, nil
}

// ---------- L1（进程内 LRU，容量 MemMax，条目新鲜期 MemTTL） ----------

func (c *Center) insertLocked(n *l1Node) {
	if old := c.l1[n.e.TokenHash]; old != nil {
		c.removeLocked(old) // 替换旧节点：list 与 map 单一事实源，防双记账泄漏
	}
	n.elem = c.lru.PushFront(n)
	c.l1[n.e.TokenHash] = n
	if n.e.Kind == store.AuthKindUser {
		set := c.byUser[n.e.UserID]
		if set == nil {
			set = map[string]struct{}{}
			c.byUser[n.e.UserID] = set
		}
		set[n.e.TokenHash] = struct{}{}
	}
	for c.lru.Len() > c.opt.MemMax { // LRU 驱逐；dirty 被逐=至多丢一段续期（提前到期，安全方向）
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.removeLocked(back.Value.(*l1Node))
	}
}

func (c *Center) removeLocked(n *l1Node) {
	if n.elem != nil {
		c.lru.Remove(n.elem)
		n.elem = nil
	}
	delete(c.l1, n.e.TokenHash)
	if n.e.Kind == store.AuthKindUser {
		if set := c.byUser[n.e.UserID]; set != nil {
			delete(set, n.e.TokenHash)
		}
	}
}

func (c *Center) l1Insert(e *store.AuthSession, loadedAt time.Time) {
	c.mu.Lock()
	c.insertLocked(&l1Node{e: e, loadedAt: loadedAt, lastFlush: loadedAt})
	c.mu.Unlock()
}

func (c *Center) evictL1(hash string) {
	c.mu.Lock()
	if n := c.l1[hash]; n != nil {
		c.removeLocked(n)
	}
	c.mu.Unlock()
}

// ---------- L2（redis 可选层） ----------

func (c *Center) l2Put(ctx context.Context, e *store.AuthSession) {
	if c.rdb == nil {
		return
	}
	ttl := time.Until(e.ExpiresAt)
	if ttl <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	b, err := json.Marshal(toWire(e))
	if err != nil {
		return
	}
	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, keyPrefix+e.TokenHash, b, ttl)
	pipe.ZAdd(ctx, idxKey, redis.Z{Score: float64(e.LastUsedAt.Unix()), Member: e.TokenHash})
	if e.Kind == store.AuthKindUser {
		skey := userPrefix + strconv.FormatInt(e.UserID, 10)
		pipe.SAdd(ctx, skey, e.TokenHash)
		pipe.Expire(ctx, skey, ttl) // 集合生命周期随最新条目滚动延长；残留指针由 RevokeUser 的 EXISTS 核验自清
	}
	if _, err := pipe.Exec(ctx); err != nil {
		logx.Warn("auth: warn: L2 put: %v", err) // L3 是事实源，L2 写失败不致命（读路径会回源）
	}
}

// l2Get 返回 (entry, foundOrHardError)：命中/确认不存在/读故障（调用方落 L3）。
func (c *Center) l2Get(ctx context.Context, hash string) (*store.AuthSession, bool) {
	if c.rdb == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	b, err := c.rdb.Get(ctx, keyPrefix+hash).Bytes()
	if err == redis.Nil {
		return nil, false
	}
	if err != nil {
		logx.Warn("auth: warn: L2 get: %v（回落持久层）", err)
		return nil, false
	}
	var w wireEntry
	if json.Unmarshal(b, &w) != nil {
		return nil, false
	}
	return w.session(hash), true
}

func (c *Center) l2Delete(ctx context.Context, hash string, uid int64, kind string) error {
	if c.rdb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	pipe := c.rdb.Pipeline()
	pipe.Unlink(ctx, keyPrefix+hash)
	pipe.ZRem(ctx, idxKey, hash)
	if kind == store.AuthKindUser {
		pipe.SRem(ctx, userPrefix+strconv.FormatInt(uid, 10), hash)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// l2DropUser 清空某用户的 L2 反向集合（含不在本实例 L1 的散点凭证）。
func (c *Center) l2DropUser(ctx context.Context, uid int64) {
	if c.rdb == nil {
		return
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	skey := userPrefix + strconv.FormatInt(uid, 10)
	hashes, err := c.rdb.SMembers(ctx2, skey).Result()
	if err == nil && len(hashes) > 0 {
		pipe := c.rdb.Pipeline()
		exists := make([]*redis.IntCmd, len(hashes))
		for i, h := range hashes {
			exists[i] = pipe.Exists(ctx2, keyPrefix+h)
		}
		_, _ = pipe.Exec(ctx2)
		pipe = c.rdb.Pipeline()
		for i, h := range hashes {
			if exists[i].Val() > 0 {
				pipe.Unlink(ctx2, keyPrefix+h)
				pipe.ZRem(ctx2, idxKey, h)
			}
		}
		_, _ = pipe.Exec(ctx2)
	}
	_ = c.rdb.Del(ctx2, skey).Err()
}

// ---------- 对外操作 ----------

// Issue 签发新凭证：L3（Persist=true，同步）→ L2 → L1。返回明文凭证（仅此一次）。
// L3 写失败一律报错——绝不发放不可吊销的凭证（撤权以持久层为锚）。
func (c *Center) Issue(ctx context.Context, sp Spec) (string, error) {
	plain, hash, err := NewCredential()
	if err != nil {
		return "", err
	}
	now := time.Now()
	e := &store.AuthSession{Kind: sp.Kind, UserID: sp.UserID, Username: sp.Username,
		Role: sp.Role, Super: sp.Super, Gen: sp.Gen, Ref: sp.Ref, ClientID: sp.ClientID, TokenHash: hash,
		ExpiresAt: now.Add(sp.TTL), LastUsedAt: now, CreatedAt: now}
	if sp.Persist {
		if err := c.st.CreateAuthSession(e); err != nil {
			return "", fmt.Errorf("auth: persist session: %w", err)
		}
	}
	c.l2Put(ctx, e)
	c.l1Insert(e, now)
	return plain, nil
}

// Lookup 校验凭证（不改滑动时间——续期由调用方另行 Touch）。
// ErrBadCredential=无效/过期/已撤；其余 error=存储故障（handler 归 503 语义）。
func (c *Center) Lookup(ctx context.Context, credential string) (*store.AuthSession, error) {
	hash := HashToken(credential)
	now := time.Now()

	c.mu.Lock()
	if _, bad := c.pendingRevoke[hash]; bad {
		c.mu.Unlock()
		return nil, ErrBadCredential // 撤权未完全落地期间的本地兜底（清扫器在重试 L2）
	}
	n := c.l1[hash]
	fresh := n != nil && now.Sub(n.loadedAt) < c.opt.MemTTL
	if fresh {
		if n.e.ExpiresAt.Before(now) {
			exp := n.e
			c.removeLocked(n)
			c.mu.Unlock()
			c.expireAll(ctx, exp)
			return nil, ErrBadCredential
		}
		c.lru.MoveToFront(n.elem)
		out := *n.e
		c.mu.Unlock()
		return &out, nil
	}
	c.mu.Unlock()

	if e, ok := c.l2Get(ctx, hash); ok {
		if e.ExpiresAt.Before(now) {
			c.expireAll(ctx, e)
			return nil, ErrBadCredential
		}
		c.l1Insert(e, now)
		out := *e
		return &out, nil
	}
	e, err := c.st.GetAuthSessionByTokenHash(hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoDB) {
			c.evictL1(hash) // L1 陈旧节点被事实源证伪（他实例已撤/清）
			return nil, ErrBadCredential
		}
		return nil, err
	}
	if e.ExpiresAt.Before(now) {
		c.expireAll(ctx, e)
		return nil, ErrBadCredential
	}
	c.l1Insert(e, now)
	c.l2Put(ctx, e) // 陈旧回源顺带重灌 L2（滑动值同步）
	out := *e
	return &out, nil
}

// Touch 滑动续期（仅普通用户会话调用；策略判定在调用方）：L1 立即前推 +
// 置脏（写回受 TouchWriteEvery 节流），L2/L3 由清扫器批量补写。永不返回错误。
func (c *Center) Touch(e *store.AuthSession, window time.Duration) {
	if e == nil || window <= 0 {
		return
	}
	now := time.Now()
	target := now.Add(window)
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.l1[e.TokenHash]
	if n == nil {
		return // 已驱逐：下次读路径回源；本次续期丢失=提前到期，安全方向
	}
	if n.e.ExpiresAt.Before(target) {
		n.e.ExpiresAt = target
	}
	n.e.LastUsedAt = now
	if now.Sub(n.lastFlush) >= c.opt.TouchWriteEvery {
		n.dirty = true
	}
}

// Revoke 按明文凭证撤权（登出/轮换旧凭证）。L1 立删 → L3 → L2；
// L2 失败进 pendingRevoke 由清扫器重试。L3 ErrNotFound/ErrNoDB 容忍（幂等）。
func (c *Center) Revoke(ctx context.Context, credential string) error {
	hash := HashToken(credential)
	c.mu.Lock()
	var uid int64
	var kind string
	if n := c.l1[hash]; n != nil {
		uid, kind = n.e.UserID, n.e.Kind
		c.removeLocked(n)
	}
	c.pendingRevoke[hash] = struct{}{}
	c.mu.Unlock()
	return c.revokeTail(ctx, hash, uid, kind)
}

func (c *Center) revokeTail(ctx context.Context, hash string, uid int64, kind string) error {
	err := c.st.DeleteAuthSessionByTokenHash(hash)
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNoDB) {
		return fmt.Errorf("auth: revoke persist: %w", err)
	}
	if c.rdb == nil {
		c.mu.Lock()
		delete(c.pendingRevoke, hash)
		c.mu.Unlock()
		return nil
	}
	if lerr := c.l2Delete(ctx, hash, uid, kind); lerr != nil {
		// hash 是 SHA-256 摘要前缀（非凭证值本身），仅作撤权重试定位线索
		logx.Warn("auth: warn: L2 撤权失败（%v），清扫器重试 hash=%s…", lerr, hash[:8])
		return nil // L3 已删=事实源已撤：他实例回源即拒；本地 pendingRevoke 兜底
	}
	c.mu.Lock()
	delete(c.pendingRevoke, hash)
	c.mu.Unlock()
	return nil
}

// RevokeUser 按用户撤权（改密/管理员重置/删号/超管纪元轮换=传 SuperAdminID）。
// 拒 userID<0（share 保留哨兵，绝不误伤分享会话）。三层全清。
func (c *Center) RevokeUser(ctx context.Context, userID int64) error {
	if userID < 0 {
		return fmt.Errorf("auth: RevokeUser invalid userID %d", userID)
	}
	if err := c.st.DeleteAuthSessionsByUser(userID); err != nil && !errors.Is(err, store.ErrNoDB) {
		return err
	}
	c.mu.Lock()
	hashes := make([]string, 0, len(c.byUser[userID]))
	for h := range c.byUser[userID] {
		hashes = append(hashes, h)
	}
	for _, h := range hashes {
		if n := c.l1[h]; n != nil {
			c.removeLocked(n)
		}
		c.pendingRevoke[h] = struct{}{}
	}
	c.mu.Unlock()
	if c.rdb == nil {
		c.mu.Lock()
		for _, h := range hashes {
			delete(c.pendingRevoke, h)
		}
		c.mu.Unlock()
	} else {
		for _, h := range hashes {
			if err := c.l2Delete(ctx, h, userID, store.AuthKindUser); err == nil {
				c.mu.Lock()
				delete(c.pendingRevoke, h)
				c.mu.Unlock()
			}
		}
		c.l2DropUser(ctx, userID) // 覆盖不在本实例 L1 的散点凭证
	}
	return nil
}

// expireAll 条目自然过期回收：三层删除、不进 pendingRevoke（L2 键有自身 TTL）。
func (c *Center) expireAll(ctx context.Context, e *store.AuthSession) {
	c.evictL1(e.TokenHash)
	if e.ID > 0 {
		_ = c.st.DeleteAuthSessionByTokenHash(e.TokenHash)
	}
	if c.rdb != nil {
		_ = c.l2Delete(ctx, e.TokenHash, e.UserID, e.Kind)
	}
}

// WarmStart 重启预热：未过期条目按 token_hash 稳定窗口载入 L1（上限 MemMax）。
// 冷凭证仍经 Lookup 惰性恢复；预热只优化首访延迟，不是正确性前提。
func (c *Center) WarmStart(ctx context.Context) error {
	list0, err := c.st.ListActiveAuthSessions(c.opt.MemMax)
	if err != nil {
		return err
	}
	now := time.Now()
	for _, e := range list0 {
		c.l1Insert(e, now)
	}
	log.Printf("auth: warm-start 载入活跃会话 %d 条（L1 上限 %d）", len(list0), c.opt.MemMax)
	return nil
}

// Run 清扫器：每 SweepInterval 回收过期/写回脏续期/重试 pendingRevoke/修剪 L2；
// ctx 结束时末次写回后退出。
func (c *Center) Run(ctx context.Context) {
	t := time.NewTicker(c.opt.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.sweep(ctx)
			return
		case <-t.C:
			c.sweep(ctx)
		}
	}
}

func (c *Center) sweep(ctx context.Context) {
	now := time.Now()
	c.mu.Lock()
	var expired []*store.AuthSession
	var dirty []*l1Node
	for _, n := range c.l1 {
		if n.e.ExpiresAt.Before(now) {
			expired = append(expired, n.e)
			continue
		}
		if n.dirty {
			dirty = append(dirty, n)
		}
	}
	pend := make([]string, 0, len(c.pendingRevoke))
	for h := range c.pendingRevoke {
		pend = append(pend, h)
	}
	c.mu.Unlock()

	for _, e := range expired {
		c.expireAll(ctx, e)
	}
	for _, n := range dirty {
		c.mu.Lock()
		e := *n.e
		cur, alive := c.l1[e.TokenHash]
		c.mu.Unlock()
		if !alive || cur != n {
			continue
		}
		if e.ID > 0 { // Persist=false 的降级条目（ID=0）无 L3 行可续（P7）
			if err := c.st.RenewAuthSession(e.ID, e.ExpiresAt, e.LastUsedAt); err != nil {
				if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoDB) {
					// 事实源证伪（他实例撤权/清扫/引擎切换）：摘除上层，绝不续住已吊销条目
					c.expireAll(ctx, &e)
					continue
				}
				logx.Warn("auth: warn: 续期写回失败（下轮重试）: %v", err)
				continue
			}
		}
		c.l2Put(ctx, &e)
		c.mu.Lock()
		if cur2 := c.l1[e.TokenHash]; cur2 == n {
			n.dirty = false
			n.lastFlush = now
		}
		c.mu.Unlock()
	}
	for _, h := range pend {
		if err := c.l2Delete(ctx, h, 0, ""); err == nil {
			c.mu.Lock()
			delete(c.pendingRevoke, h)
			c.mu.Unlock()
		}
	}
	if c.rdb != nil {
		c.mu.Lock()
		c.sweeps++
		round := c.sweeps
		c.mu.Unlock()
		ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
		if card, err := c.rdb.ZCard(ctx2, idxKey).Result(); err == nil && card > c.opt.RedisMax {
			_ = c.rdb.ZRemRangeByRank(ctx2, idxKey, 0, card-c.opt.RedisMax-1).Err() // 近似 LRU：score=lastUsed
		}
		if round%6 == 0 { // 抽样修剪孤儿索引成员（键已按 TTL 消亡、idx 残留）
			if hs, err := c.rdb.ZRange(ctx2, idxKey, 0, 499).Result(); err == nil && len(hs) > 0 {
				pipe := c.rdb.Pipeline()
				exists := make([]*redis.IntCmd, len(hs))
				for i, h := range hs {
					exists[i] = pipe.Exists(ctx2, keyPrefix+h)
				}
				_, _ = pipe.Exec(ctx2)
				pipe = c.rdb.Pipeline()
				for i, h := range hs {
					if exists[i].Val() == 0 {
						pipe.ZRem(ctx2, idxKey, h)
					}
				}
				_, _ = pipe.Exec(ctx2)
			}
		}
		cancel()
	}
}
