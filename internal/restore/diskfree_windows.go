//go:build windows

package restore

import "golang.org/x/sys/windows"

// diskFree 返回 path 所在卷对当前用户的可用字节数（R-RS-02 磁盘预检）。
func diskFree(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, nil, nil); err != nil {
		return 0, err
	}
	return avail, nil
}
