// filemanage-server 入口（需求1：cmd/internal 标准布局）。
//
// 运行模式：
//
//	-mode worker    （默认）文件服务节点：REST API + 管理界面 /admin
//	-mode balancer  负载均衡节点（需求27）：轮询反向代理到多个 worker
//
// 用法：
//
//	./filemanage-server -config server.json
//	./filemanage-server -mode balancer -config balancer.json
//	./filemanage-server -v          打印版本信息（tag/commit_id/编译时间）后退出
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"filemanageServer/internal/api"
	"filemanageServer/internal/balancer"
	"filemanageServer/internal/cache"
	"filemanageServer/internal/config"
	"filemanageServer/internal/fileops"
	store "filemanageServer/internal/store"
	"filemanageServer/internal/superadmin"
	"filemanageServer/internal/tlsx"
	"filemanageServer/internal/version"
)

func main() {
	cfgPath := flag.String("config", "server.json", "配置文件路径")
	mode := flag.String("mode", "worker", "运行模式: worker | balancer")
	showVersion := flag.Bool("v", false, "打印版本信息（tag/commit_id/编译时间）后退出")
	flag.Parse()

	if *showVersion {
		fmt.Printf("filemanage-server %s (tag %s, commit %s, built %s)\n",
			version.Version, version.GitTag, version.GitCommit, version.BuildTime)
		return
	}

	switch *mode {
	case "balancer":
		runBalancer(*cfgPath)
	case "worker", "":
		runWorker(*cfgPath)
	default:
		log.Fatalf("unknown mode %q (worker|balancer)", *mode)
	}
}

// runBalancer 负载均衡模式（需求27）：独立进程，不访问数据库/文件。
func runBalancer(cfgPath string) {
	if _, err := os.Stat(cfgPath); err != nil {
		if err := config.WriteBalancerExample(cfgPath); err != nil {
			log.Fatalf("write example balancer config: %v", err)
		}
		log.Fatalf("未找到配置文件，已生成 balancer 示例 %s（请编辑 balancer.workers 后重启）", cfgPath)
	}
	cfg, err := config.LoadBalancerConfig(cfgPath)
	if err != nil {
		log.Fatalf("load balancer config: %v", err)
	}
	if err := balancer.Run(cfg.Listen, cfg.Balancer.Workers, cfg.LANOnly); err != nil {
		log.Fatal(err)
	}
}

// runWorker 文件服务节点（默认模式）。
func runWorker(cfgPath string) {
	if _, err := os.Stat(cfgPath); err != nil && cfgPath == "server.json" {
		if err := config.WriteExample(cfgPath); err != nil {
			log.Printf("warn: %v", err)
		} else {
			log.Printf("未找到配置文件，已生成示例 %s（请编辑 roots 后重启）", cfgPath)
		}
	}

	cfg, err := config.LoadConfig(cfgPath) // 含 schema 迁移与安全升级（需求25）
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// 超级管理员账户（0.1.0 需求32/34）：文件承载、不依赖元数据库，降级模式同样可登录。
	super := superadmin.New(cfg.AdminAccountFile())
	name, pw, created, err := super.EnsureCreated()
	if err != nil {
		log.Fatalf("init superadmin account: %v", err)
	}
	if created {
		log.Printf("已生成超级管理员账户 %s（文件 %s）：初始口令 %q —— 仅首次登录前为明文，登录后自动转密文",
			name, cfg.AdminAccountFile(), pw)
	}
	if cfg.AdminPassword != "" {
		// 需求32"通过文件重置"的配置面入口：adminPassword 非空 = 以该明文重置超管口令。
		// 纪元推进即吊销旧令牌；用后请清空该字段（不自动清除：配置回写有泄 jwtSecret
		// 结构风险，沿用"人工清空"的 0.7.x 恢复口令约定）。
		if _, serr := super.SetCredentials("", cfg.AdminPassword); serr != nil {
			log.Fatalf("reset superadmin password from config: %v", serr)
		}
		log.Printf("adminPassword: 已重置超级管理员 %q 的口令（明文，首次登录后转密文）；请清空配置文件中的 adminPassword 字段", name)
	}

	// 元数据库（0.3.0 起为 internal/store）。0.1.1 起未配置数据库时默认打开
	// 内置文件存储（<dataDir>/filestore，全功能可用）——旧"降级模式"取消；
	// 打开失败（目录损坏/被他实例 flock）仍 fail-fast 退出。
	// 0.7.0：外面套一层 store.Proxy——管理台"数据库"页可运行期热切换底层存储
	// （connect→建表→可选全表搬迁→原子换入）；装饰顺序 Open→Proxy→cache.Wrap(Proxy)。
	st, err := store.Open(store.Options{Driver: cfg.DB.Driver, DSN: cfg.DB.DSN})
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	proxy := store.NewProxy(st)
	defer proxy.Close()
	var eff store.Store = proxy
	var cch cache.Cache
	if !store.Degraded(proxy) {
		if err := st.Migrate(); err != nil {
			log.Fatalf("migrate: %v", err)
		}
		// 0.1.0 需求32：超管改由账户文件承载。0.7.x 曾在库内建恢复管理员
		// （常为 "admin"）——登录按超管优先匹配后该记录被永久遮蔽，告警提示处理。
		warnShadowedAdmins(st, super)
		// 0.5.0 jail 模型：把存量 AllowedRoots 一次性转为显式 user_roots 分配。
		migrateUserRoots(st, cfg)
		// 0.7.0 需求3（顺序在 jailmodel 之后：两者都是 SetUserRoots 全量替换语义）：
		// 存量用户补齐 <dataDir>/Users/<用户名> 家目录与 _users 分配（幂等标记）。
		if err := fileops.EnsureUserHomes(st, cfg.UsersDir()); err != nil {
			log.Printf("warn: 用户家目录迁移未完成（下次启动重试）: %v", err)
		}
		// 暴露面提示（0.1.0 改写）：引导注册已移除、超管恒在账户文件——
		// 对外开放不再有"首注抢管理员"风险，但管理端口 adminAccess=lan/any
		// 且超管口令弱时仍可被在线爆破，给出运维提醒。
		if cfg.AdminAccess.Mode != "local" {
			log.Printf("提示: 管理台 IP 准入为 %q（adminAccess）；对外暴露请确保超管口令足够强，或收紧为 local/custom（需求38）", cfg.AdminAccess.Mode)
		}
	} else {
		// 0.1.1 正常路径不可达（无库=文件存储恒可开）；保留作纵深防御提示。
		log.Printf("warn: 元数据存储不可用（异常路径：文件存储未打开成功？），依赖元数据的端点返回 503；管理台可在\"数据库\"页配置并热连接")
	}
	// 可选 redis 缓存（0.3.0，非必须）：启用时启动即 PING，连不上 fail-fast；
	// 装饰器包在 Proxy 之上（热切换发生在其下，0.7.0）。降级模式无法启用缓存；
	// 降级→有库的热连接不会拉起 redis（需配置 cache 后重启，见 README）。
	if cfg.Cache.Enabled && !store.Degraded(proxy) {
		rc, cerr := cache.New(cfg.Cache)
		if cerr != nil {
			log.Fatalf("cache: %v", cerr)
		}
		defer rc.Close()
		eff = cache.Wrap(proxy, rc)
		cch = rc
		log.Printf("redis 缓存已启用: addr=%s kind=%s", cfg.Cache.Redis.Addr, rc.Kind())
	}

	srv, err := api.NewServer(cfg, eff, proxy, cch, cfgPath, super)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	for _, dir := range []string{cfg.TrashDir, cfg.UploadStagingDir, cfg.Versions.Dir, cfg.UsersDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	// 需求37：后台清扫存量用户文件的执行位（大树不阻塞启动；写路径已即时剥离）
	go sweepExecBits(cfg)

	// 后台清理（需求4：回收站 30 天；需求22：过期会话；需求3：失效上传会话）
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cleaner := &fileops.Cleaner{
		Store:      eff,
		TrashDir:   cfg.TrashDir,
		UsersDir:   cfg.UsersDir(), // 0.7.0：家目录内 .fmtrash 实体清理
		Retention:  time.Duration(cfg.TrashRetentionDays) * 24 * time.Hour,
		StagingDir: cfg.UploadStagingDir,
		Interval:   time.Hour,
	}
	go cleaner.Run(ctx)

	handler := srv.Handler()
	adminHandler := srv.AdminHandler() // 0.5.0：管理面独立引擎（用户端口无任何管理路由）
	addr := cfg.Listen
	if cfg.LANOnly && (strings.HasPrefix(addr, "127.") || strings.HasPrefix(addr, "localhost")) {
		log.Printf("提示: lanOnly=true 但监听 %s，仅本机能访问；局域网访问请监听 0.0.0.0:%s", addr, portOf(addr))
	}
	dbDriver := "none(降级模式)"
	if proxy.Driver() != "" {
		dbDriver = proxy.Driver()
	}
	log.Printf("filemanageServer %s 启动: mode=worker listen=%s adminListen=%s lanOnly=%v tls=%v anonymous=%v db=%s instance=%s roots=%d ai=%v",
		version.Version, addr, cfg.AdminListen, cfg.LANOnly, cfg.TLS.Enabled, cfg.Anonymous.Enabled,
		dbDriver, cfg.InstanceID, len(cfg.Roots), srv.AI.Name())

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	lnAdmin, err := net.Listen("tcp", cfg.AdminListen)
	if err != nil {
		log.Fatalf("管理端口监听 %s 失败（0.5.0 起管理面独立端口，客户端不可达）: %v", cfg.AdminListen, err)
	}

	// TLS（需求3：传输过程加密）。管理端口同样承载（本机浏览器用 https 时信任自签）。
	if cfg.TLS.Enabled {
		certFile, keyFile := cfg.TLS.CertFile, cfg.TLS.KeyFile
		if certFile == "" || keyFile == "" {
			if !cfg.TLS.AutoSelfSign {
				log.Fatal("tls.enabled=true 但未配置 certFile/keyFile（或开启 autoSelfSign）")
			}
			certFile, keyFile, err = tlsx.EnsureSelfSigned(cfg.TLSDir()) // 0.7.0：归入 dataDir
			if err != nil {
				log.Fatalf("generate self-signed cert: %v", err)
			}
			log.Printf("已生成自签证书: %s / %s（客户端需信任或开启跳过校验）", certFile, keyFile)
		}
		log.Printf("TLS 已启用（HTTPS），访问地址请使用 https://；管理台: https://%s/admin/", cfg.AdminListen)
		go func() { log.Fatal(http.ServeTLS(lnAdmin, adminHandler, certFile, keyFile)) }()
		log.Fatal(http.ServeTLS(ln, handler, certFile, keyFile))
	}
	log.Printf("管理台（仅 %s 可达）: http://%s/admin/", cfg.AdminListen, cfg.AdminListen)
	go func() { log.Fatal(http.Serve(lnAdmin, adminHandler)) }()
	log.Fatal(http.Serve(ln, handler))
}

// migrateUserRoots 0.5.0 jail 模型一次性存量迁移（server_meta 标记幂等）：
// 普通用户的 AllowedRoots（含"空=全部"旧语义）转为显式 user_roots 分配（subPath=""，
// 等价旧行为，权限不缩小；管理员随后在管理台按目录收紧）。管理员不自动分配——
// 其用户端口访问面与管理面分离是本次模型的目标形态，物理根浏览只在管理台进行。
func migrateUserRoots(st store.Store, cfg *config.Config) {
	done, err := st.GetMeta("jailmodel.v5")
	if err != nil {
		log.Printf("warn: jail 模型迁移检查失败: %v", err)
		return
	}
	if done == "done" {
		return
	}
	users, err := st.ListUsers()
	if err != nil {
		log.Printf("warn: jail 模型迁移失败: %v", err)
		return
	}
	n := 0
	for _, u := range users {
		if u.Role == store.RoleAdmin {
			continue
		}
		exist, _ := st.ListUserRoots(u.ID)
		if len(exist) > 0 {
			continue
		}
		ids := u.AllowedRoots
		if len(ids) == 0 {
			for i := range cfg.Roots {
				ids = append(ids, cfg.Roots[i].ID)
			}
		}
		list := make([]*store.UserRoot, 0, len(ids))
		for _, id := range ids {
			if _, ok := serverHasRoot(cfg, id); !ok {
				continue
			}
			list = append(list, &store.UserRoot{UserID: u.ID, RootID: id})
		}
		if len(list) == 0 {
			continue
		}
		if err := st.SetUserRoots(u.ID, list); err != nil {
			log.Printf("warn: 用户 %s 的目录分配迁移失败: %v", u.Username, err)
			continue
		}
		n++
	}
	if err := st.SetMeta("jailmodel.v5", "done"); err != nil {
		log.Printf("warn: jail 模型迁移标记写入失败: %v", err)
		return
	}
	if n > 0 {
		log.Printf("jail 模型: 已为 %d 个存量用户生成显式目录分配（AllowedRoots → user_roots）", n)
	}
}

func serverHasRoot(cfg *config.Config, id string) (int, bool) {
	for i := range cfg.Roots {
		if cfg.Roots[i].ID == id {
			return i, true
		}
	}
	return -1, false
}

// warnShadowedAdmins 0.1.0 需求32：超管改存账户文件后，handleLogin 对超管
// 用户名优先匹配——库内同名遗留记录（0.7.x applyAdminPassword 创建的恢复
// 管理员）将永远无法登录，启动时告警提示管理员在管理台处理该死账户。
func warnShadowedAdmins(st store.Store, super *superadmin.Store) {
	name, err := super.Username()
	if err != nil {
		return
	}
	users, err := st.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		if u.Username == name {
			log.Printf("警告: 数据库中存在与超级管理员同名的账户 %q（id=%d，0.7.x 恢复口令流程遗留）——登录按超管优先匹配，该记录已无法登录，请在管理台确认后删除或改名其分配/权限", name, u.ID)
		}
	}
}

// sweepExecBits 需求37：启动后台清扫用户文件面（配置根 + 家目录）中常规文件
// 的执行位。异步执行不拖慢开机；此后新落地的文件由 fileops 写路径即时剥离。
func sweepExecBits(cfg *config.Config) {
	dirs := make([]string, 0, len(cfg.Roots)+1)
	for i := range cfg.Roots {
		dirs = append(dirs, cfg.Roots[i].Path)
	}
	dirs = append(dirs, cfg.UsersDir())
	if n := fileops.SweepExec(dirs); n > 0 {
		log.Printf("需求37: 已去除 %d 个用户文件的执行位（roots + Users 目录启动清扫）", n)
	}
}

func portOf(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		return p
	}
	return "?"
}
