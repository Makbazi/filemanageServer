#!/bin/bash
# =============================================================================
# build.sh —— filemanageServer 多平台构建脚本（Docker 编译环境版）
#
# 编译环境镜像（IMAGE，见 build-env/Dockerfile.rockylinux9）。全部静态编译
# （CGO_ENABLED=0，三目标均为 Go 原生交叉）。镜像管理（-i/-n）与工程编译（-a/-d）
# 为两类独立操作：给了 -i 或 -n 只操作镜像、不编译工程。
#
# 用法:
#   ./build.sh -a <平台> [-d <debug|release>]   # 编译工程
#   ./build.sh -i                               # 构建编译镜像（不编译工程）
#   ./build.sh -n                               # 拉取编译镜像（不编译工程）
#     -a  编译平台：linux | windows | arm64 | all（all=三平台全出）
#     -d  debug 或 release（默认 release）；debug 产物输出到 build/debug/，
#         release 产物输出到 build/，文件名均保持 工程名_平台_版本号[.exe] 规范
#     -i  构建编译环境镜像（docker build），完成后退出，不编译工程
#     -n  拉取编译环境镜像（docker pull）编译前拉取镜像。
#     -f  重建编译镜像（改 Dockerfile 后需要；本地缺镜像时亦会自动构建）,完成后退出，不编译工程
#     -h  显示本帮助
#
# 示例:
#   ./build.sh -n                # 先从仓库拉取编译镜像，不推出
#   ./build.sh -i                # 或本地构建编译镜像，构建编译镜像后推出
#   ./build.sh -a all            # 三平台 release → build/（本地缺镜像则自动构建）
#   ./build.sh -a windows -d debug
#
# 产物版本号（非 version.go 常量，改由 git tag 推导并经 -ldflags 注入 version.Version）：
#   取最新 git tag（去 v 前缀）；工作区有未提交改动（跟踪文件）追加 -dirty 后缀。
#   version.go 里 Version 默认为 "unknown"，仅供直接 go build 兜底，正式产物以 git tag 为准。
#   例：HEAD 处于 tag v0.1.3 且工作区干净 → 版本号 0.1.3
#     build/filemanageServer_linux_0.1.3
#     build/filemanageServer_arm64_0.1.3
#     build/filemanageServer_windows_0.1.3.exe
#   工作区有未提交改动 → build/filemanageServer_linux_0.1.3-dirty
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
BUILD_IMAGE=0
PULL_IMAGE=0
FORCE_BUILD=0

usage() {   # 打印文件头注释（第 2 行起连续 # 块）
    awk 'NR>1 && /^#/{sub(/^# ?/,""); print; next} NR>1{exit}' "$0"
    exit "${1:-0}"
}

while getopts ":a:d:infh" opt; do
    case "$opt" in
        a) PLATFORMS="$OPTARG" ;;
        d) MODE="$OPTARG" ;;
        i) BUILD_IMAGE=1 ;;
        n) PULL_IMAGE=1 ;;
        f) FORCE_BUILD=1 ;;
        h) usage 0 ;;
        *) echo "未知参数 -$OPTARG（-h 查看用法）" >&2; usage 2 ;;
    esac
done

cd "$SCRIPT_DIR"

# 镜像管理操作（-i 构建 / -n 拉取）：完成后退出，不编译工程。
if [[ "$BUILD_IMAGE" -eq 1 ]]; then
    echo "==> 构建编译环境镜像 $IMAGE ..."
    docker build -f "$SCRIPT_DIR/$DOCKERFILE" -t "$IMAGE" "$SCRIPT_DIR/build-env" \
        || { echo "!! 编译环境镜像构建失败：$IMAGE" >&2; exit 1; }
    echo "==> 镜像已构建：$IMAGE"
    exit 0
fi
if [[ "$PULL_IMAGE" -eq 1 ]]; then
    echo "==> 拉取编译环境镜像 $IMAGE ..."
    docker pull "$IMAGE" || { echo "!! 拉取镜像失败：$IMAGE" >&2; exit 1; }
    echo "==> 镜像已就绪：$IMAGE"
    # exit 0
fi
# 编译镜像就绪：-f 强制重建，否则本地缺失时自动构建（显式拉取用 -n，构建用 -i）
if [[ "$FORCE_BUILD" -eq 1 ]] || ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "==> 构建编译环境镜像 $IMAGE ..."
    docker build -f "$SCRIPT_DIR/$DOCKERFILE" -t "$IMAGE" "$SCRIPT_DIR/build-env" \
        || { echo "!! 编译环境镜像构建失败：$IMAGE" >&2; exit 1; }
    exit 0;
fi

# 编译路径：必须 -a
[[ -n "$PLATFORMS" ]] || { echo "错误：必须用 -a 指定编译平台（linux|windows|arm64|all）" >&2; usage 2; }
[[ "$MODE" == "debug" || "$MODE" == "release" ]] || { echo "错误：-d 只能为 debug 或 release" >&2; exit 2; }

# 展开 all
TARGETS="$PLATFORMS"
if [[ "$PLATFORMS" == "all" ]]; then
    TARGETS="linux,arm64,windows"
fi



# 版本信息注入源（宿主 git 计算，避免容器内 dubious ownership 问题）。
# 产物版本号 VERSION = 最新 git tag（去 v 前缀），工作区有未提交改动（跟踪文件）追加 -dirty；
# 经 BUILD_VERSION 传入容器，容器脚本据此命名产物并 -ldflags 注入 version.Version。
GIT_TAG="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERSION="$(git describe --tags --abbrev=0 2>/dev/null || echo unknown)"
VERSION="${VERSION#v}"
[[ -n "$VERSION" ]] || VERSION="unknown"
if [[ -n "$(git status --porcelain --untracked-files=no 2>/dev/null)" ]]; then
    VERSION="${VERSION}-dirty"
fi

docker volume create "$VOL_GOPATH" >/dev/null
docker volume create "$VOL_GOCACHE" >/dev/null

echo "==> 容器内构建：targets=$TARGETS mode=$MODE version=$VERSION（tag=${GIT_TAG} commit=${GIT_COMMIT}）"
docker run --rm \
    -v "$SCRIPT_DIR":/src \
    -v "$VOL_GOPATH":/root/go \
    -v "$VOL_GOCACHE":/root/.cache/go-build \
    -e GIT_TAG="$GIT_TAG" -e GIT_COMMIT="$GIT_COMMIT" -e BUILD_TIME="$BUILD_TIME" \
    -e BUILD_VERSION="$VERSION" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "$IMAGE" bash /src/build-env/container-build.sh "$TARGETS" "$MODE"

echo "==> 产物清单："
OUT_DIR="build"
[[ "$MODE" == "debug" ]] && OUT_DIR="build/debug"
ls -lh "$SCRIPT_DIR/$OUT_DIR"/filemanageServer_* 2>/dev/null || true
