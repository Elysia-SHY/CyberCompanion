package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

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
	fmt.Print(Banner)

	configFile := flag.String("config", "config.json", "配置文件路径")
	webPort := flag.Int("port", 0, "嵌入式 Web 控制台端口 (默认使用配置中的端口或 8088)")
	flag.Parse()

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
