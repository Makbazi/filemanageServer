package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// CurrentSchema 当前配置 schema 版本。
// 每次配置结构发生不兼容变化时 +1，并在下方追加对应迁移函数（需求25：
// 服务端安全升级——旧配置文件自动迁移、原文件备份、未知字段保留）。
const CurrentSchema = 8

// MigrateMap 对通用 map 形式的配置执行迁移链，返回是否发生变化。
// 缺失 schemaVersion 的旧配置视为 v1。
func MigrateMap(m map[string]any) (bool, error) {
	ver := 1
	if v, ok := m["schemaVersion"].(float64); ok && v > 0 {
		ver = int(v)
	}
	if ver > CurrentSchema {
		return false, fmt.Errorf("config schemaVersion %d is newer than supported %d (请升级服务端)", ver, CurrentSchema)
	}
	changed := false
	for ver < CurrentSchema {
		switch ver {
		case 1:
			changed = migrate1to2(m) || changed
			ver = 2
		case 2:
			changed = migrate2to3(m) || changed
			ver = 3
		case 3:
			changed = migrate3to4(m) || changed
			ver = 4
		case 4:
			changed = migrate4to5(m) || changed
			ver = 5
		case 5:
			changed = migrate5to6(m) || changed
			ver = 6
		case 6:
			changed = migrate6to7(m) || changed
			ver = 7
		case 7:
			changed = migrate7to8(m) || changed
			ver = 8
		default:
			return changed, fmt.Errorf("no migration path from schemaVersion %d", ver)
		}
	}
	if existing, _ := m["schemaVersion"].(float64); int(existing) != CurrentSchema {
		m["schemaVersion"] = CurrentSchema
		changed = true
	}
	return changed, nil
}

// migrate1to2 v1（0.1.x 平铺配置）→ v2（0.2.0）：
// 只补齐新增键的默认值，绝不覆盖/删除已有配置项。
func migrate1to2(m map[string]any) bool {
	changed := false
	setIfAbsent := func(k string, v any) {
		if _, ok := m[k]; !ok {
			m[k] = v
			changed = true
		}
	}
	setIfAbsent("instanceId", "")
	setIfAbsent("secretFile", "./data/jwt.secret")
	setIfAbsent("uploadStagingDir", "./data/staging")
	setIfAbsent("trashRetentionDays", 30)
	setIfAbsent("adminPassword", "")
	setIfAbsent("tls", map[string]any{"enabled": false, "certFile": "", "keyFile": "", "autoSelfSign": true})
	setIfAbsent("session", map[string]any{"accessTTL": "1h", "idleDays": 21})
	setIfAbsent("anonymous", map[string]any{"enabled": true})
	setIfAbsent("rateLimit", map[string]any{"uploadKBps": 0, "downloadKBps": 0})
	setIfAbsent("versions", map[string]any{"enabled": true, "maxVersions": 10, "dir": "./data/versions"})
	setIfAbsent("balancer", map[string]any{"workers": []any{}})
	// jwtSecret 保持原语义；未显式配置时由 secretFile 机制接管
	return changed
}

// migrate2to3 v2（0.2.x）→ v3（0.3.0）：
//  1. db.driver json|memory → sqlite3（json/memory 驱动已随 filemanageDb 并入而移除，
//     0.2.x 的 meta.json 元数据不再读取——破坏性变更见 CHANGELOG）；
//     *.json 的 dsn 改写为同目录同名 .db；自定义 sqlite 路径保持不变。
//  2. 新增 cache（redis）配置段，默认关闭。
//     注意：本迁移只改写"值为 json/memory"的配置，绝不覆盖用户已有的 sqlite/mysql 配置。
func migrate2to3(m map[string]any) bool {
	changed := false
	if raw, ok := m["db"].(map[string]any); ok {
		if drv, _ := raw["driver"].(string); drv == "json" || drv == "memory" {
			raw["driver"] = "sqlite3"
			changed = true
			if dsn, _ := raw["dsn"].(string); drv == "memory" || dsn == "" {
				raw["dsn"] = "./data/filemanage.db"
			} else if strings.HasSuffix(dsn, ".json") {
				raw["dsn"] = strings.TrimSuffix(dsn, ".json") + ".db"
			}
		}
	}
	if _, ok := m["cache"]; !ok {
		m["cache"] = map[string]any{
			"enabled": false,
			"redis": map[string]any{
				"addr": "127.0.0.1:6379", "password": "", "db": 0,
				"userTTLSeconds": 300, "aclTTLSeconds": 60,
			},
		}
		changed = true
	}
	return changed
}

// migrate3to4 v3（0.3.x）→ v4（0.3.1）：新增 maxUploadBytes
// （单次上传大小硬上限，字节，0=不限）。只补默认键，绝不覆盖已有值（需求25）。
func migrate3to4(m map[string]any) bool {
	if _, ok := m["maxUploadBytes"]; !ok {
		m["maxUploadBytes"] = float64(2147483648) // 2 GiB
		return true
	}
	return false
}

// migrate4to5 v4（0.3.1）→ v5（0.5.0）：新增 adminListen（管理面独立端口，
// 管理台与 admin API 从用户端口迁出）。只补默认键，绝不覆盖已有值（需求25）。
func migrate4to5(m map[string]any) bool {
	if _, ok := m["adminListen"]; !ok {
		m["adminListen"] = "127.0.0.1:8485"
		return true
	}
	return false
}

// migrate5to6 v5（0.5.0）→ v6（0.5.1）：rateLimit 补 qps 键、顶层补 corsAllowOrigins。
// 只补缺省，绝不覆盖已有值（需求25）。
func migrate5to6(m map[string]any) bool {
	changed := false
	if rl, ok := m["rateLimit"].(map[string]any); ok {
		if _, has := rl["qps"]; !has {
			rl["qps"] = float64(0)
			changed = true
		}
	} else {
		m["rateLimit"] = map[string]any{"uploadKBps": float64(0), "downloadKBps": float64(0), "qps": float64(0)}
		changed = true
	}
	if _, ok := m["corsAllowOrigins"]; !ok {
		m["corsAllowOrigins"] = []any{}
		changed = true
	}
	return changed
}

// migrate6to7 v6（0.5.1/0.6.x）→ v7（0.7.0）：新增 dataDir（运行数据根目录，
// 派生 trash/staging/versions/secret/sqlite 默认路径与 <dataDir>/Users 用户主目录）。
// 只补默认键（需求25）：v6 配置本就带 trashDir/uploadStagingDir/secretFile/
// versions.dir 显式值（v1-v6 迁移链所种），这些显式值原样保留、优先于派生，
// 因此 dataDir:"./data" 与旧配置的落盘位置完全一致，升级零移动。
func migrate6to7(m map[string]any) bool {
	if _, ok := m["dataDir"]; !ok {
		m["dataDir"] = "./data"
		return true
	}
	return false
}

// migrate7to8 v7（0.7.x）→ v8（0.1.0）：新增 adminAccess（需求38 管理界面 IP 准入）。
// 存量配置补 {"mode":"lan"}：与 0.7.x 管理端口沿用 lanOnly=true（默认）时的
// 判定语义逐字等价（回环+私网+link-local 放行），升级零行为变化（需求25）。
// 新装配置不经过本函数，直接吃 DefaultConfig 的 "local"（仅本机，更安全）。
func migrate7to8(m map[string]any) bool {
	if _, ok := m["adminAccess"]; !ok {
		m["adminAccess"] = map[string]any{"mode": "lan", "ips": []any{}}
		return true
	}
	return false
}

// LoadBalancerConfig 加载 balancer 模式配置：不要求 roots，不生成 JWT 密钥。
func LoadBalancerConfig(path string) (*Config, error) {
	c := DefaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err == nil {
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
			if _, err := MigrateMap(raw); err != nil {
				return nil, err
			}
			migrated, _ := json.Marshal(raw)
			c = DefaultConfig()
			if err := json.Unmarshal(migrated, c); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
		}
	}
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8480"
	}
	if c.InstanceID == "" {
		c.InstanceID = "balancer-" + time.Now().Format("150405")
	}
	if len(c.Balancer.Workers) == 0 {
		return nil, fmt.Errorf("config: balancer.workers is required in balancer mode")
	}
	return c, nil
}
