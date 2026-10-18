# filemanageServer

FileManage 体系的服务端（gin 提供 REST API，统一 `/api/v1`）：不透明随机凭证认证
（服务端三层认证图，0.2.3 起**不再使用 JWT**）、目录级权限（ACL）、临时分享链接、
分片上传/断点续传、文件版本管理、回收站 30 天自动清理、
传输限速、TLS、内嵌管理员 Web 界面（`/admin/`）、多实例与负载均衡（内置 balancer / nginx）、
容器化部署。元数据存储内建于 `internal/store`（**file / sqlite3 / mysql / postgresql** 四驱动可选，
可选 redis 缓存；未配置数据库时默认内置 `file` 文件存储、全功能可用——单实例 flock，见 0.1.1），
store 层为工程内建（独立工程 filemanageDb 已并入、不再存在）。

当前版本：**0.2.8**（产物版本由 git tag 经 `-ldflags` 注入 `internal/version`，见 `CHANGELOG.md`；0.2.8=上传限额默认批：maxUploadBytes 2GiB→0=不限，config schemaVersion 11→12）

## 快速开始（Get Start）

```bash
# 编译（Docker 多平台编译环境，rockylinux:9；产物输出到脚本同级 build/ 目录）
./build.sh -a all -d release      # linux + windows(.exe) + arm64，静态编译
./build.sh -a linux               # 仅本机架构平台，release（默认）；-d debug → build/debug/
# （镜像缺失自动构建；命名规范与细节见 build-env/README.md）

# 查看版本信息（版本号 / git tag / commit_id / 编译时间）
./build/filemanageServer_linux_0.2.8 -v

# 首次运行自动生成示例配置 server.json
./build/filemanageServer_linux_0.2.8 -config server.json
# 编辑 server.json 的 roots（至少一个根目录）后重启
```

**首次上手（约 3 分钟）：**

1. 编译并运行：`./build.sh -a linux && ./build/filemanageServer_linux_0.2.8 -config server.json`
   —— 首启自动生成示例配置并在 `dataDir`（默认 `./data`）下生成**超级管理员账户文件**
   `admin-account.json`；
2. 编辑 `server.json`：至少配置一个 `roots` 条目（托管目录），按需改 `listen`/
   `adminListen`/`lanOnly`/`adminAccess`；重启服务；
3. 从启动**控制台输出**取初始口令：`已生成超级管理员账户 admin（…）：初始口令 "…"（仅首次登录前为明文）`
   ——敏感口令只回显控制台，**不写入日志文件**（0.2.7 敏感纪律）；
4. 浏览器打开 `http://127.0.0.1:8485/admin/`，以 `admin` + 该口令登录管理台
   （**无库=文件存储、热切换换库后同样可登录**——超管存账户文件不依赖库；首次登录成功后口令自动转 bcrypt 密文）；
5. 管理台"数据库"页选择驱动并连接（sqlite3 留空 DSN 即可；MySQL/PostgreSQL 填 DSN，
   **支持运行时热切换**、可选全量迁移）→ "用户管理"建普通用户并分配目录
   （`<dataDir>/Users/<用户名>` 家目录自动分配）→ 客户端地址填用户端口
   `http://<host>:8484` 以普通账户登录使用；
6. 忘记超管口令：编辑 `admin-account.json` 把 `password` 写回明文（或配置
   `adminPassword` 后重启）即重置，旧登录令牌立即作废。

- **0.1.0：超级管理员存于账户文件**（不进数据库）——首次启动自动生成
  `<dataDir>/admin-account.json`（用户名 `admin` + 随机明文口令，**只回显启动控制台、
  不入日志文件**——0.2.7 敏感纪律），
  浏览器打开 `http://127.0.0.1:8485/admin/` 用该账户登录管理台（**任何存储形态——
  含无库文件存储——都能登录**）。口令状态机：**使用前为明文**（管理台"超级管理员"页可回显），
  **首次成功登录后自动转 bcrypt 密文**（此后不可回显）。管理台含
  用户/用户组/文件夹权限/分享链接/审计/**数据库**/**超级管理员**（管理面独立端口，
  用户端口无管理路由；普通用户仍可自助改密，管理员不可登录客户端）
- **0.1.0 忘记/重置超管口令**：编辑 `<dataDir>/admin-account.json` 把 `password` 写回
  明文即重置（下次登录又转密文，旧令牌即废）；或在 server.json 设 `adminPassword` 后
  重启（等价于以该明文重置，用后请清空该字段）。账户名也可在"超级管理员"页修改
- **0.1.0 权限分层**：超级管理员可创建/管理**其他管理员**（存数据库 users 表 role=admin）
  与普通用户；其他管理员只能管理普通用户与日常运营，**不能管理超级管理员、不能增删管理员**
- **0.1.0 管理台 IP 准入可配**：`adminAccess.mode` = `local`（仅本机，默认）/
  `lan`（局域网）/ `any`（任意来源）/ `custom`（`adminAccess.ips` 指定 IP 或 CIDR 列表），
  远程访问需同时把 `adminListen` 绑到非回环地址；建议对外时配强口令 + TLS
- **数据库可在管理台在线配置**——驱动选型（file/sqlite3/mysql/postgresql）、状态查看、
  测试连接、**运行时热切换**（可选全量数据迁移），无需重启（0.1.0 起配库前需先以超管登录，
  旧的"首启开放/口令解锁"三态流程已移除）。热切换仅作用于本进程，多实例请逐台执行；
  0.2.3 起登录凭证存于元数据库认证图（auth_sessions）：勾选全量迁移时认证条目随行、
  会话跨引擎存续；未迁移时各类登录会话在条目新鲜期（auth.memTTL）后失效需重登
- **创建用户自动分配家目录** `<dataDir>/Users/<用户名>`（用户名须可安全作目录名），
  用户默认只能访问该目录，其全部文件**含回收站**（内嵌隐藏目录 `.fmtrash`，界面上不可见
  不可写）都在其中；管理员可再追加其他目录分配。存量用户升级启动时自动补齐（幂等）
- **0.1.0：用户文件不可执行**：托管内容（配置根 + Users）中常规文件的执行位在启动清扫与
  move/copy 时被剥离（目录保留穿越位），防止被服务端本地 exec

## 配置（server.json，schemaVersion=11）

```jsonc
{
  "schemaVersion": 11,
  "listen": "0.0.0.0:8484",
  "adminListen": "127.0.0.1:8485",       // 管理面独立端口（/admin/ 与全部管理 API 仅此端口可达）
  "lanOnly": true,                       // 仅局域网可访问（用户端口；管理端口改由 adminAccess 判定）
  // 管理界面 IP 准入（0.1.0 需求38，仅作用于管理端口，不再叠加 lanOnly）：
  //   local 仅本机回环（新装默认）/ lan 局域网 / any 任意来源 / custom 指定 ips（IP 或 CIDR 列表）
  //   存量 v7 配置自动迁移补 "lan"（与旧 lanOnly 语义一致）；远程访问需 adminListen 绑非回环地址
  "adminAccess": { "mode": "local", "ips": [] },
  "instanceId": "node0",                 // 多实例部署标识
  // jwtSecret/secretFile 已弃用（0.2.3 认证改走三层认证图、不再读取二者）：字段与
  // jwt.secret 物化仅为存量配置兼容保留，新部署可整段删除
  "jwtSecret": "",
  // 运行数据根目录：未显式配置的 secretFile/trashDir/uploadStagingDir/
  // versions.dir/sqlite DSN/TLS 证书目录都从它派生；用户家目录固定 <dataDir>/Users/<用户名>
  "dataDir": "./data",
  "secretFile": "./data/jwt.secret",
  // 元数据库：file|sqlite3|mysql|postgresql；"none"/留空 driver = file（0.1.1 起无库
  // 即内置文件存储、全功能可用、单实例 flock；旧"降级模式仅匿名浏览"已废止）。
  // 0.2.x 的 json/memory 驱动已移除（旧值自动归一为 file）。
  // sqlite 留空 DSN 默认 <dataDir>/filemanage.db；mysql DSN 例
  // `user:pw@tcp(127.0.0.1:3306)/fm`（自动追加 parseTime/utf8mb4；路径列上限 VARCHAR(700)）；
  // postgres DSN 例 `postgres://user:pw@127.0.0.1:5432/fm`（自动 sslmode=disable）。
  "db": { "driver": "file", "dsn": "" },   // 留空 dsn=<dataDir>/filestore
  // 可选 redis 缓存（默认关闭）：缓存用户/目录ACL/用户组，写路径自动失效；
  // 启用后启动即 PING，连不上直接报错退出（fail-fast）。缓存装饰器不碰会话/认证图；
  // 0.2.3 认证中心在同开关启用时自建 L2 连接（fm:auth:* 键、上限见 auth.redisMaxEntries）。
  "cache": { "enabled": false, "redis": { "addr": "127.0.0.1:6379", "password": "",
             "db": 0, "userTTLSeconds": 300, "aclTTLSeconds": 60 } },
  "roots": [{ "id": "main", "name": "主目录", "path": "/srv/files" }],
  // 以下四项可省略（留空 = 从 dataDir 派生）；显式配置依旧优先、升级不搬移：
  "trashDir": "",                        // 无家目录用户的中央回收站回退位置
  "trashRetentionDays": 30,              // 回收站自动清理（需求4）
  "uploadStagingDir": "",                // 分片上传暂存
  "maxUploadBytes": 0,                   // 单次上传大小硬上限（字节，0=不限）。0.2.8 起默认不限——历代 2GiB 默认让 >2GiB 文件必现 413（上传"彻底不能用"根因）；填正数即恢复限额（schemaVersion 11→12 仅升级历代默认字面量，自定义限额保留）
  "versions": { "enabled": true, "maxVersions": 10, "dir": "" },
  "session": { "accessTTL": "1h", "idleMinutes": 60, "idleDays": 21 },   // 管理台会话绝对时长 / 用户凭证滑动窗口（需求22；0.2.4 起 idleMinutes 优先、默认 1h，idleDays 弃用回退）
  // 三层认证图（0.2.3，需求2）：L1 进程内 LRU 上限与新鲜期 / L2 redis 条目上限 / 清扫与续期写回节奏
  "auth": { "memMaxEntries": 200000, "memTTL": "5m", "redisMaxEntries": 5000000,
            "sweepInterval": "10m", "touchWriteEvery": "10m" },
  // 服务端日志（0.2.7）：等级 debug|info|warn|error（默认 warn，非法取值收敛 warn）；
  // dir 空=<dataDir>/logs；含当前文件在内最多 maxFiles 个、单文件 ≤maxMB MB 按大小轮转；
  // 日志同时镜像 stdout（前台/容器直读）。敏感值（口令/令牌/请求体/头）永不入日志。
  "log": { "level": "warn", "dir": "", "maxMB": 10, "maxFiles": 3 },
  "anonymous": { "enabled": true },      // 未登录只读浏览（需求6）
  "rateLimit": { "uploadKBps": 0, "downloadKBps": 0 }, // 0=不限速（需求18）
  "tls": { "enabled": false, "certFile": "", "keyFile": "", "autoSelfSign": true },
  "adminPassword": "",                   // 超管口令重置触发器（0.1.0 需求32），平时留空；非空=启动时以明文重置账户文件
  "ai": { "provider": "none", "baseUrl": "", "apiKey": "", "model": "", "maxFileMB": 20 },
  "balancer": { "workers": [] }
}
```

**安全升级（需求25）**：旧版配置启动时自动按 schemaVersion 迁移，原文件备份为
`server.json.bak-<时间戳>`，未知字段原样保留。

## 运行模式

```bash
./filemanage-server -config server.json                    # worker（默认）
./filemanage-server -mode balancer -config balancer.json   # 负载均衡节点（需求27）
```

多实例部署（需求26）：每个实例独立配置/数据目录即可独立运行；如需组成集群，
让各实例共享同一元数据库（sqlite 共享文件，或直接用 mysql/postgresql）、roots、
trashDir、uploadStagingDir，以及 **`<dataDir>/Users`（用户文件与
家目录内回收站实体在此，必须与其它共享目录同放共享卷，否则跨实例不可见）**。
认证自 0.2.3 起凭证存共享元数据库（auth_sessions）——任一节点签发、全节点互认，
无需共享任何密钥文件（jwt.secret 已弃用）；cache.enabled 时认证 L2 随共享 redis 天然多实例一致。
可选 redis 缓存同样支持多实例共享；管理台数据库热切换仅对本进程生效，
多实例请逐台执行（或改配置后重启）。

负载均衡两种方式（详见 `deploy/README.md`）：
1. 内置 balancer：`balancer.json` 配 `balancer.workers`，轮询+健康检查
2. nginx：`deploy/nginx.conf.sample`

容器化（需求28）：`deploy/docker/{ubuntu22.04,debian12,rockylinux}/Dockerfile`，
`NODES_PER_CONTAINER` 控制每容器节点数，docker-compose 控制容器数量。

## 日志（0.2.7，internal/logx）

标准库 `log/slog` 实现，**零新依赖、`/api/v1` 数据协议零变更**（仅响应新增 `X-Trace-Id` 头）：

- **文件轮转**：`<logDir>/filemanage-server.log`（`log.dir` 空则 `<dataDir>/logs`），写前判长
  按大小轮转 `.log.1`/`.log.2`（oldest-first、溢出删除）——**含当前文件在内最多 `log.maxFiles`
  个（默认 3）、每个 ≤ `log.maxMB` MB（默认 10）**；同时镜像 stdout（前台/Docker 直接可看）。
- **等级**：`log.level` = `debug|info|warn|error`，**默认 warn**（未知取值收敛 warn）。
  存量 stdlib `log.Printf` 桥接为 INFO——生命周期横幅/计数行默认静默，真实故障站点
  （internal error、审计/配库/认证/清理器失败等）显式走 `logx.Warn/Error` 保持默认可见。
- **链路追踪**：每请求一个 trace ID（入头 `X-Trace-Id`/`X-Request-Id` 消毒透传，非法或缺失
  自动生成 16 位十六进制），回显响应头 `X-Trace-Id`；请求日志行
  `trace=<id> <METHOD> <path?query> <status> <dur> from=<remote>`，按状态分级
  （5xx=Error、4xx=Warn、其余=Info）；panic 由 `recoverGin` 兜底记录（trace+截断堆栈）
  并回 500 错误信封，进程不再因单个 handler 崩溃。用户端口与管理端口同享。
- **敏感信息纪律**：**绝不记录请求体与任何请求头**（凭证/口令只存在于二者）、绝不记录
  口令与令牌值；首启超管明文口令只回显启动控制台、不写入日志文件。
- balancer 模式同样初始化日志（其 stdlib 请求行经桥接落同一轮转文件）。

## API 摘要（/api/v1，`GET /api` 可发现版本）

| 分组 | 端点 |
|---|---|
| 认证 | `auth/login`（0.2.4 起请求 `{username,password,clientId?}`，用户端口响应只 `{token}`；管理台端口另回 `user` 概要） `auth/logout` `{token}` `auth/me`（0.2.4 起仅回 `{username}`——客户端不查询身份/角色） `auth/change-password`（0.1.0 自助改密；`auth/register` 引导注册、0.2.4 `auth/refresh` 已移除） |
| 浏览（可匿名，只读） | `roots` `files` `stat` `search`（名称/类型/大小/时间） `thumb`（0.2.1 服务端缩略图，守卫/匿名语义同 `preview`；格式白名单 jpeg/png/gif/RAW，0.2.2 起补 bmp——常见 BI_RGB/BI_BITFIELDS/RLE 子集，纯标准库解码；0.2.4 起 jpeg/RAW 走 YCbCr 平面路径（50MP/9000px 闸门，手机大原图也出图），其余格式 415=客户端图标） |
| 文件（需登录+ACL） | `mkdir` `mkdirs`（0.2.0 批量建目录，需求41 目录上传） `rename` `move` `copy` `delete` |
| 传输 | `download` `preview`（Range+限速） `upload`（兼容） `upload/session` `upload/chunk` `upload/complete`（分片/断点续传/并发保护） `check`（0.2.1 目录重传批量校验 missing/same/differ/unknown） |
| 回收站 | `trash` `trash/restore` `trash/remove` `trash/clear`（30 天自动清理） |
| 版本 | `versions` `versions/content` `versions/rollback` `versions/{id}` |
| 分享 | `share/links`(CRUD)；匿名：`/s/{token}` `/s/{token}/verify` `/s/{token}/files` `/s/{token}/download` |
| 权限 | `acl`(GET/PUT) `acl/{id}`(DELETE) `acl/subjects` |
| AI | `ai/describe` `ai/description` `ai/search` |
| 管理（仅管理端口） | `admin/info` `admin/super`(GET/PUT，0.1.0 超管账户，仅超管) `admin/users`(CRUD) `admin/users/{id}/roots` `admin/userroots` `admin/fs` `admin/groups`(CRUD) `admin/acls` `admin/links` `admin/audits` `admin/db/{status,drivers,test,connect,export,pause}`（配库热切换；0.1.0 起统一管理员登录态） |

## 目录结构

```
cmd/filemanage-server/   入口（worker/balancer 两种模式）
internal/store/          元数据抽象：Store 接口 + sqlStore 单实现 + sqlite3/mysql/pg 方言层
internal/cache/          可选 redis 缓存（Store 装饰器：读穿透 + 写失效）
internal/config/         配置加载 + schema 迁移链（当前 v11：log 服务端日志段）
internal/superadmin/     超管账户文件（0.1.0：明文→首用转密文、generation 纪元吊销、无库可用）
internal/api/            Server 聚合 + gin 双引擎路由（用户口 lanFilter/管理口 adminIPFilter；0.2.7 引擎级 recoverGin/traceGin/loggerGin）+ v1_* handlers
internal/auth/           不透明随机凭证 + 三层认证图（L1 LRU / L2 redis / L3 auth_sessions）
internal/perm/           目录权限引擎（用户/用户组/public，就近祖先继承）
internal/transfer/       分片上传管理 / 路径锁 / 令牌桶限速
internal/fileops/        路径安全 / 复制移动 / 版本库 / 后台清理器
internal/ai/             AIProvider 接口 + openai/ollama/none
internal/adminui/        go:embed 管理员 Web 界面（web/）
internal/balancer/       内置负载均衡反向代理
internal/tlsx/           自签证书生成
internal/logx/           服务端日志（0.2.7：slog 轮转 ≤3 文件×10MB / 等级默认 warn / stdlib 桥接 / trace 变体 / 敏感纪律）
deploy/                  Docker / compose / nginx 样例
```

## 版本兼容

见 `CHANGELOG.md` 兼容矩阵。当前基线：**Server 0.2.x（0.2.8）↔ Client 0.3.x
（推荐 0.3.8）**——0.2.8 上传限额默认批（`maxUploadBytes` 默认 2GiB→0=不限，修复 >2GiB
文件 `POST /upload/session` 必现 413=用户报"上传彻底不能用"的根因；判定与 413 语义保留，
显式限额时依旧生效；**`/api/v1` 数据协议与响应形状零变更**，对 Client 0.1.x–0.3.8 全系列
无感兼容；config schemaVersion 11→12，migrate11to12 只升级"恰为历代默认字面量"的配置、
用户自定义限额保留）。0.2.7 服务端日志为纯可观测性增强（新增响应头 `X-Trace-Id`）。0.2.4
认证收敛（登录只回 `{token}`、refresh 删除、绑定会话查 `X-Client-Id` 头、滑动窗口 1h）为
破坏批，旧客户端 ≤0.3.5 登录/刷新/分享/管理台全链路兼容（升级后存量令牌一次性失效、重登
一次）。元数据层自含（无独立 Db 工程）；旧配置启动时按 schemaVersion 自动迁移（当前链至
v12）。
