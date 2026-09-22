# =============================================================================
# filemanage-server —— Debian 12 (bookworm) slim 运行镜像（需求27/28）
#
# 构建命令（0.3.0 起服务端自含元数据层 internal/store，不再依赖任何同级模块；
# 为兼容 compose 组织方式仍以仓库根为构建上下文，路径相对于仓库根）：
#   cd /path/to/filemanage
#   docker build -f filemanageServer/deploy/docker/debian12/Dockerfile \
#        -t filemanage-server:debian12 .
#
# 运行（单容器内起 2 个 worker 节点 + 1 个内置 balancer）：
#   docker run -d --name fm-debian -p 8480:8480 \
#        -e FM_NODES=2 -e FM_BALANCER=1 -v fm-debian-data:/data \
#        filemanage-server:debian12
# =============================================================================

# ---------- 第一阶段：编译 ----------
# 说明：任务模板写的是 golang:1.25，但本工程 go.mod 声明 `go 1.27.1`，
# 而官方 golang 镜像设置了 GOTOOLCHAIN=local（不会自动下载更高版本工具链），
# 用 1.25 编译会直接报 "go.mod requires go >= 1.27.1"。
# 因此这里选用不低于 go.mod 要求的 golang:1.27，保证构建可用。
FROM golang:1.27 AS builder

WORKDIR /src

# 复制服务端工程（含 go.mod/go.sum；元数据存储层已并入 internal/store）。
# 路径相对于构建上下文（仓库根）。
COPY filemanageServer/ ./filemanageServer/

WORKDIR /src/filemanageServer

# CGO_ENABLED=0：静态链接（sqlite 驱动为纯 Go 实现，无需 cgo），
# -trimpath 去除本机路径信息，-s -w 裁剪符号表与调试信息以减小体积。
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o /out/filemanage-server ./cmd/filemanage-server

# ---------- 第二阶段：运行镜像 ----------
# debian:12-slim 为精简版镜像：apt 可用，但预装包很少，
# 需要显式安装 ca-certificates/tzdata/bash，并在装完后清理 apt 缓存。
FROM debian:12-slim

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates tzdata bash \
 && rm -rf /var/lib/apt/lists/*

# 服务端二进制与入口脚本（COPY 源路径相对于构建上下文，即仓库根）
COPY --from=builder /out/filemanage-server /usr/local/bin/filemanage-server
COPY filemanageServer/deploy/docker/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod 755 /usr/local/bin/filemanage-server /usr/local/bin/entrypoint.sh

# 数据卷：所有节点的配置、数据库、文件、回收站、暂存、版本目录都在 /data 下
VOLUME /data

# 默认环境变量（可被 docker run -e / compose environment 覆盖），含义见 entrypoint.sh
ENV FM_NODES=1 \
    FM_BASE_PORT=8484 \
    FM_DATA_DIR=/data \
    FM_BALANCER=1 \
    FM_BALANCER_PORT=8480

# 8480：内置 balancer；8484-8486：最多 3 个 worker 节点直连端口
EXPOSE 8480 8484 8485 8486

ENTRYPOINT ["entrypoint.sh"]
