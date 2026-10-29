//go:build !windows

package restore

import "syscall"

// diskFree 返回 path 所在文件系统的可用字节数（非 root 配额，R-RS-02 磁盘预检）。
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
