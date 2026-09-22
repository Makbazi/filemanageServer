# =============================================================================
# filemanage-server —— Ubuntu 22.04 运行镜像（需求27/28：容器化多实例部署）
#
# 构建命令（0.3.0 起服务端自含元数据层 internal/store，不再依赖任何同级模块；
# 为兼容 compose 组织方式仍以仓库根为构建上下文，路径相对于仓库根）：
#   cd /path/to/filemanage
#   docker build -f filemanageServer/deploy/docker/ubuntu22.04/Dockerfile \
#        -t filemanage-server:ubuntu22.04 .
#
# 运行（单容器内起 2 个 worker 节点 + 1 个内置 balancer，对外只暴露 balancer 端口）：
#   docker run -d --name fm-ubuntu -p 8480:8480 \
#        -e FM_NODES=2 -e FM_BALANCER=1 -v fm-ubuntu-data:/data \
#        filemanage-server:ubuntu22.04
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

# CGO_ENABLED=0：静态链接（sqlite 驱动为纯 Go 实现 modernc.org/sqlite，无需 cgo），
# -trimpath 去除本机路径信息，-s -w 裁剪符号表与调试信息以减小体积。
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
        -o /out/filemanage-server ./cmd/filemanage-server

# ---------- 第二阶段：运行镜像 ----------
FROM ubuntu:22.04

# 基础运行时依赖：
#   ca-certificates —— 出站 HTTPS（如 AI provider / 拉取证书校验）
#   tzdata          —— 时区数据
#   bash            —— entrypoint.sh 使用 bash 特性（数组、wait -n 等）
# --no-install-recommends 避免装入推荐包，装完清理 apt 缓存减小镜像体积。
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

# 默认环境变量（可被 docker run -e / compose environment 覆盖）：
#   FM_NODES         本容器内启动的 worker 节点数
#   FM_BASE_PORT     第一个节点的监听端口，后续节点依次 +1
#   FM_DATA_DIR      数据根目录
#   FM_BALANCER      是否启动内置负载均衡（1/0；FM_NODES>1 或显式开启时生效）
#   FM_BALANCER_PORT 内置 balancer 监听端口（对外统一入口）
ENV FM_NODES=1 \
    FM_BASE_PORT=8484 \
    FM_DATA_DIR=/data \
    FM_BALANCER=1 \
    FM_BALANCER_PORT=8480

# 8480：内置 balancer；8484-8486：最多 3 个 worker 节点直连端口
EXPOSE 8480 8484 8485 8486

ENTRYPOINT ["entrypoint.sh"]
