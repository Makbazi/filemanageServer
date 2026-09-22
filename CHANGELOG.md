# Changelog — filemanageServer

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。
**每次版本变更必须声明兼容的其他模块版本（需求20）。**

> 需求39（2026-09-21 维护者决策，本批用户清单第 9 条）：0.1.0 之前从未正式发布，历史变更清单整体清空、
> 从此文件重新开始记录；此前版本明细可查本仓库 git 历史（分支 feature/dev_new_able）。

## 兼容矩阵

| filemanageServer | 元数据存储 | 兼容 filemanageClient |
|---|---|---|
| 0.1.x（当前 0.1.0） | **自含**（表结构无变更；会话表 user_id=0 为超管哨兵，无 FK、与自增主键不冲突） | **Client 0.1.0 完整功能（推荐）**；0.6.x/0.5.x 客户端功能完整（破坏性变更均为管理面/认证面，客户端数据协议仅**新增** change-password 端点，旧客户端无感）——注意：0.6.0 起的管理员不可登录客户端规则在 0.1.0 对文件超管同样生效 |
| ≤0.7.1 | 自含 | 见 git 历史（已按需求39清空） |

## 未发布版本

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
  0.7.0 的 `/admin/db/unlock`、firstBoot 开放/`X-DB-Unlock` 三态闸门——配库面收敛为
  单一管理员登录态；`/admin/db/status` 移除 `firstBoot/needsUnlock` 键、新增
  `superAdmin`。管理 API 拆分：`/admin/info` 与 `/admin/super` 注册于 requireDB
  之外（降级模式可达），其余仍需元数据库。
- `POST /auth/login` 不再挂路由级 requireDB（handler 内分流：超管无库可用，
  普通账户无库仍 503）；降级模式下携带超管 token 的用户端口请求由 503 改为
  403（语义修正）。
- 管理员口令重置（配置 adminPassword）不再在库内创建/重置管理员账户。

### 兼容
- Store 接口/表结构零变更（三方言约束未触发）；客户端数据协议仅加性。
