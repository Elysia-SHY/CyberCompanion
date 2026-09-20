package web

import (
	_ "embed"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
)

// ─── 版本号 ───────────────────────────────────────────────────────────────────
//
// 面板侧边栏此前把版本硬编码成 v1.0.0，与实际构建版本长期不一致。
// main.version 由 -ldflags 注入到 main 包，web 包读不到，因此由主程序
// 启动时通过 SetVersion 注入一次。

var (
	versionMu    sync.RWMutex
	buildVersion string
)

// SetVersion 注入构建期版本号（ldflags -X main.version=...）。
func SetVersion(v string) {
	versionMu.Lock()
	defer versionMu.Unlock()
	buildVersion = v
}

// BuildVersion 返回用于展示的版本号。
func BuildVersion() string {
	versionMu.RLock()
	v := buildVersion
	versionMu.RUnlock()
	if v != "" {
		return v
	}
	// 未注入时回退到模块构建信息（例如通过 go install 安装的场景）
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
	}
	return "dev"
}

// ─── 更新日志 ─────────────────────────────────────────────────────────────────

//go:embed static/CHANGELOG.md
var embeddedChangelog string

// changelogText 返回更新日志原文（Markdown）。
//
// 仓库根的 CHANGELOG.md 是唯一真源，由 scripts/sync-changelog.sh 同步一份到
// static/ 供构建期嵌入（Go 的 embed 不能引用包目录之外的文件）。
// 运行时若能在工作目录或可执行文件旁找到 CHANGELOG.md 则优先用磁盘版本，
// 这样源码目录下直接 go run 时永远能看到最新内容，不必先跑同步脚本。
func changelogText() string {
	candidates := []string{"CHANGELOG.md"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "CHANGELOG.md"))
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	return embeddedChangelog
}
