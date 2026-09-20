package plugin

import (
	"context"
	"fmt"
	"strings"

	"cybercompanion/internal/hal"
	"cybercompanion/internal/store"
)

// DevicePlugin 暴露宿主设备的硬件状态（优化建议书第十三节）。
//
// 这是本项目相对通用聊天机器人的差异化能力：它跑在设备本体上，
// 「手机还剩多少电」这个问题只有它能真实回答。
type DevicePlugin struct{}

func (DevicePlugin) Name() string { return "device" }

func (DevicePlugin) Description() string {
	return "查询运行本机器人的设备的真实硬件状态：电量、温度、CPU、内存、存储、网络与信号"
}

func (DevicePlugin) Schema() Schema {
	return Schema{
		Params: []Param{
			{Name: "item", Description: "查询项：status(概览)/battery/cpu/memory/disk/network/temp", Required: false},
		},
		Examples: []string{"手机还有多少电", "设备状态怎么样", "CPU 占用多少", "内存还剩多少"},
	}
}

func (DevicePlugin) MinRole() store.Role { return store.RoleTrusted }
func (DevicePlugin) Permission() string  { return store.PermDeviceRead }

func (DevicePlugin) Execute(ctx context.Context, args map[string]string, ec *ExecContext) Result {
	driver := hal.GetDriver()
	if driver == nil {
		return Result{Error: fmt.Errorf("硬件抽象层未初始化")}
	}
	info := driver.GetInfo()

	item := strings.ToLower(strings.TrimSpace(args["item"]))
	if item == "" {
		item = "status"
	}

	var b strings.Builder
	switch item {
	case "battery":
		b.WriteString(batteryLine(info))
	case "cpu":
		b.WriteString(cpuLines(info))
	case "memory", "mem":
		b.WriteString(memoryLine(info))
	case "disk", "storage":
		b.WriteString(diskLine(info))
	case "network", "net":
		b.WriteString(networkLines(info))
	case "temp", "temperature":
		b.WriteString(temperatureLines(info))
	default:
		b.WriteString(statusSummary(info))
	}

	text := strings.TrimSpace(b.String())
	if text == "" {
		text = "这个设备暂时读不到该项数据。"
	}

	// Handled=true 是关键：设备数据已经格式化完毕，不需要也没必要
	// 再交给模型复述一遍 —— 那既浪费 token，又给了它添油加醋的机会。
	return Result{Text: text, Handled: true}
}

// statusSummary 生成概览。
func statusSummary(info hal.DeviceInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📟 %s\n", fallback(info.DeviceType, "设备"))
	fmt.Fprintf(&b, "运行时长：%s\n", fallback(info.Uptime, "未知"))
	b.WriteString(batteryLine(info))
	b.WriteString(memoryLine(info))
	b.WriteString(cpuLines(info))
	b.WriteString(diskLine(info))
	b.WriteString(networkLines(info))
	b.WriteString(temperatureLines(info))
	return b.String()
}

// batteryLine 生成电量行。
//
// 设备没电池时（软路由、NAS）返回一句明确说明，而不是编一个 100%
// —— 这正是本项目一直坚持的口径：读不到就说读不到。
func batteryLine(info hal.DeviceInfo) string {
	d := info.Details
	if d.BatteryLevel < 0 {
		return "🔌 电源：无电池（市电供电设备）\n"
	}
	status := d.BatteryStatus
	if status == "" {
		status = "未知"
	}
	return fmt.Sprintf("🔋 电量：%d%%（%s）\n", d.BatteryLevel, status)
}

func cpuLines(info hal.DeviceInfo) string {
	d := info.Details
	var b strings.Builder
	fmt.Fprintf(&b, "🧠 处理器：%s\n", fallback(d.CPUModel, "未提供"))
	if d.CPUCores > 0 {
		fmt.Fprintf(&b, "核心数：%d\n", d.CPUCores)
	}
	// CPUUsage 为负表示「读不到」，与「真的是 0%」必须区分开
	if d.CPUUsage >= 0 {
		fmt.Fprintf(&b, "占用率：%.1f%%\n", d.CPUUsage)
	} else {
		b.WriteString("占用率：未提供\n")
	}
	if d.LoadAvg != "" {
		fmt.Fprintf(&b, "平均负载：%s\n", d.LoadAvg)
	}
	return b.String()
}

func memoryLine(info hal.DeviceInfo) string {
	d := info.Details
	if d.MemoryTotalMB <= 0 {
		return "💾 内存：未提供\n"
	}
	return fmt.Sprintf("💾 内存：%d MB / %d MB（%d%%）\n",
		d.MemoryUsedMB, d.MemoryTotalMB, d.MemoryPercent)
}

func diskLine(info hal.DeviceInfo) string {
	d := info.Details
	if d.DiskTotalGB <= 0 {
		return "🗄 存储：未提供\n"
	}
	return fmt.Sprintf("🗄 存储：%.1f GB / %.1f GB（%s）\n",
		d.DiskUsedGB, d.DiskTotalGB, fallback(d.DiskMount, "根分区"))
}

func networkLines(info hal.DeviceInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📶 网络：%s\n", fallback(info.NetworkType, "未知"))
	if info.SignalRSRP != "" && info.SignalRSRP != "N/A" {
		fmt.Fprintf(&b, "信号强度：%s\n", info.SignalRSRP)
	}
	if info.Details.TrafficTotal != "" {
		fmt.Fprintf(&b, "累计流量：%s\n", info.Details.TrafficTotal)
	}
	if len(info.Details.NetworkIPs) > 0 {
		fmt.Fprintf(&b, "本机地址：%s\n", strings.Join(info.Details.NetworkIPs, ", "))
	}
	return b.String()
}

func temperatureLines(info hal.DeviceInfo) string {
	if len(info.Temperatures) == 0 {
		return ""
	}
	return fmt.Sprintf("🌡 温度：%s\n", strings.Join(info.Temperatures, " | "))
}

// fallback 在字符串为空时返回默认值。
func fallback(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
