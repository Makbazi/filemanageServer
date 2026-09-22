// Package superadmin 提供文件承载的超级管理员账户（0.1.0，需求32/33/34/36）。
//
// 超管只存 <dataDir>/admin-account.json（0600），**不进元数据库**：
// 无库降级模式与运行期热切换数据库后同样可用；数据库 users 表中的
// role=admin 是"其他管理员"（需求35），由超管创建与管理，其任何管理接口
// 均不可触达超管（需求33——超管根本不在表内，接口按 id 寻址天然隔离）。
//
// 凭据存储状态机（需求34）：
//
//	写入明文（首启自动生成 / 管理台修改 / 直接编辑文件重置，需求32）
//	  → 首次成功登录前文件为明文，管理台可回显明文
//	  → 首次成功登录即转 bcrypt 密文回写，此后不可回显
//	  → 文件再次被写成明文 = 再次重置，回到"使用前可显示明文"
//
// generation 为凭据纪元：任何凭据变更（含检测到外部文件编辑）都会 +1 并
// 回写，超管访问令牌嵌入纪元、authCtx 逐请求核对——口令轮换/文件重置即
// 吊销全部旧令牌与会话。多实例部署时该文件与 jwt.secret 同理需放共享卷
// 或逐节点保持一致（见 server CLAUDE.md 约定7）。
package superadmin

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"filemanageServer/internal/auth"
)

// ErrNoAccount 账户文件尚不存在（首启，EnsureCreated 之前）。
var ErrNoAccount = errors.New("superadmin: account file not found")

// Account 账户文件形态。Password 为明文（未使用）或 bcrypt 密文（已使用）。
type Account struct {
	Username   string    `json:"username"`
	Password   string    `json:"password"`
	Generation int64     `json:"generation"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Store 超管账户文件读写器（全方法并发安全；启动期装配一次，之后只读改文件）。
type Store struct {
	mu   sync.Mutex
	path string
	last *Account // 上次读到的账户（外部编辑检测基准）
}

func New(path string) *Store { return &Store{path: path} }

// isHashed bcrypt 摘要前缀判定（$2a/$2b/$2x/$2y 都以 $2 开头）；非摘要 = 明文。
func isHashed(s string) bool { return strings.HasPrefix(s, "$2") }

// loadLocked 读文件 + 外部编辑检测：凭据值变了但纪元没前进 → 视为通过文件
// 重置（需求32），纪元 +1 回写以吊销旧令牌；外部把纪元改小则沿用内存更高值。
// 返回副本，调用方改完须显式 writeLocked 才落盘。
func (st *Store) loadLocked() (*Account, error) {
	b, err := os.ReadFile(st.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoAccount
		}
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse superadmin account %s: %w", st.path, err)
	}
	if a.Username == "" || a.Password == "" {
		return nil, fmt.Errorf("superadmin account file %s is corrupt (empty username/password)", st.path)
	}
	if st.last != nil {
		credChanged := a.Password != st.last.Password || a.Username != st.last.Username
		switch {
		case credChanged && a.Generation <= st.last.Generation:
			a.Generation = st.last.Generation + 1
			a.UpdatedAt = time.Now()
			if err := st.writeLocked(&a); err != nil {
				return nil, err
			}
		case !credChanged && a.Generation < st.last.Generation:
			a.Generation = st.last.Generation
		}
	}
	st.last = &a
	cp := a
	return &cp, nil
}

// writeLocked 原子落盘（tmp+rename，0600——含口令或其哈希，权限从紧）。
func (st *Store) writeLocked(a *Account) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// EnsureCreated 首启自动生成账户文件（用户名 admin + 随机明文口令）。
// 返回 created=true 时 plaintext 仅供启动日志一次性展示（需求34：使用前为明文）。
func (st *Store) EnsureCreated() (username, plaintext string, created bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if a, lerr := st.loadLocked(); lerr == nil {
		return a.Username, "", false, nil
	} else if !errors.Is(lerr, ErrNoAccount) {
		return "", "", false, lerr
	}
	pw, err := auth.RandomPassword(12)
	if err != nil {
		return "", "", false, err
	}
	a := &Account{Username: "admin", Password: pw, Generation: 1, UpdatedAt: time.Now()}
	if err := st.writeLocked(a); err != nil {
		return "", "", false, err
	}
	st.last = a
	return a.Username, pw, true, nil
}

// Match username 是否为超管账户名。
func (st *Store) Match(username string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if errors.Is(err, ErrNoAccount) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return a.Username == username, nil
}

// Username 当前超管账户名（文件缺失返回 ErrNoAccount）。
func (st *Store) Username() (string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if err != nil {
		return "", err
	}
	return a.Username, nil
}

// Generation 当前凭据纪元（签发/核对超管令牌用）。
func (st *Store) Generation() (int64, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if err != nil {
		return 0, err
	}
	return a.Generation, nil
}

// Plaintext 未使用前（需求34）返回明文供管理台回显；已转密文后 plaintext=false。
func (st *Store) Plaintext() (pw string, plaintext bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if err != nil {
		return "", false, err
	}
	if isHashed(a.Password) {
		return "", false, nil
	}
	return a.Password, true, nil
}

// Verify 校验超管口令。明文期常数时间比对，首次成功即转 bcrypt 密文回写
// 并推进纪元（需求34"使用后更改为密文保存"）；密文期走 bcrypt。
// 调用方（handleLogin）负责防爆破计数与审计。
func (st *Store) Verify(username, password string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if err != nil {
		return false, err
	}
	if a.Username != username {
		return false, nil
	}
	if isHashed(a.Password) {
		return bcrypt.CompareHashAndPassword([]byte(a.Password), []byte(password)) == nil, nil
	}
	if subtle.ConstantTimeCompare([]byte(a.Password), []byte(password)) != 1 {
		return false, nil
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	a.Password = string(h)
	a.Generation++
	a.UpdatedAt = time.Now()
	if err := st.writeLocked(a); err != nil {
		return false, err
	}
	st.last = a
	return true, nil
}

// SetCredentials 管理台修改超管账户名/口令（需求32"名称和密码可以修改"）。
// 空字段=保持不变；password 以明文写入（转密文时机仍为首次登录，需求34），
// 拒绝传入摘要串。返回新副本（纪元已 +1），调用方据此重发令牌。
func (st *Store) SetCredentials(username, password string) (*Account, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	a, err := st.loadLocked()
	if err != nil {
		return nil, err
	}
	if username == "" && password == "" {
		return nil, errors.New("superadmin: nothing to update (username/password both empty)")
	}
	if username != "" {
		if strings.TrimSpace(username) != username || username == "" ||
			strings.ContainsAny(username, " \t\r\n:/\\") || len(username) > 64 {
			return nil, errors.New("superadmin: invalid username (no spaces, ':' or '\\', max 64 chars)")
		}
		a.Username = username
	}
	if password != "" {
		if len(password) < 6 {
			return nil, errors.New("superadmin: password too short (min 6)")
		}
		if isHashed(password) {
			return nil, errors.New("superadmin: pass a plaintext password, not a hash")
		}
		a.Password = password // 明文存储直至首次登录转密文（需求34）
	}
	a.Generation++
	a.UpdatedAt = time.Now()
	if err := st.writeLocked(a); err != nil {
		return nil, err
	}
	st.last = a
	cp := *a
	return &cp, nil
}
