// Package version 提供服务端独立迭代版本号（需求19）。
// 变更时必须同步更新 CHANGELOG.md，并声明兼容的 filemanageClient
// 版本范围（需求20；元数据存储层已并入本工程 internal/store）。
package version

// Version 服务端版本号，/api/v1/health 与 /admin 界面均引用此处。
// 默认 "unknown"，由 build.sh 在编译时经 -ldflags
// "-X filemanageServer/internal/version.Version=..." 注入（取自最新 git tag，
// 去 v 前缀，工作区有未提交改动带 -dirty 后缀）；直接 go build 未注入时保持 "unknown"。
var Version = "unknown"

// 构建信息由 build.sh 在编译时经 -ldflags "-X filemanageServer/internal/version.Xxx=..."
// 注入（可执行程序 `-v` 输出）；直接 go build 未注入时保持以下默认值。
var (
	// GitTag 构建时的 git tag（例如 v0.1.0；工作区有未提交改动带 -dirty 后缀）。
	GitTag = "dev"
	// GitCommit 构建时的 git commit 短哈希（commit_id）。
	GitCommit = "unknown"
	// BuildTime 编译时间（UTC，ISO8601）。
	BuildTime = "unknown"
)

// APIVersions 服务端支持的 API 版本列表（需求17：版本迭代的 API 管理）。
// 新版本以子路由方式追加（/api/v2/...），旧版本保持兼容直至废弃公告。
var APIVersions = []string{"v1"}

// CurrentAPI 当前默认 API 版本。
const CurrentAPI = "v1"
