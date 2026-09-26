//go:build !unix

// filelock_other.go：非 unix 平台不提供文件存储驱动（本工程部署目标为 linux，
// 见 deploy/；Windows 交叉构建仍可编译，但 Open 会显式拒绝）。
package store

import "errors"

func lockStoreFile(fd uintptr) error {
	return errors.New("filestore: 文件存储驱动仅支持 unix 平台（请配置数据库）")
}

func unlockStoreFile(fd uintptr) {}
