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

	"cybercompanion/internal/agent"
	"cybercompanion/internal/config"
	"cybercompanion/internal/hal"
	"cybercompanion/internal/mcp"
	"cybercompanion/internal/memory"
	"cybercompanion/internal/persona"
	"cybercompanion/internal/plugin"
	"cybercompanion/internal/provider"
	"cybercompanion/internal/qq"
	"cybercompanion/internal/scheduler"
	"cybercompanion/internal/stickers"
	"cybercompanion/internal/store"
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

func init() {
	// 在 Android / 嵌入式 Linux 系统中（如随身 WiFi、开发板），缺少常规 /etc/ssl/certs 证书包，
	// 导致 Go 原生 TLS 无法校验证书。检测常见 CA 证书路径并自动配置 SSL_CERT_FILE / SSL_CERT_DIR。
	if os.Getenv("SSL_CERT_FILE") == "" {
		for _, p := range []string{
			"/data/qq-bot/cacert.pem",
			"/etc/ssl/certs/ca-certificates.crt",
			"/system/etc/security/cacert.pem",
		} {
			if _, err := os.Stat(p); err == nil {
				_ = os.Setenv("SSL_CERT_FILE", p)
				break
			}
		}
	}
	if os.Getenv("SSL_CERT_DIR") == "" {
		if fi, err := os.Stat("/system/etc/security/cacerts"); err == nil && fi.IsDir() {
			_ = os.Setenv("SSL_CERT_DIR", "/system/etc/security/cacerts")
		}
	}
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

	// 3.1 持久层（SQLite）。
	//
	// 数据库并非「必需」：它承载长期记忆、用户档案与统计这些增强能力，
	// 而聊天本身不需要它。因此打开失败只降级不退出 —— 一个磁盘写满的
	// 设备仍应该能回话，而不是彻底不起。
	db, dbErr := store.Open(config.ConfigDir())
	if dbErr != nil {
		qq.AddLog("[Store] ⚠️ 持久层不可用，长期记忆 / 用户档案 / 调用统计已停用: %v", dbErr)
	} else {
		qq.AddLog("[Store] 持久层就绪: %s（结构版本 v%d）", store.Path(), db.SchemaVersion())
		defer func() { _ = store.Close() }()

		// 老配置里的主人列表导入数据库：用户表是新的权威来源，
		// 但绝不能因此把老用户的主人身份弄丢。
		if n, err := db.ImportOwners(cfg.Owners); err != nil {
			qq.AddLog("[Store] ⚠️ 导入主人列表失败: %v", err)
		} else if n > 0 {
			qq.AddLog("[Store] 已从配置导入 %d 位主人到用户表", n)
		}

		// 内置人格落库：只在缺失时写入，不覆盖用户改过的提示词
		if n, err := db.SeedPersonas(builtinPersonaSeeds()); err != nil {
			qq.AddLog("[Persona] ⚠️ 写入内置人格失败: %v", err)
		} else if n > 0 {
			qq.AddLog("[Persona] 已写入 %d 个内置人格", n)
		}
	}

	// 3.2 模型路由（多 Provider）
	var recorder provider.UsageRecorder
	if db != nil {
		// 必须显式判断：把 nil 的 *store.DB 塞进接口会得到一个「非nil接口」，
		// 后续调用会以空指针 panic 收场。
		recorder = db
	}
	providers := provider.NewRegistry(recorder)
	if ep, err := providers.Resolve(provider.PurposeChat); err == nil {
		qq.AddLog("[Provider] 对话模型: %s（%s）", ep.Model, ep.Name)
	} else {
		qq.AddLog("[Provider] ⚠️ %v", err)
	}

	// 3.3 长期记忆系统
	memCfg := memory.Config{
		Enabled:          cfg.Memory.MemoryEnabled(),
		RecallLimit:      cfg.Memory.RecallLimitOr(),
		ExtractEnabled:   cfg.Memory.MemoryExtractEnabled(),
		ExtractBatch:     cfg.Memory.ExtractBatchOr(),
		SummaryThreshold: cfg.Memory.SummaryThresholdOr(),
		MaxPerOwner:      cfg.Memory.MaxPerOwnerOr(),
		MinImportance:    cfg.Memory.MinImportanceOr(),
	}
	memMgr := memory.NewManager(db, memCfg, provider.NewExtractor(providers))
	qq.SetMemory(memMgr)
	if memMgr.Available() {
		total, owners, _ := memMgr.Stats()
		qq.AddLog("[Memory] 记忆系统就绪：已有 %d 条记忆，覆盖 %d 个对话边界", total, owners)
	} else {
		qq.AddLog("[Memory] 记忆系统未启用（可在配置的 memory.enabled 打开）")
	}

	// 3.4 能力插件
	plugins := plugin.NewRegistry(db)
	plugins.Register(plugin.NewDicePlugin())
	plugins.Register(plugin.DevicePlugin{})
	plugins.Register(plugin.NewExecPlugin())
	plugins.Register(plugin.NewRecallPlugin(memMgr))
	plugins.Register(plugin.NewForgetPlugin(memMgr))
	qq.AddLog("[Plugin] 已注册能力: %v", plugins.SortedNames())

	// 3.5 外部 MCP 能力（优化建议书第七节）
	//
	// 接入发生在内置插件注册之后：这样外部服务若与内置能力重名，
	// 会被识别为冲突而跳过，而不是悄悄顶掉 device 或 exec。
	mcpMgr := mcp.NewManager(qq.AddLog)
	qq.SetMCPSource(mcpMgr.Status)
	defer func() { _ = mcpMgr.Close() }()

	mcpMgr.StartAll(rootCtx, cfg.MCP, plugins)
	if servers, tools := mcpMgr.Count(); servers > 0 {
		qq.AddLog("[MCP] 外部能力接入完成：%d 个服务 / %d 个工具，已并入统一能力清单", servers, tools)
	}

	// 3.6 对话引擎（记忆 + 权限 + 能力 + 模型的编排）
	engine := agent.NewEngine(providers, plugins, memMgr)
	qq.SetEngine(engine)

	// 3.7 会话记忆持久化：进程重启后仍记得之前聊过什么
	qq.StartSessionStore(rootCtx, config.ConfigDir())

	// 3.8 主动消息调度（定时提醒 / 天气播报 / 日程推送）
	sched := scheduler.New(db, qq.SchedulerNotifier{})
	sched.Register(scheduler.NewModelGenerator(providers))
	sched.Register(scheduler.NewPluginGenerator(qq.PluginCallerForScheduler{}))
	if sched.Available() && cfg.Schedule.ScheduleEnabled() {
		sched.Start(rootCtx)
		if tasks, err := db.ListSchedules(""); err == nil {
			qq.AddLog("[Schedule] 主动消息调度就绪：%d 个任务，轮询间隔 %d 秒", len(tasks), cfg.Schedule.CheckIntervalOr())
		}
		if start, end, ok := cfg.Schedule.QuietHours(); ok {
			qq.AddLog("[Schedule] 免打扰时段：%02d:00 - %02d:00", start, end)
		}
	}

	// 3.9 记忆与数据的周期性维护
	startMaintenance(rootCtx, memMgr, db)

	// 3.10 初始化表情库（图床 + 本地表情）。失败不致命：机器人照常运行，
	// 只是表情功能暂时不可用。首次运行会把内置表情解压到配置目录。
	if err := stickers.Load(config.ConfigDir()); err != nil {
		qq.AddLog("[Stickers] ⚠️ 表情库初始化失败，表情功能暂不可用: %v", err)
	}

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

	// 停主动消息调度：避免关闭过程中又推出一条消息
	sched.Stop()

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

// builtinPersonaSeeds 把源码里的内置人格预设转成可落库的种子数据。
//
// 预设仍写在 persona 包里（它们随版本迭代，属于代码而非用户数据），
// 落库之后用户就能在它的基础上新增、修改出「某个群专用」「某个人专用」的人格。
func builtinPersonaSeeds() []store.SeedPersona {
	presets := persona.GetAllPresets()
	out := make([]store.SeedPersona, 0, len(presets))
	for _, p := range presets {
		out = append(out, store.SeedPersona{
			ID:          p.ID,
			Name:        p.Name,
			Title:       p.Title,
			Description: p.Description,
			Prompt:      p.Prompt,
			AvatarStyle: p.AvatarStyle,
		})
	}
	return out
}

// maintenanceInterval 是数据维护的周期。
//
// 取 6 小时而不是更短：维护动作都要全表扫描，而低功耗设备的 IO 与电量
// 都是稀缺资源；这些数据的时效性要求也远没有那么高。
const maintenanceInterval = 6 * time.Hour

// startMaintenance 启动周期性数据维护。
//
// 三件事，都是「不做也不会立刻出问题、但长期必然出问题」的那一类：
//   - 记忆衰减：让陈年旧事自然退场，避免记忆库只增不减、检索里全是垃圾
//   - 消息流水裁剪：原始对话只是提炼记忆的原料，提炼过就不必长期保留
//   - 过期访客清理：只清「没有任何权限」的访客，避免误删被授权的账号
func startMaintenance(ctx context.Context, mem *memory.Manager, db *store.DB) {
	if db == nil {
		return
	}

	go func() {
		ticker := time.NewTicker(maintenanceInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			if mem != nil {
				if decayed, pruned, err := mem.Maintain(time.Now()); err != nil {
					qq.AddLog("[Memory] 记忆维护失败: %v", err)
				} else if decayed > 0 || pruned > 0 {
					qq.AddLog("[Memory] 记忆维护完成：衰减 %d 条，淘汰 %d 条", decayed, pruned)
				}
			}

			if n, err := db.PruneMessages(500); err != nil {
				qq.AddLog("[Store] 消息流水裁剪失败: %v", err)
			} else if n > 0 {
				qq.AddLog("[Store] 已裁剪 %d 条过期消息流水", n)
			}

			// 30 天未活跃且没有任何权限的访客档案可以安全清理
			if n, err := db.PurgeInactiveUsers(time.Now().AddDate(0, 0, -30)); err != nil {
				qq.AddLog("[Store] 过期用户清理失败: %v", err)
			} else if n > 0 {
				qq.AddLog("[Store] 已清理 %d 个过期访客档案", n)
			}

			// 调用统计保留 90 天：再久的数据对面板趋势图没有意义
			if n, err := db.PruneUsage(time.Now().AddDate(0, 0, -90)); err != nil {
				qq.AddLog("[Store] 统计清理失败: %v", err)
			} else if n > 0 {
				qq.AddLog("[Store] 已清理 %d 条过期调用统计", n)
			}
		}
	}()
}
