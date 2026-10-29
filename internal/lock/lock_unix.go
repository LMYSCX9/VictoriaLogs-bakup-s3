//go:build !windows

package lock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock 尝试对锁文件加排他锁，立即返回（不阻塞）。
// busy 为 true 表示锁已被其他进程持有；err 为其他内核错误。
func tryLock(f *os.File) (busy bool, err error) {
	e := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case e == nil:
		return false, nil
	case errors.Is(e, syscall.EWOULDBLOCK), errors.Is(e, syscall.EAGAIN), errors.Is(e, syscall.EINTR):
		return true, nil
	default:
		return false, e
	}
}

// unlock 释放内核锁。
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
