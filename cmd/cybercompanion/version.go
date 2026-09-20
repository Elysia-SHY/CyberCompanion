package main

import (
	"os"
	"runtime"
	"runtime/debug"
)

// version 由构建时通过 -ldflags "-X main.version=..." 注入。
// 未注入时依次回退到：环境变量 CYBERCOMPANION_VERSION、Go 构建信息、字面量 dev，
// 避免出现"版本号是空的"这种情况。
var (
	version = ""
	commit  = ""
	date    = ""
)

// versionEnvKey 是宿主（Android 壳）通过环境变量告知核心版本号的键。
//
// 为什么需要它：Android 的 APK 里，核心 .so 由 Gradle 构建流程顺带编译，
// 走不到 release.yml 里那套从 tag 推导 VERSION 再 -ldflags 注入的逻辑，
// 于是核心只能自报 dev，面板侧栏就会显示成 vdev。
// 壳进程读取自身的 versionName 传进来，即可与 tag 对齐，且不需要改动 CI 工作流。
const versionEnvKey = "CYBERCOMPANION_VERSION"

// resolveVersion 返回用于展示的版本信息。
// 优先级：ldflags 注入 > 宿主环境变量 > 模块构建信息里的版本 > 兜底 dev。
func resolveVersion() (ver, com, buildDate string) {
	ver, com, buildDate = version, commit, date

	if ver == "" {
		if v := os.Getenv(versionEnvKey); v != "" {
			ver = v
		}
	}

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
