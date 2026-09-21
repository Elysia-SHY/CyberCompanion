//go:build linux || android

package hal

import (
	"runtime"
	"strconv"
)

// AndroidDriver 通过 Java 壳写下的设备快照采集硬件信息。
//
// 为什么不按 runtime.GOOS 判断：CI 里只有 arm64-v8a 用 GOOS=android 编译，
// armeabi-v7a 与 x86_64 两个 ABI 是用 GOOS=linux 编的（见 .github/workflows/ci.yml）。
// 如果按 GOOS 判断，这两个 ABI 上的安卓设备会被当成桌面 Linux，
// 退回到只读 /proc、/sys 的采集路径。快照是 Java 壳自己写的，
// 「快照在不在」才是「核心是不是跑在安卓 App 里」的可靠判据。
type AndroidDriver struct{}

func init() {
	RegisterDriver(&AndroidDriver{})
}

// Priority 高于所有通用驱动：只要快照在，就以框架层数据为准。
func (d *AndroidDriver) Priority() int { return 100 }

func (d *AndroidDriver) Detect() bool {
	return readAndroidSnapshot() != nil
}

func (d *AndroidDriver) Name() string {
	if s := readAndroidSnapshot(); s != nil {
		if label := s.DeviceLabel(); label != "" {
			return label
		}
	}
	return "Android (" + runtime.GOARCH + ")"
}

// GetInfo 以 Java 快照为主、/proc 与 /sys 直读为辅，采集安卓设备信息。
func (d *AndroidDriver) GetInfo() DeviceInfo {
	snap := readAndroidSnapshot()

	info := DeviceInfo{
		DeviceType:   d.Name(),
		Arch:         runtime.GOARCH,
		OS:           runtime.GOOS,
		Hostname:     getHostname(),
		Uptime:       GetBaseUptime(),
		NetworkType:  "网络状态未知",
		SignalRSRP:   "未提供",
		Temperatures: []string{},
	}
	if snap != nil {
		// runtime.GOOS 只能给出 "android"，面板上「Android 16」显然更有用
		if osLabel := snap.AndroidOSLabel(); osLabel != "" {
			info.OS = osLabel
		}
	}

	// Enrich 里已经优先采用快照，这里负责补齐「只有驱动层才掌握」的展示字段
	info.Enrich()

	if snap == nil {
		return info
	}

	if label := snap.NetworkLabel(); label != "" {
		info.NetworkType = label
	}
	if snap.SignalDBm < 0 {
		info.SignalRSRP = strconv.Itoa(snap.SignalDBm) + " dBm"
		info.SignalBar = clampSignalBar(snap.SignalBars)
	} else if cell := ProbeCellular(); cell != nil && cell.SignalRSRP != "" {
		info.SignalRSRP = cell.SignalRSRP
		info.SignalBar = cell.SignalBar
		if cell.SignalDetail != "" {
			info.Details.SignalDetail = cell.SignalDetail
		}
		if cell.Band != "" {
			info.Details.CellularBand = cell.Band
		}
		if cell.Operator != "" {
			info.Details.CellularOperator = cell.Operator
		}
		if info.NetworkType == "" || info.NetworkType == "网络状态未知" {
			info.NetworkType = cell.NetworkType
		}
	} else if info.NetworkType != "" && info.NetworkType != "网络状态未知" {
		// 连接是确定存在的（有连接类型），只是 ROM 没把强度暴露给应用
		info.SignalRSRP = "已连网（信号强度未暴露）"
		info.SignalBar = 3
	}

	// 安卓上 os.Hostname() 恒为 "localhost"，对用户毫无信息量，
	// 这里换成机型，面板「主机名」一栏才有意义。
	if snap.DeviceType != "" {
		info.Hostname = snap.DeviceType
	}

	return info
}

func (d *AndroidDriver) ExecuteRootCmd(cmd string) (string, error) {
	return RunRootCmdWithShell(cmd, "sh", "-c")
}

func clampSignalBar(n int) int {
	if n < 0 {
		return 0
	}
	if n > 5 {
		return 5
	}
	return n
}
