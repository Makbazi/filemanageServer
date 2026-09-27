# CLAUDE.md — filemanageServer

> 本文件供 Claude Code 及开发者快速了解本工程。**自动更新约定（需求2）**：
> 任何 API/配置/目录结构/行为变更，**必须在同一次改动中**同步更新本文件、
> `README.md`（如涉及用法）、`CHANGELOG.md`（含兼容矩阵）与 `internal/version/version.go`。

## 工程定位

FileManage 服务端（Go + **gin**，module `filemanageServer`）。元数据一律经本工程
`internal/store` 的 `Store` 接口读写（0.3.0 起内建，sqlite3/mysql/postgresql 三 SQL 驱动
+ 可选 redis 缓存），**禁止**绕过接口直接操作数据库。**0.1.1 起未配置数据库时默认使用
内置 `file` 文件存储驱动（全功能可用，旧"降级模式 503"取消，单实例 flock 保护）**。

## 目录结构（v0.3.0，cmd/internal 布局）

| 路径 | 职责 |
|---|---|
| `cmd/filemanage-server/main.go` | 入口：`-mode worker|balancer`、配置加载、装配、TLS/清理器启动；**0.1.0：超管账户文件初始化（首启生成随机明文口令入日志）+ `adminPassword` 重置触发器 + 遗留同名库内管理员告警（warnShadowedAdmins）+ 执行位后台清扫（sweepExecBits→fileops.SweepExec）** |
| `internal/store/` | 元数据抽象（0.3.0 并入，原 filemanageDb）：`Store` 接口 + 单一 `sqlStore` 实现（0.6.0 起查询/upsert/行映射一律 **gorm**，`rows.go` 行模型）+ `dialect_*.go` 方言层（仅剩幂等 DDL 清单/DSN 归一/LIKE 转义/pg 序列对齐）+ **0.7.0 `proxy.go`（可运行期原子换库的 Store 代理，全工程降级判定统一 `store.Degraded`）** 与 **`copy.go`（跨驱动全表迁移复制，keyset 分批保留主键）**；0.5.0 新增 `user_roots`（目录分配）/`server_meta`（迁移标记）、0.5.1 新增 `audit_events`（审计日志，保留 90 天）表——0.7.0/0.1.0 无新表/新列：0.1.0 超管会话复用 sessions 表 `user_id=0` 哨兵（`store.SuperAdminID`，无外键 + users 自增主键从 1 起天然不冲突）；超管账户本身在 `internal/superadmin` 文件、不入库。**0.1.1 `file` 驱动（需求40）**：`filelog.go`（帧 `op+len(LE32)+data+crc32c`，文件头 FMFS/v1/kind，MANIFEST 双代原子换，末段尾撕裂截断/中段 CRC 硬失败）+ `fileindex.go`（全内存 PK/唯一键二级索引，Put/Del 单一事实源）+ `filestore.go`（生命周期/写路径/用户等表；RWMutex 单写者，50ms 批量刷盘+关键操作 fsync+8MiB 段滚动）+ `filequery.go`（其余表，语义逐方法对齐 sqlstore）+ `filecomp.go`（快照压缩回收墓碑）+ `filecopy.go`（rowSource/rowSink 实现，file↔db 双向）+ `filelock_unix/other.go`（flock 单实例）；`DriverFile="file"`，""/none 归一为 file，DSN=目录（默认 `<dataDir>/filestore`） |
| `internal/cache/` | 可选 redis 缓存：`Cache` 接口 + Noop/Redis 实现 + `decorator.go`（Store 装饰器，读穿透/写失效；0.7.0 装饰包在 Proxy 之上，透传 Degraded、提供 FlushAll）|
| `internal/config/` | Config 类型 + 默认值 + 校验；`migrate.go` schemaVersion 迁移链 v1→…→v7（dataDir）→**v8（adminAccess 管理界面 IP 准入，需求38：local/lan/any/custom+IP/CIDR 列表，finalize 预解析网段、custom 非法值硬报错；存量 v7 配置补 `lan` 保持 0.7.x 语义，新装默认 `local`）**（改配置结构必须加迁移）；0.7.0 起 trashDir/staging/versions/secretFile/sqlite DSN 未显式配置时从 `dataDir` 派生；`AdminAccountFile()=<dataDir>/admin-account.json`、`AdminAllows(ip)`；`PersistDBEdit` 供管理台最小侵入回写 db 段（绝不整体序列化 Config——会泄 jwtSecret） |
| `internal/api/` | `Server` 聚合结构 + **gin 双引擎路由**（`router.go`：`Handler()` 用户端口 / `AdminHandler()` 管理端口，registerV1 + registerAdminAPI + **registerAdminDBAPI（0.7.0 配库面）**）+ `scope.go`（0.5.0 jail 虚拟根唯一翻译层；0.7.0 集中拦截家目录保留名 `.fmtrash`）+ gin 中间件 + `v1_*.go` 各领域 handler；**0.1.0：`newEngine(netGuard,...)` 参数化网络准入（用户口 lanFilter / 管理口 adminIPFilter）；`Server.Super *superadmin.Store`；authCtx 先解析 claims 再分流（超管令牌零 Store 调用，降级可用）；`v1_super.go` 超管账户页（GET/PUT /admin/super + requireSuperAdmin 守卫）；`v1_auth.go` change-password + 超管登录优先 + 移除引导注册；`v1_dbconfig.go` 鉴权收敛为单一管理员登录态（删 firstBoot/dbUnlock 三态）；`/api/v1/admin` 拆 lite 组（info/super 组外 requireDB，其余仍 requireDB）；0.1.1：`manualPause/autoPause atomic.Bool` + `maintenanceGate()`（挂 v1 组/share 组/管理业务组，health/版本发现/配库面/超管页豁免）；`v1_dbconfig.go` 新增 `POST /admin/db/export`（纯导出 DB→文件存储不换引擎）与 `POST /admin/db/pause`（手动暂停开关），connect 支持 `driver=file`（含同目录拒切）、迁移全程 `withAutoPause` 屏障包裹** |
| `internal/auth/` | JWT access / refresh 哈希 / 分享令牌 / 随机密码；**0.1.0：`Claims`+`ParseAccessClaims`（替代 ParseAccess）、`IssueSuperAccess`（sub=0 + super + gen 凭据纪元）；删 Issue/ParseDBUnlockToken（三态解锁移除）** |
| `internal/perm/` | ACL 权限引擎：admin 全权；命中 ACL 层就近裁决（同层 OR）；匿名强制不可下载/上传/删除且默认只读；无库降级分支=仅浏览（0.5.0 D1） |
| `internal/transfer/` | 分片上传 Manager、per-path 锁注册表、令牌桶限速 |
| `internal/fileops/` | 路径安全（SafeRel/JoinRoot）、copy/move、SHA256、版本库 VersionStore、后台 Cleaner；**0.7.0 `homes.go`：用户名目录名校验 / 家目录与 `.fmtrash` 路径 / EnsureHomeFor（建号即建目录+追加 `_users` 分配）/ EnsureUserHomes（启动幂等补齐，标记 `userhomes.v7`）**；**0.1.0 需求37：`stripExec`（MovePath 两分支 + CopyFile 收尾，仅常规文件 `perm&^0o111`）/ `SweepExec(dirs)`（启动后台清扫 roots+Users 存量文件执行位）** |
| `internal/superadmin/` | **0.1.0 需求32/33/34/36 新包**：文件承载超管（`<dataDir>/admin-account.json` 0600，不进数据库，无库降级/热切换后同样可用）；明文↔bcrypt 密文状态机（首次成功登录转密文，需求34）、`generation` 凭据纪元（外部编辑文件即视为重置→纪元+1 回写吊销旧令牌）、`EnsureCreated/Match/Verify/SetCredentials/Plaintext/Generation`；其他管理员 = 数据库 users 表 role=admin（需求35），由超管经管理台创建 |
| `internal/ai/` | AIProvider 接口 + openai/ollama/none |
| `internal/adminui/` | go:embed `web/`（纯手写 HTML/JS/CSS，无构建链）挂 `/admin/`（**仅管理端口**，0.5.0 含目录分配编辑器与物理根浏览器；0.7.0 "数据库"页：状态/选型/测连/热切换/迁移；**0.1.0：登录页降级提示改"以超管账户登录"（删解锁卡/引导注册卡/setup-entry/`refreshTabAccess` 按超管身份+降级态收敛页签）；新"超级管理员"页（账户名/新口令、明文有效期徽章、PUT 后换用重发令牌）；用户页管理员行编辑/删除与 role=admin 选项仅超管可用；概览加 adminAccess/超管账户名卡；0.1.1："数据库"页驱动列表含 file（无库默认，dsn 可留空=派生目录、输入框不禁用），新增"导出到文件存储（不切换）"按钮（复用 DSN 目录字段+覆盖勾选，409 自动勾覆盖）、"暂停/恢复客户端操作"按钮（状态卡新增"客户端操作"徽章 = paused）、`/status` 的 `paused` 字段驱动渲染）**） |
| `internal/balancer/` | 轮询反向代理 + 健康检查（`-mode balancer`） |
| `internal/tlsx/` | 自签证书生成 |
| `internal/version/` | `Version` 常量与 `APIVersions`；0.7.1 增 `GitTag/GitCommit/BuildTime`（build.sh 经 `-ldflags -X` 注入，`-v` 输出） |
| `deploy/` | Docker×3 / entrypoint / compose / nginx 样例（**运行**镜像） |
| `build-env/` | **编译**环境（Dockerfile.rockylinux9 + container-build.sh + README；产物规范 linux/windows(.exe)/arm64 静态编译，经 `./build.sh -a -d` 调用） |

## 运行

```bash
./build.sh -a all -d release    # Docker 编译环境（build-env/Dockerfile.rockylinux9，镜像
                                # filemanage-server-build:rockylinux9，缺失自动构建）多平台静态编译：
                                # build/filemanageServer_linux_0.1.1 / _arm64_0.1.1 / _windows_0.1.1.exe
                                # -a 取 linux|windows|arm64|all；-d debug → build/debug/
                                # （release: -trimpath -s -w；debug: -N -l；均 CGO_ENABLED=0 静态、均注入版本信息）
./build/filemanageServer_linux_0.1.1 -v                            # 打印 版本号/tag/commit_id/编译时间 后退出
./build/filemanageServer_linux_0.1.1 -config server.json           # worker（用户端口 + 管理端口双 listener）
./build/filemanageServer_linux_0.1.1 -mode balancer -config balancer.json
```

首次运行生成示例配置；`/api` 版本发现；`/api/v1/health` 健康检查（0.1.1 起无库默认 `db:"file"`——文件存储引擎；仅异常路径可能 `db:"none"`）。
0.5.0 起管理台在**独立管理端口**（`adminListen`，默认 `127.0.0.1:8485`）：浏览器本机访问
`http://127.0.0.1:8485/admin/`；用户端口无任何管理路由（404）。多实例部署时每个实例
各自绑定唯一 adminListen（建议保持 127.0.0.1，经 SSH 隧道访问）。
**0.1.0 需求32**：首启自动生成超管账户 `<dataDir>/admin-account.json`（用户名 `admin` +
随机明文口令，打印在启动日志），**任何模式（含无库降级）都用它登录管理台**；首次登录
成功后口令转 bcrypt 密文（日志明文随即失效）；忘记口令 → 编辑该文件写回明文（或配置
`adminPassword` 后重启）即重置、旧令牌即废。**需求38**：管理台访问范围按 `adminAccess`
（`local` 仅本机/`lan` 局域网/`any` 任意/`custom` 指定 IP·CIDR 列表）判定，
`adminListen` 改绑 `0.0.0.0` 或 `custom` 含远程网段才能远程访问（配合强口令 + TLS）。
`db.driver="none"`（或 ""/file）= 不配数据库启动，**0.1.1 起默认打开内置文件存储**
（`<dataDir>/filestore`，全功能可用——登录/分享/权限/版本/回收站/AI/上传均不再 503；
**单实例**，目录 flock 排他，第二实例 fail-fast 拒绝启动）；旧"降级模式仅匿名浏览"已取消
（匿名访问一律按 public ACL，与有库模式统一）。0.7.0 起管理台在线配库并**热生效**
（免重启），0.1.0 起配库前先以超管登录；0.1.1 起三独立操作：切换（connect，可选携数据
迁移）、导出（export，DB→文件纯复制不换引擎）、暂停（pause，冻结用户端口保一致性）。
0.7.0 起所有落盘数据默认归入 `dataDir`（含 `<dataDir>/Users/<用户名>` 用户家目录，
建用户自动分配；多实例共享部署时 Users 目录与 versions/staging 同放共享卷）；
**0.1.0 需求37**：用户文件（roots + Users）常规文件的执行位在启动后台清扫 + move/copy
写路径被剥离（目录保留 0755 穿越位），服务端托管内容不可被本地 exec。

## 关键约定（改动前必读）

1. **路径安全**：所有文件访问必须经 `Server.scope()`/`resolve()`（0.5.0：root 白名单 +
   `fileops.JoinRoot` 防逃逸 + jail/公开/分享基座校验，统一于 `scope.go`）。任何新 handler
   不得绕过；请求 rootId 是虚拟根 id（`physID[:jail]`），存储层键一律用
   `scopeInfo.PhysID/Phys`（显示↔物理换算只允许发生在 scope/displayOf）。
2. **权限模型（0.5.0 jail）**：管理员经**管理端口**把"物理根+子目录"分配给用户
   （`user_roots` 表，一人可多目录）；用户端口任何主体（含管理员）只见/只可操作自己的
   分配；匿名只能进入 public-view 目录（`GET /roots` = 公开共享目录清单）；被 ACL
   分享给 user/group 的目录自动成为被分享者的虚拟根。登录写操作先 `requirePerm(...)`
   （五位 Decision，判定作用于物理路径）；引擎强制匿名 Download/Upload/Delete=false；
   降级模式（仅异常路径可达）匿名**仅可浏览不可下载**（0.5.0 D1 收紧，degradedView/perm
   nil-Store 分支；**0.1.1 起无库=文件存储，正常部署不再可达**）。
   **所有依赖元数据的路由/分组必须挂 `requireDB()`**（存储不可用时统一 503）；
   **0.1.1 维护暂停（maintenanceGate）**：业务/分享/管理常规组另挂暂停守卫
   （manualPause ∥ autoPause；health/版本发现/lite 组/`/admin/db` 配置面永不拦）。
   管理 API 只注册在 `AdminHandler`（registerAdminAPI），用户端口不存在这些路由（404）。
   **0.1.0 管理面分层（需求33）**：管理员账户的 create/update/delete、角色变更、管理员的
   目录分配是**超管专属**（`isSuperAdmin(c)`，其他管理员 403）；users 表接口一律拒
   `id==store.SuperAdminID(0)`（400，超管不在库内）；`authCtx` 分流后超管令牌的
   `RequireAdmin/RequireLogin` 判定与 DB 管理员同构（Role=admin）。管理端点按是否依赖
   元数据库拆 `lite`（info/super，组外 requireDB）与常规组（见约定11/13）。
3. **写文件并发保护**：覆盖已有文件必须：持 `Locks.Lock(transfer.Key(物理rootID, 物理rel))` →
   `Versions.Snapshot` 旧内容 → tmp+rename 原子替换（锁/版本/回收站/上传会话/ACL/file_meta/
   分享链接一律**物理键**，0.5.0，参考 `upload/complete` 与旧 `upload`）。
4. **配置变更**：新增配置字段必须给默认值（`DefaultConfig`/`finalize`）且加入
   `migrate2to3` 式迁移（schemaVersion+1），保证旧配置升级不丢数据（需求25）。
   **注意**：`Store` 接口或方言层任何改动必须四方言（sqlite3/mysql/postgresql +
   **0.1.1 file 文件存储**）语义一致（排序/ErrNotFound/ErrExists/GetMeta 缺键=""，
   见 filequery.go 各方法头注释），且 sqlite 不得改动已发布列/表（磁盘兼容既有 .db
   文件，见 dialect_sqlite.go 头注释）。**file 驱动无新表/新列**，仅新增驱动实现。
   0.1.1 无配置结构变更，schemaVersion 保持 8（需求25 不触发；driver 默认值改为
   "file" 属语义变更非结构变更）。
5. **API 版本**：新端点加进 `registerV1`（gin 分组）；破坏性变更须新开 `registerV2` 并更新
   `version.APIVersions`，旧版本保留（需求17）。对外信封 `{"error":msg}` 与状态码
   是客户端契约，不得随框架变化。
6. **敏感信息**：响应中永不返回密码哈希（模型 json:"-"）；分享链接明文密码只在创建响应出现一次。
7. **多实例**：新增落盘状态必须放在可共享目录（trashDir/uploadStagingDir/versions.dir/
   secretFile）或入库，避免实例间不可见（需求26/27）。
8. **错误映射**：内部错误经 `statusOf()` 映射 HTTP 码；transfer 包的哨兵错误
   （ErrConflict→409 等）已接入。
9. 编译验证：`go build ./... && go vet ./...`（不写测试，需求29）。
10. **不做 git 提交/分支/tag**（需求30），版本 tag 由维护者操作；发版时手动同步
    `internal/version/version.go` 与 CHANGELOG。
11. **Store 装配与热切换（0.7.0）**：装配顺序固定 `store.Open → store.NewProxy →
    （可选 cache.Wrap(proxy)）→ api.NewServer(cfg, eff, proxy, cch, cfgPath,
    super)（0.1.0 末位传 `*superadmin.Store`）/ Cleaner`；`Server.Store` 恒非 nil（代理），
    **降级判定一律用 `store.Degraded(st)`（穿透装饰器），禁止再写 `Store == nil`**。管理台
    connect 是唯一允许运行期换库的入口（`dbSwapMu` 串行；换入前 Migrate、可选
    CopyAllTables、换入后 FlushAll + PersistDBEdit + 关旧库）。新增依赖元数据的路由仍须挂
    `requireDB()`；配库端点与 `/admin/info`、`/admin/super`（lite 组）注册在 requireDB 之外，
    且**这些组外路由零 Store 依赖或经 Proxy 降级安全**（超管无库可达）。0.1.0 起配库鉴权
    收敛为单一管理员登录态（`dbConfigGuard` = authCtx+requireAdmin，超管或 DB 管理员皆可）。
    **0.1.1 需求40 扩展**：① `store.Open` 对 ""/none 恒返回 file 驱动（无库不再降级，
    Open 失败 fail-fast；`Degraded` 路径仅异常可达，requireDB 保留作纵深防御）。
    ② 管理面三独立操作：connect（切换，migrate 可选携全表复制——rowSource/rowSink
    泛化后支持 file↔db 双向保留主键）、export（DB→文件存储纯复制不换引擎不写配置，
    目标非空 409/force 清空重建）、pause（手动维护开关，`/admin/db/status` 回显 `paused`）；
    迁移/导出全程包 `withAutoPause` 屏障（`maintenanceGate` 拦业务/分享/管理常规组，
    health/版本发现/lite 组/`/admin/db` 配置面永不拦——操作员绝不被锁死）。
    ③ 同目录 file→file 切换显式 400（flock 自撞保护）。④ file 存储 `TableCounts/
    TableNonEmpty` 走索引计数，状态卡/表统计天然可用。
12. **用户家目录模型（0.7.0 需求3）**：内部合成根 `store.HomeRootID="_users"`
    （路径 `<dataDir>/Users`，**只注入 `Server.Roots`，绝不写进 cfg.Roots**；配置
    校验禁止用户根 id 以 `_` 开头）；任何用户创建路径必须过
    `fileops.UsernameAsDir` + `EnsureHomeFor`；分配编辑器保存时家目录行自动补回、
    `_users` 仅可分配给本人。回收站实体在 `<home>/.fmtrash`（无家目录回退中央，
    解析用 `Server.resolveTrashBlob` 双候选）；`.fmtrash` 拦截集中在 `scope()`
    （保留名段 → 403），列表隐藏于 `handleList`，改名占用拒于 `handleRename`——
    新 handler 经 scope 即自动继承该拦截。删除守卫必须同时比较
    `sc.Root.Path` 与 `sc.BaseAbs`（jail 基座不可删）。
13. **超级管理员模型（0.1.0 需求32/33/34/35/36）**：超管 = 文件账户（`internal/superadmin`
    读写 `<dataDir>/admin-account.json`，0600，**不进 users 表**），其他管理员/普通用户
    在数据库（role=admin/user）。判定入口：`authCtx` 解析 JWT 后**先分流超管令牌**
    （claims.Super → 校 generation == 账户文件当前纪元 → 注入合成
    `store.User{ID: store.SuperAdminID(=0), Role: admin}` + **零 Store 调用**，故无库
    降级模式可用、天然绕开 redis 用户缓存）；普通令牌才 `GetUserByID`。**任何 users 表
    写接口收到 `id==store.SuperAdminID` 必须显式 400**（`DeleteUser(0)` 的级联会删
    sessions.user_id=0 = 吊销全部超管会话，属哨兵碰撞）。管理面分层：`isSuperAdmin(c)`
    （即 `currentUser(c).ID==0`）为超管专属判据——create/update/delete/role 变更/admin
    的 roots 分配均需超管（其他管理员返回 403），`createUser` 另拒超管保留名。超管令牌
    走用户端口一律 403（Role admin && !adminMode，与 DB 管理员同规则，需求36）。凭据
    纪元 generation：Verify 明文口令成功后转 bcrypt 并 +1、SetCredentials 必 +1、
    检测到文件 password/username 被外部改动（纪元未前进）也 +1 回写——**纪元变化即
    吊销全部已发超管令牌**（authCtx 逐请求核对，无需黑名单）。超管不落 users 故
    `warnShadowedAdmins` 需告警 0.7.x 遗留的同名库内 admin 账户（已被登录优先匹配遮蔽）。

## 当前版本

`internal/version.Version = "0.1.1"`（需求40：无数据库默认改用内置**文件存储驱动**
`file`——`internal/store/file*.go`：二进制帧 op+length+data+crc32c（CRC Castagnoli）
追加日志 + 全内存 PK/唯一键二级索引 + 快照压缩 + MANIFEST 双代原子换 + 末段尾撕裂
截断容忍/中段 CRC 硬失败 + 50ms 批量刷盘（账户/会话/SetMeta 等关键操作同步 fsync）+
段 8MiB 滚动 + 目录 flock 单实例；""/none/file 归一，`Open` 不再产生降级 (nil,nil)，
旧降级模式自此取消（匿名一律按 public ACL，requireDB/degradedView 仅异常路径可达）；
管理面三独立操作 connect(切换,可选migrate)/export(纯导出不换引擎)/pause(维护暂停)，
`maintenanceGate` 冻结用户端口一切操作与管理业务组保切换/导出/暂停窗口一致性；全表
迁移通道泛化为 rowSource/rowSink 支持 file↔db 双向保留主键；schemaVersion 保持 8
无结构变更）。**承前 0.1.0 基线要点**（需求39：正式首发前从未发布，CHANGELOG 历史
已清空并从 0.1.0 重记；0.1.0 为**首个基线版本**——2026-09-22 经 sqlite3/MySQL 双库
全功能回归（超管账户/分层/改密/jail 分配/文件/分片/版本/分享/ACL/会话/审计/热切换，
AI 除外）99 项断言全通过后定版；API：v1。0.1.0 破坏性变更集中在管理面/认证面——超管改文件承载
`internal/superadmin`（需求32/34/35/36：明文→首用转 bcrypt 密文、generation 纪元吊销、文件/
adminPassword 重置、无库降级同样登录），管理面分层（需求33：管理员增删改仅超管、其他管理员
只管普通用户、id=0 哨兵在 users 表接口显式拒绝），用户自助改密 `POST /auth/change-password`
（需求31），用户文件去执行位（需求37），管理界面 IP 准入 `adminAccess`（需求38：
local/lan/any/custom，schema 7→8）。**移除** `POST /auth/register` 引导注册、
`/admin/db/unlock` 与 firstBoot/needsUnlock 三态（配库面收敛为单一管理员登录态）；
`POST /auth/login` 摘路由级 requireDB（超管无库可用，普通账户 handler 内 503）；
管理 API 拆 lite 组（info/super 组外 requireDB）。客户端数据协议仅加性（新客户端 0.1.0
用 change-password）。承前版本要点保留——
0.3.1 maxUploadBytes（schema 3→4）；0.5.0 jail + adminListen 独立端口（schema 4→5）；
0.5.1 审计/qps/CORS/symlink 防护（schema 5→6）；0.6.0 SQL gorm 化 + admin 客户端隔离
（用户端口拒绝 admin 登录/续期、遗留 token 403）+ public-view 只读可见 + 分享去物理化；
0.7.0 管理台"数据库"页（热切换/全表迁移，schema 6→7 加 dataDir，store.Proxy，见约定11）+
用户家目录自动分配 `<dataDir>/Users/<用户名>` + `.fmtrash`（合成根 `_users`，见约定12）。**）
存量迁移：普通用户 AllowedRoots 启动时一次性转为显式 user_roots 分配（server_meta 标记幂等）；
0.7.0 起另有存量用户家目录补齐（标记 `userhomes.v7`）。
0.5.1 安全要点：新 handler 经 scope 即自动获得 symlink 防护与审计能力（`s.audit`）；
审计/限速默认值不改变 0.5.0 行为。0.6.0 安全要点：admin 身份在用户端口任何路径
（登录/刷新/遗留令牌）均不可用，只能走管理台；分享访问者不见物理位置。
**0.1.0 安全要点（见约定13）**：超管口令仅未使用前为明文可回显、首用即转 bcrypt 密文；
`/admin/super` 仅超管本人可达（DB 管理员 403）；管理面凭据变更推进 generation 即时吊销
全部超管令牌/会话；文件重置（外部编辑 admin-account.json 的 password/username）同样被
检测并推进纪元；`adminPassword` 配置非空 = 以其为明文重置超管口令（用后须人工清空）；
其他管理员的 create/update/delete/role 变更/roots 分配一律超管专属；用户文件执行位由
启动清扫 + MovePath/CopyFile 收尾剥离。元数据层已内建；redis 缓存可选（默认关闭）。
**0.1.1 安全要点（需求40，见约定2/4/11）**：文件存储帧 CRC 校验——中段损坏拒绝启动
（绝不半应用已提交批次）、仅末段尾撕裂可截断；敏感哈希（password_hash/token_hash）
只存帧内（行模型无 json:"-"，文件 0600、目录 0700），永不出 API；flock 保证单实例
（两实例同目录并写不可能）；maintenanceGate 屏障窗口内源/目标存储无并发写
（409/force 语义防误覆盖既有数据）；文件存储非关键元数据掉电丢 ≤50ms 批量为
**明示的降级契约**（要持久性配数据库）；SearchDescriptions 大小写敏感（对齐 pg）。
依赖：gin v1.10.1 / **gorm v1.31（sqlite 用 glebarez 纯 Go 驱动，mysql/postgres 官方
gorm 驱动）** / go-redis v9 / golang-jwt v5 / x/crypto。SQL 一律经 gorm 构建器
（占位符参数化、clause.OnConflict upsert、按方言渲染）；仅幂等建表 DDL 清单经
`Migrate()` 直发（保 sqlite 字节兼容，不走 AutoMigrate）——除 store 层外禁止任何直接 SQL。
