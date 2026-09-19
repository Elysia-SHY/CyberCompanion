package main

import (
	"runtime"
	"runtime/debug"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
// 未注入时保留默认值，resolveVersion 会尝试从 Go 构建信息里补一个可读的标识，
// 避免出现"版本号是空的"这种情况。
var (
	version = ""
	commit  = ""
	date    = ""
)

// resolveVersion 返回用于展示的版本信息。
// 优先级：ldflags 注入 > 模块构建信息里的 VCS 修订 > 兜底 dev。
func resolveVersion() (ver, com, buildDate string) {
	ver, com, buildDate = version, commit, date

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if com == "" {
					com = s.Value
				}
			case "vcs.time":
				if buildDate == "" {
					buildDate = s.Value
				}
			}
		}
		// 未通过 ldflags 注入时，用主模块版本；本地构建通常是 (devel)
		if ver == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			ver = info.Main.Version
		}
	}

	if ver == "" {
		ver = "dev"
	}
	// 提交号只展示前 8 位，与 CI 里的 ${GITHUB_SHA::8} 保持一致
	if len(com) > 8 {
		com = com[:8]
	}
	return ver, com, buildDate
}

// buildInfoLine 返回单行版本摘要，供脚本消费（避免多行解析）。
func buildInfoLine() string {
	ver, com, buildDate := resolveVersion()
	s := "cybercompanion " + ver
	if com != "" {
		s += " (" + com + ")"
	}
	s += " " + runtime.GOOS + "/" + runtime.GOARCH
	if buildDate != "" {
		s += " built " + buildDate
	}
	return s
}

// verboseVersion 返回多行版本详情。
func verboseVersion() string {
	ver, com, buildDate := resolveVersion()
	if com == "" {
		com = "unknown"
	}
	if buildDate == "" {
		buildDate = "unknown"
	}
	return "CyberCompanion 边缘硬件 AI 伴侣\n" +
		"  版本    : " + ver + "\n" +
		"  提交    : " + com + "\n" +
		"  构建时间: " + buildDate + "\n" +
		"  运行平台: " + runtime.GOOS + "/" + runtime.GOARCH + "\n" +
		"  Go 版本 : " + runtime.Version()
}

// resolveVersionString 只返回版本号本身，供横幅等场景使用。
func resolveVersionString() string {
	ver, _, _ := resolveVersion()
	return ver
}
