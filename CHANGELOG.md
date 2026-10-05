# Changelog — filemanageServer

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)。
**每次版本变更必须声明兼容的其他模块版本（需求20）。**

> 需求39（2026-09-21 维护者决策，本批用户清单第 9 条）：0.1.0 之前从未正式发布，历史变更清单整体清空、
> 从此文件重新开始记录；此前版本明细可查本仓库 git 历史（分支 feature/dev_new_able）。

## 兼容矩阵

| filemanageServer | 元数据存储 | 兼容 filemanageClient |
|---|---|---|
| 0.2.x（当前 **0.2.1**） | 自含（同 0.1.x；0.2.1 纯 API 面加性 `GET /thumb`+`POST /check` 与静默修复，无新表/新列、schemaVersion 保持 8） | **Client 0.3.2 完整功能（推荐：浏览零下载+目录重传预检依赖本版本端点）**；Client 0.2.0/0.3.x 早期版本全功能无感（不调用新端点即无感）；Client 0.1.x 及以下同 |
| 0.2.0 | 自含（同 0.1.x；0.2.0 纯 API 面增项 `POST /api/v1/mkdirs`） | **Client 0.2.0 完整功能（目录上传）**；Client 0.1.x 及以下全功能无感（mkdirs 为加性端点，旧客户端不调用） |
| 0.1.x | **自含**；0.1.1 新增内置 `file` 驱动（无库默认，全功能）+ 切换/导出/暂停管理面，SQL 表结构无变更 | **Client 0.1.0 完整功能**；0.6.x/0.5.x 客户端功能完整（0.1.1 为服务端存储引擎与管理面增项，`/api/v1` 数据协议零变更；health 的 `db` 字段可为 `"file"`、暂停期返回 503——均为客户端既有容错路径，旧客户端无感）；**Client 0.2.0 配本系列服务端时目录上传自动退化为逐个 `mkdir`（幂等递归，语义一致、仅往返更多）** |
| 0.1.0 | 自含（表结构无变更；会话 user_id=0 超管哨兵） | 同上（0.1.1 无破坏性协议变更，0.1.0 客户端对 0.1.1 服务端全功能） |
| ≤0.7.1 | 自含 | 见 git 历史（已按需求39清空） |

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
- **旧客户端零影响**（不调用即无感）：Client 0.1.x/0.6.x/0.5.x 配本版本全功能。
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
