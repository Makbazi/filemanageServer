# filemanageServer

FileManage 体系的服务端（gin 提供 REST API，统一 `/api/v1`）：JWT+refresh 会话、
目录级权限（ACL）、临时分享链接、分片上传/断点续传、文件版本管理、回收站 30 天自动清理、
传输限速、TLS、内嵌管理员 Web 界面（`/admin/`）、多实例与负载均衡（内置 balancer / nginx）、
容器化部署。元数据存储内建于 `internal/store`（**sqlite3 / mysql / postgresql** 三驱动可选，
可选 redis 缓存；未配置数据库时进入降级模式），0.3.0 起不再依赖独立工程 filemanageDb。

当前版本：**0.2.0**（产物版本由 git tag 经 `-ldflags` 注入 `internal/version`，见 `CHANGELOG.md`）

## 快速开始（Get Start）

```bash
# 编译（Docker 多平台编译环境，rockylinux:9；产物输出到脚本同级 build/ 目录）
./build.sh -a all -d release      # linux + windows(.exe) + arm64，静态编译
./build.sh -a linux               # 仅本机架构平台，release（默认）；-d debug → build/debug/
# （镜像缺失自动构建；命名规范与细节见 build-env/README.md）

# 查看版本信息（版本号 / git tag / commit_id / 编译时间）
./build/filemanageServer_linux_0.2.0 -v

# 首次运行自动生成示例配置 server.json
./build/filemanageServer_linux_0.2.0 -config server.json
# 编辑 server.json 的 roots（至少一个根目录）后重启
```

**首次上手（约 3 分钟）：**

1. 编译并运行：`./build.sh -a linux && ./build/filemanageServer_linux_0.2.0 -config server.json`
   —— 首启自动生成示例配置并在 `dataDir`（默认 `./data`）下生成**超级管理员账户文件**
   `admin-account.json`；
2. 编辑 `server.json`：至少配置一个 `roots` 条目（托管目录），按需改 `listen`/
   `adminListen`/`lanOnly`/`adminAccess`；重启服务；
3. 从启动日志取初始口令：`已生成超级管理员账户 admin（…）：初始口令 "…"（仅首次登录前为明文）`；
4. 浏览器打开 `http://127.0.0.1:8485/admin/`，以 `admin` + 该口令登录管理台
   （**未配置数据库的降级模式同样可登录**；首次登录成功后口令自动转 bcrypt 密文）；
5. 管理台"数据库"页选择驱动并连接（sqlite3 留空 DSN 即可；MySQL/PostgreSQL 填 DSN，
   **支持运行时热切换**、可选全量迁移）→ "用户管理"建普通用户并分配目录
   （`<dataDir>/Users/<用户名>` 家目录自动分配）→ 客户端地址填用户端口
   `http://<host>:8484` 以普通账户登录使用；
6. 忘记超管口令：编辑 `admin-account.json` 把 `password` 写回明文（或配置
   `adminPassword` 后重启）即重置，旧登录令牌立即作废。

- **0.1.0：超级管理员存于账户文件**（不进数据库）——首次启动自动生成
  `<dataDir>/admin-account.json`（用户名 `admin` + 随机明文口令，**打印在启动日志**），
  浏览器打开 `http://127.0.0.1:8485/admin/` 用该账户登录管理台（**任何模式含无库降级
  都能登录**）。口令状态机：**使用前为明文**（管理台"超级管理员"页可回显），
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
- **0.7.0：数据库可在管理台在线配置**——驱动选型（sqlite3/mysql/postgresql）、状态查看、
  测试连接、**运行时热切换**（可选全量数据迁移），无需重启（0.1.0 起配库前需先以超管登录，
  旧的"首启开放/口令解锁"三态流程已移除）。热切换仅作用于本进程，多实例请逐台执行；
  未迁移时数据库管理员会话失效需重登（超管令牌走账户文件、不随换库失效）
- **0.7.0：创建用户自动分配家目录** `<dataDir>/Users/<用户名>`（用户名须可安全作目录名），
  用户默认只能访问该目录，其全部文件**含回收站**（内嵌隐藏目录 `.fmtrash`，界面上不可见
  不可写）都在其中；管理员可再追加其他目录分配。存量用户升级启动时自动补齐（幂等）
- **0.1.0：用户文件不可执行**：托管内容（配置根 + Users）中常规文件的执行位在启动清扫与
  move/copy 时被剥离（目录保留穿越位），防止被服务端本地 exec

## 配置（server.json，schemaVersion=8）

```jsonc
{
  "schemaVersion": 8,
  "listen": "0.0.0.0:8484",
  "adminListen": "127.0.0.1:8485",       // 管理面独立端口（/admin/ 与全部管理 API 仅此端口可达）
  "lanOnly": true,                       // 仅局域网可访问（用户端口；管理端口改由 adminAccess 判定）
  // 管理界面 IP 准入（0.1.0 需求38，仅作用于管理端口，不再叠加 lanOnly）：
  //   local 仅本机回环（新装默认）/ lan 局域网 / any 任意来源 / custom 指定 ips（IP 或 CIDR 列表）
  //   存量 v7 配置自动迁移补 "lan"（与旧 lanOnly 语义一致）；远程访问需 adminListen 绑非回环地址
  "adminAccess": { "mode": "local", "ips": [] },
  "instanceId": "node0",                 // 多实例部署标识
  "jwtSecret": "",                       // 留空则用 secretFile 持久化随机密钥
  // 运行数据根目录（0.7.0）：未显式配置的 secretFile/trashDir/uploadStagingDir/
  // versions.dir/sqlite DSN/TLS 证书目录都从它派生；用户家目录固定 <dataDir>/Users/<用户名>
  "dataDir": "./data",
  "secretFile": "./data/jwt.secret",
  // 元数据库：sqlite3|mysql|postgresql；"none"（或留空 driver）= 降级模式，
  // 仅匿名只读浏览可用（不可下载），登录/分享/ACL/版本/回收站/AI/上传一律 503。
  // 0.2.x 的 json/memory 驱动已移除，旧配置自动改写为 sqlite3（元数据不迁移）。
  // sqlite 留空 DSN 默认 <dataDir>/filemanage.db；mysql DSN 例
  // `user:pw@tcp(127.0.0.1:3306)/fm`（自动追加 parseTime/utf8mb4；路径列上限 VARCHAR(700)）；
  // postgres DSN 例 `postgres://user:pw@127.0.0.1:5432/fm`（自动 sslmode=disable）。
  "db": { "driver": "sqlite3", "dsn": "" },
  // 可选 redis 缓存（默认关闭）：缓存用户/目录ACL/用户组，写路径自动失效；
  // 启用后启动即 PING，连不上直接报错退出（fail-fast）。会话不参与缓存。
  "cache": { "enabled": false, "redis": { "addr": "127.0.0.1:6379", "password": "",
             "db": 0, "userTTLSeconds": 300, "aclTTLSeconds": 60 } },
  "roots": [{ "id": "main", "name": "主目录", "path": "/srv/files" }],
  // 0.7.0 起以下四项可省略（留空 = 从 dataDir 派生）；显式配置依旧优先、升级不搬移：
  "trashDir": "",                        // 无家目录用户的中央回收站回退位置
  "trashRetentionDays": 30,              // 回收站自动清理（需求4）
  "uploadStagingDir": "",                // 分片上传暂存
  "versions": { "enabled": true, "maxVersions": 10, "dir": "" },
  "session": { "accessTTL": "1h", "idleDays": 21 },   // 3 周滑动会话（需求22）
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
trashDir、uploadStagingDir、secretFile，以及 **`<dataDir>/Users`（0.7.0：用户文件与
家目录内回收站实体在此，必须与其它共享目录同放共享卷，否则跨实例不可见）**。
可选 redis 缓存同样支持多实例共享；管理台数据库热切换仅对本进程生效，
多实例请逐台执行（或改配置后重启）。

负载均衡两种方式（详见 `deploy/README.md`）：
1. 内置 balancer：`balancer.json` 配 `balancer.workers`，轮询+健康检查
2. nginx：`deploy/nginx.conf.sample`

容器化（需求28）：`deploy/docker/{ubuntu22.04,debian12,rockylinux}/Dockerfile`，
`NODES_PER_CONTAINER` 控制每容器节点数，docker-compose 控制容器数量。

## API 摘要（/api/v1，`GET /api` 可发现版本）

| 分组 | 端点 |
|---|---|
| 认证 | `auth/login` `auth/refresh` `auth/logout` `auth/me` `auth/change-password`（0.1.0 自助改密；`auth/register` 引导注册已移除） |
| 浏览（可匿名，只读） | `roots` `files` `stat` `search`（名称/类型/大小/时间） `thumb`（0.2.1 服务端缩略图，守卫/匿名语义同 `preview`；格式白名单 jpeg/png/gif/RAW，0.2.2 起补 bmp——常见 BI_RGB/BI_BITFIELDS/RLE 子集，纯标准库解码；其余格式 415 由客户端回退） |
| 文件（需登录+ACL） | `mkdir` `mkdirs`（0.2.0 批量建目录，需求41 目录上传） `rename` `move` `copy` `delete` |
| 传输 | `download` `preview`（Range+限速） `upload`（兼容） `upload/session` `upload/chunk` `upload/complete`（分片/断点续传/并发保护） `check`（0.2.1 目录重传批量校验 missing/same/differ/unknown） |
| 回收站 | `trash` `trash/restore` `trash/remove` `trash/clear`（30 天自动清理） |
| 版本 | `versions` `versions/content` `versions/rollback` `versions/{id}` |
| 分享 | `share/links`(CRUD)；匿名：`/s/{token}` `/s/{token}/verify` `/s/{token}/files` `/s/{token}/download` |
| 权限 | `acl`(GET/PUT) `acl/{id}`(DELETE) `acl/subjects` |
| AI | `ai/describe` `ai/description` `ai/search` |
| 管理（仅管理端口） | `admin/info` `admin/super`(GET/PUT，0.1.0 超管账户，仅超管) `admin/users`(CRUD) `admin/users/{id}/roots` `admin/userroots` `admin/fs` `admin/groups`(CRUD) `admin/acls` `admin/links` `admin/audits` `admin/db/{status,drivers,test,connect}`（0.7.0 配库热切换；0.1.0 起统一管理员登录态） |

## 目录结构

```
cmd/filemanage-server/   入口（worker/balancer 两种模式）
internal/store/          元数据抽象：Store 接口 + sqlStore 单实现 + sqlite3/mysql/pg 方言层
internal/cache/          可选 redis 缓存（Store 装饰器：读穿透 + 写失效）
internal/config/         配置加载 + schema 迁移链（当前 v8：adminAccess 管理面 IP 准入）
internal/superadmin/     超管账户文件（0.1.0：明文→首用转密文、generation 纪元吊销、无库可用）
internal/api/            Server 聚合 + gin 双引擎路由（用户口 lanFilter/管理口 adminIPFilter）+ v1_* handlers
internal/auth/           JWT / refresh token / 分享令牌
internal/perm/           目录权限引擎（用户/用户组/public，就近祖先继承）
internal/transfer/       分片上传管理 / 路径锁 / 令牌桶限速
internal/fileops/        路径安全 / 复制移动 / 版本库 / 后台清理器
internal/ai/             AIProvider 接口 + openai/ollama/none
internal/adminui/        go:embed 管理员 Web 界面（web/）
internal/balancer/       内置负载均衡反向代理
internal/tlsx/           自签证书生成
deploy/                  Docker / compose / nginx 样例
```

## 版本兼容

见 `CHANGELOG.md` 兼容矩阵：Server 0.3.x 自含元数据层（无需独立 Db 工程），
兼容 Client 0.2.x/0.3.x（API 契约未破坏，仅 health 加性新增 `db:"none"` 字段）。
0.2.x Server 升级为 0.3.0 时配置自动迁移，但 json 驱动的元数据（用户/分享/版本）
不再读取，需重新初始化或手工迁移。
