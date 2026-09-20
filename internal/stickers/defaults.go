package stickers

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// 内置表情随二进制分发。
//
// 这些图片原先以 base64 巨行形式硬编码在源码里（stickers.go 801KB），
// 现在拆成独立资源文件并经过压缩（love 那张从 431KB 降到 33KB），
// 整包 216KB。首次启动时解压到 <配置目录>/stickers/defaults/，
// 之后用户可以随意增删改 —— 内置副本只负责「开箱可用」。
//
//go:embed all:defaults
var defaultsFS embed.FS

// defaultsPrefix 是内置资源在 embed.FS 与本地媒体目录里的共同子路径。
const defaultsPrefix = "defaults/"

// defaultLibrary 返回出厂表情库。
//
// ID 刻意不与场景名相同：标记解析时「场景」优先于「ID」，
// 于是 [表情:love] 表示「随便来一张 love 场景的图」，
// 而 [表情:love1] 才表示「就要这一张」。
func defaultLibrary() []*Sticker {
	return []*Sticker{
		{ID: "love1", Scene: "love", Source: SourceLocal, File: "defaults/love_0.jpg", Note: "贴贴", Enabled: true},
		{ID: "greeting1", Scene: "greeting", Source: SourceLocal, File: "defaults/greeting_0.webp", Note: "打招呼", Enabled: true},
		{ID: "tsundere1", Scene: "tsundere", Source: SourceLocal, File: "defaults/tsundere_0.webp", Note: "傲娇", Enabled: true},
		{ID: "hungry1", Scene: "hungry", Source: SourceLocal, File: "defaults/hungry_0.webp", Note: "饿了", Enabled: true},
		{ID: "panic1", Scene: "panic", Source: SourceLocal, File: "defaults/panic_0.webp", Note: "慌张", Enabled: true},
	}
}

// extractDefaults 把内置图片解压到 dir/defaults/。
//
// 只在「文件缺失或大小不一致」时写入：既能把升级时换掉的内置图带上，
// 又不会每次都覆写，用户手动替换过的内置文件不会被无谓冲掉。
func extractDefaults(dir string) error {
	if dir == "" {
		return nil
	}
	return fs.WalkDir(defaultsFS, "defaults", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := defaultsFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(p), defaultsPrefix)
		if rel == "" {
			return nil
		}
		dst := filepath.Join(dir, defaultsPrefix, filepath.FromSlash(rel))
		if info, statErr := os.Stat(dst); statErr == nil && info.Size() == int64(len(data)) {
			return nil
		}
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			return mkErr
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// ReadEmbeddedDefault 读取内置副本。
// 本地解压失败时（例如只读文件系统）作为兜底数据源。
func ReadEmbeddedDefault(rel string) ([]byte, error) {
	clean := sanitizeRelPath(rel)
	if clean == "" {
		return nil, os.ErrNotExist
	}
	return defaultsFS.ReadFile(filepath.ToSlash(clean))
}
