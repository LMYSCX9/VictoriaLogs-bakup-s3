//go:build windows

package lock

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock 尝试对锁文件第 0 字节加排他锁，立即返回（不阻塞）。
// busy 为 true 表示锁已被其他进程持有；err 为其他内核错误。
func tryLock(f *os.File) (busy bool, err error) {
	ol := new(windows.Overlapped)
	e := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if e == nil {
		return false, nil
	}
	switch e {
	case windows.ERROR_LOCK_VIOLATION, windows.ERROR_IO_PENDING:
		return true, nil
	default:
		return false, e
	}
}

// unlock 释放内核锁。
func unlock(f *os.File) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
}
