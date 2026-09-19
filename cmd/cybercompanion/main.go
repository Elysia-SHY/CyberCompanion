package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/qq"
	"cybercompanion/internal/web"
)

const Banner = `
========================================================================
   ____      _                  ____                                  _             
  / ___|   _| |__   ___ _ __   / ___|___  _ __ ___  _ __   __ _ _ __ (_) ___  _ __  
 | |  | | | | '_ \ / _ \ '__| | |   / _ \| '_ ` + "`" + ` _ \| '_ \ / _` + "`" + ` | '_ \| |/ _ \| '_ \ 
 | |__| |_| | |_) |  __/ |    | |__| (_) | | | | | | |_) | (_| | | | | | (_) | | | |
  \____\__, |_.__/ \___|_|     \____\___/|_| |_| |_| .__/ \__,_|_| |_|_|\___/|_| |_|
       |___/                                       |_|                              
  >> 边缘硬件与随身 WiFi 多模态 AI 伴侣 · 开源版 v1.0.0
========================================================================
`

func main() {
	configFile := flag.String("config", "config.json", "配置文件路径")
	webPort := flag.Int("port", 0, "嵌入式 Web 控制台端口 (默认使用配置中的端口或 8088)")
	showHardware := flag.Bool("hardware", false, "打印本机详细硬件信息后退出")
	showHardwareJSON := flag.Bool("hardware-json", false, "以 JSON 格式打印本机详细硬件信息后退出")
	flag.Parse()

	// 硬件信息自检模式：任何平台的设备都能用同一条命令看到完整采集结果，
	// 便于插上小主机/随身 WiFi 后确认识别情况，无需先启动机器人和面板。
	// 注意：此分支必须在打印 Banner 之前返回 —— JSON 模式下 Banner 会污染标准输出，
	// 导致 `cybercompanion -hardware-json | jq` 直接解析失败。
	if *showHardware || *showHardwareJSON {
		reportHardware(*showHardwareJSON)
		return
	}

	fmt.Print(Banner)

	// 1. Load or initialize configuration
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("[Fatal] 加载配置文件失败: %v", err)
	}
	qq.AddLog("[Config] 成功加载配置: %s", *configFile)

	if *webPort > 0 {
		cfg.WebPort = *webPort
	}

	// 2. Initialize Hardware Abstraction Layer (HAL)
	driver := hal.InitHAL()
	qq.AddLog("[HAL] 硬件平台适配就绪: %s", driver.Name())
	info := driver.GetInfo()
	qq.AddLog("[HAL] 宿主系统: %s (%s) | 主机名: %s", info.OS, info.Arch, info.Hostname)

	// 3. Start QQ Bot Gateway Loop
	qq.AddLog("[QQ Bot] 正在初始化 QQ 官方机器人网关引擎...")
	qq.StartBotGateway()

	// 4. Handle OS signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		qq.AddLog("[System] 收到退出信号 (%v)，正在安全退出...", sig)
		os.Exit(0)
	}()

	// 5. Start Embedded WebUI Server (Blocks main goroutine)
	port := cfg.WebPort
	if port <= 0 {
		port = 8088
	}
	qq.AddLog("[WebUI] 仪表盘已启动，请用浏览器访问: http://0.0.0.0:%d", port)
	if err := web.StartServer(port); err != nil {
		log.Fatalf("[WebUI] Web 服务启动异常: %v", err)
	}
}

// reportHardware 打印本机详细硬件信息。
// 所有平台（Linux/树莓派/U20/高通卡片机/Windows/macOS/容器）都走同一套输出，
// 每一项都标明"未提供"而不是静默返回假数据。
func reportHardware(asJSON bool) {
	driver := hal.InitHAL()
	// CPU 占用率是差值采样，等一个采样周期才能拿到有效值
	time.Sleep(3500 * time.Millisecond)

	info := driver.GetInfo()

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"driver": driver.Name(),
			"device": info,
		})
		return
	}

	d := info.Details
	fmt.Println("========== 硬件信息 ==========")
	fmt.Printf("平台驱动   : %s\n", driver.Name())
	fmt.Printf("设备标识   : %s\n", info.DeviceType)
	fmt.Printf("主机名     : %s\n", info.Hostname)
	fmt.Printf("系统 / 架构: %s / %s\n", info.OS, info.Arch)
	fmt.Println("---------- CPU ----------")
	fmt.Printf("型号       : %s\n", orNA(d.CPUModel))
	fmt.Printf("逻辑核心   : %s\n", orNAInt(d.CPUCores))
	fmt.Printf("当前主频   : %s\n", mhzOrNA(d.CPUFreqMHz))
	fmt.Printf("占用率     : %.1f%%\n", d.CPUUsage)
	fmt.Printf("平均负载   : %s\n", orNA(d.LoadAvg))
	fmt.Printf("调频策略   : %s\n", orNA(d.CPUGovernor))
	fmt.Println("---------- 内存 ----------")
	if d.MemoryTotalMB > 0 {
		fmt.Printf("物理内存   : %d MB / %d MB (%d%%)\n", d.MemoryUsedMB, d.MemoryTotalMB, d.MemoryPercent)
	} else {
		fmt.Printf("物理内存   : 未提供\n")
	}
	if d.SwapTotalMB > 0 {
		fmt.Printf("交换分区   : %d MB / %d MB\n", d.SwapUsedMB, d.SwapTotalMB)
	} else {
		fmt.Printf("交换分区   : 未提供\n")
	}
	fmt.Println("---------- 存储 ----------")
	if d.DiskTotalGB > 0 {
		fmt.Printf("根分区     : %.1f GB / %.1f GB  (%s)\n", d.DiskUsedGB, d.DiskTotalGB, d.DiskMount)
	} else {
		fmt.Printf("根分区     : 未提供\n")
	}
	fmt.Println("---------- 系统 ----------")
	fmt.Printf("运行时长   : %s\n", orNA(d.SystemUptime))
	fmt.Printf("内核版本   : %s\n", orNA(d.Kernel))
	fmt.Printf("进程数     : %s\n", orNAInt(d.ProcessCount))
	fmt.Printf("协程数     : %d\n", d.GoRoutines)
	fmt.Println("---------- 温度 ----------")
	if len(info.Temperatures) > 0 {
		fmt.Printf("温度       : %s\n", strings.Join(info.Temperatures, " | "))
	} else {
		fmt.Printf("温度       : 本平台未提供（不再返回估算值）\n")
	}
	fmt.Println("---------- 电源 ----------")
	if d.BatteryLevel >= 0 {
		fmt.Printf("电池       : %d%% (%s)\n", d.BatteryLevel, orNA(d.BatteryStatus))
	} else {
		fmt.Printf("电池       : 无电池或不可用\n")
	}
	fmt.Println("---------- 网络 ----------")
	fmt.Printf("网络类型   : %s\n", info.NetworkType)
	fmt.Printf("信号       : %s\n", orNA(info.SignalRSRP))
	fmt.Printf("累计流量   : %s\n", orNA(d.TrafficTotal))
	if len(d.NetworkIPs) > 0 {
		fmt.Printf("本机地址   : %s\n", strings.Join(d.NetworkIPs, ", "))
	} else {
		fmt.Printf("本机地址   : 未检测到\n")
	}
	if len(d.Unavailable) > 0 {
		fmt.Printf("\n注：以下项当前平台未提供 → %s\n", strings.Join(d.Unavailable, ", "))
	}
	fmt.Println("==============================")
}

func orNA(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未提供"
	}
	return s
}

func orNAInt(n int) string {
	if n <= 0 {
		return "未提供"
	}
	return strconv.Itoa(n)
}

func mhzOrNA(n int) string {
	if n <= 0 {
		return "未提供"
	}
	return strconv.Itoa(n) + " MHz"
}
