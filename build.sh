#!/bin/bash
# =============================================================================
# build.sh —— filemanageServer 多平台构建脚本（Docker 编译环境版）
#
# 编译环境：filemanage-server-build:rockylinux9（build-env/Dockerfile.rockylinux9，
# 镜像缺失时自动构建）。全部静态编译（CGO_ENABLED=0，三目标均为 Go 原生交叉）。
#
# 用法:
#   ./build.sh -a <平台> [-d <debug|release>]
#     -a  编译平台：linux | windows | arm64 | all（all=三平台全出）
#     -d  debug 或 release（默认 release）；debug 产物输出到 build/debug/，
#         release 产物输出到 build/，文件名均保持 工程名_平台_版本号[.exe] 规范
#     -f  强制重建编译环境镜像（修改 build-env/Dockerfile.rockylinux9 后需要）
#
# 示例:
#   ./build.sh -a all            # 三平台 release → build/
#   ./build.sh -a windows -d debug
#   ./build.sh -a all -f         # -f 强制重建编译环境镜像（改 Dockerfile 后需要）
#
# 产物命名（版本号取自 internal/version/version.go）：
#   build/filemanageServer_linux_0.1.1
#   build/filemanageServer_arm64_0.1.1
#   build/filemanageServer_windows_0.1.1.exe
#
# 历史说明：本脚本取代旧"宿主直编 + -r"方式（旧 -r ≈ 现 -d release -a linux）。
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="harbor.localserver.com:9080/filemanage/filemanage-server-build:latest"
DOCKERFILE="build-env/Dockerfile.rockylinux9"
VOL_GOPATH="fms-gopath"
VOL_GOCACHE="fms-gocache"

PLATFORMS=""
MODE="release"

usage() {   # 打印文件头注释（第 2 行起连续 # 块）
    awk 'NR>1 && /^#/{sub(/^# ?/,""); print; next} NR>1{exit}' "$0"
    exit "${1:-0}"
}

FORCE_BUILD=0
while getopts ":a:d:fh" opt; do
    case "$opt" in
        a) PLATFORMS="$OPTARG" ;;
        d) MODE="$OPTARG" ;;
        f) FORCE_BUILD=1 ;;
        h) usage 0 ;;
        *) echo "未知参数 -$OPTARG（-h 查看用法）" >&2; usage 2 ;;
    esac
done

[[ -n "$PLATFORMS" ]] || { echo "错误：必须用 -a 指定编译平台（linux|windows|arm64|all）" >&2; usage 2; }
[[ "$MODE" == "debug" || "$MODE" == "release" ]] || { echo "错误：-d 只能为 debug 或 release" >&2; exit 2; }

# 展开 all
TARGETS="$PLATFORMS"
if [[ "$PLATFORMS" == "all" ]]; then
    TARGETS="linux,arm64,windows"
fi

# 镜像缺失（或 -f 强制）时自动构建（上下文取 build-env/，镜像不 COPY 工程，极小）
if [[ "$FORCE_BUILD" -eq 1 ]] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "==> 编译环境镜像不存在，构建 $IMAGE ..."
    docker build -f "$SCRIPT_DIR/$DOCKERFILE" -t "$IMAGE" "$SCRIPT_DIR/build-env" \
        || { echo "!! 编译环境镜像构建失败：$IMAGE" >&2; exit 1; }
fi

# 版本信息注入源（宿主 git 计算，避免容器内 dubious ownership 问题）
cd "$SCRIPT_DIR"
GIT_TAG="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

docker volume create "$VOL_GOPATH" >/dev/null
docker volume create "$VOL_GOCACHE" >/dev/null

echo "==> 容器内构建：targets=$TARGETS mode=$MODE（tag=${GIT_TAG} commit=${GIT_COMMIT}）"
docker run --rm \
    -v "$SCRIPT_DIR":/src \
    -v "$VOL_GOPATH":/root/go \
    -v "$VOL_GOCACHE":/root/.cache/go-build \
    -e GIT_TAG="$GIT_TAG" -e GIT_COMMIT="$GIT_COMMIT" -e BUILD_TIME="$BUILD_TIME" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "$IMAGE" bash /src/build-env/container-build.sh "$TARGETS" "$MODE"

echo "==> 产物清单："
OUT_DIR="build"
[[ "$MODE" == "debug" ]] && OUT_DIR="build/debug"
ls -lh "$SCRIPT_DIR/$OUT_DIR"/filemanageServer_* 2>/dev/null || true
