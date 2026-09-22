# =============================================================================
# filemanage-server —— Rocky Linux 9 运行镜像（需求27/28）
#
# 基础镜像选型说明：
#   - Rocky 系官方现行镜像为 Rocky Linux 9（RHEL 9 同源重建，维护到 2032 年），
#     等价满足"rockylinux 系"部署要求，故选用 9 而非 8。
#   - Docker 官方库（library）里的 `rockylinux:9` 标签已冻结在 9.3（2023-11），
#     Docker Hub 页面亦提示 Rocky 无法继续向官方库推送更新；
#     Rocky 基金会实际维护、按月滚动更新的是 `rockylinux/rockylinux:9`。
#     因此这里 FROM rockylinux/rockylinux:9，以获得持续的安全更新。
#     （如内网环境只镜像了 library 标签，可改回 `FROM rockylinux:9`，构建步骤相同。）
#
# 构建命令（0.3.0 起服务端自含元数据层 internal/store，不再依赖任何同级模块；
# 为兼容 compose 组织方式仍以仓库根为构建上下文，路径相对于仓库根）：
#   cd /path/to/filemanage
#   docker build -f filemanageServer/deploy/docker/rockylinux/Dockerfile \
#        -t filemanage-server:rockylinux .
#
# 运行（单容器内起 2 个 worker 节点 + 1 个内置 balancer）：
#   docker run -d --name fm-rocky -p 8480:8480 \
#        -e FM_NODES=2 -e FM_BALANCER=1 -v fm-rocky-data:/data \
#        filemanage-server:rockylinux
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
FROM rockylinux/rockylinux:9

# 基础运行时依赖：ca-certificates（出站 HTTPS）、tzdata（时区）、bash（入口脚本）。
# rockylinux/rockylinux 标准镜像带 dnf；若换用 *-minimal 变体则只有 microdnf。
# 这里做兼容判断：优先 microdnf（体积更小），否则用 dnf；
# --setopt=install_weak_deps=False 跳过弱依赖，装完清理缓存减小镜像体积。
RUN set -eux; \
    if command -v microdnf >/dev/null 2>&1; then \
        microdnf install --setopt=install_weak_deps=0 -y \
            ca-certificates tzdata bash; \
        microdnf clean all; \
    else \
        dnf install -y --setopt=install_weak_deps=False \
            ca-certificates tzdata bash; \
        dnf clean all; \
        rm -rf /var/cache/dnf /var/cache/yum; \
    fi

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
