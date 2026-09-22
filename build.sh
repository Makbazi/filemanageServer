#!/bin/bash
# build.sh — filemanageServer 构建脚本
# 产物输出到脚本同级的 build/ 目录（无论从何处调用）。
# 用法: ./build.sh [-r]
#   -r  release 构建：注入版本信息 + -trimpath + -s -w 去符号 + CGO_ENABLED=0 静态链接
#   默认（无 -r）为开发构建，同样注入版本信息（tag/commit_id/编译时间，-v 可查）。
set -euo pipefail

RELEASE=0
while getopts ":r" opt; do
  case "$opt" in
    r) RELEASE=1 ;;
    *) echo "用法: $0 [-r]" >&2; exit 2 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$SCRIPT_DIR/build"
BIN_NAME="filemanage-server"
VERSION_PKG="filemanageServer/internal/version"

cd "$SCRIPT_DIR"

# 构建信息：git tag / commit 短哈希 / 编译时间（UTC）；无 git 或非仓库时回退占位值
GIT_TAG="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
GIT_COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

LDFLAGS="-X ${VERSION_PKG}.GitTag=${GIT_TAG} -X ${VERSION_PKG}.GitCommit=${GIT_COMMIT} -X ${VERSION_PKG}.BuildTime=${BUILD_TIME}"

GO_EXTRA=()
if [[ "$RELEASE" -eq 1 ]]; then
  LDFLAGS="-s -w ${LDFLAGS}"
  GO_EXTRA=(-trimpath)
  export CGO_ENABLED=0
  echo "==> release 构建（trimpath + strip + 静态链接）"
fi

echo "==> go vet ./..."
go vet ./...

mkdir -p "$OUT_DIR"

echo "==> go build -o build/$BIN_NAME ./cmd/filemanage-server"
go build ${GO_EXTRA[@]+"${GO_EXTRA[@]}"} -ldflags "$LDFLAGS" -o "$OUT_DIR/$BIN_NAME" ./cmd/filemanage-server

echo "==> 构建完成: $OUT_DIR/$BIN_NAME"
echo "    tag=${GIT_TAG} commit=${GIT_COMMIT} built=${BUILD_TIME}"
ls -lh "$OUT_DIR/$BIN_NAME"
