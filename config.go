package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// RootCfg 服务端托管的一个根目录。所有文件操作被限制在这些目录内。
type RootCfg struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// AICfg AI 能力配置。provider: none | openai | ollama。
// openai 兼容任意 OpenAI Chat Completions 协议的服务（云厂商或本地网关）；
// ollama 使用本机 /api/chat 接口。二者均预留，未配置时 AI 功能返回明确提示。
type AICfg struct {
	Provider string `json:"provider"` // none | openai | ollama
	BaseURL  string `json:"baseUrl"`  // 如 https://api.openai.com/v1 或 http://127.0.0.1:11434
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	MaxFileMB int   `json:"maxFileMB"` // 送入模型的文件大小上限
}

// Config 服务端配置。
type Config struct {
	Listen    string       `json:"listen"`    // 默认 0.0.0.0:8484
	LANOnly   bool         `json:"lanOnly"`   // 默认 true：只接受局域网（私有地址）客户端
	JWTSecret string       `json:"jwtSecret"` // 为空时随机生成（重启后 token 失效）
	DB        struct {
		Driver string `json:"driver"` // memory | json | sqlite
		DSN    string `json:"dsn"`
	} `json:"db"`
	Roots    []RootCfg `json:"roots"`
	TrashDir string    `json:"trashDir"`
	AI       AICfg     `json:"ai"`
}

func defaultConfig() *Config {
	c := &Config{
		Listen:  "0.0.0.0:8484",
		LANOnly: true,
		TrashDir: "./data/trash",
	}
	c.DB.Driver = "json"
	c.DB.DSN = "./data/meta.json"
	c.AI.Provider = "none"
	c.AI.MaxFileMB = 20
	return c
}

// LoadConfig 读取 JSON 配置文件（不存在则用默认值），并补齐缺省字段。
func LoadConfig(path string) (*Config, error) {
	c := defaultConfig()
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8484"
	}
	if c.DB.Driver == "" {
		c.DB.Driver = "json"
	}
	if c.TrashDir == "" {
		c.TrashDir = "./data/trash"
	}
	if c.JWTSecret == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		c.JWTSecret = hex.EncodeToString(b)
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
		if seen[r.ID] {
			return nil, fmt.Errorf("config: duplicate root id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Name == "" {
			r.Name = r.ID
		}
		abs, err := absClean(r.Path)
		if err != nil {
			return nil, err
		}
		r.Path = abs
	}
	if c.AI.Provider == "" {
		c.AI.Provider = "none"
	}
	if c.AI.MaxFileMB <= 0 {
		c.AI.MaxFileMB = 20
	}
	return c, nil
}
