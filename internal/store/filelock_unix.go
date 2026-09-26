//go:build unix

// filelock_unix.go：文件存储目录的进程间排他锁（0.1.1，需求26/27 的显式边界：
// 文件存储为单机引擎，两实例同目录并写会导致日志分叉，Open 即拒绝）。
package store

import (
	"syscall"
)

func lockStoreFile(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlockStoreFile(fd uintptr) {
	_ = syscall.Flock(int(fd), syscall.LOCK_UN)
}
