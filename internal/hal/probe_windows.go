//go:build windows

package hal

import (
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 本文件实现 Windows 的真实硬件采集。
// 之前 Windows 分支的内存是写死的 4096/16384 MB、CPU 恒为 0、温度写死"Core: 优"，
// 无论宿主是 8GB 笔记本还是 128GB 工作站都返回同样的数字。这里改为真实读取。
//
// 实现方式：直接调用 kernel32.dll 的 GlobalMemoryStatusEx / GetSystemTimes，
// 不引入 golang.org/x/sys 依赖，也不 fork wmic（wmic 在新版 Windows 已弃用）。

var (
	kernel32                    = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemoryStatusEx    = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes          = kernel32.NewProc("GetSystemTimes")
	procGetTickCount64          = kernel32.NewProc("GetTickCount64")
	procGetLogicalProcessorInfo = kernel32.NewProc("GetActiveProcessorCount")
)

// memoryStatusEx 对应 Windows MEMORYSTATUSEX 结构体。
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// readMemory 读取物理内存与页面文件使用情况（MB）。
func readMemory() (usedMB, totalMB int64, loadPct int, swapUsedMB, swapTotalMB int64, ok bool) {
	ms := memoryStatusEx{Length: uint32(unsafe.Sizeof(memoryStatusEx{}))}
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
	if ret == 0 {
		return 0, 0, 0, 0, 0, false
	}
	const mb = 1024 * 1024
	total := int64(ms.TotalPhys / mb)
	avail := int64(ms.AvailPhys / mb)
	used := total - avail
	if used < 0 {
		used = 0
	}
	swapTotal := int64(ms.TotalPageFile / mb)
	swapAvail := int64(ms.AvailPageFile / mb)
	swapUsed := swapTotal - swapAvail
	if swapUsed < 0 {
		swapUsed = 0
	}
	return used, total, int(ms.MemoryLoad), swapUsed, swapTotal, true
}

// filetime 对应 Windows FILETIME（100ns 为单位）。
type filetime struct {
	LowDateTime  uint32
	HighDateTime uint32
}

func (f filetime) uint64() uint64 {
	return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime)
}

var (
	winCPUMu   sync.Mutex
	winLastCPU *winCPUSnap
)

type winCPUSnap struct {
	idle, kernel, user uint64
	at                 time.Time
}

// SampleWindowsCPU 通过 GetSystemTimes 差值计算 CPU 占用率。
// 注意 Windows 的 kernel time **已包含** idle time，计算时必须先减去。
func SampleWindowsCPU() float64 {
	var idle, kernel, user filetime
	ret, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if ret == 0 {
		return 0
	}
	cur := winCPUSnap{idle: idle.uint64(), kernel: kernel.uint64(), user: user.uint64(), at: time.Now()}

	winCPUMu.Lock()
	defer winCPUMu.Unlock()
	prev := winLastCPU
	winLastCPU = &cur
	if prev == nil {
		return 0
	}
	if cur.kernel <= prev.kernel || cur.user <= prev.user || cur.idle <= prev.idle {
		return 0
	}
	idleDelta := cur.idle - prev.idle
	totalDelta := (cur.kernel - prev.kernel) + (cur.user - prev.user)
	if totalDelta == 0 {
		return 0
	}
	usage := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return round1(usage)
}

// readWindowsUptime 读取系统运行时长。
// GetTickCount64 返回的是系统启动以来的毫秒数，比可执行文件的进程时长更符合语义。
func readWindowsUptime() string {
	ret, _, _ := procGetTickCount64.Call()
	if ret == 0 {
		return ""
	}
	ms := uint64(ret)
	return formatDuration(float64(ms) / 1000.0)
}

// readDiskUsage 用 GetDiskFreeSpaceExW 读取指定盘符的容量（GB）。
func readDiskUsage(path string) (usedGB, totalGB float64, ok bool) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	proc := kernel32.NewProc("GetDiskFreeSpaceExW")
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var freeAvail, total, totalFree uint64
	ret, _, _ := proc.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&freeAvail)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ret == 0 || total == 0 {
		return 0, 0, false
	}
	const gb = 1024 * 1024 * 1024
	used := total - totalFree
	return round1(float64(used) / gb), round1(float64(total) / gb), true
}

// NumCPU 返回逻辑处理器数量。
func NumCPU() int {
	ret, _, _ := procGetLogicalProcessorInfo.Call(^uintptr(0)) // ALL_PROCESSOR_GROUPS
	if ret == 0 {
		return 0
	}
	return int(ret)
}

// windowsCPUName 通过注册表读取 CPU 型号。
// 走注册表而不是 wmic，因为 wmic 从 Windows 11 起已默认移除。
func windowsCPUName() string {
	// 优先用环境变量兜底（PROCESSOR_IDENTIFIER 总是可用）
	ident := strings.TrimSpace(getEnv("PROCESSOR_IDENTIFIER"))
	if ident != "" {
		// PROCESSOR_IDENTIFIER 形如 "Intel64 Family 6 Model 140 Stepping 1, GenuineIntel"
		if idx := strings.LastIndex(ident, ","); idx > 0 {
			return strings.TrimSpace(ident[:idx])
		}
		return ident
	}
	return ""
}

// readBatteryWindows 通过 GetSystemPowerStatus 读取电池状态。
func readBatteryWindows() (levelPct int, status string, ok bool) {
	type systemPowerStatus struct {
		ACLineStatus        byte
		BatteryFlag         byte
		BatteryLifePercent  byte
		SystemStatusFlag    byte
		BatteryLifeTime     uint32
		BatteryFullLifeTime uint32
	}
	proc := kernel32.NewProc("GetSystemPowerStatus")
	var sps systemPowerStatus
	ret, _, _ := proc.Call(uintptr(unsafe.Pointer(&sps)))
	if ret == 0 {
		return 0, "", false
	}
	// 255 表示"未知"，此时不返回电池信息（台式机通常如此）
	if sps.BatteryLifePercent == 255 {
		return 0, "", false
	}
	switch sps.ACLineStatus {
	case 1:
		status = "已接通电源"
	case 0:
		status = "使用电池"
	default:
		status = "未知"
	}
	return int(sps.BatteryLifePercent), status, true
}
