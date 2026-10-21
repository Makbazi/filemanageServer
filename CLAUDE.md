# CLAUDE.md — filemanageServer

> 本文件供 Claude Code 及开发者快速了解本工程。**自动更新约定（需求2）**：
> 任何 API/配置/目录结构/行为变更，**必须在同一次改动中**同步更新本文件、
> `README.md`（如涉及用法）、`CHANGELOG.md`（含兼容矩阵）。自 0.1.1 起**产物版本号取
> 最新 git tag**（build.sh 经 `-ldflags -X` 注入 `internal/version/version.go` 的 `Version`，
> 其源码默认 `"unknown"`），打 tag 即定版，通常无需再手改 version.go 常量。
> **0.2.12（2026-10-21）**：认证批——配套 Client 0.3.13 / mobile 0.2.0（用户
> 2026-10-21 清单 1–6 条）：按推荐方案表落地 **OAuth2/OIDC 授权码+PKCE**（本服务
> 端=私有 IdP：`/oauth/authorize` 登录页、`/oauth/token` 三 grant（授权码/刷新
> rotation/RFC 8628 设备码）、`/oauth/revoke`、`/oauth/userinfo`、发现文档；
> Access Token 仍是认证图不透明 Bearer 凭证——0.2.3"凭证不编码身份"红线不破，
> id_token 为最小 HS256 声明回执不参与鉴权，密钥 `<dataDir>/oidc.key`）；
> **二次验证**（TOTP=RFC 6238 纯标准库 / 邮箱验证码=net-smtp mailx；默认不启用、
> 客户端自助选择性启用、管理端 `PUT /admin/users/:id/mfa` 治理；已启用者旧直登
> 端点 403）；**邮箱**非必须配置（`mail` 段未配=邮箱通道明确报错、TOTP 不受影响）；
> **管理台 BFF+Cookie Session**（fm_admin HttpOnly/SameSite=Strict + `X-FM-CSRF:1`
> 屏障 + `POST /admin/logout`；localStorage token 废除）；auth_sessions 加列
> flow_data、kind 扩 flow/code/browser/refresh/device/devuser（authCtx 只认 user）、
> users 加列 email/email_verified/mfa_method/mfa_secret；Store 新方法
> UpdateUserAuth/UpdateAuthSessionFlow；Center 新 LookupHash/RevokeByHash/UpdateFlow/
> Spec.Plain/FlowData，**byUser 反向索引扩至全部非 share kind**（改密即连带清
> 未消费授权码/中转/浏览器态/刷新/设备行）；config schemaVersion **12→13**
> （session.refreshDays/loginTTL、mail 段、oauth 段）。旧客户端 ≤0.3.12 全功能
> 无感（POST /auth/login 保留——维护者决策）。定版即打 tag `v0.2.12`。
> **0.2.11（2026-10-20）**：破坏批——配套 Client 0.3.12（用户 2026-10-20 清单 1–6 条）：
> 管理/用户 API 彻底拆分（管理台专属 login/acl/links 端点）、账户/昵称/用户 id 三分离
> （users 加列 nickname；家目录 `user_<账户>_<用户id>`、回收站并级 `<目录名>_trash`，
> 一次性迁移 `userhomes.v8` 含七表物理键改写）、用户端口数字用户 id 全隐藏、超管建分享
> 410 修复、暂停期管理台可登录。config schemaVersion 保持 12。定版即打 tag `v0.2.11`。
> **0.2.9（2026-10-16）**：版本号对齐批——配套 Client 0.3.9 修复批，服务端代码零改动、
> `/api/v1` 协议/响应形状/配置 schema（12）/Store 全部不变，定版即打 tag `v0.2.9`。

## 工程定位

FileManage 服务端（Go + **gin**，module `filemanageServer`）。元数据一律经本工程
`internal/store` 的 `Store` 接口读写（内建，file/sqlite3/mysql/postgresql 四驱动
+ 可选 redis 缓存），**禁止**绕过接口直接操作数据库。**0.1.1 起未配置数据库时默认使用
内置 `file` 文件存储驱动（全功能可用，旧"降级模式 503"取消，单实例 flock 保护）**。
**0.2.3 起认证不使用 JWT**：不透明随机凭证 + 服务端三层认证图（见 `internal/auth`）；
**0.2.4 起**用户端口登录只回 `{"token"}`（管理台保留 `{token,user}`）、`/auth/refresh`
移除（滑动续期服务端无感、凭证不轮换）、会话可选绑定客户端唯一标识（`X-Client-Id`
逐请求核对、多端互不挤占）、用户会话滑动窗口 1h（`session.idleMinutes`）。

## 目录结构（cmd/internal 布局）

| 路径 | 职责 |
|---|---|
| `cmd/filemanage-server/main.go` | 入口：`-mode worker|balancer`、配置加载、装配、TLS/清理器启动；**0.1.0：超管账户文件初始化 + `adminPassword` 重置触发器 + 遗留同名库内管理员告警（warnShadowedAdmins）+ 执行位后台清扫（sweepExecBits→fileops.SweepExec）；0.2.7：配置加载后 `logx.Setup(cfg.Log, cfg.DataDir)`（失败打印 stderr 并退出 1，worker/balancer 两模式同）；首启超管随机明文口令 `fmt.Printf` 只回显控制台、不入日志文件（敏感纪律）** |
| `internal/store/` | 元数据抽象（内建，原 filemanageDb）：`Store` 接口 + 单一 `sqlStore` 实现（查询/upsert/行映射一律 **gorm**（需求2），`rows.go` 行模型）+ `dialect_*.go` 方言层（仅剩幂等 DDL 清单/DSN 归一/LIKE 转义/pg 序列对齐）+ **`proxy.go`（可运行期原子换库的 Store 代理，全工程降级判定统一 `store.Degraded`）** 与 **`copy.go`（跨驱动全表迁移复制，keyset 分批保留主键）**；`user_roots`（目录分配）/`server_meta`（迁移标记）/`audit_events`（审计日志，保留 90 天）基线即有；0.1.0 超管会话复用 sessions 表 `user_id=0` 哨兵（`store.SuperAdminID`，无外键 + users 自增主键从 1 起天然不冲突）；超管账户本身在 `internal/superadmin` 文件、不入库。**0.2.3 新表 `auth_sessions`（三层认证图持久层）**：kind(user|share)/user_id(0=超管哨兵、-1=share 保留防级联误伤)/username/role/super/gen/ref/client_id(0.2.4 加列，默认 ''=未绑定)/token_hash UNIQUE/expires_at/last_used_at/created_at——四方言 ddl 清单追加（幂等只增不改，sqlite 既有 .db 字节兼容）+ pgSeqReset + file 驱动新表序 14（旧帧 1..13 语义不变、maxid 短数组补零兼容）+ Store 接口 7 新方法（Create/Get by hash/Renew by id（无行→ErrNotFound，撤权竞态判据）/Delete by hash/Delete by user/DeleteExpired/ListActive）sqlStore/fileStore/proxy 全实现、装饰器透传；**0.2.4 `client_id` 列**：四方言 CREATE 文本含列，存量 0.2.3 库经 `dialect.extra` 幂等加列守卫（`Migrate()` 建表清单后执行——sqlite `PRAGMA table_info` 探查后 ALTER / mysql information_schema 探查后 ALTER / pg `ADD COLUMN IF NOT EXISTS`；只增不改，需求25），file 驱动 JSON 行天然加性（旧行解出空串）；**旧 `sessions` 表与其 6 方法自此冻结不再读写**（磁盘兼容保留，不 ALTER 不 DROP）；DeleteUser 级联两侧（sql tx 清单 + filestore 批次）补 auth_sessions。**0.1.1 `file` 驱动（需求40）**：`filelog.go`（帧 `op+len(LE32)+data+crc32c`，文件头 FMFS/v1/kind，MANIFEST 双代原子换，末段尾撕裂截断/中段 CRC 硬失败）+ `fileindex.go`（全内存 PK/唯一键二级索引，Put/Del 单一事实源）+ `filestore.go`（生命周期/写路径/用户等表；RWMutex 单写者，50ms 批量刷盘+关键操作 fsync+8MiB 段滚动）+ `filequery.go`（其余表，语义逐方法对齐 sqlstore）+ `filecomp.go`（快照压缩回收墓碑）+ `filecopy.go`（rowSource/rowSink 实现，file↔db 双向）+ `filelock_unix/other.go`（flock 单实例）；`DriverFile="file"`，""/none 归一为 file，DSN=目录（默认 `<dataDir>/filestore`）；**0.2.1 `PutFileMeta` content_hash 空值保留语义（sqlstore+filestore 双实现、四方言一致）：incoming 哈希为空且旧行 size/mtime（秒）未变 → 保留旧 content_hash（handleList 例行刷行不再清空）；哈希由 recordMeta 在整传/分片 complete/回滚时写入**；**0.2.11 需求4：`users` 加列 `nickname`（TEXT/VARCHAR(128) NOT NULL DEFAULT ''——四方言 CREATE 含列 + `dialect.extra` 幂等加列守卫照 client_id 先例，file 驱动 JSON 加性；`User.Nickname`/`userRow` 接线）；Store 接口新增 `UpdateUserNickname(id,nickname)`（仿 UpdateUserPassword）与 `RewritePhysicalPath(rootID,oldPrefix,newPrefix)`（七表 file_metas/ai_descriptions/trash_items/folder_acls/share_links/file_versions/upload_sessions 的存量物理键前缀改写——sqlStore 单事务按主键 select-then-update、Go 侧前缀判据免 LIKE 方言差异，fileStore 单写锁内摘旧二级键/改字段/重挂/追加帧+一次 syncNow，proxy 透传、cache 装饰器透传后 FlushAll；幂等二次跑 0 行；0.2.11 布局迁移 userhomes.v8 专用）** |
| `internal/cache/` | 可选 redis 缓存：`Cache` 接口 + Noop/Redis 实现 + `decorator.go`（Store 装饰器，读穿透/写失效；装饰包在 Proxy 之上，透传 Degraded、提供 FlushAll。会话/认证图刻意不经本装饰器——0.2.3 认证中心自建 redis L2，哈希键 + 每用户反向集合解决"按用户撤权无法枚举失效键"的旧约束）|
| `internal/config/` | Config 类型 + 默认值 + 校验；`migrate.go` schemaVersion 迁移链 v1→…→v7（dataDir）**→v13（0.2.12：session.refreshDays=30（OAuth 刷新令牌绝对期）+session.loginTTL="12h"（授权页浏览器登录态）、`mail` 段（host/port/ssl/username/password/from/fromName——host 空=邮箱通道关闭，口令绝不入日志）、`oauth` 段（issuer 空=按请求 Host 派生；clients 空=finalize 落内置注册表 desktop/mobile/web——redirectUris 精确匹配+AllowLoopback 任意端口 http loopback，RFC 8252）；migrate12to13 只补缺；访问器 RefreshDuration/LoginDuration/OAuthClientOf/Mail.Enabled、文件访问器 OIDCKeyFile）**→v12（0.2.8：maxUploadBytes 默认 0=不限）**→v8（adminAccess 管理界面 IP 准入，需求38：local/lan/any/custom+IP/CIDR 列表，finalize 预解析网段、custom 非法值硬报错；存量配置补 `lan` 保持旧管理端口 lanOnly 语义，新装默认 `local`）→**v9（0.2.3：新增 `auth` 段——三层认证图 memMaxEntries=200000/memTTL=5m/redisMaxEntries=5000000/sweepInterval=10m/touchWriteEvery=10m，migrate8to9 只补缺不覆盖）**→**v10（0.2.4：`session.idleMinutes`——用户会话滑动闲置窗口默认 **60 分钟**（需求22 微调；idleDays 弃用保留仅作回退），migrate9to10 只补缺不覆盖）**→**v11（0.2.7：`log` 段——服务端日志 level=warn（debug|info|warn|error，非法取值收敛 warn；0.2.11 扩为六级 trace|debug|info|warn|error|fatal——仅取值域加宽、无结构变更，schemaVersion 不再动）/dir（空=`<dataDir>/logs`）/maxMB=10/maxFiles=3（含当前文件、上限 10），见 internal/logx；migrate10to11 只补缺不覆盖，DefaultConfig/finalize/示例配置同步带出）**→**v12（0.2.8：`maxUploadBytes` 默认 2GiB→**0=不限**——大文件（视频/RAW 归档）在 2GiB 默认限额下 `POST /upload/session` 必现 413=用户报"上传彻底不能用"的根因；migrate11to12：键缺失补 0、值恰为历代默认字面量 2147483648（DefaultConfig/示例/migrate3to4 同串）升 0、**用户显式自定义限额一律保留**；判定与 413 语义原样——显式限额时依旧生效）**（改配置结构必须加迁移）；trashDir/staging/versions/secretFile/sqlite DSN 未显式配置时从 `dataDir` 派生；`AdminAccountFile()=<dataDir>/admin-account.json`、`AdminAllows(ip)`、`LogDir()`（日志目录访问器）；`AccessTTLDuration()`（管理台会话绝对时长）/`IdleDuration()`（用户凭证滑动窗口；0.2.4 起 `idleMinutes` 优先、默认 1h，`idleDays` 弃用仅回退）/`AuthMemTTL|AuthSweepInterval|AuthTouchWriteEvery()`；**`jwtSecret`/`secretFile` 已弃用（0.2.3 认证不再读取二者）**——字段与 jwt.secret 物化保留仅为存量配置兼容，`PersistDBEdit` "绝不整体序列化 Config（会落 jwtSecret）"纪律随之保真 |
| `internal/api/` | `Server` 聚合结构 + **gin 双引擎路由**（`router.go`：`Handler()` 用户端口 / `AdminHandler()` 管理端口，registerV1 + registerAdminAPI + **registerAdminDBAPI（配库面）**；**0.2.12 认证批**：registerV1 新增 `/oauth/*` 组（authorize/login/mfa/token/revoke/userinfo/device/device-input/device-verify）与 `/.well-known/openid-configuration`、auth 组新增 `/auth/mfa` 自助六端点；AdminHandler 引擎级 `adminCsrf()`（非 GET 须 X-FM-CSRF:1）+ `POST /admin/logout`（lite 组）+ 登录/super 重发凭证同步 Set-Cookie fm_admin（HttpOnly/SameSite=Strict/Path=/，TLS 启用加 Secure）+ `/admin/info` 加性 `who`；`v1_oauth.go`＝三 grant token 端点/flowData 协议态编解码/浏览器静默授权/PKCE 校验/redirect 白名单/设备码双生行互引（哈希）/最小 id_token 组装；`oauthpage.go`＝登录/批准页内嵌 HTML（字符串替换渲染、无外部资源、JS 只调同源 JSON 端点，绝对 base 由服务端下发）；`v1_mfa.go`＝自助二验（enroll 口令复验+确认才落密钥/emailbind flow 行 OTP/disable 吊销全会话）+ 管理端 `PUT /users/:id/mfa`（off/totp/email/reset；目标 admin/super 400）；`v1_auth.go`：handleLogin 二验账户 403 拒绝（旧端点保留）、handleMe 加性 email/emailVerified/mfa、handleAdminLogin/Logout Cookie 化、新增 checkPassword；`middleware.go`：authCtx 管理端口 Cookie 回退（用户端口绝不读）、adminCsrf、sanitizeLogTarget 键表扩 code/device_code/refresh_token/code_verifier/user_code/access_token；`server.go`：Server.oidcKey 惰性加载（失败只降级不发 id_token，绝不 Fatal 于请求路径）；authCtx kind 门控天然拒 flow/code/browser/refresh/device/devuser（非 user→401）；`internal/mailx`＝net/smtp+tls 出站邮件（SSL=465 隐式/否则 STARTTLS 必需——不支持则拒发）|**0.2.0 用户端口新增 `POST /api/v1/mkdirs` 批量建目录（需求41 目录上传，加性端点：{rootId,paths[1..1000]} 先全量校验（permsBatch Upload + scope jail）任一失败整请求 4xx 零创建，再逐路径持锁幂等 MkdirAll**）+ `scope.go`（jail 虚拟根唯一翻译层；集中拦截家目录保留名 `.fmtrash`）+ gin 中间件 + `v1_*.go` 各领域 handler；**0.1.0：`newEngine(netGuard,...)` 参数化网络准入（用户口 lanFilter / 管理口 adminIPFilter）；`Server.Super *superadmin.Store`；authCtx 先解析 claims 再分流（超管令牌零 Store 调用，降级可用）；`v1_super.go` 超管账户页（GET/PUT /admin/super + requireSuperAdmin 守卫）；`v1_auth.go` change-password + 超管登录优先 + 移除引导注册；`v1_dbconfig.go` 鉴权收敛为单一管理员登录态（删 firstBoot/dbUnlock 三态）；`/api/v1/admin` 拆 lite 组（info/super 组外 requireDB，其余仍 requireDB）；0.1.1：`manualPause/autoPause atomic.Bool` + `maintenanceGate()`（挂 v1 组/share 组/管理业务组，health/版本发现/配库面/超管页豁免）；`v1_dbconfig.go` 新增 `POST /admin/db/export`（纯导出 DB→文件存储不换引擎）与 `POST /admin/db/pause`（手动暂停开关），connect 支持 `driver=file`（含同目录拒切）、迁移全程 `withAutoPause` 屏障包裹**；**0.2.1：`v1_thumb.go`+`thumbdec.go` 新增 `GET /api/v1/thumb?rootId&path&w=`（anon 组、守卫逐行同 handlePreview——degradedView(Download)+resolve，匿名语义与 /preview 完全一致；服务端生成 ≤512px JPEG，标准库解码+12MP/6000px/64MiB 闸门+包级信号量限流，RAW 内嵌 JPEG 段链扫描移植自客户端 thumb.rs，扩展名表三处同抄；404/413/415/429 供客户端退化链。**0.2.2：白名单补 BMP（thumbdec.go 纯标准库自实现解码：CORE/INFO/V4/V5 头 × BI_RGB 1/4/8/16/24/32bpp 调色板与真彩、BI_BITFIELDS R/G/B 掩码位宽拉伸、BI_RLE8/RLE4、负高度自上而下、非 RLE 4 字节行对齐；读取前判长绝不 panic；bmpDimensions 先过尺寸闸门；畸形→415 走既有回退链）——使服务端网格出图能力与客户端本地缩放机制对齐；webp 维持 415（VP8 不可标准库自研；Client 0.3.6 起 415 一律图标、无整文件兜底）**）；`v1_check.go` 新增 `POST /api/v1/check {rootId,files:[{path,size,hash?}]}`（auth 组，permsBatch Upload+scope 全量校验整请求 4xx 语义同 mkdirs；state=missing|differ|same|unknown——file_metas.content_hash+秒级 mtime 新鲜性判据，目录重传预检支撑）；`recordMeta` 携带 FileEntry.Hash 入 content_hash；`respond.go` `isNetDisconnect`→499+info 日志（断连不记 internal error）；mtime 口径统一真实 stat（整传/complete/回滚三处，v1_versions 删 time.Now() 覆盖行）**；**0.2.3 认证层去 JWT：`Server.Auth *auth.Center`（NewServer 末位参数）；authCtx/v1_auth（login/refresh/logout/changePassword）/v1_share（st 签发校验 kind=share）/v1_super（PUT 纪元轮换=RevokeUser(0)+重发）/v1_dbconfig（dbStatusAuthed）全部走认证图——Bearer 凭证 Lookup、身份取自条目（不再逐请求 GetUserByID）、撤权三层同步（logout/改密/重置密码/角色变更/删号）、share 凭证与登录身份按 kind 互斥门控、响应字段形状不变（refreshToken=同值别名）**；**0.2.4 认证响应面收敛（配套 Client 0.3.6，破坏批）：用户端口 `POST /auth/login` 只回 `{"token"}`（管理台 adminModeKey 分支保留 `{"token","user"}`——控制台门禁依赖）；`handleRefresh` 与路由删除（滑动续期由 authCtx→Touch 服务端无感完成、凭证串永不轮换；旧客户端 refresh 退化=401 重登）；`handleLogin` 可选 `clientId`（≤64 字符，超长 400）绑定 kind=user 会话入认证图；authCtx 对绑定会话逐请求核对 `X-Client-Id` 头（缺失/不符=401；未绑定会话=旧客户端/移动端/管理台不查头，行为同前）；logout 接受 `{token}`（兼容旧字段 refreshToken、再回退 Authorization 头凭证）；`handleMe` 仅回 `{username}`（不查库、不查角色——身份/权限判定全在服务端认证图内闭环）；`PUT /admin/super` 响应删 accessToken/expiresIn 别名；`thumbdec.go` JPEG/RAW 改 YCbCr 平面盒式直降采样（无全幅 RGBA 中间层），分档闸门 `thumbMaxPixelsJpeg=50MP/thumbMaxSideJpeg=9000`（48MP 手机原图可出图；PNG/GIF/BMP 维持 12MP/6000px RGBA 路径），配合 Client 0.3.6"浏览零整文件"**；**0.2.7 日志批（响应体形状零变更）：`middleware.go` 新增 `traceGin()`（`X-Trace-Id`→`X-Request-Id` 取值并消毒 ≤64 `[0-9A-Za-z._-]`，非法/缺失 crypto/rand 16hex 重生成；回显响应头 `X-Trace-Id`）与 `recoverGin()`（全仓首个 panic 兜底：logx.Error 记 trace+panic+截断 4KB 折叠堆栈、按 respond 信封回 500）；`loggerGin()` 重写走 logx——5xx=Error/4xx=Warn/其余=Info，行 `trace=<id> <METHOD> <path?query> <status> <dur> from=<remote>`，**绝不记头/体**；`newEngine` 统一链序 recover→trace→logger→网络准入→qps→cors（两引擎同享）；`respond.go` internal error→`logx.ErrorT`（499 断连行保留桥接 INFO，0.2.1 契约不变）、`server.go` audit 失败→`logx.WarnT`、`v1_auth.go` logout 撤权失败与 `v1_dbconfig.go` flush/ensure-homes 失败→`logx.WarnT`**；**0.2.8 上传限额默认批（代码零改动、协议/响应形状零变更）：`v1_transfer.go` 两处 413 判定（整文件 `WriteFileAtomicLimit` 与分片 `POST /upload/session` 的 size>maxUploadBytes）随默认值 2GiB→0=不限而默认不触发——显式配置正数限额时行为与历代完全一致**；**0.2.11 端口彻底拆分与身份三分离（破坏批，配套 Client 0.3.12）**：`AdminHandler` 删 `registerV1` 重复注册（两端口注册集零交集，管理台不再反向调用业务端点）——新增管理专属裸组 `POST /api/v1/admin/login`（`handleAdminLogin`：超管+DB 管理员分支从 handleLogin 抽出、普通账户 403；组外 requireDB/authCtx/maintenanceGate——暂停期可登）、`PUT /api/v1/admin/acl`（物理键直存无角色分叉）、`POST/PUT/DELETE /api/v1/admin/links`（管理员代管分享，响应 linkViewFull）；`handleLogin`/authCtx 的 adminModeKey 角色分叉删除（保留标记本身供 authCtx 分流与 adminUI 字段）、`computeBases` 管理整根分支删除；`v1_auth.go`：`handleMe`→`{username,nickname}`（现读库取昵称）、新 `handleUpdateProfile` `PUT /auth/profile`（normalizeNickname ≤64 rune 去控制符、不吊销会话）、`createUser` 加 nickname 形参、`userView` 加 nickname（仅管理面）；`scope.go`：家目录根客户端令牌逐段遮蔽用户 id（`homeBaseToken/vrootIDOut/jailOut` 出方向统一 handleRoots 的 ID/Jail/alias 键、scopeInfo.VRoot、displayOf；入方向 `translateHomeJail`，scopePhys 直通）、`.fmtrash` 拦截保留；`v1_trash.go` 实体解析并级+三候选；`v1_acl.go`：用户端口主体账户化（`/acl/subjects`→{account,nickname,subject}、条目视图 subject/subjectName 去数字 id/createdBy/物理 rootId，PUT 兼容旧 subjectId 数字入参；解析表请求级 ListUsers 建）；`v1_share.go`：`linkView` 去 createdBy、`linkViewFull` 补 createdBy/rootId/path、创建/更新体解析抽共用助手、**`validLink` 超管创建者（CreatedBy=0 哨兵）跳过校验——修超管建链接必 410**；`v1_versions.go`/`v1_ai.go` 响应视图化（不再转储 store 行——物理路径/userId/createdBy 泄漏面收敛）；`v1_admin.go`：用户 CRUD 收 nickname（Update 用 *string 区分未传/清空）、SetUserRoots 家目录校验改 `HomeDirName`、删号保持只删元数据（孤儿目录留盘） |
| `internal/auth/` | **0.2.12 扩**：`totp.go`＝RFC 6238 TOTP 纯标准库（GenerateTotpSecret/TotpCode/ValidateTotp ±1 步/TotpURI；SHA1/30s/6 位，160-bit 密钥）；`oidc.go`＝id_token 最小 HS256 手工签验（**不引回 golang-jwt**；LoadOrCreateOidcKey `<dataDir>/oidc.key` 0600 首用自生——多实例共享 dataDir 即共享；SignIDToken claims map→JWS）；`center.go` 新增 `Spec.Plain`（user_code 等人类可读凭证直作条目键）、`Spec.FlowData`、`LookupHash`/`RevokeByHash`/`UpdateFlow`（三层同步推进协议态）、`indexedByUser` 判据——**L1 byUser/L2 `fm:auth:u:` 反向索引覆盖全部非 share kind**（user/flow/code/browser/refresh/device/devuser），改密/删号/纪元轮换即时清未消费授权码与中转行（根治 ≤memTTL 窗口双花）；wireEntry 补 flowData（旧 L2 条目缺键解出 ""）。**0.2.3 整包重写（去 JWT，`golang-jwt` 依赖移除）**：`auth.go`＝不透明凭证原语（`NewCredential` 32 字节随机 hex + `HashToken` SHA-256 + `RandomPassword`）；`center.go`＝三层认证图 `Center`：L1 进程内手写 LRU（memMaxEntries 默认 20 万，条目新鲜期 memTTL 默认 5m 超龄回源，byUser 反向索引）→ L2 可选 redis（`fm:auth:<hash>` SETEX + ZSET idx 近似 LRU 上限 500 万裁剪 + `fm:auth:u:<uid>` 反向集合，自建连接池独立于 cache.Cache）→ L3 Store（事实源）；`Issue`（L3 先同步落库，发放不可吊销凭证=硬错误）/`Lookup`（L1→L2→L3 逐级回填，L2 故障落 L3 fail-open、pendingRevoke 本地兜底）/`Touch`（滑动续期 L1 即时+置脏节流）/`Revoke`/`RevokeUser`（拒负 ID；三层全清）/`WarmStart`（token_hash 稳定窗口预热，失败非致命）/`Run`（清扫器：过期三层回收、脏续期批量写回、Renew RowsAffected=0 证伪即摘、pendingRevoke 重试、L2 索引修剪；ctx 末次刷脏）。全部 JWT 面已删：IssueAccess/ParseAccessClaims/IssueSuperAccess/IssueShareToken/ParseShareToken |
| `internal/perm/` | ACL 权限引擎：admin 全权；命中 ACL 层就近裁决（同层 OR）；匿名强制不可下载/上传/删除且默认只读；无库降级分支=仅浏览（D1 决策，0.1.1 起仅异常路径可达） |
| `internal/transfer/` | 分片上传 Manager、per-path 锁注册表、令牌桶限速；**0.2.1**：CreateOrResume 同用户命中旧会话但 size/chunkSize 不符→作废重建（chunkSize 先归一再比对）、WriteChunk 标记段 per-session 锁（`Key("#sess",id)`）重读-合并-回写（修并发丢标记）、Complete 成功后 DB/清账失败记日志不再静默 |
| `internal/fileops/` | 路径安全（SafeRel/JoinRoot）、copy/move、SHA256、版本库 VersionStore、后台 Cleaner；**`homes.go`：用户名目录名校验 / 家目录与 `.fmtrash` 路径 / EnsureHomeFor（建号即建目录+追加 `_users` 分配）/ EnsureUserHomes（启动幂等补齐，标记 `userhomes.v7`）**；**0.1.0 需求37：`stripExec`（MovePath 两分支 + CopyFile 收尾，仅常规文件 `perm&^0o111`）/ `SweepExec(dirs)`（启动后台清扫 roots+Users 存量文件执行位）**；**0.2.1：`SweepExec` 同一次遍历顺带回收崩溃残留写路径碎片（`*.fmmerge-*`/`*.fmtmp-*` 常规文件且 mtime>24h，返回 (去位数, 清残数)）；Cleaner uploading 僵尸判据 CreatedAt→UpdatedAt（慢速续传不连坐；completed/aborted 留 24h、孤儿 staging mtime>24h 不变**；**0.2.11 需求4/5：`homes.go` 重写——`HomeDirName(账户,id)="user_<账户>_<id>"`、`HomeAccountOf`（右锚定解析，供 scope 令牌遮蔽）、并级回收站 `UserTrashDir/UserTrashPath(<目录名>_trash)`、`TrashBlobCandidates`（并级→中间态嵌套→旧名嵌套）、`UsernameAsDir` 作用域改账户名、`EnsureHomeFor` 建家目录+`_trash` 双目录（id 唯一命名根除同名复用旧目录 bug）、一次性迁移 `MigrateUserHomeLayout`（标记 `userhomes.v8`：磁盘先改名→`.fmtrash` 条目逐搬→七表 RewritePhysicalPath→SetUserRoots 换 SubPath；两代目录名并存=运维手工布置大声拒绝、不置标记下次重试）；`Cleaner.removeTrashBlob` 多候选化不再按用户名直拼** |
| `internal/superadmin/` | **0.1.0 需求32/33/34/36 新包**：文件承载超管（`<dataDir>/admin-account.json` 0600，不进数据库，无库降级/热切换后同样可用）；明文↔bcrypt 密文状态机（首次成功登录转密文，需求34）、`generation` 凭据纪元（外部编辑文件即视为重置→纪元+1 回写吊销旧令牌）、`EnsureCreated/Match/Verify/SetCredentials/Plaintext/Generation`；其他管理员 = 数据库 users 表 role=admin（需求35），由超管经管理台创建 |
| `internal/ai/` | AIProvider 接口 + openai/ollama/none |
| `internal/adminui/` | go:embed `web/`（纯手写 HTML/JS/CSS，无构建链）挂 `/admin/`（**仅管理端口**，含目录分配编辑器与物理根浏览器；"数据库"页：状态/选型/测连/热切换/迁移；**0.1.0：登录页降级提示改"以超管账户登录"（删解锁卡/引导注册卡/setup-entry/`refreshTabAccess` 按超管身份+降级态收敛页签）；新"超级管理员"页（账户名/新口令、明文有效期徽章、PUT 后换用重发令牌——0.2.3 起该令牌为认证图随机凭证，app.js 无格式假设零改动）；用户页管理员行编辑/删除与 role=admin 选项仅超管可用；概览加 adminAccess/超管账户名卡；0.1.1："数据库"页驱动列表含 file（无库默认，dsn 可留空=派生目录、输入框不禁用），新增"导出到文件存储（不切换）"按钮（复用 DSN 目录字段+覆盖勾选，409 自动勾覆盖）、"暂停/恢复客户端操作"按钮（状态卡新增"客户端操作"徽章 = paused）、`/status` 的 `paused` 字段驱动渲染**；**0.2.11 需求3：app.js 全部改调管理端口专属端点（登录→`/api/v1/admin/login`、ACL 保存→`PUT /api/v1/admin/acl`、分享 CRUD→`/api/v1/admin/links`——不再复用用户端口业务 API）；分享完整 URL 按 `/admin/info` 的 `listen`+`tlsEnabled`+location.hostname 构造（`userPortBase()`，`/s/` 仅用户端口注册）；用户页表头与弹窗改"账户"+新增昵称列/输入框（创建/编辑均提交 nickname）、删号确认文案注记"家目录与回收站目录以孤儿态保留 Users/ 下"、顶栏欢迎显示 `nickname||username`、"用户名"字样统一改"账户"**） |
| `internal/balancer/` | 轮询反向代理 + 健康检查（`-mode balancer`） |
| `internal/tlsx/` | 自签证书生成 |
| `internal/logx/` | **0.2.7 服务端日志（引入标准库 log/slog 日志库，零新依赖；0.2.11 六级化）**：`Setup(config.LogConf, dataDir)`——`<logDir>/filemanage-server.log` 按大小轮转（写前判长、改名链 `.log.1`/`.log.2` oldest-first、溢出删除，**含当前最多 maxFiles=3 个、单文件 ≤maxMB=10MB**，互斥守护、重启续写既有大小；日志目录自动创建）+ `io.MultiWriter` 镜像 stdout；等级**六级（0.2.11）**`trace|debug|info|warn|error|fatal` **默认 warn**（未知取值收敛 warn；trace=-8、fatal=16，Handler ReplaceAttr 渲染 TRACE/FATAL 等级名）；printf helpers `Trace/Debug/Info/Warn/Error/Fatal` 与链路变体 `TraceT/DebugT/InfoT/WarnT/ErrorT`（行首 `trace=`）；**Fatal=六级最高、任何配置等级下必落行后 os.Exit(1)**——启动 fail-fast 站点全部改用 logx.Fatal（旧 stdlib log.Fatalf 经桥接按 INFO 走、默认 warn 下进程死得无声无息，本批根治）；stdlib log 桥接（`SetFlags(0)`+逐行按 **INFO** 送 slog——存量生命周期行默认等级下静默，属"默认 warn"的刻意降噪）；敏感值（口令/令牌/请求体/头）永不入日志；0.2.11 再扩：**URL 携带的分享凭证也脱敏**（api `sanitizeLogTarget`：`/s/<token>` 路径段→`/s/**`、`st/token/password` 等查询值→`**`，loggerGin/writeErr 同用）；本批补充必要日志——authCtx 拒绝类（X-Client-Id 不符/超管·管理员凭证出现用户口/超管纪元吊销=WARN，坏凭证=DEBUG）、登录结果（用户口/管理台成功=INFO、防爆破锁定=WARN）、改密/改昵称/建删账户/目录分配/ACL/分享变更（INFO，删账户 WARN）、balancer worker 摘除（WARN）、家目录迁移 skip 类（WARN）；均只记账户名/结果，绝不记凭证口令值——纪律见包注释与 CHANGELOG |
| `internal/version/` | `Version`（`var`，源码默认 `"unknown"`，build.sh 经 `-ldflags -X` 注入 git tag 版本号）与 `APIVersions`；另有 `GitTag/GitCommit/BuildTime`（同经 `-ldflags -X` 注入，`-v` 输出） |
| `deploy/` | Docker×3 / entrypoint / compose / nginx 样例（**运行**镜像） |
| `build-env/` | **编译**环境（Dockerfile.rockylinux9 + container-build.sh + README；产物规范 linux/windows(.exe)/arm64 静态编译，经 `./build.sh -a -d` 编译 / `-i` 构建镜像 / `-n` 拉取镜像调用；版本号由 git tag 推导，工作区脏带 `-dirty`） |

## 运行

```bash
./build.sh -i                   # 构建编译环境镜像（docker build，不编译工程）
./build.sh -n                   # 拉取编译环境镜像（docker pull，不编译工程）
./build.sh -a all -d release    # Docker 编译环境（build-env/Dockerfile.rockylinux9）多平台静态编译：
                                # 版本号取最新 git tag（去 v；工作区脏带 -dirty），非 version.go 常量
                                # build/filemanageServer_linux_0.2.8 / _arm64_0.2.8 / _windows_0.2.8.exe
                                # -a 取 linux|windows|arm64|all；-d debug → build/debug/；-f 强制重建镜像
                                # （release: -trimpath -s -w；debug: -N -l；均 CGO_ENABLED=0 静态、均注入版本信息）
./build/filemanageServer_linux_0.2.8 -v                            # 打印 版本号/tag/commit_id/编译时间 后退出
./build/filemanageServer_linux_0.2.8 -config server.json           # worker（用户端口 + 管理端口双 listener）
./build/filemanageServer_linux_0.2.8 -mode balancer -config balancer.json
```

首次运行生成示例配置；`/api` 版本发现；`/api/v1/health` 健康检查（0.1.1 起无库默认 `db:"file"`——文件存储引擎；仅异常路径可能 `db:"none"`）。
**0.2.7 日志**（0.2.11 六级化）：请求全链路带 `X-Trace-Id` 响应头；日志默认落 `<dataDir>/logs/filemanage-server.log`
（含当前最多 3 个、单文件 ≤10MB 轮转，`log.dir/level/maxMB/maxFiles` 可配、等级 `trace|debug|info|warn|error|fatal` 默认 warn）；
口令/令牌/请求体/头永不入日志（首启超管明文口令仅控制台回显）；URL 中分享凭证也脱敏后记录（`/s/**`、`st/password=**`）；启动 fail-fast 站点走 logx.Fatal——任何等级配置下必落盘。
管理台在**独立管理端口**（`adminListen`，默认 `127.0.0.1:8485`）：浏览器本机访问
`http://127.0.0.1:8485/admin/`；用户端口无任何管理路由（404）。**0.2.11 需求1/3 起反向
亦然：管理端口不注册任何用户业务 API（`/api/v1/roots`、`/auth/login`、`/s/:token` 等
一律 404），管理台只调 `/api/v1/admin/*` 专属端点（登录 `POST /api/v1/admin/login`、
ACL 写 `PUT /api/v1/admin/acl`、分享 CRUD `/api/v1/admin/links`）——两端口注册集零交集、
handler 不复用。** 多实例部署时每个实例
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
（匿名访问一律按 public ACL，与有库模式统一）。管理台在线配库并**热生效**
（免重启），0.1.0 起配库前先以超管登录；0.1.1 起三独立操作：切换（connect，可选携数据
迁移）、导出（export，DB→文件纯复制不换引擎）、暂停（pause，冻结用户端口保一致性）；
0.2.3 起全表迁移通道含 `auth_sessions`——换库/导出携带认证条目，切换后既有登录会话
经 L1 memTTL 回源新引擎继续有效。
落盘数据默认归入 `dataDir`（含 `<dataDir>/Users/user_<账户>_<用户id>` 用户家目录与并级
`..._trash` 回收站目录，建用户自动分配；0.2.11 前存量 `<用户名>` 旧布局由启动迁移
`userhomes.v8` 自动改道，删号目录以孤儿态留盘；多实例共享部署时 Users 目录与
versions/staging 同放共享卷——**升级时先单节点迁移或停全节点，勿并行**）；
**0.1.0 需求37**：用户文件（roots + Users）常规文件的执行位在启动后台清扫 + move/copy
写路径被剥离（目录保留 0755 穿越位），服务端托管内容不可被本地 exec。

## 关键约定（改动前必读）

1. **路径安全**：所有文件访问必须经 `Server.scope()`/`resolve()`（root 白名单 +
   `fileops.JoinRoot` 防逃逸 + jail/公开/分享基座校验，统一于 `scope.go`）。任何新 handler
   不得绕过；请求 rootId 是虚拟根 id（`physID[:jail]`），存储层键一律用
   `scopeInfo.PhysID/Phys`（显示↔物理换算只允许发生在 scope/displayOf）。
2. **权限模型（jail）**：管理员经**管理端口**把"物理根+子目录"分配给用户
   （`user_roots` 表，一人可多目录）；用户端口任何主体（含管理员）只见/只可操作自己的
   分配；匿名只能进入 public-view 目录（`GET /roots` = 公开共享目录清单）；被 ACL
   分享给 user/group 的目录自动成为被分享者的虚拟根。登录写操作先 `requirePerm(...)`
   （五位 Decision，判定作用于物理路径）；引擎强制匿名 Download/Upload/Delete=false；
   降级模式（仅异常路径可达）匿名**仅可浏览不可下载**（D1 收紧，degradedView/perm
   nil-Store 分支；**0.1.1 起无库=文件存储，正常部署不再可达**）。
   **所有依赖元数据的路由/分组必须挂 `requireDB()`**（存储不可用时统一 503）；
   **0.1.1 维护暂停（maintenanceGate）**：业务/分享/管理常规组另挂暂停守卫
   （manualPause ∥ autoPause；health/版本发现/lite 组/`/admin/db` 配置面永不拦）。
   管理 API 只注册在 `AdminHandler`（registerAdminAPI），用户端口不存在这些路由（404）；
   **0.2.11 需求1/2/3 强化为"注册集零交集、handler 不复用"**——管理端口同样不注册
   registerV1，管理台所需登录/ACL 写/分享写全部为管理端口专属端点；用户端口响应面
   数字用户 id 永不可见（账户化寻址，见约定14），家目录虚拟根令牌遮蔽用户 id（约定12）。
   **0.1.0 管理面分层（需求33）**：管理员账户的 create/update/delete、角色变更、管理员的
   目录分配是**超管专属**（`isSuperAdmin(c)`，其他管理员 403）；users 表接口一律拒
   `id==store.SuperAdminID(0)`（400，超管不在库内）；`authCtx` 分流后超管令牌的
   `RequireAdmin/RequireLogin` 判定与 DB 管理员同构（Role=admin）。管理端点按是否依赖
   元数据库拆 `lite`（info/super，组外 requireDB）与常规组（见约定11/13）。
3. **写文件并发保护**：覆盖已有文件必须：持 `Locks.Lock(transfer.Key(物理rootID, 物理rel))` →
   `Versions.Snapshot` 旧内容 → tmp+rename 原子替换（锁/版本/回收站/上传会话/ACL/file_meta/
   分享链接一律**物理键**（基线既定），参考 `upload/complete` 与旧 `upload`）。
4. **配置变更**：新增配置字段必须给默认值（`DefaultConfig`/`finalize`）且加入
   `migrate2to3` 式迁移（schemaVersion+1），保证旧配置升级不丢数据（需求25）。
   **注意**：`Store` 接口或方言层任何改动必须四方言（sqlite3/mysql/postgresql +
   **0.1.1 file 文件存储**）语义一致（排序/ErrNotFound/ErrExists/GetMeta 缺键=""，
   见 filequery.go 各方法头注释），且 sqlite 不得改动已发布列/表（磁盘兼容既有 .db
   文件，见 dialect_sqlite.go 头注释）。**file 驱动表演进=追加表序**（0.2.3
   tAuthSessions=14；旧帧 1..13 语义不变，maxid 水位短 JSON 数组补零兼容）。
   0.1.1 无配置结构变更，schemaVersion 保持 8（需求25 不触发；driver 默认值改为
   "file" 属语义变更非结构变更）。**0.2.3：配置新增 `auth` 段 → schemaVersion 8→9
   （migrate8to9 只补缺不覆盖）；Store 侧 auth_sessions 为幂等新建表（四方言 ddl 清单
   追加，非 ALTER）——旧 `sessions` 表与其方法冻结保留、不再读写。**
   **0.2.1：`PutFileMeta` 的 content_hash 空值保留语义
   （incoming 空+size/mtime 秒级未变→留旧哈希）必须在 sqlStore 与 fileStore 双侧同实现；
   读-改-写非原子（多实例并写竞态最坏退化 unknown，方法注释已声明）——任何新调用方
   刷行前须确认此语义不被绕过。**
5. **API 版本**：新端点加进 `registerV1`（gin 分组）；破坏性变更须新开 `registerV2` 并更新
   `version.APIVersions`，旧版本保留（需求17）。对外信封 `{"error":msg}` 与状态码
   是客户端契约，不得随框架变化。
6. **敏感信息**：响应中永不返回密码哈希（模型 json:"-"）；分享链接明文密码只在创建响应出现一次。
7. **多实例**：新增落盘状态必须放在可共享目录（trashDir/uploadStagingDir/versions.dir）
   或入库，避免实例间不可见（需求26/27）。0.2.3 认证图天然满足此条：L3=共享库
   `auth_sessions`——任一节点签发、全节点互认（jwt.secret 共享要求随 JWT 一并废止）；
   L1 为实例内缓存（撤权可见延迟 ≤ `auth.memTTL`），cache.enabled 时 L2 亦为全节点
   共享 redis。（`secretFile` 字段已弃用，不再列入共享清单。）
8. **错误映射**：内部错误经 `statusOf()` 映射 HTTP 码；transfer 包的哨兵错误
   （ErrConflict→409 等）已接入。**0.2.1：客户端断连/连接重置类错误（`isNetDisconnect`：
   context.Canceled/http.ErrAbortHandler/ECONNRESET/EPIPE/io.ErrUnexpectedEOF 及文案兜底）归
   499 非标准码，`writeErr` 只记一行 info——上传大分片被中间设备 reset 属网络常态，
   不得刷 internal error 日志；499 永不进入成功路径语义。**
9. 编译验证：`go build ./... && go vet ./...`（不写测试，需求29）。
10. **不做 git 提交/分支/tag**（需求30），版本 tag 由维护者操作；发版时手动同步
    `internal/version/version.go` 与 CHANGELOG。
11. **Store 装配与热切换**：装配顺序固定 `store.Open → store.NewProxy →
    （可选 cache.Wrap(proxy)）→ auth.New(center，L3=裸 proxy；WarmStart+go Run(ctx)）→
    api.NewServer(cfg, eff, proxy, cch, cfgPath, super, center)（0.1.0 末位传
    `*superadmin.Store`、0.2.3 再末位传 `*auth.Center`）/ Cleaner`；`Server.Store` 恒非 nil（代理），
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
12. **用户家目录模型（需求3；0.2.11 需求4/5 重制）**：内部合成根 `store.HomeRootID="_users"`
    （路径 `<dataDir>/Users`，**只注入 `Server.Roots`，绝不写进 cfg.Roots**；配置
    校验禁止用户根 id 以 `_` 开头）；任何用户创建路径必须过
    `fileops.UsernameAsDir`（作用于**账户名**）+ `EnsureHomeFor`——家目录
    `Users/user_<账户>_<用户id>`（`fileops.HomeDirName`；id 自增永不复用 →
    **删号后同名重建必落新目录**，旧 `MkdirAll(Users/<用户名>)` 静默复用删号
    遗留目录的串号 bug 从结构上消除；删号仅删元数据，目录留孤儿、运维自清），
    回收站实体在**并级** `Users/user_<账户>_<用户id>_trash/<trashID>`
    （无家目录回退中央 `trashDir/u<id>/`；解析候选序=并级→迁移中间态内嵌
    `.fmtrash`→旧名内嵌→中央，`fileops.TrashBlobCandidates`/
    `Server.resolveTrashBlob`）。存量布局一次性迁移
    `fileops.MigrateUserHomeLayout`（标记 `userhomes.v8`，启动+热切换双站点，
    磁盘先改名后写库、可重入；库内物理键行经 `Store.RewritePhysicalPath`
    七表改写——trash/acl/share/version/meta/upload/ai 的 `_users` 根存量行）。
    **客户端可见虚拟根令牌遮蔽用户 id**：`_users` 基座逐段映射
    `user_<账户>_<id>`→账户（`scope.go homeBaseToken/vrootIDOut/jailOut/displayOf`
    出方向统一；入方向 `translateHomeJail`，`scopePhys` 物理键直通），分享进家目录
    的基座同样遮蔽（`alice/docs`）；升级前后 `_users:<账户>` 字面恒效。分配编辑器
    保存时家目录行自动补回（按实存名优先新布局）、`_users` 仅可分配给本人；
    `.fmtrash` 拦截集中在 `scope()`（保留名段 → 403）、列表隐藏于 `handleList`、
    改名占用拒于 `handleRename`——0.2.11 起作为未迁孤儿目录的纵深防御**保留**。
    删除守卫必须同时比较 `sc.Root.Path` 与 `sc.BaseAbs`（jail 基座不可删）。
13. **超级管理员模型（0.1.0 需求32/33/34/35/36）**：超管 = 文件账户（`internal/superadmin`
    读写 `<dataDir>/admin-account.json`，0600，**不进 users 表**），其他管理员/普通用户
    在数据库（role=admin/user）。判定入口：`authCtx` 经认证图解析凭证后**先分流超管条目**
    （entry.Super → 校 generation == 账户文件当前纪元 → 注入合成
    `store.User{ID: store.SuperAdminID(=0), Role: admin}` + **零 Store 调用**，故无库
    降级模式可用、天然绕开 redis 用户缓存）；普通条目身份直接取认证图、不逐请求查库
    （0.2.3）。**任何 users 表
    写接口收到 `id==store.SuperAdminID` 必须显式 400**（`DeleteUser(0)` 的级联会删
    sessions/0.2.3 起还含 auth_sessions 的 user_id=0 行 = 吊销全部超管会话，属哨兵碰撞）。管理面分层：`isSuperAdmin(c)`
    （即 `currentUser(c).ID==0`）为超管专属判据——create/update/delete/role 变更/admin
    的 roots 分配均需超管（其他管理员返回 403），`createUser` 另拒超管保留名。超管令牌
    走用户端口一律 403（Role admin && !adminMode，与 DB 管理员同规则，需求36）。凭据
    纪元 generation：Verify 明文口令成功后转 bcrypt 并 +1、SetCredentials 必 +1、
    检测到文件 password/username 被外部改动（纪元未前进）也 +1 回写——**纪元变化即
    吊销全部已发超管令牌**（authCtx 逐请求核对，无需黑名单；0.2.3 起 PUT /admin/super
    另显式 `Auth.RevokeUser(SuperAdminID)` 三层清空，纪元核对仍作纵深防线）。超管不落 users 故
    `warnShadowedAdmins` 需告警历史遗留的同名库内 admin 账户（已被登录优先匹配遮蔽）。

14. **认证图契约（0.2.3 需求2，本仓认证唯一形态）**：
    - 凭证 = `auth.NewCredential()` 随机 32 字节 hex，**不编码任何身份**；任何层只存其
      SHA-256，明文凭证仅在 Issue 响应出现一次、永不入库/入缓存/出日志。
    - 新 handler 获取身份一律经 `currentUser(c)`（authCtx 注入）；**不得**再引入任何
      自带身份信息（token 内嵌、cookie、query 参数身份）的通道。
    - 撤权必须走 Center（`Revoke`/`RevokeUser`），不得只删库——三层同步是撤权语义的
      定义；`RenewAuthSession` 返回 ErrNotFound 即条目已被他处吊销，Center 会摘除上层。
    - kind 隔离：share 条目永远不代表登录身份（authCtx 拒）；
      user_id=-1 为 share 保留、0 为超管哨兵——users 表写接口拒 id=0 的规则沿用。
    - **响应面（0.2.4 收敛；0.2.11 再收）**：用户端口 `POST /auth/login` 只回 `{"token"}`（身份/角色/
      有效期永不出现在登录响应，全部只在认证图内）；管理台登录改**专属端点**
      `POST /api/v1/admin/login`（裸组、暂停期与无库均可用；回 `{"token","user"}`——
      userView 0.2.11 起含 nickname，管理台保留数字 id 属管理面）；`GET /auth/me` 回
      `{username(账户), nickname}`（0.2.11 需求4：昵称现读库；角色/数字 id/其余档案不对
      客户端可见；新增 `PUT /auth/profile {nickname}` 自助改昵称、不吊销会话）；
      用户端口响应面**数字用户 id 绝不可见**（/acl/subjects 与 ACL 条目账户化寻址、
      /versions /ai /share 响应视图化）。`/auth/refresh` 已删除——活跃滑动续期对客户端无感且凭证串
      **永不轮换**，"重新验证才换凭证"=重登语义；logout 撤销单凭证。
    - **客户端标识绑定（0.2.4）**：登录可选 `clientId`（Client 0.3.6 全请求带
      `X-Client-Id` 头）；绑定条目逐请求核对头（不符/缺失=401 重登），未绑定条目
      （旧客户端/移动端/管理台/超管）不查头；同账号多客户端各持己凭证**互不挤占**
      （登录从不吊销他人会话；改密/重置/删号仍走 `RevokeUser` 全端吊销）。
    - 有效期策略只看持久字段：kind=user 且 role≠admin → 滑动 `IdleDuration()`
      （0.2.4 起 session.idleMinutes 默认 **1 小时**，每次认证请求 Touch 续期、按
      touchWriteEvery 节流写回）；super/admin（管理台）→ 绝对 accessTTL 不滑动；
      share → 绝对 2h。重启 WarmStart 后策略判定不变。
    - **0.2.12 OAuth kind 族（flow/code/browser/refresh/device/devuser）**：全部走
      同一三层图、同一"只存哈希"纪律——`authCtx` 身份注入只认 kind=user（其余
      一律 401，share 互斥沿用）；有效期一律**绝对**（flow 10min/code 60s/
      browser=loginTTL 12h/refresh=refreshDays 30d/device·devuser 10min），滑动
      不适用（Touch 只由 user 行触发）；协议态 JSON 只存 `flow_data`（含 PKCE
      challenge、redirect、nonce、OTP 哈希、设备行互引哈希——**绝不含明文凭证/
      口令**，json:"-" 永不出 API）；一次性消费=先 `Revoke`（或 `RevokeByHash`）
      再签发（flow 双花以 Revoke+ErrBadCredential 拒绝；授权码并发窗口为明示
      残余风险——同秒双兑换需同时持有 code 与 verifier，见 CHANGELOG）；
      **撤权覆盖面**：L1 byUser/L2 反向集合扩至全部非 share kind——改密/重置/
      删号/角色变更走 `RevokeUser` 即连带清该用户未消费的 code/flow/browser/
      refresh/device 行（多实例 L1 陈旧 ≤memTTL 语义不变）。新增落盘状态全部在
      认证图=共享库（约定7 满足；oidc.key 属运行数据加入共享清单）。
    - **0.2.12 管理台 BFF**：fm_admin Cookie 凭证仍是认证图 user 条目（role=admin
      /super），仅"到达方式"多了 Cookie——authCtx 只在管理端口回退读取（用户端口
      读到也不看：adminModeKey 判据不变，admin/super 凭证在用户口 403 依旧）；
      非 GET 必带 X-FM-CSRF:1（adminCsrf，同版锁定的内嵌台恒带）。
    - 降级异常路径（无存储）：Persist=false 条目只存 L1/L2，重启失效——仅超管可达，
      与旧"控制台令牌不落库"语义对齐。

## 当前版本

`internal/version.Version`（源码默认 `"unknown"`，build 时经 `-ldflags -X` 注入 git tag 版本号；本基线对应 **0.2.12**）——**0.2.12（2026-10-21，用户 2026-10-21 清单 1–6 条；认证批，配套 Client 0.3.13 / filemanagemobile 0.2.0；破坏批（前端内嵌台形态）+ 大量加性）**：按推荐认证方案表落地——**①OAuth2 授权码+PKCE**（本服务端=私有 IdP：`GET/POST /api/v1/oauth/authorize|login|mfa` 内嵌登录页闭环、`POST /oauth/token` 三 grant（authorization_code 60s 一次性+PKCE / refresh_token rotation 绝对 30 天重开窗口 / RFC 8628 device_code）、`POST /oauth/revoke`、`GET /oauth/userinfo`、`GET /.well-known/openid-configuration`；redirect_uri 注册表白名单+loopback 任意端口（RFC 8252）、非法绝不重定向；**Access Token=认证图不透明随机凭证（Bearer）**——0.2.3 凭证不编码身份红线不破；**id_token=最小 HS256 声明回执**（`internal/auth/oidc.go` 标准库手工签、`<dataDir>/oidc.key` 0600 首用自生、sub=账户名、**不参与鉴权**、第一方客户端不验签）；**②二次验证**：`internal/auth/totp.go`（RFC 6238 纯标准库 ±1 步）与 `internal/mailx`（net/smtp；不支持 STARTTLS 拒发明文）——**默认不使用、客户端设置页自助选择性启用（口令复验+确认才落库）、管理端 `PUT /admin/users/:id/mfa {off|totp|email|reset}` 治理（策略变更 RevokeUser 强制重登；目标 admin/super 400——管理台登录本批不做二验）**；已启用二验账户在旧 `POST /auth/login` 一律 **403 明确拒绝**；**③邮箱**：users 加列 email/email_verified（非必须；管理端 PUT users 可登记 email——变更即清验证态；验证=用户自助 OTP）；**④管理台 BFF+Cookie Session**：`fm_admin` HttpOnly+SameSite=Strict（TLS 加 Secure）+ authCtx 管理端口 Cookie 回退（用户端口绝不读）+ 全端口非 GET `X-FM-CSRF:1` 屏障 + `POST /admin/logout`；`/admin/info` 加性 `who`；内嵌台 localStorage token 废除、用户页新增邮箱/二验列与治理；**⑤auth_sessions 加列 flow_data、kind 扩 flow/code/browser/refresh/device/devuser**（authCtx kind 门控只认 user；Center 新增 LookupHash/RevokeByHash/UpdateFlow/Spec.Plain/FlowData；**byUser 反向索引扩至全部非 share kind**——改密/删号即时清未消费授权码/中转/浏览器态/刷新/设备行，根治 ≤memTTL 双花窗口）；Store 新方法 UpdateUserAuth/UpdateAuthSessionFlow（四方言+file+proxy+cache 全实现）；**config schemaVersion 12→13**（session.refreshDays/loginTTL、mail 段、oauth 段 issuer+clients 默认注册表 desktop/mobile/web——migrate12to13 只补缺）；`GET /auth/me` 加性 email/emailVerified/mfa；日志脱敏扩 code/device_code/refresh_token/user_code/code_verifier。旧客户端 ≤0.3.12/旧移动端全功能无感（/auth/login 保留、不调新端点即不触新面；升级零一次性代价——存量凭证原样有效）。详见 CHANGELOG 0.2.12 与约定14。**承前 0.2.11（2026-10-20，用户清单 1–6 条；破坏批，配套 Client 0.3.12；跳号 0.2.9→0.2.11 系维护者定版指令）**：①管理/用户 API 彻底拆分——AdminHandler 删 registerV1（两端口注册集零交集），管理台专属 `POST /admin/login`（裸组，暂停期/无库可登）、`PUT /admin/acl`、`/admin/links` CRUD；`/s/:token` 仅用户端口。②账户/昵称/用户 id 三分离——users 加列 nickname（四方言+file 加性）、家目录 `user_<账户>_<用户id>`（id 永不复用根除"删号同名重建串目录"bug；删号目录留孤儿）、回收站并级 `<目录名>_trash`、一次性迁移 `userhomes.v8`（含 `Store.RewritePhysicalPath` 七表物理键改写）；`/auth/me` 扩 `{username,nickname}`、新 `PUT /auth/profile`；用户端口响应面数字用户 id 全隐藏（/acl/subjects 与 ACL 条目账户化、/versions //ai /share 视图化；`_users:<账户>` 令牌遮蔽，升级前后字面恒效）；登录请求/响应形状不变。③修复超管建分享必 410（validLink 哨兵跳过）。④config schemaVersion 保持 12。⑤日志六级化（logx 引入 trace/fatal，Fatal 必落盘后退出、根治启动 fail-fast 被默认等级吞没）+ URL 凭证脱敏（/s/<token>→/s/**、st/password 值遮蔽）+ 补必要日志（认证拒绝/登录结果/账户与权限变更/迁移 skip 等站点；只记账户与结果，绝不记敏感值）。详见 CHANGELOG 0.2.11 条目与约定2/12/14。**承前 0.2.9（2026-10-16）为配套 Client 0.3.9 的版本号对齐批，代码零改动**——**0.2.8（用户 2026-10-16 清单第 1/6 条，上传修复批）**：`maxUploadBytes` 默认 **2GiB→0=不限**——大文件（视频/RAW 归档 >2GiB）在历代默认下 `POST /upload/session` 必现 413（"上传功能彻底不能用"的实测根因；413 判定仅此一处+整文件通道，判定代码与语义原样保留，显式限额时依旧生效）；**schemaVersion 11→12**（migrate11to12：键缺失补 0；值恰为历代默认字面量 2147483648 的配置升 0——历代 DefaultConfig/示例/migrate3to4 写同一串，把"从未被用户显式决定的默认值"升级；**用户自定义限额一律保留**）；`/api/v1` 数据协议与响应形状**零变更**，对一切客户端 0.1.x–**0.3.9** 无感（配套基线 Client 0.3.8、推荐 **0.3.9**：≤0.2.7 旧服务端的 413 改中文指引）。**承前 0.2.7（用户 2026-10-14 清单第 4 条，服务端日志；跳号 0.2.4→0.2.7 系维护者定版指令）**：新增 `internal/logx`（标准库 slog，零新依赖）——`<dataDir>/logs/filemanage-server.log` 按大小轮转（**含当前最多 3 个文件、单文件 ≤10MB**，`log` 段可配，migrate10to11 只补缺、**schemaVersion 10→11**）+ stdout 镜像；等级 `debug|info|warn|error` **默认 warn**；stdlib log 桥接为 INFO（生命周期行默认静默）；`traceGin` 链路 ID（`X-Trace-Id`/`X-Request-Id` 消毒透传或重生成，回显响应头）贯穿 `loggerGin`（5xx=Error/4xx=Warn/其余=Info）与 `recoverGin`（全仓首次 panic 兜底→500 信封）双引擎；真实故障站点（respond internal error/审计失败/配库告警/认证 L2/清理器/上传收尾）转 logx 保持默认可见；**敏感纪律**——请求体/头/口令/令牌永不入日志，首启超管明文口令只回显控制台（fmt.Printf）。**`/api/v1` 数据协议零变更**（仅新增响应头 X-Trace-Id，客户端无感；对一切客户端 0.1.x–0.3.7 兼容）。**承前 0.2.4（用户 2026-10-12 清单第 2/3 条，认证响应面收敛 + 缩略图 JPEG 出图面扩展；破坏批，配套 Client 0.3.6）**：用户端口 `POST /auth/login` **只回 `{"token"}`**——删除 accessToken/refreshToken/expiresIn/user 回显（管理台 adminModeKey 分支保留 `{token,user}`，控制台门禁依赖）；**`/auth/refresh` 端点删除**（滑动续期由 authCtx→Touch 服务端无感完成、凭证串永不轮换；旧客户端/移动端 refresh 链路退化为 401→重登）；登录新增可选 `clientId`（≤64 字符）绑定会话——`auth_sessions` 加列 `client_id`（四方言 CREATE 含列 + 存量库 `dialect.extra` 幂等加列守卫，只增不改），绑定条目逐请求核对 `X-Client-Id` 头（不符=401；未绑定=旧客户端/管理台/超管不查头），多客户端同账号**互不挤占**；用户会话滑动窗口 **21 天→1 小时**（`session.idleMinutes` 默认 60、idleDays 弃用回退，**schemaVersion 9→10**，migrate9to10 只补缺）；logout 接受 `{token}`（兼容旧字段/头凭证回退）；`thumbdec.go` JPEG/RAW 改 YCbCr 平面盒式直降采样（无全幅 RGBA 中间层），分档闸门 50MP/9000px——48MP 手机原图直接出服务端小图（配合客户端 0.3.6 浏览零整文件；PNG/GIF/BMP 维持 12MP/6000px）。旧客户端 0.1.x–0.3.5 与移动端可用但降级（未绑定不查头、过期即重登、窗口 1h）。**承前 0.2.3（用户 2026-10 第三批清单，认证重构去 JWT）**：`/api/v1` 身份认证不再使用 JWT——客户端认证通过后**只拿到一个随机哈希凭证**（不编码身份），真实 user id/username/role 存服务端三层认证图（`internal/auth.Center`：L1 进程内 LRU 默认 20 万条（auth.memMaxEntries，条目新鲜期 auth.memTTL=5m 超龄回源）→ L2 可选 redis 默认上限 500 万条（auth.redisMaxEntries，ZSET 近似 LRU 裁剪）→ L3 `auth_sessions` 表=事实源，四方言+file 驱动）；普通用户凭证**滑动续期**（session.idleDays，写回按 auth.touchWriteEvery 节流）、超管/管理台绝对 accessTTL（保持每小时重登节奏）、分享 st 绝对 2h（kind=share）；定时清扫过期+重启 WarmStart/惰性回源恢复；撤权（logout/refresh 轮换/改密/管理员重置/角色变更/删号/超管纪元）三层同步，多实例 L1 陈旧 ≤memTTL；`golang-jwt` 依赖移除，`jwtSecret`/`secretFile` 弃用保留（存量配置兼容），旧 `sessions` 表冻结不读写（磁盘兼容），配置 schemaVersion 8→9（auth 段，migrate8to9）。**协议字段形状零变更**（`refreshToken`=同值兼容别名），旧客户端 ≤0.3.3 登录/刷新/分享/管理台全链路可用；升级一次性代价=存量令牌全失效重登一次（详见 CHANGELOG 与约定14）。**承前 0.2.2（用户 2026-10 第二批清单，缩略图格式面扩展）**：`GET /api/v1/thumb` 白名单补 **BMP**——thumbdec.go 纯标准库自实现解码（CORE/INFO/V4/V5 头 × BI_RGB 1/4/8/16/24/32bpp、BI_BITFIELDS 16/32 位掩码、BI_RLE8/RLE4、自上而下/行对齐；全部读取前判长、畸形 BMP 不 panic；先过 12MP/6000px 尺寸闸门再解码；64MiB/并发/415 回退链同旧），对齐客户端本地缩放（image crate 支持 bmp）的出图能力（需求：服务端缩略图与本地目录机制一致）。webp 有意不做并记录理由：VP8 位流无法在「标准库 only+离线构建镜像零新依赖+零测试面（需求29）」约束下安全自研，手搓解码器出图错误比 415 更糟——替代形态为 Client 0.3.3 静默缓存通道本地解码出图（≤8MiB）/图标（超限）。无新端点/新表/新列、schemaVersion 保持 8；对旧客户端 BMP 415→200 纯增益、其余语义无感。**承前 0.2.1（用户 2026-10 清单：上传中断兜底+浏览零下载端点）**：加性 `GET /api/v1/thumb`（anon 组、守卫同 /preview 匿名语义不变；标准库 ≤512px JPEG 生成，RAW 内嵌预览段链扫描，尺寸/大小/并发三重闸门）与 `POST /api/v1/check`（auth 组，目录重传 missing/same/differ/unknown 批量校验）；file_metas.content_hash 开始真实落库（recordMeta 携带+PutFileMeta 空值保留语义+整传/complete/回滚 mtime 口径统一真实 stat）；断连错误归 499（respond.isNetDisconnect；日志降级 info）；CreateOrResume 版本不符作废重建、WriteChunk per-session 标记锁、Complete DB 失败不再静默、Cleaner 僵尸判据 UpdatedAt、SweepExec 同遍历回收 .fmmerge/.fmtmp 崩溃碎片；无新表/新列、schemaVersion 保持 8、旧客户端无感零变化（不调新端点即无感；GET/DELETE upload/session 早已存在，0.2.1 仅行为修复）。**承前 0.2.0——需求41**：用户端口新增加性端点 `POST /api/v1/mkdirs` 批量建目录（目录上传服务端支持：先全量校验 Upload 权限 + scope jail/保留名 `.fmtrash`——任一失败整请求 4xx 且不创建任何东西，再逐路径持 `transfer.Key` 锁幂等 `MkdirAll`；不入 store/不存版本，与 mkdir 同语义；旧客户端不调用即无感，Client 0.2.0 配旧服务端目录上传自动退化逐个 mkdir）。**承前 0.1.1——需求40**：无数据库默认改用内置**文件存储驱动**
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
用 change-password）。基线（0.1.0）已含的历史演进要点（需求39 清零后不再逐号标注，
记 schema 台阶）——maxUploadBytes（schema 3→4）；jail + adminListen 独立端口（schema 4→5）；
审计/qps/CORS/symlink 防护（schema 5→6）；SQL gorm 化 + admin 客户端隔离
（用户端口拒绝 admin 登录/续期、遗留 token 403）+ public-view 只读可见 + 分享去物理化；
管理台"数据库"页（热切换/全表迁移，schema 6→7 加 dataDir，store.Proxy，见约定11）+
用户家目录自动分配 `<dataDir>/Users/<用户名>` + `.fmtrash`（合成根 `_users`；**0.2.11 起改为
`user_<账户>_<用户id>` + 并级 `_trash`，见约定12**）；
adminAccess（schema 7→8）；**认证三层图 auth 段（schema 8→9，0.2.3，见约定14）**。**）
存量迁移：普通用户 AllowedRoots 启动时一次性转为显式 user_roots 分配（server_meta 标记幂等）；
另有对存量用户的一次性家目录补齐（标记 `userhomes.v7`）。
安全要点（基线既定）：新 handler 经 scope 即自动获得 symlink 防护与审计能力（`s.audit`）；
审计/限速默认值不改变 jail 行为。admin 身份在用户端口任何路径
（登录/刷新/遗留凭证）均不可用，只能走管理台；分享访问者不见物理位置。
**0.2.3 安全要点（认证图，见约定14）**：凭证为随机数、不编码身份，明文永不落层
（三层均只存 SHA-256）；伪造/JWT 形态串/旧令牌一律 401（非 503，客户端据此走刷新/重登）；
share 凭证与登录身份按 kind 硬互斥；超管纪元逐请求核对且轮换时显式三层清空；
Issue 必先落 L3（发放不可吊销凭证=硬失败）；L2 撤销失败进 pendingRevoke 本地拒绝+清扫重试。
**0.2.4 安全要点（认证收敛，见约定14）**：登录响应不再出任何身份/权限/有效期信息
（用户端口只 `{"token"}`）；`X-Client-Id` 为会话绑定核对项而非机密——绑定凭证在他
设备形态下直接 401，闲置 1h 未活动即过期（滑动只由认证请求驱动），客户端退出尽力
登出即时失效；未绑定会话（旧客户端/移动端/管理台）沿用 0.2.3 语义不查头。
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
gorm 驱动）** / go-redis v9 / x/crypto（**0.2.3 移除 golang-jwt——认证不再使用 JWT**）。
SQL 一律经 gorm 构建器
（占位符参数化、clause.OnConflict upsert、按方言渲染）；仅幂等建表 DDL 清单经
`Migrate()` 直发（保 sqlite 字节兼容，不走 AutoMigrate）——除 store 层外禁止任何直接 SQL。
