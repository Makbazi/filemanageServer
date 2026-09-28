#!/bin/bash
# =============================================================================
# container-build.sh —— filemanageServer 容器内构建脚本
# （由宿主机 build.sh 经 docker run 挂载调用；也可按 Dockerfile 头注释手动执行）
#
# 用法: container-build.sh <targets> <debug|release>
#   targets 逗号列表: linux | windows | arm64 （大小写不敏感）
#   环境变量 GIT_TAG / GIT_COMMIT / BUILD_TIME 由宿主传入（版本信息注入，`-v` 可查）；
#   BUILD_VERSION = 产物版本号（宿主从 git tag 推导，工作区有未提交改动带 -dirty 后缀）
#
# 产物命名规范: 工程名_平台_版本号[.exe] → /src/build/[debug/]
#   filemanageServer_linux_0.1.3 / filemanageServer_arm64_0.1.3 /
#   filemanageServer_windows_0.1.3.exe   （版本号来自 BUILD_VERSION）
# 一律 CGO_ENABLED=0 静态编译（sqlite 驱动为 glebarez 纯 Go 实现）。
# =============================================================================
set -euo pipefail

TARGETS_RAW="${1:?用法: container-build.sh <targets> <debug|release>}"
MODE="${2:?用法: container-build.sh <targets> <debug|release>}"
[[ "$MODE" == "debug" || "$MODE" == "release" ]] || { echo "非法 -d 值: $MODE" >&2; exit 2; }

cd /src
VERSION_PKG="filemanageServer/internal/version"

# 版本号由宿主 build.sh 经 BUILD_VERSION 传入（取自最新 git tag）；未传时兜底读 version.go
VERSION="${BUILD_VERSION:-}"
[[ -n "$VERSION" ]] || VERSION="$(sed -n 's/^var Version = "\([^"]*\)".*/\1/p' internal/version/version.go)"
[[ -n "$VERSION" ]] || { echo "无法确定版本号（BUILD_VERSION 未传且 version.go 解析失败）" >&2; exit 1; }

GIT_TAG="${GIT_TAG:-unknown}"
GIT_COMMIT="${GIT_COMMIT:-unknown}"
BUILD_TIME="${BUILD_TIME:-unknown}"

INJECT="-X ${VERSION_PKG}.Version=${VERSION} -X ${VERSION_PKG}.GitTag=${GIT_TAG} -X ${VERSION_PKG}.GitCommit=${GIT_COMMIT} -X ${VERSION_PKG}.BuildTime=${BUILD_TIME}"
if [[ "$MODE" == "release" ]]; then
    LDFLAGS="-s -w ${INJECT}"
    EXTRA=(-trimpath)
else
    LDFLAGS="${INJECT}"
    EXTRA=(-gcflags "all=-N -l")
fi

OUT_DIR="build"
[[ "$MODE" == "debug" ]] && OUT_DIR="build/debug"
mkdir -p "$OUT_DIR"

export CGO_ENABLED=0

echo "==> go vet ./...（需求29 底线验证）"
go vet ./...

fail=0
IFS=',' read -ra TARGETS <<< "$TARGETS_RAW"
for t in "${TARGETS[@]}"; do
    case "$(echo "$t" | tr '[:upper:]' '[:lower:]')" in
        linux)   GOOS=linux   GOARCH=amd64 PLAT=linux   EXT="" ;;
        arm64)   GOOS=linux   GOARCH=arm64 PLAT=arm64   EXT="" ;;
        windows) GOOS=windows GOARCH=amd64 PLAT=windows EXT=".exe" ;;
        *) echo "  !! 未知目标: $t（跳过）" >&2; fail=1; continue ;;
    esac
    NAME="filemanageServer_${PLAT}_${VERSION}${EXT}"
    echo "==> 构建 $t（GOOS=$GOOS GOARCH=$GOARCH $MODE）→ $OUT_DIR/$NAME"
    if GOOS=$GOOS GOARCH=$GOARCH go build "${EXTRA[@]}" -ldflags "$LDFLAGS" \
        -o "$OUT_DIR/$NAME" ./cmd/filemanage-server; then
        echo "    产物: $OUT_DIR/$NAME"
    else
        echo "    !! $t 构建失败" >&2
        fail=1
    fi
done

# 产物归属改回宿主用户（容器以 root 运行，避免 root 属主文件需 sudo 清理）
if [[ -n "${HOST_UID:-}" && "${HOST_UID}" != "0" ]]; then
    chown -R "${HOST_UID}:${HOST_GID:-$HOST_UID}" "$OUT_DIR" 2>/dev/null || true
fi

exit "$fail"
