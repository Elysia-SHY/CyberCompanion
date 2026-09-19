package config

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 以「临时文件 → fsync → 原子重命名」方式写入文件。
//
// 这样即使写入过程中进程被 kill 或设备断电，目标文件也只会是
// 「旧内容」或「新内容」二者之一，不会出现半截 JSON 导致下次启动解析失败。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()

	// 无论成功失败都清理临时文件（成功重命名后 Remove 会因文件不存在而静默失败）
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// 先落盘再重命名，否则重命名后内容仍可能停留在页缓存中
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// fsync 父目录，确保「重命名」这个元数据变更本身也已持久化
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
