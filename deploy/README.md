# filemanage-server 部署物说明（deploy/）

本目录提供 filemanage-server 的容器化与负载均衡部署物（需求26/27/28）：

```
deploy/
├── README.md                        # 本文档
├── nginx.conf.sample                # Nginx 反向代理负载均衡配置示例（内置 balancer 之外的备选方案）
└── docker/
    ├── docker-compose.yml           # 三种基础镜像的多容器编排示例
    ├── entrypoint.sh                # 容器入口脚本：生成多节点配置、启动 worker + 内置 balancer
    ├── ubuntu22.04/Dockerfile       # Ubuntu 22.04 运行镜像
    ├── debian12/Dockerfile          # Debian 12 (bookworm) slim 运行镜像
    └── rockylinux/Dockerfile        # Rocky Linux 9 运行镜像
```

---

## 一、多实例部署（需求26）

服务端天然支持多实例：**每个实例一份独立的 config（server.json）+ 独立的数据目录**，
即可在同一台或多台机器上并行运行，互不干扰。最小示例（同机 3 实例）：

```bash
# 实例 i（0/1/2）：用户口监听 8484+i，数据全部放在 /data/node$i/ 下
./filemanage-server -config /data/node0/server.json   # listen 0.0.0.0:8484
./filemanage-server -config /data/node1/server.json   # listen 0.0.0.0:8485
./filemanage-server -config /data/node2/server.json   # listen 0.0.0.0:8486
```

> **0.5.0+/0.1.0 提醒**：每个实例还必须绑定**唯一**的 `adminListen`（如
> `127.0.0.1:18485+i`，全部用默认值会互相冲突启动失败）。管理面访问范围由
> `adminAccess` 决定（`local` 仅本机回环=默认/`lan`/`any`/`custom` 指定 IP·CIDR），
> 与用户端口的 `lanOnly` 互不影响；建议保持 127.0.0.1 绑定 + SSH 隧道访问。
> 组成共享集群时 `admin-account.json`（0.1.0 超管账户文件，位于各自 `dataDir`）
> 需放共享卷或逐节点保持一致，否则各节点超管口令独立。

worker 配置示例（`server.json`）：

```json
{
  "schemaVersion": 2,
  "listen": "0.0.0.0:8484",
  "lanOnly": false,
  "instanceId": "node0",
  "db": {"driver": "sqlite", "dsn": "/data/node0/meta.db"},
  "roots": [{"id": "main", "name": "主目录", "path": "/data/node0/files"}],
  "trashDir": "/data/node0/trash",
  "uploadStagingDir": "/data/node0/staging",
  "secretFile": "/data/node0/jwt.secret",
  "versions": {"enabled": true, "maxVersions": 10, "dir": "/data/node0/versions"},
  "ai": {"provider": "none"}
}
```

**共享数据组成集群**：若希望多实例共享同一份数据（而不是各自独立），
让所有节点配置指向**相同**的 `db.dsn`、`roots[].path`、`trashDir`、
`uploadStagingDir`、`secretFile` 即可（例如统一用 `/data/shared/...`）。
0.7.0 起用户文件与家目录内回收站实体位于 `<dataDir>/Users/<用户名>`——共享部署时
`dataDir` 也必须指向共享卷（管理台"数据库"页的热切换仅对本节点生效，需逐台操作；
0.1.0 超管账户文件 `admin-account.json` 同样随 dataDir，多实例需共享或逐节点一致）。
注意：

- 共享同一个 sqlite db 时，数据目录应放在本地盘（sqlite 不适合网络文件系统），
  并发写依赖 sqlite 自身的锁；
- 所有节点必须共用同一个 `secretFile`，否则一个节点签发的 JWT 其他节点不认。

`docker/entrypoint.sh` 默认按"每节点独立目录"生成配置（互不干扰），
共享模式的写法在该脚本内有注释示例。

---

## 二、负载均衡（需求27）：两种方式

### 方式 1：内置 balancer 模式（推荐，零额外组件）

服务端自带轮询反向代理模式：

```bash
./filemanage-server -mode balancer -config balancer.json
```

`balancer.json` 示例（`balancer.workers` 为 worker 地址列表，balancer 监听
`listen` 地址并对 workers 做轮询转发）：

```json
{
  "schemaVersion": 2,
  "listen": "0.0.0.0:8480",
  "lanOnly": false,
  "balancer": {"workers": ["127.0.0.1:8484", "127.0.0.1:8485", "127.0.0.1:8486"]}
}
```

容器场景下 `docker/entrypoint.sh` 会自动生成该配置并启动 balancer，
对外只需暴露 balancer 端口（默认 8480）。

### 方式 2：Nginx 反向代理（备选）

见 [`nginx.conf.sample`](nginx.conf.sample)。适合已有 Nginx 基础设施、
需要 HTTPS 终结、访问日志、限流等能力的场景；直接对多个默认模式 worker
（127.0.0.1:8484/8485/8486）做 upstream 轮询，含大文件上传相关调优
（`client_max_body_size 0`、`proxy_request_buffering off`、`proxy_read_timeout 600s`）
与可选的 443 自签证书 HTTPS 配置示例。

---

## 三、Docker 镜像构建（需求28）

0.3.0 起服务端自含元数据层（`internal/store`，sqlite3/mysql/postgresql 三驱动，
纯 Go 无 CGO），无任何同级模块依赖——**构建上下文按下方示例取仓库根目录**
（`filemanage/`，与 compose 组织保持一致；单独以 `filemanageServer/` 为上下文亦可，
需相应调整 Dockerfile 内 COPY 路径）。三个 Dockerfile 均为多阶段构建
（golang 编译 + 精简运行镜像，静态编译 CGO_ENABLED=0）。

在仓库根目录执行：

```bash
# Ubuntu 22.04
docker build -f filemanageServer/deploy/docker/ubuntu22.04/Dockerfile \
     -t filemanage-server:ubuntu22.04 .

# Debian 12 slim
docker build -f filemanageServer/deploy/docker/debian12/Dockerfile \
     -t filemanage-server:debian12 .

# Rocky Linux 9（rockylinux/rockylinux:9，Rocky 官方现行滚动维护镜像；
#  Docker 官方库的 rockylinux:9 标签已冻结在 9.3，详见该 Dockerfile 头部注释）
docker build -f filemanageServer/deploy/docker/rockylinux/Dockerfile \
     -t filemanage-server:rockylinux .
```

直接运行单个镜像（容器内 2 个 worker + 1 个内置 balancer）：

```bash
docker run -d --name fm -p 8480:8480 -e FM_NODES=2 -e FM_BALANCER=1 \
     -v fm-data:/data filemanage-server:ubuntu22.04
```

### entrypoint.sh 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `FM_NODES` | `1` | 本容器内 worker 节点数（节点 i 监听 `FM_BASE_PORT+i`） |
| `FM_BASE_PORT` | `8484` | 第一个 worker 的监听端口 |
| `FM_DATA_DIR` | `/data` | 数据根目录（节点 i 在 `$FM_DATA_DIR/node$i/`） |
| `FM_BALANCER` | `1` | 是否启动内置 balancer（`FM_NODES>1` 或显式置 1 时启动） |
| `FM_BALANCER_PORT` | `8480` | balancer 监听端口（容器对外统一入口） |
| `FM_CONFIG` | 空 | 指定已有配置文件时直接以单节点运行该配置，跳过生成逻辑 |

---

## 四、docker compose 用法

`docker/docker-compose.yml` 定义了 fm-ubuntu / fm-debian / fm-rocky 三个服务，
分别映射宿主端口 8481/8482/8483 到各自容器内 balancer 的 8480，数据各自使用命名卷。

```bash
cd filemanageServer/deploy/docker

# 构建并启动全部三个容器
docker compose up -d --build

# 访问入口
#   http://127.0.0.1:8481/  （Ubuntu 容器）
#   http://127.0.0.1:8482/  （Debian 容器）
#   http://127.0.0.1:8483/  （Rocky 容器）

docker compose logs -f fm-ubuntu   # 查看日志
docker compose down                # 停止（保留数据卷）
docker compose down -v             # 停止并删除数据卷
```

### 每容器节点数：NODES_PER_CONTAINER

compose 中三个服务均为 `FM_NODES: ${NODES_PER_CONTAINER:-2}`，即默认每容器 2 个节点。
调整方法：

```bash
# 方法 1：在 docker-compose.yml 同目录创建 .env 文件
echo "NODES_PER_CONTAINER=3" > .env
docker compose up -d

# 方法 2：启动命令前临时指定
NODES_PER_CONTAINER=4 docker compose up -d
```

### 容器数量控制

- **做法 A（scale）**：`docker compose up -d --scale fm-ubuntu=3`。
  注意固定端口映射会冲突，需改用端口范围（如 `"8481-8491:8480"`）
  或去掉 `ports` 仅走 compose 内部网络（通过服务名 `http://fm-ubuntu:8480` 直连），
  两种做法在 compose 文件头部注释中有详细说明。
- **做法 B（复制服务定义）**：在 compose 文件中复制出 fm-ubuntu-2、fm-ubuntu-3 …
  各用不同宿主端口与命名卷；数量固定、端口明确的场景推荐此法。

---

## 五、注意事项

- Dockerfile 构建阶段使用 `golang:1.27`：本工程 go.mod 声明 `go 1.27.1`，
  官方 golang 镜像 `GOTOOLCHAIN=local` 不会自动升级工具链，低于 1.27 的
  builder（如模板提到的 1.25）会构建失败，相关理由已注释在各 Dockerfile 中。
- 镜像 `EXPOSE 8480 8484 8485 8486`：`FM_NODES` 超过 3 时节点仍会正常启动，
  仅更多 worker 端口未声明；对外统一走 balancer 端口不受影响。
- entrypoint 采用 `wait -n`：任一子进程（worker/balancer）退出即整容器退出，
  配合 `restart: unless-stopped`（compose 已设置）由编排系统整体拉起。
- 生成的 server.json 显式包含 `"ai": {"provider": "none"}`，关闭 AI 能力避免歧义；
  db driver 固定为 `sqlite`。
