package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
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
========================================================================
`

// printBanner 打印启动横幅。
// 版本号改为运行时注入，避免每次发版都要手改这个字符串常量
// （此前硬编码 v1.0.0，与注入的 main.version 长期不一致）。
func printBanner() {
	fmt.Print(Banner)
	fmt.Printf("  >> 边缘硬件与随身 WiFi 多模态 AI 伴侣 · 开源版 %s\n", resolveVersionString())
	fmt.Println("  >> 使用 -version 查看完整版本信息，-hardware 查看本机硬件详情")
	fmt.Println()
}

func main() {
	configFile := flag.String("config", "config.json", "配置文件路径")
	webPort := flag.Int("port", 0, "嵌入式 Web 控制台端口 (默认使用配置中的端口或 8088)")
	showHardware := flag.Bool("hardware", false, "打印本机详细硬件信息后退出")
	showHardwareJSON := flag.Bool("hardware-json", false, "以 JSON 格式打印本机详细硬件信息后退出")
	showVersion := flag.Bool("version", false, "打印版本信息后退出")
	showVersionShort := flag.Bool("V", false, "打印单行版本信息后退出（便于脚本消费）")
	healthCheck := flag.Bool("health", false, "健康检查模式：探测本机 /healthz 并按结果设置退出码（供 Docker / 编排探针调用）")
	flag.Parse()

	if *healthCheck {
		os.Exit(runHealthCheck(*configFile))
	}

	// 版本查询必须走 os.Exit(0)，不能 return —— 之前 CI 里的冒烟测试
	// 执行 `-version` 时因为该参数未定义而报错，但进程仍以 0 退出，
	// 导致校验形同虚设。现在参数真实存在，且用退出码表达结果。
	if *showVersion {
		fmt.Println(verboseVersion())
		os.Exit(0)
	}
	if *showVersionShort {
		fmt.Println(buildInfoLine())
		os.Exit(0)
	}

	// 硬件信息自检模式：任何平台的设备都能用同一条命令看到完整采集结果，
	// 便于插上小主机/随身 WiFi 后确认识别情况，无需先启动机器人和面板。
	// 注意：此分支必须在打印 Banner 之前返回 —— JSON 模式下 Banner 会污染标准输出，
	// 导致 `cybercompanion -hardware-json | jq` 直接解析失败。
	if *showHardware || *showHardwareJSON {
		reportHardware(*showHardwareJSON)
		return
	}

	printBanner()

	// 让面板侧边栏显示与二进制一致的版本号（此前前端硬编码 v1.0.0）
	web.SetVersion(resolveVersionString())

	// 1. Load or initialize configuration
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		log.Fatalf("[Fatal] 加载配置文件失败: %v", err)
	}
	qq.AddLog("[Config] 成功加载配置: %s", *configFile)

	// 启动体检：把「配错了只会看到一直重连」变成启动时的明确提示
	for _, issue := range cfg.Validate() {
		qq.AddLog("[Config] ⚠️ %s", issue)
	}

	if *webPort > 0 {
		cfg.WebPort = *webPort
	}

	// 2. Initialize Hardware Abstraction Layer (HAL)
	driver := hal.InitHAL()
	qq.AddLog("[HAL] 硬件平台适配就绪: %s", driver.Name())
	info := driver.GetInfo()
	qq.AddLog("[HAL] 宿主系统: %s (%s) | 主机名: %s", info.OS, info.Arch, info.Hostname)

	// 3. root context：所有后台协程都挂在这里，退出时统一取消
	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 会话记忆持久化：进程重启后仍记得之前聊过什么
	qq.StartSessionStore(rootCtx, config.ConfigDir())

	// 4. Start QQ Bot Gateway Loop
	qq.AddLog("[QQ Bot] 正在初始化 QQ 官方机器人网关引擎...")
	qq.StartBotGateway(rootCtx)

	// 5. Start Embedded WebUI Server
	port := cfg.WebPort
	if port <= 0 {
		port = 8088
	}
	_, srv, err := web.NewServer(port, qq.Service{})
	if err != nil {
		log.Fatalf("[WebUI] Web 服务初始化失败: %v", err)
	}
	go func() {
		qq.AddLog("[WebUI] 仪表盘已启动，请用浏览器访问: http://127.0.0.1:%d", port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			qq.AddLog("[WebUI] Web 服务异常退出: %v", err)
		}
	}()

	// 6. 等待退出信号，然后按序优雅关闭。
	//
	// 之前这里直接 os.Exit(0)：所有 defer 被跳过，WebSocket 不发 Close 帧、
	// 会话没有落盘、队列里的消息全部丢失（优化建议书 2.4）。
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	qq.AddLog("[System] 收到退出信号，正在安全退出...")

	// 先停面板：不再接收新请求
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		qq.AddLog("[WebUI] 关闭超时: %v", err)
	}

	// 再停网关：cancel 会触发 WS Close 帧并让重连循环退出
	cancel()

	// 排空发送队列，最后落盘会话
	qq.StopSender()
	qq.StopSessionStore()
	qq.AddLog("[System] 已安全退出")
}

// runHealthCheck 以进程退出码表达服务健康状态，供容器探针使用。
//
// distroless 这类镜像里没有 curl / wget / shell，只能靠程序自带的探针。
// 只读配置文件拿端口，绝不写盘 —— 探针可能每秒被调用一次。
func runHealthCheck(configFile string) int {
	port := readPortFromConfig(configFile)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		fmt.Printf("unhealthy: %v\n", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fmt.Println("ok")
		return 0
	}
	fmt.Printf("degraded: HTTP %d\n", resp.StatusCode)
	return 1
}

// readPortFromConfig 只解析 web_port 一个字段，失败时回退到默认端口。
func readPortFromConfig(path string) int {
	const defaultPort = 8088
	if path == "" {
		return defaultPort
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultPort
	}
	var partial struct {
		WebPort int `json:"web_port"`
	}
	if err := json.Unmarshal(data, &partial); err != nil || partial.WebPort <= 0 {
		return defaultPort
	}
	return partial.WebPort
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
