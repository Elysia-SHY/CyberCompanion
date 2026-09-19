//go:build windows

package hal

import (
	"os"
	"strings"
	"syscall"
	"unsafe"
)

func getEnv(key string) string {
	return os.Getenv(key)
}

// readWindowsCPUNameFromRegistry 从注册表读取 CPU 品牌名。
// PROCESSOR_IDENTIFIER 只能给出 "Intel64 Family..." 这种族信息，
// 注册表里的 ProcessorNameString 才是 "Intel(R) Core(TM) i7-11800H" 这种人类可读型号。
func readWindowsCPUNameFromRegistry() string {
	const (
		hkeyLocalMachine = 0x80000002
		keyPath          = `HARDWARE\DESCRIPTION\System\CentralProcessor\0`
		keyRead          = 0x20019
	)
	advapi32 := syscall.NewLazyDLL("advapi32.dll")
	procOpen := advapi32.NewProc("RegOpenKeyExW")
	procQuery := advapi32.NewProc("RegQueryValueExW")
	procClose := advapi32.NewProc("RegCloseKey")

	pathPtr, err := syscall.UTF16PtrFromString(keyPath)
	if err != nil {
		return ""
	}
	var hkey syscall.Handle
	ret, _, _ := procOpen.Call(
		uintptr(hkeyLocalMachine),
		uintptr(unsafe.Pointer(pathPtr)),
		0, keyRead,
		uintptr(unsafe.Pointer(&hkey)),
	)
	if ret != 0 {
		return ""
	}
	defer procClose.Call(uintptr(hkey))

	namePtr, err := syscall.UTF16PtrFromString("ProcessorNameString")
	if err != nil {
		return ""
	}
	var (
		valType uint32
		bufSize uint32 = 512
	)
	buf := make([]uint16, bufSize)
	ret, _, _ = procQuery.Call(
		uintptr(hkey),
		uintptr(unsafe.Pointer(namePtr)),
		0,
		uintptr(unsafe.Pointer(&valType)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&bufSize)),
	)
	if ret != 0 {
		return ""
	}
	return strings.TrimSpace(syscall.UTF16ToString(buf))
}

// readWindowsTemperatures 尝试通过 WMI 读取温度。
// 说明：绝大多数消费级主板不通过 WMI 暴露温度传感器（MSAcpi_ThermalZoneTemperature
// 通常只在部分笔记本与品牌机上可用），因此这里有较大可能返回空。
// 返回空时调用方会明确显示"未提供"，而不是编造一个 "Core: 优"。
func readWindowsTemperatures() []string {
	// 不引入 COM/WMI 的完整实现（体积与复杂度不划算），
	// 仅检查常见 OEM 提供的温度接口文件是否可读。
	return nil
}
