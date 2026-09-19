//go:build linux || android || darwin

package hal

import "syscall"

// statfs 返回文件系统总容量与可用容量（字节）。
func statfs(path string) (total, free uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bsize := uint64(st.Bsize)
	return st.Blocks * bsize, st.Bavail * bsize, nil
}
