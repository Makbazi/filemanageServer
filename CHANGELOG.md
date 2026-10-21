# Changelog — filemanageServer

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。
**每次版本变更必须声明兼容的其他模块版本（需求20）。**

> 需求39（2026-09-21 维护者决策，本批用户清单第 9 条）：0.1.0 之前从未正式发布，历史变更清单整体清空、
> 从此文件重新开始记录；此前版本明细可查本仓库 git 历史（分支 feature/dev_new_able）。

## 兼容矩阵

| filemanageServer | 元数据存储 | 兼容 filemanageClient |
|---|---|---|
| **0.2.12（当前；配套 Client 0.3.13 / mobile 0.2.0；用户 2026-10-21 清单 1–6 条；OAuth2/OIDC+PKCE、二次验证、邮箱、管理台 BFF）**——认证面按推荐方案改造：用户端口新增 **OAuth 2.0 授权码+PKCE**（`/oauth/authorize` 服务端登录页、`/oauth/token` 三 grant：authorization_code/refresh_token/设备码 RFC 8628、`/oauth/revoke`、`/oauth/userinfo`、发现文档 `/.well-known/openid-configuration`；私有 IdP=本服务端自身，id_token 最小 HS256 声明回执不参与鉴权，Access Token 仍是认证图不透明随机凭证=Bearer）；**二次验证**（TOTP=RFC 6238 纯标准库 / 邮箱验证码=net-smtp；默认**不启用**、客户端自助选择性启用、管理端 `PUT /admin/users/:id/mfa` 治理）；**邮箱**为非必须配置（config 新 `mail` 段；未配置时邮箱通道明确报错、TOTP 不受影响）；**管理台改 BFF+Cookie Session**（`fm_admin` HttpOnly+SameSite=Strict、全端口非 GET 须 `X-FM-CSRF:1`、`POST /admin/logout` 新增）；`POST /auth/login` **保留**（旧客户端降级直登；已启用二验账户在此 403）。旧客户端 ≤0.3.12/旧移动端全功能无感（不调新端点即无感）；新客户端/新移动端对 ≤0.2.11 服务端自动退化直登。**破坏点**：管理台前端与内嵌台同版锁定（token localStorage 形态废除）；users 表加列 email/email_verified/mfa_method/mfa_secret、auth_sessions 加列 flow_data、auth kind 扩 flow/code/browser/refresh/device/devuser（只增不改幂等，四方言+file 全实现；Store 新方法 UpdateUserAuth/UpdateAuthSessionFlow）；config schemaVersion **12→13**（session.refreshDays=30/loginTTL=12h、mail 段、oauth 段——migrate12to13 只补缺）；`GET /auth/me` 加性 `email/emailVerified/mfa`；`PUT /admin/users/:id` 加性 `email`（变更即清验证态）；`<dataDir>/oidc.key`（0600，首用自生）入共享清单。详见 0.2.12 条目 |
| 0.2.11（破坏批；配套 Client 0.3.12）——端口彻底拆分（管理端口不再注册用户 API，专属 `/admin/login`、`PUT /admin/acl`、`/admin/links` CRUD）+ 账户/昵称/用户 id 三分离（users 加列 nickname；家目录 `user_<账户>_<用户id>`、回收站并级 `<目录名>_trash`；一次性布局迁移 `userhomes.v8` 含七表物理键改写）+ 用户端口数字用户 id 全隐藏（/auth/me 加 nickname、新增 PUT /auth/profile、/acl/subjects 与 ACL 条目账户化、/versions /ai /share 响应视图化）+ 超管建分享 410 潜在 bug 修复 + 暂停期管理台可登录 | 自含（0.2.11 users 加列 `nickname` 只增不改——四方言 CREATE 含列 + extra 幂等加列守卫（sqlite PRAGMA/mysql information_schema/pg IF NOT EXISTS）、file 驱动 JSON 加性；Store 接口新增 `UpdateUserNickname`/`RewritePhysicalPath` 两方法（sqlStore/fileStore/proxy/cache 全实现）；config schemaVersion **保持 12**（无配置结构变更）；布局迁移标记 `userhomes.v8`（启动与热切换双站点，磁盘先改名后写库、可重入）） | **推荐 Client 0.3.12**。用户端口破坏点：`/acl/subjects` 不再含数字 id/role/members、ACL 条目响应改 `subject:"user:<账户>"`（PUT 入参兼容旧数字 subjectId）、`/versions` 不再转储 store 行（物理 rootId/path/storeId/createdBy 移除）、share `linkView` 去 `createdBy`、`/ai/*` 响应视图化（去 userId/物理键）。**登录请求/响应形状不变**（`{username,password,clientId?}`→`{token}`，username 语义=账户），家目录虚拟根 id `_users:<账户>` 形式升级前后字面恒效（旧基座名即账户名；新布局含 id 的目录名经逐段令牌映射不外露）。旧桌面 0.1.x–0.3.11 与移动端**可用但按需降级**（ACL 编辑/昵称功能不可用面按兼容记录适配；登录/浏览/上传下载/回收站/分享访问全兼容）。管理台内嵌前端改调用管理端口专属端点（`{token,user}` 保留并含 nickname）；`/s/:token` 仅用户端口——管理台分享 URL 按 `/admin/info` 的 listen+tlsEnabled 构造 |
| 0.2.x（0.2.9=版本号对齐批——代码/协议/配置 schema/Store/go.mod 零变更，配套 Client 0.3.9 修复批；0.2.8 上传限额默认批；0.2.7 日志批；0.2.4 认证收敛；0.2.3 去 JWT） | 自含（同 0.2.4；0.2.8 上传限额默认值放宽：`maxUploadBytes` 默认 2GiB→**0=不限**（migrate11to12 恰好等于历代默认字面量 2147483648 的配置升为 0，用户显式自定义限额原样保留；schemaVersion 11→12；无新表/新列/新端点，go.mod 零变更）；0.2.7 服务端日志（log 段，schema 10→11）与 0.2.4 认证收敛、0.2.3 auth_sessions/去 JWT 沿用） | 承前各批对 Client 0.1.x–0.3.11 **`/api/v1` 数据协议零变更、响应形状零变更**——0.2.8 仅默认限额值变化（`POST /upload/session` 的 413 判定从"默认必现"变为"仅显式配置限额时"），兼容 Client 0.1.x–**0.3.8** 全系列一切历代版本；0.3.6 起认证收敛语义全保留、0.3.7 起转码/恢复/渲染/批量/设置能力对协议零依赖。旧客户端 0.1.x–0.3.5 与移动端**可用但降级**（会话未绑定不查头；refresh 401/404 退化重登；窗口同降 1h）。管理台内嵌前端兼容（保留 `{token,user}`）；`GET /thumb` JPEG/RAW 出图面 50MP/9000px 沿用|
| 0.2.3 | 自含（同 0.1.x；0.2.3 认证重构：新表 `auth_sessions`（幂等增量，四方言+file 驱动）、配置 schemaVersion 8→9（auth 段）、`golang-jwt` 依赖移除；旧 `sessions` 表冻结不再读写但保留磁盘兼容） | Client 0.3.4（推荐位由 0.3.6 取代；认证协议字段形状零变更，0.1.x–0.3.4 登录/刷新/分享/管理台全链路可用）；**一次性代价**：升级到该版本时所有既有 JWT/refresh/分享 st 令牌失效，重登一次 |
| 0.2.2 | 自含（同 0.1.x；仅 `GET /thumb` 白名单扩 BMP（thumbdec.go 纯标准库自实现），无新端点/新表/新列、schemaVersion 保持 8） | Client 0.3.3（服务端网格 BMP 出图=本地缩放机制）；Client 0.3.2/0.2.x 无感可用且 BMP 由 415 变 200 为纯增益面（旧客户端 size_hint 回退语义不变）；Client 0.1.x 及以下同 |
| 0.2.1 | 自含（同 0.1.x；纯 API 面加性 `GET /thumb`+`POST /check` 与静默修复，无新表/新列、schemaVersion 保持 8） | **Client 0.3.2 完整功能（浏览零下载+目录重传预检依赖本版本端点）**；Client 0.2.0/0.3.x 早期版本全功能无感（不调用新端点即无感）；Client 0.1.x 及以下同 |
| 0.2.0 | 自含（同 0.1.x；纯 API 面增项 `POST /api/v1/mkdirs`） | **Client 0.2.0 完整功能（目录上传）**；Client 0.1.x 及以下全功能无感（mkdirs 为加性端点，旧客户端不调用） |
| 0.1.x | **自含**；0.1.1 新增内置 `file` 驱动（无库默认，全功能）+ 切换/导出/暂停管理面，SQL 表结构无变更 | **Client 0.1.0 完整功能**（0.1.1 为服务端存储引擎与管理面增项，`/api/v1` 数据协议零变更；health 的 `db` 字段可为 `"file"`、暂停期返回 503——均为客户端既有容错路径，旧客户端无感）；**Client 0.2.0 配本系列服务端时目录上传自动退化为逐个 `mkdir`（幂等递归，语义一致、仅往返更多）** |
| 0.1.0 | 自含（表结构无变更；会话 user_id=0 超管哨兵） | 同上（0.1.1 无破坏性协议变更，0.1.0 客户端对 0.1.1 服务端全功能） |

## 未发布版本

### 构建体系（2026-09-27，无版本号变更/无协议影响）
- 新增 `build-env/` 多平台**编译环境**（`Dockerfile.rockylinux9`，镜像命名规范
  `filemanage-server-build:rockylinux9`）+ 容器内构建脚本 `container-build.sh`。
- `build.sh` 重写为 Docker 化多平台静态构建：`-a linux|windows|arm64|all`
  `-d debug|release`（默认 release）；产物命名 `filemanageServer_<平台>_<版本号>[.exe]`
  输出 `build/`（debug → `build/debug/`）；三目标均 CGO_ENABLED=0 静态编译
  （glebarez 纯 Go sqlite），版本信息注入（`-v` 可查）与旧脚本一致。
- 旧接口 `./build.sh [-r]`（宿主直编）移除；等价 `./build.sh -a linux -d release`。

### 构建体系（2026-09-28，无版本号变更/无协议影响）
- `build.sh` 新增镜像管理参数（与工程编译解耦）：`-i` **构建编译镜像**（docker build）、
  `-n` **拉取编译镜像**（docker pull）——任一给定即只操作镜像、完成后退出，不编译工程；
  `-f` 编译时强制重建镜像（本地缺镜像时仍自动构建）。
- **产物版本号改由 git tag 推导**，不再取 `internal/version/version.go` 常量：
  取最新可达 git tag（去 `v` 前缀，`git describe --tags --abbrev=0`），工作区有未提交
  改动（跟踪文件，`git status --porcelain --untracked-files=no`）追加 `-dirty` 后缀，
  经 `BUILD_VERSION` 传入容器并 `-ldflags -X` 注入 `version.Version`。
  `version.Version` 由 `const` 改为 `var`、默认 `"unknown"`（`-X` 只能注入变量；直接
  `go build` 未注入时保持 unknown，health/admin/`-v` 读取路径不变、无协议影响）。
- CI 兜底 `FM_VERSION`：Jenkins 浅克隆/未 fetch tag 会使 `git describe --tags` 失败→
  版本号落到 `"unknown"`。设 `FM_VERSION=x.y.z ./build.sh -a all` 可跳过 git 推导固定版本
  （此时也不追加 `-dirty`）；亦可在流水线先 `git fetch --tags --force`（浅克隆加
  `--unshallow`）并 `git config --global --add safe.directory "$WORKSPACE"` 让 tag 真正可用。

## [0.2.12] - 2026-10-21（认证批：OAuth2/OIDC 授权码+PKCE + 二次验证（TOTP/邮箱）+ 邮箱配置/绑定 + 管理台 BFF Cookie Session；用户 2026-10-21 清单 1–6 条；配套 Client 0.3.13 / filemanagemobile 0.2.0；tag 由维护者打）

依据维护者提供的推荐认证方案表（Windows/Linux 桌面、Android 移动=授权码+PKCE+
Bearer+系统安全存储；管理端 Web=OIDC+BFF+Cookie Session）落地。本服务端为**私有
IdP/OP**（无外部认证方），凭证模型沿用 0.2.3 起的三层认证图——**Access Token 仍是
不透明随机凭证（Bearer）**，0.2.3"凭证不编码身份"红线不破；id_token 仅为 OIDC
协议声明回执（HS256、sub=账户名），**不参与任何鉴权路径**。

### 新增：OAuth2/OIDC 面（用户端口 /api/v1，v1 维护屏障内；`internal/api/v1_oauth.go` + 内嵌登录页 `oauthpage.go`）
- `GET /oauth/authorize`：授权端点——response_type=code、client_id 注册表校验、
  **redirect_uri 白名单**（精确匹配 + loopback 任意端口 http 回调，RFC 8252 §7.3；
  校验失败绝不重定向防开放重定向）、**PKCE 强制**（S256；plain 仅历史兼容）；
  已登录浏览器（fm_browser HttpOnly Cookie，绝对 `session.loginTTL` 默认 12h）
  静默出码 302；`prompt=login` 强制重输。
- 登录页（服务端自渲染 HTML，无外部资源）：账密 → 按需二次验证（TOTP 动态码 /
  邮箱验证码 / 强制开通时现场 enroll 展示密钥+otpauth——**不出二维码图**，
  纯标准库无 QR 编码器，手工录入 otpauth 为明示形态）。中转条目 kind=flow
  （10 分钟一次性）+ fm_flow Cookie 双承载。
- `POST /oauth/token`：三 grant——`authorization_code`（code 60 秒一次性消费 +
  PKCE 核对 + client 匹配）、`refresh_token`（**rotation**：旧令牌即时吊销、
  新 access+新 refresh 成对换发；refresh 绝对 `session.refreshDays` 默认 30 天，
  每次刷新重开窗口）、`urn:ietf:params:oauth:grant-type:device_code`。
  响应 `{access_token, token_type:"Bearer", expires_in, refresh_token, scope, id_token}`。
  `X-Client-Id` 安装标识绑定语义沿用 0.2.4（token 请求携带即绑定 access+refresh，
  逐请求/刷新核对）。
- **设备授权码流（RFC 8628，无浏览器输入端的移动/CLI 登录）**：`POST /oauth/device`
  → device_code+user_code（XXXX-XXXX，10 分钟）；浏览器 `GET /oauth/device/input`
  批准页（账密+二验/已登录直接批准）；设备轮询 pending/approved/denied/expired、
  批准一次性消费双生行（kind=device/devuser 哈希互引）。
- `POST /oauth/revoke`（RFC 7009 语义：未知一律 200）、`GET /oauth/userinfo`
  （Bearer；sub/username/nickname/email/emailVerified/mfa）、
  `GET /.well-known/openid-configuration`（发现文档；issuer=配置或按请求 Host 派生）。
- id_token：最小 HS256 JWT（`internal/auth/oidc.go` 标准库手工签验，**不引回
  golang-jwt**）；claims=iss/sub(账户)/aud(client_id)/exp/iat/auth_time/nonce/
  preferred_username/nickname/email(_verified)/amr；签名密钥 `<dataDir>/oidc.key`
  （0600 首用自生——多实例共享 dataDir 即共享，列入共享清单；失败只降级不发
  id_token 不杀进程）。

### 新增：二次验证与邮箱（需求2/3：**默认不使用、选择性启用、非必须配置**）
- users 加列 `email/email_verified/mfa_method/mfa_secret`（只增不改；四方言
  CREATE+extra 幂等加列、file JSON 加性；MfaSecret 敏感纪律同 PasswordHash——
  json:"-"，永不出 API/日志；redis 用户影子结构补 MfaSecret 完整往返）。
- 自助面（auth 组，`internal/api/v1_mfa.go`）：`GET /auth/mfa`（形态+mail 可用性）、
  `POST /auth/mfa/totp/enroll|confirm`（口令复验；确认才落密钥）、
  `POST /auth/mfa/email/send|confirm`（OTP 存 kind=flow 行 mode=emailbind、
  5 分钟一次性；**mail 未配置回明确中文错误**）、`POST /auth/mfa/disable`
  （口令复验+关闭=安全降级→**吊销全部会话**）。超管不经此面（账户文件，本批
  明确不做超管二验——记录为已知边界）。
- 管理端治理：`PUT /api/v1/admin/users/:id/mfa {method:"off"|"totp"|"email"|"reset"}`——
  强制 totp 无密钥者下次登录页现场开通；强制 email 要求已验证邮箱；策略变更与
  reset 一律 `RevokeUser` 强制重登；目标为管理员/超管 400（管理台登录本批不
  走二验）。`PUT /admin/users/:id` 加性 `email`（登记不验证、变更即清验证态）。
- TOTP=RFC 6238（HMAC-SHA1/30s/6 位/±1 步窗口，`internal/auth/totp.go` 纯标准库）；
  邮箱 OTP 投递走 `internal/mailx`（net/smtp；465 隐式 TLS 或 587 STARTTLS，
  **不支持 STARTTLS 拒发明文投递**）。
- 登录强制：`POST /oauth/login` 按 `mfa_method` 分叉（totp 校验、email 发码+校验、
  强制未开通走 enroll）；旧 `POST /auth/login` 对已启用二验账户 **403** 明确拒绝
  （"请升级客户端走 OAuth"）——旧客户端未启用二验者行为零变化。

### 变更：管理台 BFF + Cookie Session（需求表"管理端 Web"行）
- `POST /admin/login` 成功 `Set-Cookie fm_admin`（HttpOnly/SameSite=Strict/Path=/，
  TLS 启用加 Secure；响应 `{token,user}` 保留供程序化调用）；authCtx 管理端口无
  Authorization 头时回退读该 Cookie（**用户端口绝不读**——admin/super 凭证在
  用户口 403 纪律不变）；新 `POST /admin/logout`（清 Cookie+撤凭证）；
  `PUT /admin/super` 重发凭证同步 Set-Cookie；`GET /admin/info` 加性 `who`
  （当前管理员概要，刷新页面恢复顶栏）。
- CSRF 屏障 `adminCsrf()`：管理端口全部非 GET 请求必须带 `X-FM-CSRF: 1`
  （浏览器跨站表单自定义不了头 + SameSite=Strict 双保险）。
- 内嵌控制台（go:embed 同版锁定）：localStorage token 形态**废除**；api()/dbApi()
  带 CSRF 头 + Cookie 同源凭证；启动静默探测 `/admin/info` 恢复会话；用户页新增
  邮箱/二验列与治理下拉；登出真调 /admin/logout。

### 认证图扩 kind（Store/Center 跨工程契约变更——按需求2 三份文档同步）
- auth_sessions 加列 `flow_data`（协议状态 JSON：PKCE challenge/redirect/nonce/
  phase/OTP 哈希/设备互引哈希——**绝不含明文凭证**，json:"-" 不出 API）；
  kind 扩 `flow|code|browser|refresh|device|devuser`（authCtx 身份注入只认 user，
  其余一律 401；share 互斥门控不变）。
- `Store` 新方法 `UpdateUserAuth`（四列全量写）+ `UpdateAuthSessionFlow`（按主键
  推进流程态；无行→ErrNotFound=消费竞态判据）——sqlStore/fileStore/proxy/
  cache 装饰器全实现；`auth.Center` 新增 `Spec.Plain`（user_code 直作条目凭证）、
  `LookupHash`/`RevokeByHash`/`UpdateFlow`（三层同步）、`Spec.FlowData`；
  L1/L2 **按用户反向索引覆盖面扩大**：非 share 全部 kind 入 byUser/`fm:auth:u:`
  集合——**改密/删号即时吊销该用户的 flow/code/browser/refresh/device 条目**
  （旧只清 user 行会让 ≤memTTL 窗口内的授权码继续可兑换，新语义根治）。
- 清扫器既有回收（expires_at 索引）天然覆盖新 kind；WarmStart 一并预热。

### 配置（需求25：schemaVersion 12→13）
- `session.refreshDays`=30（刷新令牌绝对期）、`session.loginTTL`="12h"（浏览器
  登录态）；`mail` 段（host/port/ssl/username/password/from/fromName——host 空=
  通道关闭）；`oauth` 段（issuer 空=按请求 Host 派生；clients 空=内置注册表
  desktop（fmmanage://oauth/callback + loopback 任意端口）/mobile
  （filemanageauth://callback）/web）；migrate12to13 只补缺不覆盖。

### 验证（需求29）
- `go build ./... && go vet ./...` 全绿（file 驱动实机）；端到端冒烟全过：CSRF
  拦截/管理台 Cookie 登录/info.who/建号/分配目录→发现文档→authorize 页渲染/
  非法 redirect 400→PKCE 错 verifier 拒/对 verifier 换发全票据（id_token claims
  正确）→X-Client-Id 绑定核对 401→授权码/flow/device_code 一次性→refresh
  rotation 旧令牌作废→TOTP enroll/confirm→二验账户旧端点 403→OAuth mfa 链→
  管理端 reset→强制 totp 登录页 enroll→改密 RevokeUser 清 refresh→revoke→
  静默授权（**重启后 fm_browser 仍 302**——auth_sessions L3 持久+惰性回源）→
  userinfo→mail 未配置错误文案→config v12 自动迁移 v13（含 .bak 备份）。

### 兼容
- **旧客户端 0.1.x–0.3.12 与旧移动端 0.1.x：全功能无感**（`POST /auth/login`/
  logout/change-profile/roots/传输/分享/回收站零变更；不调新端点即不触新面；
  仅当管理员给某账户开了二验后，该账户旧客户端直登被 403 明确拒绝——属二验
  语义的必然，兼容矩阵记录）。
- **新客户端 0.3.13/新移动端 0.2.0 对 ≤0.2.11 服务端**：发现文档 404 →自动
  退化旧账密直登（客户端兼容策略，见各自 CHANGELOG）。
- 升级一次性代价：无（认证图存量 user/share 条目原样有效；新列全默认值）。
- 管理台前端与内嵌台同版锁定（go:embed），无跨版形态。

## [0.2.11] - 2026-10-20（破坏批：端口 API 彻底拆分 + 账户/昵称/用户 id 三分离 + 家目录/回收站新布局；用户 2026-10-20 清单 1–6 条；跳号 0.2.9→0.2.11 系维护者定版指令；配套 Client 0.3.12；tag 由维护者打）

### 破坏性变更①：管理/用户 API 完全独立、不复用（需求1/2/3）
- `AdminHandler` **删除 `registerV1` 重复注册**（0.2.10 前管理端口完整暴露全部用户 API 供内嵌管理台反向调用——本批起两端口注册集**零交集**：管理端口对 `/api/v1/*` 业务面与 `/s/:token` 一律 404；用户端口管理路由 404 自 0.1.0 起即为然）。
- 管理台专属端点（仅管理端口）：`POST /api/v1/admin/login`（超管+DB 管理员两分支从 `handleLogin` 抽出为 `handleAdminLogin`；注册在**裸组**——无 requireDB/无 authCtx/无 maintenanceGate，超管无库可登、暂停期操作员不被锁死，修正 0.2.4 前"暂停期管理台登录 503"与 middleware 文档不符问题）；`PUT /api/v1/admin/acl`（物理键直存、无角色分叉）；`POST/PUT/DELETE /api/v1/admin/links`（管理员代管分享，无所有权校验，响应 `linkViewFull` 含物理键与 createdBy——管理面可见）。
- `handleLogin`/`authCtx`/`handleACLPut`/`handleShareUpdate`/`shareByTokenOwner`/`handleACLDelete` 中的 `adminModeKey` 角色分叉全部移除；`computeBases` 管理端口整根分支删除（拆分后管理引擎不再有 scope() 调用者；`adminScope` 标记保留供 authCtx 分流与 `/api` 的 adminUI 字段）。用户端口响应 `{"token"}` 形状不变。
- 分享访问链不变：管理台建的链接经**用户端口** `/s/:token` 匿名访问；管理台分享完整 URL 由 `/admin/info` 的 `listen`+`tlsEnabled` 构造（不再 `location.origin`）。

### 破坏性变更②：账户/昵称/用户 id 三分离 + 同名复用目录 bug 修复（需求4）
- 模型：`username` 语义=**账户**（唯一、不可改、可作目录名片段，登录凭证名）；新增 `nickname`=**昵称**（可改、可重复、仅展示，空=显示回退账户；`users` 加列 TEXT DEFAULT ''，四方言 extra 幂等加列守卫、file 驱动 JSON 加性）；`User.ID`=用户 id（自增永不复用，**只服务端内部使用**）。
- **串目录 bug 根修**：家目录名 `user_<账户>_<用户id>`——删除账户后同名重建必落新目录（旧 `EnsureHomeFor` 的 `MkdirAll(Users/<用户名>)` 对删号遗留目录静默复用是根因；目录名含永不复用的 id 后从结构上绝迹）。删号磁盘行为保持"只删元数据、目录留孤儿"（维护者决策，运维自清）。
- 用户端口 API 边界彻底去数字用户 id：`GET /auth/me` → `{username, nickname}`（昵称现读库，热路径 authCtx 零查库红线不破）；新增 `PUT /auth/profile {nickname}`（自助改昵称，不吊销会话）；`/acl/subjects` → `{account, nickname, subject}`（去 id/role/members）；ACL 条目 GET/PUT 响应改 `subject:"user:<账户>"`+`subjectName`（存储仍数字键控，边界翻译；PUT 兼容旧数字 `subjectId` 入参）；`/versions` 不再转储 store 行（物理 rootId/path/storeId/createdBy 泄漏面一并收敛）；share `linkView` 去 `createdBy`；`/ai/*` 响应视图化（去 userId/物理键）。管理台（服务端管理面，非"客户端"）按需求4 决策保留数字 id REST 寻址。
- 家目录虚拟根遮蔽：客户端可见令牌 **`_users:<账户>`**（含分享进家目录的子基座逐段映射 `alice/docs`）；入方向 `scope()` 经 `translateHomeJail` 还原真实基座、`scopePhys` 物理键回流天然直通——升级前后 `_users:<账户>` 恒字面有效，存量客户端缓存根 id 不失效；伪造他人目录仍 403（基座命中检查原样）。
- 附带修复：`validLink` 对 `CreatedBy=0`（超管哨兵）跳过创建者校验——超管所建分享此前必 410。

### 变更③：回收站与家目录并级（需求5）+ 一次性布局迁移
- 新布局：`<dataDir>/Users/user_<账户>_<id>`（家目录）+ `Users/user_<账户>_<id>_trash`（回收站实体，**并级**不再内嵌 `.fmtrash`）。
- 迁移 `fileops.MigrateUserHomeLayout`（server_meta 标记 `userhomes.v8`，启动 + DB 热切换双站点，模式同 v7）：目录 rename → 内嵌 `.fmtrash` 条目逐搬 `*_trash` → `Store.RewritePhysicalPath(_users, 旧名, 新名)`（**七表**存量物理键行改写：file_metas/ai_descriptions/trash_items/folder_acls/share_links/file_versions/upload_sessions；新接口方法 sqlStore/fileStore/proxy/cache 全实现，单事务/单写锁、幂等）→ `SetUserRoots` 换 SubPath。**磁盘先改名后写库**（崩溃锚点），三态可重入；回收站实体解析保留三候选（并级→迁移中间态嵌套→旧名嵌套→中央），迁移中断窗口内条目仍可还原/清除。
- `Cleaner.removeTrashBlob` 候选化（不再按 `u.Username` 直拼）；`.fmtrash` 保留名拦截与列表隐藏**保留**（未迁孤儿目录纵深防御）。config schemaVersion 保持 12。

### 变更④：日志六级化与必要日志补充（用户 2026-10-20 追加指令；版本保持 0.2.11）
- **六级**：`trace < debug < info < warn < error < fatal`（logx 引入标准库 `log/slog` 日志库之上扩展：trace=-8、fatal=16，文本 Handler ReplaceAttr 渲染 `TRACE`/`FATAL` 等级名）；`log.level` 配置取值域加宽、默认 **warn** 不变、非法收敛 warn——仅取值域变化无结构变更，schemaVersion 保持 12。
- **logx.Fatal**：以 fatal 级落行（任何配置等级下必可见）后 `os.Exit(1)`；main/balancer 启动 fail-fast 站点全部改走 logx.Fatal——**根治旧 `log.Fatalf` 经 stdlib 桥接按 INFO 走、默认 warn 等级下进程死得无声无息的吞没问题**；`log.Printf("warn: …")` 各站点转 logx.Warn（家目录迁移未完成/跳过、认证预热失败、jail 迁移告警、超管同名遮蔽账户告警、balancer worker 摘除）。
- **URL 凭证脱敏**（敏感纪律扩展）：分享访问凭证存在于 URL（`/s/<token>` 路径段、`?st=` 查询值、share 口令参数）——新增 `api.sanitizeLogTarget` 统一遮蔽（`/s/**`、`st/token/password/pass/pwd=**`，键名保留便于排障；query 解析失败整体 `<unparsed>`），loggerGin 请求行与 writeErr internal error 行同用。
- **补充必要日志**（只记账户名/结果/IP/计数，绝不记口令、Bearer 凭证、分享 token 值）：
  authCtx 拒绝面——X-Client-Id 核对不符（WarnT）、超管/管理员凭证出现在用户端口（WarnT，需求36）、超管凭据纪元推进吊销（WarnT）、share 凭证被当身份/坏凭证（DebugT）、身份解析成功细节（TraceT，trace 级形态示范）；
  登录面——用户端口/管理台登录成功（InfoT）、防爆破锁定（WarnT）、跨端口登录拒绝（WarnT）、口令不符（DebugT）；
  账户与权限面——自助改密成功（InfoT，含会话全吊销）、自助改昵称（InfoT）、管理台创建/更新账户与目录分配（InfoT）、**删除账户（WarnT，注明孤儿目录留盘）**、用户端口与管理端 ACL 更新（InfoT）、分享链接创建/删除（InfoT，不记 token）；
  fileops 家目录迁移 skip 类（Warn）与改道明细（Info）。
- trace 级为"逐请求细节"层（生产常闭），排障时 `log.level=trace` 打开；六级全链路冒烟：默认 warn 下 INFO/DEBUG/TRACE 静默、WARN/ERROR/FATAL 可见；fail-fast FATAL 行落盘且退出码 1；`/s/**?st=**&password=**` 脱敏；敏感值全量 grep 日志 0 命中。

### 验证（需求29）
- `go build ./... && go vet ./...` 通过；冒烟（文件存储 + sqlite 双实例）：建号目录/回收站并级 ✓ 删号同名重建新目录互不可见 ✓ /auth/me+profile ✓ roots 令牌 `_users:alice` 无数字 id ✓ ACL 账户化往返+旧数字入参兼容+伪造拒绝 ✓ 回收站删除/还原/彻底删除 ✓ 双端口注册集零交集（404 面）✓ 管理台登录/ACL/分享 CRUD、超管链接 `/s/` 访问不 410 ✓ 暂停期用户口 503/管理台登录可用 ✓ 存量迁移（旧布局回退后启动自动迁移，还原/分享链完整）✓。

### 兼容
- **Server 0.2.11 ↔ Client 0.3.12（推荐）**。旧客户端/移动端可用但按需降级（登录/浏览/上传下载/回收站/分享访问全兼容——`_users:<账户>` 字面不变；ACL 编辑在 ≤0.3.11 桌面/移动端对新服务端仍发数字 subjectId=兼容入参可用，但 GET 响应已无数字 id——旧 UI 若回显 subjectId 会显示为空，属显示降级）；移动端 filemanagemobile 本批未适配，适配清单见根 CHANGELOG。

## [0.2.9] - 2026-10-16（版本号对齐批：配套 Client 0.3.9 修复批；tag 由维护者打）

### 说明（用户 2026-10-16 截图清单第 9 条版本变更指令）
- **本批八项问题（渲染黑屏/转码假不可用/目录上传失败/批量勾选/批量上传/右键批量
  上传/一次选目录批量下载/复制移动目录选择）经逐条实测定位，根因全部在客户端
  本地行为**（渲染绘帧竞态、媒体缓存 LRU 误杀在服硬链条目、planDirUpload 形参错位
  致 localPath 变相对路径、批量 UI 语义），服务端 `internal/` **零代码改动**。
- 0.2.9 为**版本号对齐批**：`/api/v1` 协议/响应形状/配置 schema（v12）/Store/
  go.mod 全部零变更，对一切客户端 0.1.x–**0.3.9** 与历代行为完全一致。
  产物版本号照旧由 git tag 推导（`v0.2.9`），version.go 源码默认 "unknown" 不变。

## [0.2.8] - 2026-10-16（上传修复：maxUploadBytes 默认 2GiB→不限；用户 2026-10-16 清单第 1/6 条；tag 由维护者打）

### 修复（用户 2026-10-16 清单第 1 条"上传功能彻底不能用"的根因）
- **现象**：`POST /api/v1/upload/session` 对超过 2GiB 的文件一律 413
  （`size N exceeds maxUploadBytes 2147483648`），目录组上传中该文件反复重试反复 413、
  断点恢复同撞——用户感知即"上传彻底不可用"（实测复现：小于限额建会话 201/整链路可用，
  大于限额必 413；413 只可能来自 `handleUploadSessionCreate`/整文件 `handleUpload` 两处
  maxUploadBytes 判定）。2GiB 默认对"视频/RAW 归档"这一本产品的核心场景过紧。
- **修复**：`maxUploadBytes` 默认值改为 **0=不限**（DefaultConfig/示例配置同步）；
  判定逻辑与 413 语义**原样保留**——显式配置正数限额时行为不变（需要收紧的部署照常
  可配）。整文件通道（`WriteFileAtomicLimit`）与分片会话创建两处同受该值控制。
- **配置迁移（需求25）**：schemaVersion **11→12**，`migrate11to12`：
  键缺失→补 0（新默认）；值**恰为历代默认字面量 2147483648**（v4 起 DefaultConfig、
  示例配置与 migrate3to4 写入的同一串）→升为 0（把"从未被用户显式决定的默认值"升级，
  不丢任何用户自定义限额——自定义值一律原样保留）；旧配置文件照例自动备份。
- **兼容**：`/api/v1` **数据协议零变更、响应形状零变更**——对一切客户端 0.1.x–0.3.8
  无感；配 ≤0.2.7 旧服务端时 Client 0.3.8 另把 413 报错改写为中文可操作指引。
- 验证：`go build ./... && go vet ./...` 通过；迁移冒烟：v11 配置（含默认 2GiB 字面量）
  启动自动升 12 并解除限额、显式限额保留。

## [0.2.7] - 2026-10-14（服务端日志：轮转文件 + 链路追踪 + 等级可配 + 敏感信息纪律；用户 2026-10-14 清单第 4 条；跳号 0.2.4→0.2.7 系维护者定版指令；tag 由维护者打）

### 新增（internal/logx，标准库 log/slog，零新依赖、go.mod/go.sum 不变）
- **日志文件**：`<logDir>/filemanage-server.log` 按大小轮转（写前判长、越过上限才轮转），
  改名链 `.log.1`/`.log.2`（oldest-first、溢出删除）——**含当前文件在内最多 3 个、单文件
  ≤10MB（均可配）**；互斥守护写入，slog 文本 Handler 一行一条记录；重启续写既有文件并
  从实际大小累计，绝不让单文件超限。同时 **io.MultiWriter 镜像 stdout**（前台/容器部署直读）。
- **等级可配**：`log.level = debug|info|warn|error`，**默认 warn**（未知取值收敛为 warn，
  不因配置笔误拒绝启动）；printf 风格 `logx.Debug/Info/Warn/Error` 与链路变体
  `DebugT/InfoT/WarnT/ErrorT`（行首 `trace=<id>`）。
- **stdlib log 桥接**：Setup 后 `log.SetFlags(0)`+`log.SetOutput` 把存量 `log.Printf`
  逐行按 **INFO** 送进 slog（时间戳由 slog 统一添加），生命周期横幅/计数行经桥接在默认
  warn 下不再显示——**刻意降噪，符合"默认等级 warn"需求**；错误/告警面按下列改造显式转
  logx 保持默认可见。
- **敏感信息纪律（需求6 底线延伸）**：**绝不记录请求体、Authorization 等任何请求头、
  口令、令牌值**；请求日志只含 方法/路径?查询/状态/耗时/来源 IP 与 trace。main.go 首启
  超管明文口令改为 `fmt.Printf` **只回显控制台、不入日志文件**（需求4）；auth L2 撤权重试
  行仅含 SHA-256 摘要 8 位前缀（非凭证值）。

### 新增（请求链路追踪与 panic 兜底，internal/api）
- `traceGin()`：取请求头 `X-Trace-Id`（其次 `X-Request-Id`），入头消毒（≤64 字符且仅
  `[0-9A-Za-z._-]`，非法视为伪造重新生成），无头则 crypto/rand 生成 16 位十六进制 ID；
  存 gin 上下文并**回显响应头 `X-Trace-Id`**（响应体形状零变更，health 等既有契约不动）。
- `loggerGin()` 重写走 logx：5xx=Error、4xx=Warn、其余=Info，行格式
  `trace=<id> <METHOD> <path?query> <status> <dur> from=<remote>`（先组行再以常量格式
  传入，路径含 `%` 动词不会被二次解释——vet printf 检查同项约束）。
- `recoverGin()`（**全仓此前无任何 recovery，handler panic 会打死进程**）：捕获后按
  Error 记 trace+panic 值+截断 4KB 堆栈（换行折叠保持单行形态），按 respond.go 信封回
  `500 {"error":"internal server error"}`（原始 panic 文案只进日志不出响应）。
- 两引擎统一挂载（`newEngine` 装配点）：链序 recover → trace → logger → 网络准入 →
  qps → cors；用户口与管理口同享。`-mode balancer` 同样 `logx.Setup`（其 stdlib 请求行
  经桥接落同一轮转文件）。

### 改造（真实错误/告警站点转 logx，默认等级下保持可见）
- `respond.go` internal error → `logx.ErrorT`（带 trace；499 断连行**刻意保留** stdlib
  log=桥接 INFO——0.2.1"网络常态不刷错误"契约不变）；`server.go` 审计写入失败 →
  `logx.WarnT`；`v1_auth.go` logout 撤权失败、`v1_dbconfig.go` cache flush/ensure
  homes 失败 → `logx.WarnT`；`auth/center.go` L2 redis 不可达/读写/撤权失败/续期写回
  失败 → `logx.Warn`；`transfer/chunks.go` complete 标记/暂存清理失败、
  `fileops/cleaner.go` trash/sessions/audit/uploads 清理失败 → `logx.Warn`。
- main.go 与 store/balancer 生命周期行维持桥接（INFO，默认不可见）。

### 配置（需求25：schemaVersion 10→11）
- 新增 `log` 段：`{"level":"warn","dir":"","maxMB":10,"maxFiles":3}`（dir 空=
  `<dataDir>/logs`；finalize 钳制：非法等级→warn、maxMB≤0→10、maxFiles<1→3、>10→10）；
  访问器 `LogDir()`；示例配置与 `DefaultConfig` 同步带出。
- `migrate10to11` 只补缺不覆盖（存量显式 log 段原样保留，段内缺键由默认值兜底）；
  迁移备份/未知字段保留纪律沿用；文档注释 schemaVersion 修正为 11。

### 验证
- `go build ./... && go vet ./...` ✓、gofmt 干净。
- 冒烟：首启示例配置含 log 段（schemaVersion 11）✓；`GET /health` 响应头 `X-Trace-Id`
  出现、body 键与历代一致 ✓；`/api/v1/nonexistent` → 日志文件出现同 trace 的 WARN 行 ✓；
  外部传入 `X-Trace-Id`/`X-Request-Id` 合法即回显、含空格等非法字符重新生成 ✓；默认
  warn 启动横幅不入日志，level=info 重启后可见 ✓；level=info+maxMB=1 突发 3 万请求 →
  恰 3 个文件、轮转件各 ≤1MB（oldest 溢出删除）✓；首启超管明文口令仅出现在 stdout、
  日志文件零命中 ✓；v10 配置（无 log 段、listen 自定义）启动自动升 v11 补默认 log 段、
  自定义值存活、`.bak-<时间戳>` 备份 ✓。

### 兼容
- **`/api/v1` 数据协议零变更**：登录/健康/列表/上传/分享等全部响应体与状态码不动，
  仅新增响应头 `X-Trace-Id`——对一切客户端（**Client 0.1.x–0.3.7**）无感、无解析依赖；
  管理台与内嵌前端同。配置 schemaVersion 10→11 只补缺不覆盖（需求25），旧配置升级
  零丢值；无新表/新列、Store 接口零变更、go.mod/go.sum 零变更（纯标准库实现）。

## [0.2.4] - 2026-10-12（认证响应面收敛：单凭证 + 客户端唯一标识绑定 + 1h 滑动窗口；缩略图 JPEG 出图面扩至 50MP；用户 2026-10-12 清单第 2/3 条；tag 由维护者打）

### 破坏性变更（认证契约；未发布系列经维护者确认直接修改 /api/v1，本批版本线 Server 0.2.4 ↔ Client 0.3.6）
- `POST /api/v1/auth/login` 用户端口响应**只回 `{"token"}`**：删除 accessToken/refreshToken/
  expiresIn 兼容别名与 user 概要回显——身份/角色/有效期全存服务端认证图（需求34"身份隐藏
  贯彻到凭证层"的最终形态；客户端对凭证从无格式假设）。管理台登录（admin 端口，
  adminModeKey）保留 `{"token","user"}`——控制台进入门禁与顶栏依赖 user.role（维护者确认：
  仅裁客户端面）。`PUT /admin/super` 响应同步删除 accessToken/expiresIn 别名（控制台只读
  token/username）。`GET /auth/me` 收敛为仅回 `{username}`——角色/档案（id/role/
  allowedRoots/createdAt）不再对客户端可见：客户端只持 token，可查询的仅服务端基本信息
  （health/版本发现）与业务数据（roots 清单），身份与权限判定完全在服务端认证图+jail 内
  闭环；旧客户端/移动端读取 role 的分支落各自默认值，无害降级（桌面 ≤0.3.5 恒显「普通
  用户」、移动端 MeInfo.role 空串且解析全带缺省不崩；Client 0.3.6 起账户页不再调用本端点）。
- **移除 `POST /auth/refresh`**（路由删除，请求落 404）：活跃滑动续期由服务端无感完成
  （authCtx→Touch，续期过程中凭证字符串永不变化），"重新验证才换凭证"即重登语义。
  旧客户端（≤0.3.5）与移动端既有 401→refresh 链路自然失败后**退化为"过期即重登"**
  （登录对话框承接，无新增协议面）。
- **客户端唯一标识（client ID）绑定**：登录请求新增可选 `clientId`（≤64 字符，超长 400）；
  `auth_sessions` 新增列 `client_id` 持久化（四方言 CREATE 含列；存量 0.2.3 库 `Migrate()` 后
  幂等加列守卫——sqlite `PRAGMA table_info` 探查后 ALTER、mysql information_schema 探查后
  ALTER、pg `ADD COLUMN IF NOT EXISTS`；file 驱动 JSON 行天然加性，旧行解出空串=未绑定）。
  绑定会话**逐请求核对 `X-Client-Id` 头**：缺失/不符=401（须重新登录）。未绑定会话
  （旧客户端/移动端/管理台/超管——登录未带 clientId）不看头，行为同前。多客户端同账号
  各持己凭证**互不挤占**（登录从不吊销其他会话——既有语义保持并明示）。
- **用户会话滑动窗口 21 天 → 1 小时**：配置新增 `session.idleMinutes`（默认 60）；
  `IdleDuration()` IdleMinutes>0 优先、否则回退弃用字段 idleDays。**schemaVersion 9→10**
  （migrate9to10 只补缺不覆盖、配置备份纪律沿用）。闲置超 1h、或客户端主动登出/正常退出
  （Client 0.3.6 退出前尽力 logout）均需重新登录；管理台/超管绝对 accessTTL 1h 不滑动不变。
  `POST /auth/logout` 接受 `{token}`（兼容读取旧字段 refreshToken；两者皆无则撤销
  Authorization 头中的当前凭证——路由本要求登录态）。

### 修复/改进（缩略图面：配合 Client 0.3.6"浏览零整文件"，服务端出图能力扩展）
- thumbdec.go JPEG（含相机 RAW 内嵌预览）改走 **YCbCr 平面盒式直降采样**（Y=盒整数均值、
  色度=盒中心采样、灰度直出；无全幅 RGBA 中间层，~1.5B/px）：专用闸门
  **`thumbMaxPixelsJpeg=50MP` / `thumbMaxSideJpeg=9000px`**——48MP 手机主摄原图直接拿到
  几十 KB 服务端缩略图（旧 12MP 闸门令其整批 415，被客户端当作"整文件静默下载兜底"触发
  浏览带宽爆满——本批用户清单第 2 条的服务侧治本项）。PNG/GIF/BMP 维持 RGBA 链路
  12MP/6000px；CMYK 等异常 JPEG 形态保守按 12MP；415/413/404 响应语义不变（Client
  0.3.6 一律落图标）。解码峰值明示：JPEG ≈ 平面 75MB+输入 64MiB/slot、PNG/GIF/BMP ≈
  48MB/slot，并发槽 ≤4。

### 验证
- `go build ./... && go vet ./...` ✓、gofmt 干净。
- 冒烟（file + sqlite3 双引擎）：v9→v10 迁移补 `session.idleMinutes=60` ✓；用户端口登录
  仅 `{token}` ✓；管理台登录 `{token,user}` ✓；绑定凭证 正确头 200 / 缺头 401 / 错头 401，
  未绑定会话不查头 200 ✓；同账号两凭证并存互不吊销 ✓；logout 撤销当前凭证后 401、
  其他会话不受影响 ✓；`/auth/refresh` 404 ✓；存量无 client_id 列的 sqlite3 库启动自动
  补列、新登录落列值 ✓；24MP JPEG `GET /thumb` 200（512px ≈60KB）、15MP PNG 415、
  `w=1024` 出图 ✓。

### 兼容
- **Client 0.3.6 为本批完整语义推荐基线**（单凭证存取/标识绑定/401 自动重登/退出登出/
  浏览零整文件均为配套客户端行为）。旧客户端 0.1.x–0.3.5 与移动端**可用但降级**：不送
  clientId（会话未绑定、不查头）、登录响应无 refreshToken（其既有"无 rt 即过期重登"分支
  承接）、refresh 端点消失（401→重登）、滑动窗口同样缩至 1h（服务端策略统一生效）。
  升级一次性代价：存量会话沿用（未绑定、随新策略到期节奏）；管理台内嵌前端与本版形状
  兼容（仅读 token/username）。



## [0.2.3] - 2026-10-09（认证重构：去 JWT，服务端三层认证图；本批用户清单第 2 条；tag 由维护者打）

### 新增
- **三层认证图（`internal/auth` 整包重写，`golang-jwt` 依赖移除）**：身份认证不再使用
  JWT——客户端认证通过后**只拿到一个随机哈希凭证**（32 字节随机数 hex，无任何内部结构、
  不编码身份）；真实 user id/username/role/超管标记与凭据纪元全部存服务端认证图。三层结构：
  **L1 进程内 LRU**（`auth.memMaxEntries` 默认 **200000**，超限 LRU 驱逐；条目新鲜期
  `auth.memTTL` 默认 5m，超龄强制回源——多实例撤权可见延迟的明示上界）→ **L2 可选 redis**
  （`cache.enabled` 时自动接入，键 `fm:auth:<sha256(凭证)>` 值 SETEX=剩余有效期；
  `auth.redisMaxEntries` 默认 **5000000**，ZSET(lastUsed 分数) 近似 LRU 超限裁剪 + 每用户
  反向集合支撑按用户撤权）→ **L3 持久化 Store**（唯一事实源）。明文凭证永不落任何一层，
  三层一律以 SHA-256 为键（承接旧 refresh 只存哈希纪律）。
- **新表 `auth_sessions`（四方言幂等 DDL 只增不改 + file 驱动新表序 14 + pgSeqReset）**：
  kind(user|share)/user_id(0=超管哨兵；-1=share 保留值防级联误伤)/username/role/super/gen/
  ref/token_hash UNIQUE/expires_at/last_used_at/created_at + expires 索引；随 `Migrate()`
  自动建表（sqlite 既有 .db 字节兼容）。**重启恢复**：启动 WarmStart 按 token_hash 稳定
  窗口预热 L1（上限 memMaxEntries），未预热的冷凭证经读路径惰性回源——重启后既有登录
  会话持续有效。Store 接口新增 7 个方法（Create/Get/Renew/Delete by hash/Delete by user/
  DeleteExpired/ListActive），sqlStore（gorm）/fileStore/proxy 全实现，装饰器透传。
- **过期清扫与滑动刷新**：清扫 goroutine 每 `auth.sweepInterval`（默认 10m）三层回收过期
  条目、批量写回脏续期、重试 L2 撤销失败项（pendingRevoke 本地兜底拒绝）、修剪 L2 索引。
  普通用户凭证**滑动闲置窗口**（`session.idleDays` 默认 21 天，每次认证请求 L1 即时前推，
  L2/L3 按 `auth.touchWriteEvery` 默认 10m 节流写回——硬杀进程至多丢该窗口内续期，
  凭证提前到期属安全方向）；超管/管理台会话保持绝对 `session.accessTTL`（1h）不滑动，
  维持"控制台每小时重登"节奏；分享 st 绝对 2h。
- **配置 schemaVersion 8→9**（需求25：migrate8to9 只补 `auth` 段缺省，绝不覆盖已有值；
  原文件自动备份）。`jwtSecret`/`secretFile` 字段与 jwt.secret 物化行为保留
  （存量配置兼容/PersistDBEdit 防泄密纪律），标注 **deprecated——认证不再读取二者**。

### 变更（认证面，协议形状不变）
- **`/api/v1` 字段形状零变更**：登录/刷新响应仍为 `{token, accessToken, refreshToken,
  expiresIn, user}`；`refreshToken` 回传**与 token 同一凭证**（兼容别名，旧客户端 ≤0.3.3 的
  401→refresh→重试链路原样工作——客户端实际只持有一个凭证）。`/auth/refresh` 轮换语义
  保留（撤旧发新）；`/auth/logout`、`/auth/me`、分享 `verify→?st=` 形状不变。
  authCtx 不再逐请求 `GetUserByID`（身份直接取认证图条目，读放大显著下降）。
- **撤权即时性**：登出/轮换/改密/管理员重置密码/角色变更/删号/超管口令轮换一律三层全清；
  单实例（含 file 存储 flock）即时生效，多实例经共享 L2/L3 立删 + 各实例 L1 陈旧 ≤memTTL。
- **降级异常路径**（存储不可用，0.1.1 起正常部署不可达）：超管凭证仅 L1/L2、refreshToken
  为空、重启失效（对齐旧"控制台令牌不落库"形态）；普通用户凭证沿用 503 纵深防御。

### 兼容
- **推荐配套 Client 0.3.4**（客户端认证代码零改动——其本就对凭证无格式假设）；
  **所有旧客户端 0.1.x–0.3.3 登录/刷新/分享/管理台全链路兼容**。
- **升级一次性代价（明示）**：升级到本版本后，存量 JWT 访问令牌、旧 refresh 会话
  （`sessions` 表）与旧分享 st 一律不再被接受——用户重登一次、分享访客重输一次口令即可。
- 数据面其余端点零变更；旧 `sessions` 表与其 Store 方法保留不再读写（磁盘兼容，不 ALTER
  不 DROP）；`user_roots`/`share_links` 等既有表不动。多实例部署注意：认证图 L3 走共享库
  即跨实例一致；file 存储单实例 flock 形态下无 L1 跨实例问题。
- Store 接口为契约变更（需求2）：新增 auth_sessions 7 方法 + AuthSession 模型 +
  Kind/SuperAdminID(-0)/AuthShareUserID(-1) 常量；三方言+file 驱动语义一致。
- 验证：`go build ./... && go vet ./...` 通过、gofmt 干净；冒烟 37 项断言全绿
  （file 存储+sqlite3 双引擎：凭证随机形状、refreshToken 同值别名、轮换旧废、logout 三层清、
  重启 WarmStart 存活、伪造 JWT 串 401、分享 st 身份互斥（kind 门控）、超管纪元轮换即废、
  改密/重置密码踢线、auth_sessions 落库结构与旧 sessions 表零写入）。

## [0.2.2] - 2026-10-06（缩略图格式面扩展：GET /thumb 支持 BMP；本批用户清单第 2 条+第 5 条；tag 由维护者打）

### 新增
- **`GET /api/v1/thumb` 白名单补 BMP**（`v1_thumb.go` ext 白名单 + `thumbdec.go` 纯标准库
  自实现 BMP 解码——Go 标准库 `image` 无 BMP，而客户端本地缩放（Rust image crate）支持
  bmp/webp；对齐"服务端出图能力=本地目录机制"）：支持 BITMAPCOREHEADER(12)/
  BITMAPINFOHEADER(40)/BITMAPV4HEADER(108)/BITMAPV5HEADER(124)；BI_RGB 1/4/8 位调色板
  （3/4 字节表）+ 16/24/32 位真彩、BI_BITFIELDS 16/32 位（R/G/B 掩码按位提取、位宽精确
  拉伸到 8 位）、BI_RLE8/BI_RLE4 游程（EOL/EOF/位移/字面 run 偶数对齐）；负高度=自上而下、
  非 RLE 4 字节行对齐。JPEG/PNG 内嵌压缩、平面色等罕见变体→报错；所有切片读取前判长，
  **畸形 BMP 绝不 panic**。`bmpDimensions` 先过 12MP/6000px 闸门再整体解码（与 std 路径
  DecodeConfig 同构）；64MiB 输入上限、并发信号量、415 回退链一律不变。
- **webp 明确不做**（有意决策，记录理由）：VP8 位流解码无法在"仅标准库+build-env 离线镜像
  零新依赖+零测试面（需求29）"约束下安全自研——手搓解码器出图错误比 415 更糟。替代形态：
  Client 0.3.3 对 ≤8MiB 走静默媒体缓存通道由本地 image 解码出图（与本地目录同形），
  >8MiB 永久图标（不再重试、不出下载）。

### 兼容
- 无新端点、无 schema 变更（schemaVersion 保持 8）；对旧客户端 BMP 415→200 纯增益，其余
  404/413/415/429 退化语义原样。**Server 0.2.2 为 Client 0.3.3 的推荐配套基线**
  （0.3.3 自身为纯客户端行为批，协议零变更）。

## [0.2.1] - 2026-10-06（上传中断兜底 + 浏览零下载端点；本批用户清单第 1/2/3/5/6 条；tag 由维护者打）

### 新增
- **`GET /api/v1/thumb?rootId=&path=&w=` 服务端缩略图（新文件 `v1_thumb.go`+`thumbdec.go`）**：
  注册于 anon 组紧随 /preview，守卫逐行照抄 handlePreview（`degradedView(..., d.Download, "download(thumb)")`
  +`s.resolve()`）——匿名语义与 /preview 完全一致（有库匿名 401，需求6 无新增面）。返回 ≤w px 长边
  jpeg q80（w clamp 64-1024 默认 512）；jpeg/png/gif 标准库解码（`DecodeConfig` 闸门：>12MP 或单边
  >6000px→415），相机 RAW 整读 ≤64MiB 提取**最大完整内嵌 JPEG**（Go 移植客户端 thumb.rs 的
  SOI/段链/EOI 扫描算法，扩展名表三处同抄）；>64MiB→413、非白名单/无预览→415（客户端回退链由
  Client 0.3.2 实现：≤8MiB 走旧整文件缓存通道，其余图标）。包级信号量限并发
  （cap=min(4,max(2,numCPU/2))，取不到槽 429 不排队）。**只用 Go 标准库**（不引入 x/image——
  编译镜像 GOMODCACHE 无其 zip，离线构建有风险；缩放为迭代 2× 盒式减半+末次面积加权）。
  用途：客户端网格浏览不再把整文件拉进本地缓存（需求"打开服务端文件/文件夹不要触发下载"）。
- **`POST /api/v1/check {rootId, files:[{path,size,hash?}]}` 批量重复校验（新文件 `v1_check.go`，
  auth 组）**：目录重传预检端点（Client 0.3.2 相同跳过/不同询问流程的服务端支撑）。≤1000 条/请求
  （客户端 500/批），逐路径 permsBatch Upload + scope jail 全量校验（任一不过整请求 4xx 零判定，
  同 mkdirs 骨架可安全整体重试）。逐条判定 state：live stat 失败→`missing`；size 不符或远端为目录
  →`differ`；请求未带 hash 或 `file_metas` 无存哈希→`unknown`；存哈希≠请求哈希→`differ`；
  哈希相等但 meta.size/mtime（秒）与磁盘现状不符→`unknown`（meta 过期，防绕库直改磁盘误判）；
  全等→`same`。
- **file_metas.content_hash 真实落库（让 /check 廉价可用的前置修复）**：
  `recordMeta` 携带 `FileEntry.Hash` 入库（旧实现恒填空串——`/check` 将永远拿不到证据）；
  `PutFileMeta` upsert **空值保留语义**（四方言一致，sqlstore/filestore 双侧实现）：incoming 哈希为
  空且旧行 size/mtime（秒）未变 → 保留旧 content_hash（handleList 例行刷行不再清空已存哈希）；
  mtime 口径三处统一为真实 stat：整文件上传（写后 stat+SHA256File 补哈希）、分片 complete
  （rename 后 stat）、回滚（**删除 `v1_versions.go` 的 `e.Mtime=time.Now()` 覆盖行**+现算哈希）——
  秒级 mtime 一致是保留判据成立的前提。

### 修复
- **客户端断连不再记 internal error**：`respond.go` 新增 `isNetDisconnect`
  （context.Canceled/http.ErrAbortHandler/ECONNRESET/EPIPE/io.ErrUnexpectedEOF/文案兜底）→
  `statusOf` 归 **499**（nginx 惯例非标准码），`writeErr` 对 499 只记一行 info——上传大分片
  被中间设备 reset 时服务端日志不再刷 5xx（本批事故日志根因之一）；响应体对已断开的客户端无意义。
- **上传会话与碎片兜底**：`CreateOrResume` 同用户命中旧会话但 size/chunkSize 与本次请求不符
  →作废重建（旧行为原样返回旧参数会话，客户端拿新片数填旧会话即永久卡死；chunkSize 先归一
  再比对，省略该字段的老请求不被误判换版）；`WriteChunk` 标记段按会话锁
  （`Key("#sess",id)`）重读-合并-回写，修同会话并发传片互相丢标记；`Complete` 成功后被忽略的
  DB 状态/清账错误改为记日志（不再静默"文件已替换、会话假死 uploading"）。
- **Cleaner 僵尸会话判据 `CreatedAt+7d` → `UpdatedAt+7d`**（每片写入即刷新，已核验
  UpdateUploadSession）——跨越 7 天的慢速续传不再被连坐清除。
- **崩溃残留碎片清扫**：`SweepExec` 启动遍历（roots+Users）同时回收 `*.fmmerge-*`/`*.fmtmp-*`
  命名的常规文件（mtime>24h 才删，绝不误杀在途合并/写入），返回 (去位数, 清残数) 双计数。

### 兼容
- **全加性**：旧客户端（含 0.1.x/0.2.x/0.3.x 早期）不调用 thumb/check 即无感；499/会话重建/
  标记锁对旧客户端是行为改善而非契约变化。`/api/v1` 既有端点请求/响应形态零变更。
- 无新表/新列（file_metas 既有 content_hash 列开始被写入）；Store 接口签名不变（仅
  PutFileMeta 语义注释补充）；schemaVersion 保持 8（无配置结构变更，需求25 不触发）。
- 多实例注意：file_metas 保留语义为读-改-写非原子（竞态最坏退化 unknown，注释声明）；
  目标目录碎片清扫依赖各实例启动遍历。

### 新增
- **`POST /api/v1/mkdirs` 批量建目录（需求41）**：用户端口登录组（requireDB + authCtx +
  requireLogin + maintenanceGate）加性端点，body `{rootId, paths:["a","a/b",...]}`
  （1..1000 条，客户端按 ≤500/批发送，1MB JSON decode 上限内），需 Upload 权限。
  **两阶段语义**：① 全量校验——`fileops.CleanRel` 归一去重（空 rel=根自身静默跳过）、
  `permsBatch` 逐路径裁 Upload（ACL 只装载一次，同 handleList 模式）、`scope` 逐路径
  jail/未知根/保留名 `.fmtrash` 校验——任一路径不过**整请求 4xx 且不创建任何东西**，
  客户端可安全整体重试；② 创建——逐路径持 `transfer.Key` 锁 `os.MkdirAll(0755)` 后
  即放（串行取锁避免死锁；幂等：已存在目录 no-op；中途 I/O 失败 fail-fast，已建保留、
  重试幂等，语义同 handleMove 循环）。响应 `{ok:true,created:N}`。与 `mkdir` 一致：
  不入 store、不存版本，目录元数据仍由 `handleList` 懒登记。
- 其余 `/api/v1` 端点零变更；无新表/新列（Store 接口四方言不动）；schemaVersion 保持 8。

### 兼容
- **旧客户端零影响**（不调用即无感）：Client 0.1.x 及同期早期版本配本版本全功能。
- Client 0.2.0 的目录上传依赖本端点批量建目录骨架（含空目录）；配 Server <0.2.0 时
  自动退化为逐个 `POST /mkdir`（幂等递归，落点语义一致），仍可用。

## [0.1.1] - 2026-09-23（文件元数据存储：无库不再缺功能，本批用户清单第 1 条＝需求40）

### 新增
- **内置文件存储驱动 `file`（需求40）**：`internal/store` 新增 filestore 引擎
  （filelog/fileindex/filestore/filequery/filecomp/filecopy/filelock_*）——二进制
  追加日志，记录帧 `op + length + data + crc`（CRC32-Castagnoli 覆盖帧头+数据），
  payload 为 rows.go 行模型 JSON（含口令/令牌哈希）；**内存索引**（PK map + 与
  SQL 唯一键一一对应的二级 map，查询/排序全内存，等值 O(1)）；目录布局
  `<dataDir>/filestore/`（`MANIFEST`(+.old 代际) / `snapshot-NNNNNN.log` /
  `seg-NNNNNN.log` / `LOCK`），`db.dsn` 可改目录。**刷盘策略**：写锁内批量追加、
  后台 50ms 写穿页缓存；账户/会话/SetMeta 等安全关键操作返回前同步 fsync；
  段 8MiB 滚动；追加/墓碑超阈值触发快照压缩（真正回收空间）。崩溃恢复：快照+段
  顺序回放，**末段尾撕裂截断容忍**，中段 CRC 损坏拒绝启动（不静默丢已提交数据）。
  **单实例**：目录 flock 排他，第二实例 fail-fast（多实例请配数据库）。
  全表迁移通道泛化（rowSource/rowSink）：file↔db 双向 keyset 复制保留主键。
- **管理台"数据库"页三独立操作（维护者语义澄清）**：**切换**（connect，可选
  携数据迁移，driver=file 即切回文件存储）；**导出**（`POST /admin/db/export`——
  当前库全表复制到文件存储目录、不切换引擎不写配置，目标非空 409/force 清空覆盖）；
  **暂停**（`POST /admin/db/pause`——可选"中断按钮"，冻结用户端口一切操作与管理
  业务端点（health/版本发现/配置面/超管页豁免），用于切换/迁移/导出前后的自定义
  一致性窗口）。connect/export 勾选迁移时**自动**在复制→换入全程置暂停屏障
  （`maintenanceGate` 中间件，`/admin/db/status` 回显 `paused`）。新审计动作
  `db_export`/`db_pause`/`db_resume`。

### 变更（破坏性，未发布期允许）
- **降级模式彻底取消（维护者决策）**：`db.driver` 为 `""`/`none` 时不再返回无存储
  降级形态，一律打开文件存储（`store.Open` 不再产生 `(nil,nil)`；`NormalizeDriver`
  ""/none→file）。无库部署自此全功能可用（用户/登录/ACL/分享/回收站/版本/上传
  会话/审计）。**升级提示**：旧降级部署的"匿名直接浏览配置根"行为随降级形态废除
  ——匿名访问自此一律按 public ACL 授权（与有库模式统一；超管/普通用户登录操作
  不受影响）。`/api/v1/admin/db/drivers` 目录移除 `none` 项、新增 `file` 项；
  `/test` 对 file 返回 400（无需测连）；旧 `connect driver=none`"断开回降级"臂废除
  （等效切到文件存储）。`requireDB()`/`store.Degraded` 保留为异常路径纵深防御。
- 新装默认配置 `db.driver` 由 `sqlite3` 改为 `file`（示例配置同步；schemaVersion
  保持 8——无配置结构变更，需求25 不触发）。
- 已知语义差异：`SearchDescriptions` 文件存储为大小写敏感的子串匹配（对齐
  postgresql 方言；sqlite/mysql 的 LIKE 大小写不敏感）；文件存储非关键元数据
  掉电可能丢最后 ≤50ms 批量（账户/会话/口令/标记为同步 fsync）。

### 修复
- （无——本版为加性引擎 + 管理面改造）

### 兼容
- 客户端协议零变更（数据面仅既有 503 容错语义被更频繁触发于暂停窗口）；
  SQLite 既有 .db 文件字节兼容不受影响（未触方言层/表结构）。

## [0.1.0] - 2026-09-22（首个基线版，需求31–39 批次＝用户 2026-09 清单 1–9；sqlite3/MySQL 双库全功能回归通过后定版）

### 新增
- **超级管理员账户文件化（需求32/34/35/36）**：`internal/superadmin` 新包——超管只存
  `<dataDir>/admin-account.json`（0600，不进数据库，降级模式/换库同样可用）；
  首启自动生成账户 `admin` + 随机明文口令（打印启动日志）。**口令状态机**：明文
  （使用前/文件重置后，管理台可回显）→ 首次成功登录即转 bcrypt 密文（不可回显）；
  管理台新增"超级管理员"页（仅超管可见）与 `GET/PUT /api/v1/admin/super`（改账户名/
  口令，PUT 成功后返回续期令牌）；`generation` 凭据纪元嵌入超管访问令牌，
  改密/文件重置即吊销全部旧令牌与会话。配置文件 `adminPassword` 语义改为
  **超管口令重置触发器**（非空=启动时以其为明文写入账户文件，用后清空）。
- **管理面权限分层（需求33）**：创建/修改/删除管理员账户、变更角色、管理员的目录分配
  均为超管专属（其他管理员只管普通用户，`403`）；超管不在 users 表、管理接口按 id
  寻址触达不到（`id=0 → 400`，含堵死 `DeleteUser(0)` 级联吊销超管会话的碰撞）；
  超管账户名在 `createUser` 保留（400）。
- **用户自助改密（需求31）**：`POST /api/v1/auth/change-password {oldPassword,newPassword}`
  （仅登录用户，校验旧密码；成功后该用户全部 refresh 会话作废，需重新登录；
  access token 短 TTL 自然过期）。管理员改任何用户密码沿用既有
  `PUT /admin/users/:id {password}`。
- **用户文件禁执行（需求37）**：启动后台清扫（配置根 + `<dataDir>/Users` 全部常规文件
  去执行位）+ 写路径防护（`fileops.MovePath`/`CopyFile` 收尾剥离；rename 改走
  MovePath；目录保留 0755 穿越位）。
- **管理界面 IP 准入可配（需求38）**：新配置 `adminAccess {mode: local|lan|any|custom,
  ips:[IP/CIDR…]}`（schema 6→…→**8**，migrate7to8；新装默认 `local` 仅本机回环，
  存量配置自动补 `lan` 与旧 lanOnly 语义等价零变化）；管理端口引擎改挂
  `adminIPFilter`（RemoteAddr 判定、不信任 XFF，引擎级覆盖 /admin/ 静态台与全部
  admin/配库 API），不再叠加 lanOnly。

### 变更（破坏性，未发布期允许）
- **降级模式管理台以超管登录（需求33）**：移除 `POST /auth/register` 引导注册与
  `/admin/db/unlock`、firstBoot 开放/`X-DB-Unlock` 三态闸门——配库面收敛为
  单一管理员登录态；`/admin/db/status` 移除 `firstBoot/needsUnlock` 键、新增
  `superAdmin`。管理 API 拆分：`/admin/info` 与 `/admin/super` 注册于 requireDB
  之外（降级模式可达），其余仍需元数据库。
- `POST /auth/login` 不再挂路由级 requireDB（handler 内分流：超管无库可用，
  普通账户无库仍 503）；降级模式下携带超管 token 的用户端口请求由 503 改为
  403（语义修正）。
- 管理员口令重置（配置 adminPassword）不再在库内创建/重置管理员账户。

### 兼容
- Store 接口/表结构零变更（三方言约束未触发）；客户端数据协议仅加性。
