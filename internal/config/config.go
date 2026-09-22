// Package config 负责服务端配置的加载、默认值补齐、校验与安全升级（需求25）。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filemanageServer/internal/store"
)

// RootCfg 服务端托管的一个根目录。所有文件操作被限制在这些目录内。
type RootCfg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// AICfg AI 能力配置。provider: none | openai | ollama。
type AICfg struct {
	Provider  string `json:"provider"`
	BaseURL   string `json:"baseUrl"`
	APIKey    string `json:"apiKey"`
	Model     string `json:"model"`
	MaxFileMB int    `json:"maxFileMB"`
}

// TLSCfg 传输加密配置（需求3）。启用后服务端以 HTTPS 提供服务；
// 未提供证书且 AutoSelfSign=true 时自动生成自签证书（局域网场景）。
type TLSCfg struct {
	Enabled      bool   `json:"enabled"`
	CertFile     string `json:"certFile"`
	KeyFile      string `json:"keyFile"`
	AutoSelfSign bool   `json:"autoSelfSign"`
}

// SessionCfg 会话策略（需求22）。AccessTTL 为短期访问令牌有效期；
// IdleDays 为 refresh 会话最长闲置天数（默认 21 天 = 3 周），
// 活跃使用自动轮换续期，超过 IdleDays 未使用必须重新登录。
type SessionCfg struct {
	AccessTTL string `json:"accessTTL"`
	IdleDays  int    `json:"idleDays"`
}

// AnonymousCfg 匿名访问策略（需求6）。Enabled=true 时未登录客户端可
// 浏览（受目录 public ACL 约束），但一律不可下载/上传/修改/删除。
type AnonymousCfg struct {
	Enabled bool `json:"enabled"`
}

// AdminAccessCfg 管理界面 IP 准入策略（0.1.0 需求38），仅作用于管理端口
// （adminListen 引擎，含 /admin/ 静态台与全部 admin/配库 API）：
//   - local  只允许本机回环地址（新装默认）
//   - lan    允许局域网（回环 + RFC1918/ULA/link-local，= 0.7.x lanOnly 语义；
//     migrate7to8 给存量配置补的本值，升级不改变可达面）
//   - any    允许任何来源 IP（远程暴露时务必配合强口令与 TLS）
//   - custom 只允许 IPs 列出的精确 IP 或 CIDR 网段
//
// 判定始终基于 TCP RemoteAddr，绝不信任 X-Forwarded-For（与 lanFilter 一致）。
type AdminAccessCfg struct {
	Mode string   `json:"mode"`
	IPs  []string `json:"ips"`
}

// RateLimitCfg 传输限速（需求18），单位 KB/s，0 表示不限速（默认）。
// QPS 为每 IP 每秒请求数上限（0.5.1 安全迭代，0=关闭）。
type RateLimitCfg struct {
	UploadKBps   int `json:"uploadKBps"`
	DownloadKBps int `json:"downloadKBps"`
	QPS          int `json:"qps"`
}

// VersionsCfg 文件版本策略（需求13）。
type VersionsCfg struct {
	Enabled     bool   `json:"enabled"`
	MaxVersions int    `json:"maxVersions"`
	Dir         string `json:"dir"`
}

// BalancerCfg 负载均衡模式配置（需求27）：本节点作为反向代理，
// 将请求轮询分发到同机（或同网段）的多个 worker 实例。
type BalancerCfg struct {
	Workers []string `json:"workers"`
}

// DBCfg 元数据库配置（0.3.0）。Driver 取值：
// sqlite3 | mysql | postgresql（含 sqlite/postgres/pg/mariadb 别名，见 store.Open）；
// "none"（或 ""）= 不配置数据库，服务端以降级模式启动（仅匿名只读浏览）。
// 0.2.x 的 json/memory 驱动已移除，旧配置由 migrate2to3 自动改写。
type DBCfg struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"` // sqlite3 文件路径 / mysql 与 postgres DSN
}

// CacheCfg 可选 redis 缓存（0.3.0，非必须）。Enabled=false（默认）时不启用，
// 全部元数据读写直达数据库。
type CacheCfg struct {
	Enabled bool     `json:"enabled"`
	Redis   RedisCfg `json:"redis"`
}

// RedisCfg redis 连接与缓存 TTL（秒）。启用后启动即 PING，连不上直接报错退出。
// 缓存对象：用户（按 ID）、目录 ACL、用户组（会话/登录令牌刻意不缓存，见 internal/cache）。
type RedisCfg struct {
	Addr           string `json:"addr"` // 默认 127.0.0.1:6379
	Password       string `json:"password"`
	DB             int    `json:"db"`
	UserTTLSeconds int    `json:"userTTLSeconds"` // 默认 300
	ACLTTLSeconds  int    `json:"aclTTLSeconds"`  // 默认 60
}

// Config 服务端配置（schemaVersion=8）。
type Config struct {
	SchemaVersion int `json:"schemaVersion"`

	// DataDir 服务端运行数据根目录（0.7.0）：未显式配置的 trashDir/
	// uploadStagingDir/secretFile/versions.dir/sqlite 默认 DSN 均由其派生；
	// 用户主目录固定位于 <dataDir>/Users/<用户名>（建用户自动分配，需求3）。
	DataDir string `json:"dataDir"`

	Listen      string `json:"listen"`      // 默认 0.0.0.0:8484（用户/客户端端口，不含任何管理路由）
	AdminListen string `json:"adminListen"` // 管理面独立端口（0.5.0）：/admin/ 界面与全部 admin API 仅此端口可达；默认 bind 127.0.0.1（服务端本机）
	LANOnly     bool   `json:"lanOnly"`     // 默认 true：只接受局域网客户端

	// AdminAccess 管理界面 IP 准入策略（0.1.0 需求38）：local/lan/any/custom，
	// 仅作用于管理端口引擎。默认 local（新装）；migrate7to8 给存量配置补 lan。
	AdminAccess AdminAccessCfg `json:"adminAccess"`

	// adminNets 是 AdminAccess.Mode=custom 时 IPs 预解析出的网段（finalize 填充，
	// 不来自 JSON）。运行期只读。
	adminNets []*net.IPNet

	// CORSAllowOrigins 显式跨域来源白名单（0.5.1，可选）。空（默认）= 不下发任何
	// CORS 头（浏览器跨域读取天然被拒，与 0.3.x 行为一致）；支持条目 "*"。
	CORSAllowOrigins []string `json:"corsAllowOrigins"`
	JWTSecret        string   `json:"jwtSecret"`  // 显式密钥；为空时使用 SecretFile（多实例共享）
	SecretFile       string   `json:"secretFile"` // JWT 密钥持久化文件，默认 ./data/jwt.secret
	InstanceID       string   `json:"instanceId"` // 多实例部署标识（需求26），默认取主机名

	DB    DBCfg    `json:"db"`
	Cache CacheCfg `json:"cache"` // 可选 redis 缓存（0.3.0）

	Roots              []RootCfg `json:"roots"`
	TrashDir           string    `json:"trashDir"`
	UploadStagingDir   string    `json:"uploadStagingDir"`   // 分片上传暂存目录（多实例需共享）
	TrashRetentionDays int       `json:"trashRetentionDays"` // 回收站保留天数（需求4），默认 30
	MaxUploadBytes     int64     `json:"maxUploadBytes"`     // 单次上传大小硬上限（字节，0=不限），默认 2GiB（0.3.1）
	AdminPassword      string    `json:"adminPassword"`      // 超管口令重置触发器（需求32，0.1.0）：非空=启动时以其为明文重置超管账户文件，用后清空

	AI        AICfg        `json:"ai"`
	TLS       TLSCfg       `json:"tls"`
	Session   SessionCfg   `json:"session"`
	Anonymous AnonymousCfg `json:"anonymous"`
	RateLimit RateLimitCfg `json:"rateLimit"`
	Versions  VersionsCfg  `json:"versions"`
	Balancer  BalancerCfg  `json:"balancer"`
}

// DefaultConfig 返回带全部默认值的配置。
func DefaultConfig() *Config {
	// 0.7.0 注意：此处刻意**不**给 SecretFile/TrashDir/UploadStagingDir/
	// Versions.Dir/db.dsn 种默认值——它们在 finalize 中从 DataDir 派生
	// （LoadConfig 先 unmarshal 到带默认值的结构，非空种子会让"填空"逻辑失效）。
	c := &Config{
		SchemaVersion:      CurrentSchema,
		Listen:             "0.0.0.0:8484",
		AdminListen:        "127.0.0.1:8485",
		LANOnly:            true,
		DataDir:            "./data",
		TrashRetentionDays: 30,
		MaxUploadBytes:     2 << 30,
		Session:            SessionCfg{AccessTTL: "1h", IdleDays: 21},
		Anonymous:          AnonymousCfg{Enabled: true},
		Versions:           VersionsCfg{Enabled: true, MaxVersions: 10},
		TLS:                TLSCfg{AutoSelfSign: true},
		AdminAccess:        AdminAccessCfg{Mode: "local"}, // 新装默认仅本机；存量配置 migrate7to8 补 lan
	}
	c.DB.Driver = "sqlite3"
	c.AI.Provider = "none"
	c.AI.MaxFileMB = 20
	return c
}

// LoadConfig 读取 JSON 配置文件并执行 schema 迁移（需求25：升级不丢配置）。
// 迁移发生时原文件备份为 <path>.bak-<时间戳>，迁移结果回写。
func LoadConfig(path string) (*Config, error) {
	c := DefaultConfig()
	if path == "" {
		return finalize(c)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return finalize(c)
		}
		return nil, err
	}
	// 先解到通用 map 执行迁移，保留未知字段（向前兼容）
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	changed, err := MigrateMap(raw)
	if err != nil {
		return nil, fmt.Errorf("migrate config: %w", err)
	}
	if changed {
		backup := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, b, 0o644); err == nil {
			if nb, merr := json.MarshalIndent(raw, "", "  "); merr == nil {
				_ = os.WriteFile(path, nb, 0o644)
			}
		}
	}
	migrated, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	c = DefaultConfig()
	if err := json.Unmarshal(migrated, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return finalize(c)
}

// finalize 补齐缺省值、解析密钥并校验。
func finalize(c *Config) (*Config, error) {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8484"
	}
	if c.AdminListen == "" {
		c.AdminListen = "127.0.0.1:8485"
	}
	// 管理界面 IP 准入（0.1.0 需求38）：补默认、校验取值、custom 预解析网段。
	// custom 解析失败硬报错——绝不静默放宽准入面。
	if c.AdminAccess.Mode == "" {
		c.AdminAccess.Mode = "local"
	}
	switch c.AdminAccess.Mode {
	case "local", "lan", "any", "custom":
	default:
		return nil, fmt.Errorf("config: adminAccess.mode %q 须为 local|lan|any|custom", c.AdminAccess.Mode)
	}
	c.adminNets = nil
	if c.AdminAccess.Mode == "custom" {
		if len(c.AdminAccess.IPs) == 0 {
			return nil, fmt.Errorf("config: adminAccess.mode=custom 需至少一条 adminAccess.ips（精确 IP 或 CIDR 网段）")
		}
		for _, s := range c.AdminAccess.IPs {
			s = strings.TrimSpace(s)
			if _, ipnet, err := net.ParseCIDR(s); err == nil {
				c.adminNets = append(c.adminNets, ipnet)
				continue
			}
			if ip := net.ParseIP(s); ip != nil {
				if v4 := ip.To4(); v4 != nil {
					c.adminNets = append(c.adminNets, &net.IPNet{IP: v4, Mask: net.CIDRMask(32, 32)})
				} else {
					c.adminNets = append(c.adminNets, &net.IPNet{IP: ip, Mask: net.CIDRMask(128, 128)})
				}
				continue
			}
			return nil, fmt.Errorf("config: adminAccess.ips 条目 %q 不是合法 IP 或 CIDR", s)
		}
	}
	// 数据根目录（0.7.0）：其余落盘路径未显式配置时由它派生；取绝对路径，
	// 避免多实例/换工作目录后语义漂移（0.6.x 旧配置的显式值原样生效，不受影响）。
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if abs, err := AbsClean(c.DataDir); err == nil {
		c.DataDir = abs
	}
	// 元数据库（0.3.0）：driver 空或 "none" = 降级模式（仅匿名只读）；
	// 非法值（含 0.2.x 的 json/memory）直接报错，给出明确指引。
	dbDriver, err := store.NormalizeDriver(c.DB.Driver)
	if err != nil {
		return nil, err
	}
	if dbDriver == store.DriverSQLite && c.DB.DSN == "" {
		c.DB.DSN = filepath.Join(c.DataDir, "filemanage.db")
	}
	if dbDriver != "" && c.DB.DSN == "" {
		return nil, fmt.Errorf("config: db.dsn is required for driver %q", c.DB.Driver)
	}
	if c.Cache.Enabled && dbDriver == "" {
		return nil, fmt.Errorf("config: cache.enabled 需要已配置元数据库（降级模式下无元数据可缓存）")
	}
	if c.Cache.Enabled {
		if c.Cache.Redis.Addr == "" {
			c.Cache.Redis.Addr = "127.0.0.1:6379"
		}
		if c.Cache.Redis.UserTTLSeconds <= 0 {
			c.Cache.Redis.UserTTLSeconds = 300
		}
		if c.Cache.Redis.ACLTTLSeconds <= 0 {
			c.Cache.Redis.ACLTTLSeconds = 60
		}
	}
	// 未显式配置的落盘路径从 DataDir 派生（0.7.0）
	if c.TrashDir == "" {
		c.TrashDir = filepath.Join(c.DataDir, "trash")
	}
	if c.UploadStagingDir == "" {
		c.UploadStagingDir = filepath.Join(c.DataDir, "staging")
	}
	if c.SecretFile == "" {
		c.SecretFile = filepath.Join(c.DataDir, "jwt.secret")
	}
	if c.TrashRetentionDays <= 0 {
		c.TrashRetentionDays = 30
	}
	if c.Session.AccessTTL == "" {
		c.Session.AccessTTL = "1h"
	}
	if c.Session.IdleDays <= 0 {
		c.Session.IdleDays = 21
	}
	if c.Versions.MaxVersions <= 0 {
		c.Versions.MaxVersions = 10
	}
	if c.Versions.Dir == "" {
		c.Versions.Dir = filepath.Join(c.DataDir, "versions")
	}
	if c.AI.Provider == "" {
		c.AI.Provider = "none"
	}
	if c.AI.MaxFileMB <= 0 {
		c.AI.MaxFileMB = 20
	}
	if c.InstanceID == "" {
		if h, err := os.Hostname(); err == nil {
			c.InstanceID = h
		} else {
			c.InstanceID = "default"
		}
	}
	// JWT 密钥：显式配置优先；否则从 SecretFile 读取/生成并持久化，
	// 使同机多实例（共享数据目录）或重启后 token 仍有效。
	if c.JWTSecret == "" {
		sec, err := ensureSecretFile(c.SecretFile)
		if err != nil {
			return nil, err
		}
		c.JWTSecret = sec
	}
	if len(c.Roots) == 0 {
		return nil, fmt.Errorf("config: at least one roots entry is required")
	}
	seen := map[string]bool{}
	for i := range c.Roots {
		r := &c.Roots[i]
		if r.ID == "" || r.Path == "" {
			return nil, fmt.Errorf("config: root entry needs id and path")
		}
		if strings.ContainsRune(r.ID, ':') {
			// 0.5.0：':' 是虚拟根 id（physID:subPath）的分隔符，物理根 id 禁止使用
			return nil, fmt.Errorf("config: root id %q must not contain ':'", r.ID)
		}
		if strings.HasPrefix(r.ID, "_") {
			// 0.7.0："_" 前缀保留给服务端内部合成根（如用户主目录根 "_users"）
			return nil, fmt.Errorf("config: root id %q is reserved (leading '_')", r.ID)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("config: duplicate root id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Name == "" {
			r.Name = r.ID
		}
		abs, err := AbsClean(r.Path)
		if err != nil {
			return nil, err
		}
		r.Path = abs
	}
	return c, nil
}

// UsersDir 用户主目录的父目录（0.7.0）：<dataDir>/Users。
// 该目录作为服务端内部合成根（store.HomeRootID）注入 scope，不写回配置文件 roots。
func (c *Config) UsersDir() string { return filepath.Join(c.DataDir, "Users") }

// HomeDir 某用户的家目录绝对路径（用户名须先经 fileops.UsernameAsDir 校验）。
func (c *Config) HomeDir(username string) string { return filepath.Join(c.UsersDir(), username) }

// TLSDir 自签证书目录（0.7.0 起归入 dataDir，替代原硬编码 ./data/tls）。
func (c *Config) TLSDir() string { return filepath.Join(c.DataDir, "tls") }

// AdminAccountFile 超级管理员账户文件（0.1.0 需求32）：<dataDir>/admin-account.json。
// 与 jwt.secret 同理属运行数据：多实例共享部署需放共享卷或逐节点保持一致。
func (c *Config) AdminAccountFile() string { return filepath.Join(c.DataDir, "admin-account.json") }

// AdminAllows 判定管理端口是否放行该来源 IP（0.1.0 需求38；调用方必须传
// RemoteAddr 解析出的地址，绝不接受 X-Forwarded-For）。lan 分支与用户端口
// lanFilter 的 isLANIP 判定逐字一致。
func (c *Config) AdminAllows(ip net.IP) bool {
	switch c.AdminAccess.Mode {
	case "any":
		return true
	case "lan":
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	case "custom":
		for _, n := range c.adminNets {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	default: // local 与未知取值的安全兜底
		return ip.IsLoopback()
	}
}

// AccessTTLDuration 解析访问令牌有效期。
func (c *Config) AccessTTLDuration() time.Duration {
	d, err := time.ParseDuration(c.Session.AccessTTL)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// IdleDuration 返回 refresh 会话闲置上限（默认 3 周）。
func (c *Config) IdleDuration() time.Duration {
	return time.Duration(c.Session.IdleDays) * 24 * time.Hour
}

func ensureSecretFile(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
		return string(b[:64]), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	sec := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(sec), 0o600); err != nil {
		return "", err
	}
	return sec, nil
}

// AbsClean 返回绝对路径（不要求存在）。
func AbsClean(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// WriteExample 生成示例配置文件（首次运行）。
func WriteExample(path string) error {
	example := map[string]any{
		"schemaVersion":      CurrentSchema,
		"listen":             "0.0.0.0:8484",
		"adminListen":        "127.0.0.1:8485", // 管理面独立端口（/admin/ 与 admin API 仅此端口可达）
		"lanOnly":            true,
		"instanceId":         "node0",
		"dataDir":            "./data",                                          // 运行数据根目录（0.7.0）：trash/staging/versions/jwt.secret/sqlite 及 Users/<用户名> 家目录的派生基准
		"db":                 map[string]string{"driver": "sqlite3", "dsn": ""}, // dsn 留空=默认 <dataDir>/filemanage.db；"none"=降级模式（仅匿名只读）；亦支持 mysql/postgresql（可在管理台"数据库"页配置并热切换，0.7.0）
		"cache":              map[string]any{"enabled": false, "redis": map[string]any{"addr": "127.0.0.1:6379", "password": "", "db": 0, "userTTLSeconds": 300, "aclTTLSeconds": 60}},
		"roots":              []map[string]string{{"id": "main", "name": "主目录", "path": "./data/root-main"}},
		"trashDir":           "./data/trash",
		"uploadStagingDir":   "./data/staging",
		"trashRetentionDays": 30,
		"maxUploadBytes":     2147483648,
		"adminAccess":        map[string]any{"mode": "local", "ips": []string{}}, // 管理界面 IP 准入（需求38）：local 仅本机 / lan 局域网 / any 任何来源 / custom 指定 ips（IP 或 CIDR 列表）
		"adminPassword":      "",                                                  // 超管口令重置触发器（0.1.0 需求32）：填入明文→启动时写入超管账户文件并提示清空；平时留空
		"ai":                 map[string]any{"provider": "none", "baseUrl": "", "apiKey": "", "model": "", "maxFileMB": 20},
		"tls":                map[string]any{"enabled": false, "certFile": "", "keyFile": "", "autoSelfSign": true},
		"session":            map[string]any{"accessTTL": "1h", "idleDays": 21},
		"anonymous":          map[string]any{"enabled": true},
		"rateLimit":          map[string]any{"uploadKBps": 0, "downloadKBps": 0, "qps": 0},
		"corsAllowOrigins":   []string{},
		"versions":           map[string]any{"enabled": true, "maxVersions": 10, "dir": "./data/versions"},
		"balancer":           map[string]any{"workers": []string{}},
	}
	b, _ := json.MarshalIndent(example, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

// PersistDBEdit 以最小侵入方式改写配置文件中的 db 段（0.7.0 管理台配库）。
// 读原始 JSON → 只替换 db.driver/db.dsn → MarshalIndent 回写，先备份
// <path>.bak-<时间戳>（复用 LoadConfig 迁移回写模式）。未知字段保留；
// **绝不整体序列化 Config**——那会把运行期物化的 jwtSecret 明文写回文件。
func PersistDBEdit(path, driver, dsn string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	db, _ := raw["db"].(map[string]any)
	if db == nil {
		db = map[string]any{}
	}
	db["driver"] = driver
	db["dsn"] = dsn
	raw["db"] = db
	nb, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	backup := fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(backup, b, 0o644); err != nil {
		return fmt.Errorf("backup config: %w", err)
	}
	return os.WriteFile(path, nb, 0o644)
}

// WriteBalancerExample 生成 balancer 模式示例配置。
func WriteBalancerExample(path string) error {
	example := map[string]any{
		"schemaVersion": CurrentSchema,
		"listen":        "0.0.0.0:8480",
		"lanOnly":       false,
		"instanceId":    "balancer0",
		"balancer":      map[string]any{"workers": []string{"127.0.0.1:8484", "127.0.0.1:8485"}},
		"roots":         []map[string]string{{"id": "placeholder", "name": "占位", "path": "./data"}},
	}
	b, _ := json.MarshalIndent(example, "", "  ")
	return os.WriteFile(path, b, 0o644)
}
