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

// SessionCfg 会话策略（需求22，0.2.3 起语义微调——不再有 JWT 访问令牌；
// 0.2.4 起滑动窗口收敛为分钟级）：
// AccessTTL = 管理台会话（超管/DB 管理员）绝对有效期（默认 1h，不滑动——
// 保持"控制台每小时重登"节奏）；IdleMinutes = 客户端用户会话的滑动闲置窗口
// （0.2.4 起默认 **60 分钟**：活跃即无感续命、超过 1h 未使用必须重新登录，
// 凭证字符串在续期中永不变化）；IdleDays 为旧窗口字段（21 天），已弃用——
// 仅为存量 v9 配置兼容保留，IdleMinutes>0 时一律以 IdleMinutes 为准。
type SessionCfg struct {
	AccessTTL   string `json:"accessTTL"`
	IdleDays    int    `json:"idleDays"`
	IdleMinutes int    `json:"idleMinutes"`
}

// AuthCfg 三层认证图容量与节奏（0.2.3，替代 JWT 方案；见 internal/auth/center.go）：
//   - MemMaxEntries 进程内 LRU 条目上限（默认 200000；超限按 LRU 驱逐）；
//   - MemTTL 进程内条目新鲜期（默认 "5m"；超龄回源 redis/持久层——多实例
//     撤权可见延迟的上界，单实例 flock 部署无此问题）；
//   - RedisMaxEntries redis 认证缓存条目上限（默认 5000000，按 lastUsed 近似
//     LRU 裁剪；仅 cache.enabled 时生效，条目 TTL=剩余有效期自然回收）；
//   - SweepInterval 清扫周期（默认 "10m"：过期回收三层 + 脏续期批量写回 +
//     撤权重试 + L2 索引修剪）；
//   - TouchWriteEvery 单条目滑动续期最小写回间隔（默认 "10m"；硬杀进程至多丢
//     该窗口内的续期进度 = 凭证提前到期，安全方向）。
type AuthCfg struct {
	MemMaxEntries   int    `json:"memMaxEntries"`
	MemTTL          string `json:"memTTL"`
	RedisMaxEntries int64  `json:"redisMaxEntries"`
	SweepInterval   string `json:"sweepInterval"`
	TouchWriteEvery string `json:"touchWriteEvery"`
}

// AnonymousCfg 匿名访问策略（需求6）。Enabled=true 时未登录客户端可
// 浏览（受目录 public ACL 约束），但一律不可下载/上传/修改/删除。
type AnonymousCfg struct {
	Enabled bool `json:"enabled"`
}

// AdminAccessCfg 管理界面 IP 准入策略（0.1.0 需求38），仅作用于管理端口
// （adminListen 引擎，含 /admin/ 静态台与全部 admin/配库 API）：
//   - local  只允许本机回环地址（新装默认）
//   - lan    允许局域网（回环 + RFC1918/ULA/link-local，= lanOnly 语义；
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
// QPS 为每 IP 每秒请求数上限（安全迭代，0=关闭）。
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

// LogConf 服务端日志（0.2.7，见 internal/logx）：等级/目录/轮转上限。
// 落盘文件含当前文件在内最多 MaxFiles 个（默认 3）、每个 ≤ MaxMB MB（默认 10），
// 按大小轮转改名链 .log.1/.log.2/…；stdout 同步镜像。敏感值（口令/令牌/
// 请求体/头）永不入日志——纪律见 logx 包注释，等级默认 warn。
type LogConf struct {
	Level    string `json:"level"`    // debug|info|warn|error，默认 warn
	Dir      string `json:"dir"`      // 空=dataDir/logs
	MaxMB    int    `json:"maxMB"`    // 单文件上限 MB，默认 10
	MaxFiles int    `json:"maxFiles"` // 最多保留文件数（含当前），默认 3
}

// DBCfg 元数据库配置。Driver 取值：
// "file"（含别名 ""/"none"，0.1.1 起为默认：未配置数据库时使用内置文件存储，
// DSN=存储目录，留空派生 <dataDir>/filestore，全功能可用、仅单实例）|
// sqlite3 | mysql | postgresql（含 sqlite/postgres/pg/mariadb 别名，见 store.Open）。
// 旧的"无库降级模式（仅匿名只读）"已在 0.1.1 取消；0.2.x 的 json/memory 驱动已移除。
type DBCfg struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"` // file=存储目录 / sqlite3 文件路径 / mysql 与 postgres DSN
}

// CacheCfg 可选 redis 缓存（非必须）。Enabled=false（默认）时不启用，
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

// Config 服务端配置（schemaVersion=11）。
type Config struct {
	SchemaVersion int `json:"schemaVersion"`

	// DataDir 服务端运行数据根目录：未显式配置的 trashDir/
	// uploadStagingDir/secretFile/versions.dir/sqlite 默认 DSN 均由其派生；
	// 用户主目录固定位于 <dataDir>/Users/<用户名>（建用户自动分配，需求3）。
	DataDir string `json:"dataDir"`

	Listen      string `json:"listen"`      // 默认 0.0.0.0:8484（用户/客户端端口，不含任何管理路由）
	AdminListen string `json:"adminListen"` // 管理面独立端口：/admin/ 界面与全部 admin API 仅此端口可达；默认 bind 127.0.0.1（服务端本机）
	LANOnly     bool   `json:"lanOnly"`     // 默认 true：只接受局域网客户端

	// AdminAccess 管理界面 IP 准入策略（0.1.0 需求38）：local/lan/any/custom，
	// 仅作用于管理端口引擎。默认 local（新装）；migrate7to8 给存量配置补 lan。
	AdminAccess AdminAccessCfg `json:"adminAccess"`

	// adminNets 是 AdminAccess.Mode=custom 时 IPs 预解析出的网段（finalize 填充，
	// 不来自 JSON）。运行期只读。
	adminNets []*net.IPNet

	// CORSAllowOrigins 显式跨域来源白名单（可选）。空（默认）= 不下发任何
	// CORS 头（浏览器跨域读取天然被拒，与旧行为一致）；支持条目 "*"。
	CORSAllowOrigins []string `json:"corsAllowOrigins"`
	// JWTSecret/SecretFile 已弃用（0.2.3 认证中心取代 JWT，认证不再读取二者）。
	// 字段与 jwt.secret 物化行为刻意保留：存量配置文件原样可加载、PersistDBEdit
	// "绝不整体序列化 Config"的防泄密纪律保真；新部署可自行删除这两项。
	JWTSecret  string `json:"jwtSecret"`
	SecretFile string `json:"secretFile"`
	InstanceID string `json:"instanceId"` // 多实例部署标识（需求26），默认取主机名

	DB    DBCfg    `json:"db"`
	Cache CacheCfg `json:"cache"` // 可选 redis 缓存

	Roots              []RootCfg `json:"roots"`
	TrashDir           string    `json:"trashDir"`
	UploadStagingDir   string    `json:"uploadStagingDir"`   // 分片上传暂存目录（多实例需共享）
	TrashRetentionDays int       `json:"trashRetentionDays"` // 回收站保留天数（需求4），默认 30
	MaxUploadBytes     int64     `json:"maxUploadBytes"`     // 单次上传大小硬上限（字节，0=不限），默认 2GiB
	AdminPassword      string    `json:"adminPassword"`      // 超管口令重置触发器（需求32，0.1.0）：非空=启动时以其为明文重置超管账户文件，用后清空

	AI        AICfg        `json:"ai"`
	TLS       TLSCfg       `json:"tls"`
	Session   SessionCfg   `json:"session"`
	Auth      AuthCfg      `json:"auth"` // 三层认证图（0.2.3）
	Log       LogConf      `json:"log"`  // 服务端日志（0.2.7：等级/目录/轮转上限）
	Anonymous AnonymousCfg `json:"anonymous"`
	RateLimit RateLimitCfg `json:"rateLimit"`
	Versions  VersionsCfg  `json:"versions"`
	Balancer  BalancerCfg  `json:"balancer"`
}

// DefaultConfig 返回带全部默认值的配置。
func DefaultConfig() *Config {
	// 注意：此处刻意**不**给 SecretFile/TrashDir/UploadStagingDir/
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
		Session:            SessionCfg{AccessTTL: "1h", IdleDays: 21, IdleMinutes: 60},
		Auth:               AuthCfg{MemMaxEntries: 200000, MemTTL: "5m", RedisMaxEntries: 5000000, SweepInterval: "10m", TouchWriteEvery: "10m"},
		Log:                LogConf{Level: "warn", MaxMB: 10, MaxFiles: 3}, // Dir 留空=<dataDir>/logs（finalize/LogDir 派生）
		Anonymous:          AnonymousCfg{Enabled: true},
		Versions:           VersionsCfg{Enabled: true, MaxVersions: 10},
		TLS:                TLSCfg{AutoSelfSign: true},
		AdminAccess:        AdminAccessCfg{Mode: "local"}, // 新装默认仅本机；存量配置 migrate7to8 补 lan
	}
	c.DB.Driver = "file" // 0.1.1：新装默认内置文件存储（配库走管理台"数据库"页，可携数据迁移）
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
	// 数据根目录：其余落盘路径未显式配置时由它派生；取绝对路径，
	// 避免多实例/换工作目录后语义漂移（旧配置的显式值原样生效，不受影响）。
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if abs, err := AbsClean(c.DataDir); err == nil {
		c.DataDir = abs
	}
	// 元数据库（0.1.1 起空或 "none" = 内置文件存储，全功能可用，不再降级）；
	// 非法值（含 0.2.x 的 json/memory）直接报错，给出明确指引。
	dbDriver, err := store.NormalizeDriver(c.DB.Driver)
	if err != nil {
		return nil, err
	}
	if dbDriver == store.DriverFile && c.DB.DSN == "" {
		c.DB.DSN = filepath.Join(c.DataDir, "filestore") // 文件存储目录默认（类比 sqlite 默认 DSN）
	}
	if dbDriver == store.DriverSQLite && c.DB.DSN == "" {
		c.DB.DSN = filepath.Join(c.DataDir, "filemanage.db")
	}
	if dbDriver != "" && c.DB.DSN == "" {
		return nil, fmt.Errorf("config: db.dsn is required for driver %q", c.DB.Driver)
	}
	if c.Cache.Enabled && dbDriver == "" {
		return nil, fmt.Errorf("config: cache.enabled 需要已配置元数据存储")
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
	// 未显式配置的落盘路径从 DataDir 派生
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
	if c.Session.IdleMinutes <= 0 {
		c.Session.IdleMinutes = 60 // 0.2.4：用户会话滑动闲置窗口默认 1h（需求22 微调）
	}
	// 三层认证图默认（0.2.3）
	if c.Auth.MemMaxEntries <= 0 {
		c.Auth.MemMaxEntries = 200000
	}
	if c.Auth.MemTTL == "" {
		c.Auth.MemTTL = "5m"
	}
	if c.Auth.RedisMaxEntries <= 0 {
		c.Auth.RedisMaxEntries = 5000000
	}
	if c.Auth.SweepInterval == "" {
		c.Auth.SweepInterval = "10m"
	}
	if c.Auth.TouchWriteEvery == "" {
		c.Auth.TouchWriteEvery = "10m"
	}
	// 日志（0.2.7）：等级非法值收敛为 warn（默认等级，不因配置笔误拒绝启动）；
	// 轮转上限收敛——maxMB<=0→10、maxFiles<1→3、>10→10（防失控占用磁盘）。
	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	default:
		c.Log.Level = "warn"
	}
	if c.Log.MaxMB <= 0 {
		c.Log.MaxMB = 10
	}
	if c.Log.MaxFiles < 1 {
		c.Log.MaxFiles = 3
	}
	if c.Log.MaxFiles > 10 {
		c.Log.MaxFiles = 10
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
			// ':' 是虚拟根 id（physID:subPath）的分隔符，物理根 id 禁止使用
			return nil, fmt.Errorf("config: root id %q must not contain ':'", r.ID)
		}
		if strings.HasPrefix(r.ID, "_") {
			// "_" 前缀保留给服务端内部合成根（如用户主目录根 "_users"）
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

// UsersDir 用户主目录的父目录：<dataDir>/Users。
// 该目录作为服务端内部合成根（store.HomeRootID）注入 scope，不写回配置文件 roots。
func (c *Config) UsersDir() string { return filepath.Join(c.DataDir, "Users") }

// HomeDir 某用户的家目录绝对路径（用户名须先经 fileops.UsernameAsDir 校验）。
func (c *Config) HomeDir(username string) string { return filepath.Join(c.UsersDir(), username) }

// TLSDir 自签证书目录（归入 dataDir，替代原硬编码 ./data/tls）。
func (c *Config) TLSDir() string { return filepath.Join(c.DataDir, "tls") }

// LogDir 服务端日志目录（0.2.7）：log.dir 显式配置优先，否则 <dataDir>/logs。
func (c *Config) LogDir() string {
	if c.Log.Dir != "" {
		return c.Log.Dir
	}
	return filepath.Join(c.DataDir, "logs")
}

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

// AccessTTLDuration 解析管理台会话绝对有效期（0.2.3 语义：客户端用户会话
// 改走滑动窗口，见 IdleDuration；本值仅约束超管/DB 管理员登录会话）。
func (c *Config) AccessTTLDuration() time.Duration {
	d, err := time.ParseDuration(c.Session.AccessTTL)
	if err != nil || d <= 0 {
		return time.Hour
	}
	return d
}

// IdleDuration 返回用户会话滑动闲置上限（0.2.4 起默认 **1 小时**，每次认证
// 请求无感续期、凭证字符串不变）。IdleMinutes>0 以分钟窗口为准；仅当
// IdleMinutes 未设置（异常旧配置绕过迁移直读）时回退旧 idleDays 天窗口。
func (c *Config) IdleDuration() time.Duration {
	if c.Session.IdleMinutes > 0 {
		return time.Duration(c.Session.IdleMinutes) * time.Minute
	}
	return time.Duration(c.Session.IdleDays) * 24 * time.Hour
}

// AuthMemTTL 解析进程内认证图条目新鲜期（非法值回落 5m）。
func (c *Config) AuthMemTTL() time.Duration {
	d, err := time.ParseDuration(c.Auth.MemTTL)
	if err != nil || d <= 0 {
		return 5 * time.Minute
	}
	return d
}

// AuthSweepInterval 解析清扫周期（非法值回落 10m）。
func (c *Config) AuthSweepInterval() time.Duration {
	d, err := time.ParseDuration(c.Auth.SweepInterval)
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}

// AuthTouchWriteEvery 解析单条目续期最小写回间隔（非法值回落 10m）。
func (c *Config) AuthTouchWriteEvery() time.Duration {
	d, err := time.ParseDuration(c.Auth.TouchWriteEvery)
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
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
		"dataDir":            "./data",                                       // 运行数据根目录：trash/staging/versions/jwt.secret/sqlite 及 Users/<用户名> 家目录的派生基准
		"db":                 map[string]string{"driver": "file", "dsn": ""}, // 默认内置文件存储（全功能，单实例）；dsn 留空=<dataDir>/filestore。配数据库（sqlite3/mysql/postgresql 或管理台"数据库"页热切换）后可携数据迁移；无库降级模式已于 0.1.1 取消
		"cache":              map[string]any{"enabled": false, "redis": map[string]any{"addr": "127.0.0.1:6379", "password": "", "db": 0, "userTTLSeconds": 300, "aclTTLSeconds": 60}},
		"roots":              []map[string]string{{"id": "main", "name": "主目录", "path": "./data/root-main"}},
		"trashDir":           "./data/trash",
		"uploadStagingDir":   "./data/staging",
		"trashRetentionDays": 30,
		"maxUploadBytes":     2147483648,
		"adminAccess":        map[string]any{"mode": "local", "ips": []string{}}, // 管理界面 IP 准入（需求38）：local 仅本机 / lan 局域网 / any 任何来源 / custom 指定 ips（IP 或 CIDR 列表）
		"adminPassword":      "",                                                 // 超管口令重置触发器（0.1.0 需求32）：填入明文→启动时写入超管账户文件并提示清空；平时留空
		"ai":                 map[string]any{"provider": "none", "baseUrl": "", "apiKey": "", "model": "", "maxFileMB": 20},
		"tls":                map[string]any{"enabled": false, "certFile": "", "keyFile": "", "autoSelfSign": true},
		"session":            map[string]any{"accessTTL": "1h", "idleDays": 21},
		"auth":               map[string]any{"memMaxEntries": 200000, "memTTL": "5m", "redisMaxEntries": 5000000, "sweepInterval": "10m", "touchWriteEvery": "10m"}, // 三层认证图（0.2.3）：进程内 LRU 上限/新鲜期、redis 上限、清扫周期、续期写回间隔
		"log":                map[string]any{"level": "warn", "dir": "", "maxMB": 10, "maxFiles": 3},                                                                // 服务端日志（0.2.7）：等级 debug|info|warn|error（默认 warn）；dir 空=<dataDir>/logs；含当前文件在内最多 maxFiles 个、单文件 ≤maxMB MB 按大小轮转；敏感值（口令/令牌/请求体）永不入日志
		"anonymous":          map[string]any{"enabled": true},
		"rateLimit":          map[string]any{"uploadKBps": 0, "downloadKBps": 0, "qps": 0},
		"corsAllowOrigins":   []string{},
		"versions":           map[string]any{"enabled": true, "maxVersions": 10, "dir": "./data/versions"},
		"balancer":           map[string]any{"workers": []string{}},
	}
	b, _ := json.MarshalIndent(example, "", "  ")
	return os.WriteFile(path, b, 0o644)
}

// PersistDBEdit 以最小侵入方式改写配置文件中的 db 段（管理台配库）。
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
