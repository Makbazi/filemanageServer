package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	filemanagedb "filemanageDb"
)

// server 聚合依赖：配置、元数据存储（仅通过 filemanageDb 接口）、AI provider。
type server struct {
	cfg   *Config
	store filemanagedb.Store
	ai    AIProvider
	roots map[string]*RootCfg
}

func main() {
	cfgPath := flag.String("config", "server.json", "配置文件路径")
	flag.Parse()

	if _, err := os.Stat(*cfgPath); err != nil && *cfgPath == "server.json" {
		if err := writeExampleConfig(*cfgPath); err != nil {
			log.Printf("warn: %v", err)
		} else {
			log.Printf("未找到配置文件，已生成示例 %s（请编辑 roots 后重启）", *cfgPath)
		}
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	st, err := filemanagedb.Open(filemanagedb.Options{Driver: cfg.DB.Driver, DSN: cfg.DB.DSN})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	s := &server{cfg: cfg, store: st, ai: NewAIProvider(cfg.AI), roots: map[string]*RootCfg{}}
	for i := range cfg.Roots {
		s.roots[cfg.Roots[i].ID] = &cfg.Roots[i]
	}
	if err := os.MkdirAll(cfg.TrashDir, 0o755); err != nil {
		log.Fatalf("trash dir: %v", err)
	}

	mux := http.NewServeMux()
	// 公开接口
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	// 需认证接口
	mux.Handle("GET /api/v1/roots", s.auth(s.handleRoots))
	mux.Handle("GET /api/v1/files", s.auth(s.handleList))
	mux.Handle("GET /api/v1/stat", s.auth(s.handleStat))
	mux.Handle("POST /api/v1/mkdir", s.auth(s.handleMkdir))
	mux.Handle("POST /api/v1/rename", s.auth(s.handleRename))
	mux.Handle("POST /api/v1/move", s.auth(s.handleMove))
	mux.Handle("POST /api/v1/copy", s.auth(s.handleCopy))
	mux.Handle("POST /api/v1/delete", s.auth(s.handleDelete))
	mux.Handle("POST /api/v1/search", s.auth(s.handleNameSearch))
	mux.Handle("GET /api/v1/download", s.auth(s.handleDownload))
	mux.Handle("POST /api/v1/upload", s.auth(s.handleUpload))
	mux.Handle("GET /api/v1/trash", s.auth(s.handleTrashList))
	mux.Handle("POST /api/v1/trash/restore", s.auth(s.handleTrashRestore))
	mux.Handle("POST /api/v1/trash/remove", s.auth(s.handleTrashRemove))
	mux.Handle("POST /api/v1/trash/clear", s.auth(s.handleTrashClear))
	// AI（预留能力）
	mux.Handle("POST /api/v1/ai/describe", s.auth(s.handleAIDescribe))
	mux.Handle("GET /api/v1/ai/description", s.auth(s.handleAIGetDescription))
	mux.Handle("POST /api/v1/ai/search", s.auth(s.handleAISearch))
	// 管理员
	mux.Handle("GET /api/v1/admin/users", s.auth(s.requireAdmin(s.handleAdminListUsers)))
	mux.Handle("POST /api/v1/admin/users", s.auth(s.requireAdmin(s.handleAdminCreateUser)))
	mux.Handle("PUT /api/v1/admin/users/{id}", s.auth(s.requireAdmin(s.handleAdminUpdateUser)))
	mux.Handle("DELETE /api/v1/admin/users/{id}", s.auth(s.requireAdmin(s.handleAdminDeleteUser)))

	handler := s.lanFilter(mux)
	addr := cfg.Listen
	if cfg.LANOnly && (strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "localhost")) {
		log.Printf("提示: lanOnly=true 但监听 %s，仅本机能访问；局域网访问请监听 0.0.0.0:%s", addr, portOf(addr))
	}
	log.Printf("filemanageServer 启动: listen=%s lanOnly=%v db=%s roots=%d ai=%v",
		addr, cfg.LANOnly, st.Driver(), len(cfg.Roots), s.ai.Name())
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Fatal(http.Serve(ln, logger{mux: handler}))
}

type logger struct{ mux http.Handler }

func (l logger) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("%s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
	l.mux.ServeHTTP(w, r)
}

// lanFilter 默认只允许局域网（RFC1918 / ULA / loopback）客户端访问。
func (s *server) lanFilter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.LANOnly {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !isLANIP(ip) {
			writeErr(w, http.StatusForbidden, fmt.Errorf("access denied: server only allows LAN clients"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLANIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return "?"
}

// ---------- 通用工具 ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode[T any](r *http.Request, v *T) error {
	body := io.LimitReader(r.Body, 1<<20)
	return json.NewDecoder(body).Decode(v)
}

// absClean 返回绝对路径（不要求存在）。
func absClean(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func writeExampleConfig(path string) error {
	example := map[string]any{
		"listen":   "0.0.0.0:8484",
		"lanOnly":  true,
		"db":       map[string]string{"driver": "json", "dsn": "./data/meta.json"},
		"roots":    []map[string]string{{"id": "main", "name": "主目录", "path": "./data/root-main"}},
		"trashDir": "./data/trash",
		"ai": map[string]any{"provider": "none", "baseUrl": "", "apiKey": "", "model": "",
			"maxFileMB": 20},
	}
	b, _ := json.MarshalIndent(example, "", "  ")
	return os.WriteFile(path, b, 0o644)
}
