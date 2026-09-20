# 更新日志

本项目遵循「能用 → 可靠 → 好用 → 可维护」的演进顺序。以下为优化建议书落地后的工程化改造记录。

## v1.2.2 - 2026-09-20

本版把表情包从「硬编码在源码里的 base64 大图」升级为「URL 库 + 图床 + WebUI 管理」，让大模型能自主发表情，也让关键词触发的本地表情改走图床直链。

### 新增

- **表情包改接图床，大模型自己发**：旧实现把 5 张几百 KB 到 1.5MB 的图以 base64 硬编码进 `internal/stickers/stickers.go`（整个文件 801KB，编译慢、改不动）。现在表情改为 URL 库，每条目可以是本地文件或图床直链；系统提示词注入可用表情清单，模型可在回复里写 `[表情:ID]` 标记自主决定发哪张，流式输出时增量过滤器会扣住未写完的标记，绝不把半截标记当正文发出去。
- **关键词触发兜底**：命中场景词表（喜欢你、饿了、傲娇等）时自动补一张表情，保留旧行为，作为模型没表态时的兜底。两条路径按配置独立开关。
- **WebUI 表情包管理页**：新增「表情包与图床」面板，可配置发表情规则（来源优先级、智能发、关键词发、单条上限、CDN 前缀）、图床（上传地址、方式、鉴权、结果路径、公共前缀，含一键连通测试），以及表情库的上传、直链新增、增删改查与缩略图预览。
- **两步式发送（QQ 官方推荐路径）**：表情发送由旧的一步到位（占用主动消息频率、文件不能复用）改为先 `POST /files`（srv_send_msg=false）拿 `file_info`，再 `msg_type 7` 被动回复；同一张图在 TTL 内复用 `file_info`，对随身 WiFi 的流量是实打实的节省。两步式失败才退回一步式兜底。
- **内置表情改为图片文件随仓库分发**：5 张内置表情导出为 `internal/stickers/defaults/` 下的真实图片，首次运行解压到配置目录，开箱即有可用表情，不再依赖源码里的 base64。

### 工程

- 新增 `internal/imagehost`：可配置的通用图床客户端（multipart、预签名 PUT、Bearer、自定义头、表单鉴权、点分路径或正则兜底提取 URL、公共前缀补全），覆盖兰空、Chevereto、EasyImage、SM.MS、S3 等主流图床。
- `stickers.Load` 在启动时初始化表情库（此前从未被调用，新库一直处于未加载状态）。

## v1.2.1 - 2026-09-20

本版专治「安卓端硬件面板大部分都读不到」。

根因是 Go 核心在 Android 上以应用沙箱子进程的身份运行，Android 10 之后 SELinux 把 `/proc/net/*`、`/proc/loadavg`、`/proc/uptime`、`/sys/class/net`、`/sys/class/thermal`、`/sys/class/power_supply` 这些路径对普通应用全部关掉了，于是面板上大半栏目只能显示「未提供」，连设备类型都会退化成兜底驱动的 `Universal Hardware Profile`。

### 修复

- **安卓硬件采集改由框架层供给**：新增 `DeviceProbe`，用 `Build` / `ActivityManager.MemoryInfo` / `StatFs` / `BatteryManager` / `PowerManager` / `ConnectivityManager` / `TelephonyManager` / `TrafficStats` / `SystemClock` 采集设备型号、SoC、核心数、内存、存储、开机时长、内核、电池、散热、网络类型、运营商、信号强度、本机地址与累计流量，写成 JSON 快照交给核心子进程（环境变量 `CYBERCOMPANION_DEVICE_INFO`）。核心侧新增快照解析层，采集时优先采用快照值，快照里没有的再退回 `/proc`、`/sys` 直读。
- **驱动选择由「先到先得」改为「优先级最高者胜」**：`GenericDriver.Detect()` 恒为真，而 `hal_generic.go` 按文件名顺序又排在 `hal_linux.go` 之前，导致 Linux、树莓派、安卓设备全被兜底驱动截胡，面板上永远显示 `Universal Hardware Profile`。现在各驱动带优先级，`AndroidDriver` 以「快照是否存在」判定，因为 CI 里 armeabi-v7a 与 x86_64 两个 ABI 是用 `GOOS=linux` 编的，按 `runtime.GOOS` 判断会漏掉它们。
- **去掉「树莓派」误标**：`LinuxDriver.Name()` 此前在所有 Linux 系设备上一律返回 `Linux Standard / Raspberry Pi (arch)`，安卓设备也被冠上树莓派的名字。现在按实际平台区分，拿不到快照时如实标注。
- **CPU 占用率区分「确实是 0%」与「读不到」**：`SampleCPU()` 在采集失败、缺少采样基线、计数器回绕这三种情况下由返回 `0` 改为返回 `-1`，面板显示「未提供」，不再拿一个看着像真读数的 `0.0%` 充数。
- **安卓内存与存储改用正确口径**：内存不再依赖 `/proc/meminfo` 里内核估算的 `MemAvailable`，改用 `ActivityManager.MemoryInfo`，与系统设置里看到的数字对得上；存储不再读只读的 system 分区，改看数据分区。
- **安卓进程数不再显示假数据**：沙箱里的 `/proc` 只暴露本应用自己的进程，直接数出来是个没有意义的 1~2。现在这种情况按「未提供」上报。
- **缺失项清单改中文**：`unavailable` 从 `cpu_model`、`memory` 这类内部字段名改为「处理器型号」「内存」等面板文案，用户可以直接读。

### Android

- **新增 `READ_PHONE_STATE` 运行时权限**：用于读取蜂窝制式（5G NR / 4G LTE）与信号强度 dBm。这些字段被安卓列为受保护项，没有权限只能显示「已连网」。用户拒绝不影响主流程，面板上会明确写出原因。
- **首次启动只弹一轮权限对话框**：通知权限与电话权限合并为一次申请。
- **快照定时刷新**：电池、流量、网络类型会随时间变化，服务存活期间每 15 秒重写一次快照；服务销毁时停掉刷新线程。

### 说明

- 非 root 的 Android 上，普通应用仍然拿不到 SoC 温度、平均负载、调频策略与当前主频（`/proc/loadavg` 与 cpufreq 目录被 SELinux 关闭）。这些项会如实列进面板的「本平台未提供」，不再回退成看起来像真的假值。

## v1.2.0 - 2026-09-20

本版合并了两批工作：按《优化建议书》落地的工程化改造（阶段 1/2/3），以及首次使用体验的修正。最直观的变化是 Android 端不再打开就要密码、面板内可以直接看更新日志。

### 体验

- **首次引导创建密码**：面板密码不再于启动时自动生成随机串写进 `config.json`。此前在 Android / 随身 WiFi 这类没有终端的设备上，用户打不开配置文件，打开面板只会面对一个永远答不对的登录框。现在首次打开面板会引导创建密码，设置完成后直接进入面板；未设置密码期间所有 API 一律拒绝访问，不存在无鉴权窗口。
- **面板内更新日志**：顶栏新增「更新日志」入口，内容取自仓库 `CHANGELOG.md`；侧边栏版本号改为后端动态注入，不再硬编码 `v1.0.0`。

### Android

- **本机自动登录**：App 直接读取自身私有目录中的配置，用已保存的密码换取会话并注入 WebView，之后打开 App 直接进入面板，无需每次输入密码。密码尚未创建时跳过该流程，由面板显示创建引导。
- **版本号透传**：APK 内置核心由 Gradle 顺带编译，拿不到发行流程里从 tag 注入的 `-X main.version`，此前会自报 `dev`。现在壳进程把自身 `versionName` 通过 `CYBERCOMPANION_VERSION` 传给核心，面板侧栏显示的版本号与发行版本一致。

### 修复

- **Android 编译失败**：核心进程崩溃重启逻辑抽出 `runGoProcess()`（返回 int）后，「找不到二进制文件」分支残留裸 `return;`，`javac` 直接报 `incompatible types: missing return value`，Android 构建检查挂掉。该分支属持久性错误，重试无意义，改为返回 0 交由外层正常结束。

### 体验（本轮首批）

- **流式回复**：`llm.CallLLMStream` 以 SSE 增量接收，私聊按「1.5 秒 / 80 字」节流分段追加；服务端忽略 `stream` 参数时自动回退为非流式。新增配置项 `stream_reply`（默认开启）。
- **首字反馈**：调用大模型前在私聊发送一个 `💭`，消除"发完消息石沉大海"的等待感。
- **超长回复分段**：`SendTextSegmented` 复用既有的 `SplitMessage`（此前该函数只有测试、没有接线），按自然边界切分且总段数受 `maxSegments` 限制。
- **上下文按 token 预算裁剪**：`executeLLMChat` 接入 `trimHistoryToBudget`，替代"一刀切 40 条"。
- **图片本地转存**：附件按 `Content-Type` 分类处理，图片下载后校验类型与大小（上限 5MB）再 Base64 内联；语音/视频给出明确提示而不是静默失败。下载走独立的安全客户端（完整 TLS 校验）。

### 可靠性

- **出站发送队列**（`internal/qq/sender.go`）：单 worker 串行发送，失败按指数退避重试 3 次；队列满时丢弃新消息而不是阻塞接收循环。
- **发送状态码校验**：此前 `SendTextMessage` / `SendStickerMedia` 忽略 HTTP 状态码，403/429/500 全被当作成功。
- **LLM 重试与熔断**（`internal/llm/retry.go`）：错误按类别判定可重试性（4xx 不重试），退避上限 6 秒；连续失败 5 次熔断 60 秒。
- **面向用户的错误文案**：`llm.UserFacingMessage` 按错误类别给出人话提示，技术细节只进日志，避免泄露后端信息。
- **优雅关闭**：`main.go` 用 root context + 信号处理，退出时按序关闭面板、发送 WebSocket Close 帧、排空发送队列、落盘会话；不再 `os.Exit(0)`。
- **网关重连退避**：连续失败时重连间隔从 10 秒逐步拉长到 5 分钟，成功连接后重置。
- **会话持久化**：`internal/qq/session.go` 每 60 秒增量落盘 `sessions.json`（原子写、0600），启动时恢复未过期会话；会话总数封顶 500，超出淘汰最久未互动的。
- **序号计数器回收**：`seqCounters` 改为带时间戳的结构并定期 GC，修复 key 无界增长。

### 架构与质量

- **web 与 qq 解耦**：web 包定义 `BotService` 窄接口，由 `qq.Service` 适配、`main.go` 注入；web 包自此可脱离 QQ 网关单独测试。
- **健康检查与指标**：新增 `/healthz`（网关断线或 LLM 熔断时返回 503）、`/debug/vars`（expvar 计数）、`CC_DEBUG=1` 时挂载 pprof。
- **新增测试**：发送队列重试与丢弃、会话持久化与淘汰、错误分类与熔断、流式拼接与非流式回退、`/healthz` 状态、未登录访问受保护端点、表情场景识别。修复了 3 个在 Windows 上因缺少 POSIX 权限位而误报的测试用例。
- **Android 保活**：申请电池优化白名单；Go 核心进程崩溃后带退避自动重启（最多 5 次，稳定运行 60 秒后清零计数）。

### 部署

- **systemd 加固**：专用非特权用户、`ProtectSystem=strict` + `ReadWritePaths`、内核/设备防护、`StartLimitBurst`、内存 256M / CPU 70% 上限。
- **镜像瘦身与加固**：运行阶段改为 distroless nonroot，健康检查改用程序自带 `-health` 探针；新增 `scripts/docker-compose.yml`（只绑 127.0.0.1、`cap_drop: ALL`、只读根文件系统）。
- **新增 `-health` 参数**：探测本机 `/healthz` 并以退出码表达结果，供容器与编排探针使用（不写盘）。
