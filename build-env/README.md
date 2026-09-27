# build-env —— filemanageServer 编译环境

## 命名规范

| 项 | 规范 | 实例 |
|---|---|---|
| Dockerfile | `Dockerfile.<平台标签>` | `Dockerfile.rockylinux9` |
| 镜像 | `filemanage-server-build:<平台标签>` | `filemanage-server-build:rockylinux9` |
| 产物 | `filemanageServer_<平台>_<版本号>[.exe]` | `filemanageServer_linux_0.1.1`、`filemanageServer_windows_0.1.1.exe`、`filemanageServer_arm64_0.1.1` |
| 产物目录 | release → `build/`；debug → `build/debug/` | 同名规范，子目录区分 |

## 编译环境

需求指定基础镜像 `rockylinux:9`。服务端三个平台目标均为 **Go 原生交叉编译 +
CGO_ENABLED=0 静态链接**（sqlite 驱动是 glebarez 纯 Go 实现），无需任何目标系统
库/mingw，镜像只含 Go 工具链（版本对齐 go.mod 的 `go 1.27.1`，升级 go.mod 时同步
`ARG GO_VERSION`）与 git。

## 使用

常规入口（宿主机执行，需 docker）：

```bash
./build.sh -a all                 # linux + windows + arm64，release → build/
./build.sh -a windows -d debug    # 仅 windows debug → build/debug/
```

- `-a`：`linux` | `windows` | `arm64` | `all`（必选）
- `-d`：`debug` | `release`（默认 release）；release 带 `-trimpath -s -w`，
  debug 带 `-gcflags all=-N -l`；两者均注入版本信息（`-ldflags -X`，产物 `-v` 可查
  tag/commit/编译时间）
- 镜像缺失时脚本自动 `docker build`；`fms-gopath` / `fms-gocache` 命名卷缓存
  模块与增量编译产物

手动等价命令（调试用）见 `Dockerfile.rockylinux9` 头注释。

## 历史

`build.sh` 原为宿主直编脚本（`-r` 开关）；现重写为上述 Docker 多平台接口，
旧 `-r` 行为 ≈ `./build.sh -a linux -d release`。
