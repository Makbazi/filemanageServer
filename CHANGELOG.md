# Changelog — filemanageServer

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。
**每次版本变更必须声明兼容的其他模块版本（需求20）。**

> 需求39（2026-09-21 维护者决策，本批用户清单第 9 条）：0.1.0 之前从未正式发布，历史变更清单整体清空、
> 从此文件重新开始记录；此前版本明细可查本仓库 git 历史（分支 feature/dev_new_able）。

## 兼容矩阵

| filemanageServer | 元数据存储 | 兼容 filemanageClient |
|---|---|---|
| 0.1.x（当前 **0.1.1**） | **自含**；0.1.1 新增内置 `file` 驱动（无库默认，全功能）+ 切换/导出/暂停管理面，SQL 表结构无变更 | **Client 0.1.0 完整功能（推荐）**；0.6.x/0.5.x 客户端功能完整（0.1.1 为服务端存储引擎与管理面增项，`/api/v1` 数据协议零变更；health 的 `db` 字段可为 `"file"`、暂停期返回 503——均为客户端既有容错路径，旧客户端无感）|
| 0.1.0 | 自含（表结构无变更；会话 user_id=0 超管哨兵） | 同上（0.1.1 无破坏性协议变更，0.1.0 客户端对 0.1.1 服务端全功能） |
| ≤0.7.1 | 自含 | 见 git 历史（已按需求39清空） |

## 未发布版本

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
