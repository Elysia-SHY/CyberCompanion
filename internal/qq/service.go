package qq

// Service 是 qq 包对外暴露的能力适配器。
//
// web 包只依赖 internal/web 里定义的窄接口 BotService，不直接 import qq；
// main.go 负责把这个适配器注入进去（优化建议书 3.1）。
// 这样 web 包可以脱离 QQ 网关单独测试。
type Service struct{}

// AddLog 写入日志环形缓冲（WebUI 展示用）。
func (Service) AddLog(format string, v ...interface{}) { AddLog(format, v...) }

// GetRecentLogs 返回最近的日志条目。
func (Service) GetRecentLogs() []string { return GetRecentLogs() }

// IsConnected 报告 WebSocket 网关是否在线。
func (Service) IsConnected() bool { return IsWSConnected() }
