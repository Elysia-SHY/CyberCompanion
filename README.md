# CyberCompanion

将随身 WiFi、开发板、软路由或个人电脑改造成 24 小时在线的 QQ 机器人助手。单二进制文件部署，内置 Web 管理面板，常驻内存约 20MB。

[![CI](https://github.com/Elysia-SHY/CyberCompanion/actions/workflows/ci.yml/badge.svg)](https://github.com/Elysia-SHY/CyberCompanion/actions/workflows/ci.yml)
[![Release](https://github.com/Elysia-SHY/CyberCompanion/actions/workflows/release.yml/badge.svg)](https://github.com/Elysia-SHY/CyberCompanion/actions/workflows/release.yml)
[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-GPL--3.0-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Linux%20|%20Android%20|%20macOS%20|%20Windows-brightgreen.svg)]()
[![QQ Bot](https://img.shields.io/badge/QQ%20Official-Bot%20API-12B7F5?logo=tencent-qq)](https://q.qq.com)

---

## ⚠️ 安全须知（请先读这一节）

本程序具备**在宿主设备上执行系统命令**与**读取硬件状态**的能力，一旦口令泄露，等同于把设备交给对方。请务必遵守以下四条：

1. **主人认证口令必须由你自己设置**。程序不提供任何内置默认口令，`passcode` 为空时认证功能处于关闭状态，不会有「弱口令被猜中」的风险。
2. **不要暴露 Web 面板到公网**。面板设计为局域网/本机使用，如需外网访问请自行加反向代理并启用 HTTPS。
3. **`enable_exec` 默认关闭**。只有在确实需要远程执行命令时才打开，并配合 `exec_whitelist` 限定允许的命令前缀。
4. **所有账号密码、密钥请放在 `config.json`**，权限设为 `600`，且**不要提交到 Git**（`.gitignore` 已包含）。

---

## 特性

**核心定位**：CyberCompanion = QQ 入口 + AI 人格 + 长期记忆 + Agent 能力 + 设备控制。它跑在你自己的低功耗设备上，而不是一个只会闲聊的聊天机器人。

- **轻量单二进制**：Go 语言编写，前端静态文件打包在可执行文件内部，常驻内存约 20MB，无 Python/Node 等运行环境依赖；数据库用纯 Go 的 SQLite 实现，不依赖 cgo，交叉编译与单文件部署不受影响。
- **长期记忆系统**：区别于「聊天记录」——对话结束后异步提炼出结构化记忆（偏好 / 事实 / 关系 / 技能 / 长期要求 / 经历），下一轮按当前话题检索相关的几条注入提示词。按类别区分衰减速度：一次性事件两周后开始淡忘，长期要求几乎不衰减。支持中文全文检索。
- **能力插件体系**：内置 `device`（真实硬件状态）、`dice`、`recall`（查它记住了你什么）、`forget`、`exec`（三重约束：权限档位 + 开关 + 白名单）。模型通过回复里的 `[能力:名称 参数=值]` 标记自主调用，流式输出下标记会被门控扣住，绝不会以碎片形式泄露给用户。
- **MCP 外部能力接入**：实现了 MCP 客户端的 stdio 传输，可以接入生态里已有的能力服务（文件系统、Home Assistant、数据库……）。外部工具会被包装成本地插件并入同一份能力清单 —— 对模型而言，「查设备电量」和「远端服务提供的查询」没有区别。默认关闭（会启动外部进程），每个服务可单独配置最低权限档位与工具数上限，且不允许顶掉内置能力。
- **多模型 Provider 与路由**：按用途（闲聊 / 复杂推理 / 代码 / 视觉 / 记忆提炼）路由到不同端点与模型。闲聊用便宜的快模型，提炼记忆指向最省的那一个，复杂任务才调用推理模型。熔断按端点隔离。
- **四级权限**：主人（可执行命令）> 管理员（可管理配置）> 可信用户（只读查询）> 访客（仅闲聊），另附细粒度权限点，可对个别人开单条权限而不提升其档位。
- **人格系统**：内置 4 种人格（大肥鱼 / 爱莉希雅 / 雪球 / Jarvis）落库可改，支持 `user` / `group` / `global` 三级作用域 —— 同一个群可以有专属人格，同一个人在不同群也能得到不同的对待。
- **硬件状态抽象 (HAL)**：
  - **飞猫 U20 (Unisoc T7510)**：读取 5G NR 信号、RSRP、基站信息、当日出入流量与核心温度。
  - **高通 410 / 210 (MSM8916/8909)**：适配 OpenStick 与各分支随身 WiFi，读取基带状态与负载。
  - **macOS / Windows / Linux**：读取 CPU 架构、系统负载、内存用量与网络连接。
- **主动消息**：定时提醒、天气播报、日程推送。支持 `daily 08:00` / `hourly` / `every 30m`，带免打扰时段（跨零点正确），深夜不会把人吵醒。
- **内置 Web 管理面板**：默认监听 `8088` 端口。除状态监控、人设切换、密钥修改、实时日志外，新增「用户与记忆」面板：数据概览、模型用量（含失败率与耗时）、权限调整、记忆浏览与增删、能力开关、主动消息管理、调试信息。
- **多模态图片识别**：支持在 QQ 对话中直接发送图片，调用大模型视觉接口返回内容分析。
- **情境表情包发送**：表情以 URL 库 + 图床管理，模型可在回复里写 `[表情:ID]` 自主决定发哪张；命中场景词表时自动补一张作为兜底。
- **流式回复（私聊）**：大模型边生成边发送，长回复不再"发完消息石沉大海"，等待期间还有"💭"即时反馈。群聊仍为一次性发送，避免分段刷屏。
- **上下文按 Token 预算裁剪**：不再按"条数"截断历史。长期信息由记忆系统承担，历史窗口只管"最近聊了什么"。
- **不丢消息的发送队列**：出站消息走单 worker 串行队列，失败按指数退避重试 3 次；超长回复自动按语义边界分段。
- **大模型调用容错**：网络 / 5xx / 限流自动重试（流式同样受保护），按端点熔断；面向用户的提示是"人话"，技术细节只进日志。
- **可观测性**：`/healthz` 健康检查、`/debug/vars` 指标、`CC_DEBUG=1` 时挂载 pprof，以及控制台里的调用统计与调试面板。
- **优雅关闭**：收到 SIGINT/SIGTERM 后按序停止面板与调度器、发送 WebSocket Close 帧、排空发送队列、落盘会话。
- **数据库不可用时可降级运行**：长期记忆、用户档案与统计停用，但聊天照常 —— 一个磁盘写满的设备仍应该能回话。

---

## 架构

```mermaid
flowchart TD
    subgraph Entry ["交互入口"]
        QQEngine["QQ 官方 Bot 引擎 (WebSocket)"]
        WebUI["Web 管理面板 (:8088)"]
    end

    subgraph Core ["CyberCompanion 核心"]
        Agent["Agent 引擎<br/>意图 · 能力编排 · 提示词组装"]
        Memory["长期记忆<br/>提炼 · 检索 · 摘要 · 衰减"]
        Persona["人格系统<br/>user / group / global"]
        Perm["四级权限<br/>owner / admin / trusted / guest"]
        Router["模型路由<br/>用途 → 端点"]
        Plugins["能力插件<br/>device · dice · recall · forget · exec"]
        Sched["主动消息<br/>提醒 · 播报 · 日程"]
        Store[("SQLite<br/>用户 · 消息 · 记忆 · 统计")]
    end

    subgraph Infra ["执行端与外部能力"]
        HAL["硬件抽象层 (HAL)"]
        Providers["模型服务商<br/>DeepSeek / OpenAI / Ollama / 中转网关"]
        Drivers["设备驱动<br/>U20 · 高通 · Darwin · Linux"]
    end

    QQEngine --> Agent
    WebUI --> Store
    Agent --> Memory
    Agent --> Persona
    Agent --> Perm
    Agent --> Plugins
    Agent --> Router
    Sched --> Plugins
    Memory <--> Store
    Perm <--> Store
    Plugins --> HAL
    Router --> Providers
    HAL --> Drivers
    QQEngine <--> User["用户 (QQ 个人 / 群聊)"]
```

---

## 硬件支持

> **所有平台都提供同一套详细硬件信息。** 运行 `cybercompanion -hardware` 可在任何设备上直接查看本机采集结果，
> 无需启动机器人或 Web 面板。拿不到的项会明确标注「未提供」——**不会返回编造的占位数值**。

| 设备分类 | 代表硬件 / 芯片 | 对应架构 | 运行方式 |
| :--- | :--- | :--- | :--- |
| **5G 随身 WiFi** | 飞猫 U20 (Unisoc T7510 / SP9863A) | `linux-arm64` | 后台进程 / 开机脚本 |
| **4G 随身 WiFi** | 高通 410 (MSM8916 / 8909) | `linux-armv7` | OpenStick / Debian 棒 |
| **Mac (Apple Silicon)** | M1 / M2 / M3 / M4 | `darwin-arm64` | 直接运行 |
| **Mac (Intel)** | 2020 之前 Intel Mac | `darwin-amd64` | 直接运行 |
| **开发板 / 软路由** | 树莓派 4/5, x86 软路由 | `linux-arm64` / `linux-amd64` | Systemd / Docker |
| **Android 手机 / 平板** | Android 7.0+ (ARM64 / ARMv7) | `.apk` 安装包 | APK 直接安装运行 (免 Root) |
| **Windows PC** | Windows 10 / 11 (x64) | `windows-amd64` | `cybercompanion.exe` |

### 各平台采集项一览

`✅` 实际采集 ｜ `—` 该平台不提供（界面与 QQ 消息中会跳过，不显示假数据）

| 采集项 | Linux / 随身 WiFi | Android | Windows | macOS |
| :--- | :---: | :---: | :---: | :---: |
| CPU 型号 | ✅ `/proc/cpuinfo` | ✅ `/proc/cpuinfo` | ✅ 注册表 | ✅ `sysctl` |
| 逻辑核心数 | ✅ | ✅ | ✅ | ✅ |
| CPU 占用率 | ✅ `/proc/stat` 差值 | ✅ 同左 | ✅ `GetSystemTimes` 差值 | ✅ 负载估算 |
| 当前主频 | ✅ `cpufreq` | ✅ `cpufreq` | — | — |
| 平均负载 | ✅ `/proc/loadavg` | ✅ 同左 | — | ✅ `vm.loadavg` |
| 物理内存 | ✅ `/proc/meminfo` | ✅ 同左 | ✅ `GlobalMemoryStatusEx` | ✅ `sysctl` + `vm_stat` |
| 交换分区 | ✅ | ✅ | ✅ 页面文件 | ✅ `vm.swapusage` |
| 磁盘容量 | ✅ `statfs` | ✅ 同左 | ✅ `GetDiskFreeSpaceEx` | ✅ `statfs` |
| 系统运行时长 | ✅ `/proc/uptime` | ✅ 同左 | ✅ `GetTickCount64` | ✅ `kern.boottime` |
| 内核版本 | ✅ `/proc/version` | ✅ 同左 | — | — |
| 系统进程数 | ✅ | ✅ | — | — |
| 温度 | ✅ 热区 / `hwmon` | ✅ 同左 | — | — |
| 电池 | ✅ 若有 | ✅ 容量 + 温度 | ✅ 若有 | ✅ 若有 |
| 网络地址 | ✅ | ✅ | ✅ | ✅ |
| 累计流量 | ✅ `/proc/net/dev` | ✅ 同左 | — | — |
| 调频策略 | ✅ | ✅ | — | — |

一键自检（所有平台通用）：

```bash
./cybercompanion -hardware          # 人类可读报告
./cybercompanion -hardware-json     # JSON 输出，便于脚本采集与工单排查
```

机器人对话中发送 `状态` / `/status`，或在 Web 面板首页查看「详细硬件规格」卡片，均可获得同样的完整信息。

---

## 快速开始

### 步骤 0：首次启动会拿到什么

程序首次启动时会自动创建 `config.json`（权限 `600`），**但不会替你生成面板密码**。第一次打开面板时会先让你创建一个：

```
[WebUI] 管理面板已启动: http://127.0.0.1:8088
```

之所以不自动生成：早期版本会在启动时随机生成一串密码写进 `config.json`，在有终端的机器上只是麻烦，而在 Android / 随身 WiFi 这类看不到配置文件的设备上，用户只能面对一个永远答不对的登录框。现在密码由你自己创建，也就只有你自己知道。

> 忘记密码时，删掉 `config.json` 里的 `web_password` 字段（或整个字段留空）后重启，面板会重新进入创建流程。

> **主人认证口令（`passcode`）同样需要你自己填写** —— 它相当于把设备的控制权交出去，随机值没有意义，只有你自己记得住的口令才有用。

### 方式一：Android 手机 / 平板直接安装 (推荐手机用户)

前往 [Releases 页面](https://github.com/Elysia-SHY/CyberCompanion/releases) 下载 `CyberCompanion-Android-*.apk`。

- 安装后直接启动，应用通过前台常驻服务维持后台 24 小时运行。
- 打开应用即可在手机屏幕上直接操作 Web 控制台，无需 Root 权限。
- 首次打开会引导创建面板密码；之后应用会读取本机配置自动登录，不再重复输入。
- 已在系统层面申请忽略电池优化白名单，并以 `JobScheduler` 作为进程被系统回收后的兜底拉活手段。

### 方式二：随身 WiFi 与 Linux 一键安装

```bash
curl -sSL https://raw.githubusercontent.com/Elysia-SHY/CyberCompanion/main/scripts/install.sh | bash
```

脚本会：

1. 自动检测 CPU 架构并下载对应二进制；
2. **下载 `SHA256SUMS.txt` 并校验文件完整性**，校验不通过立即中止（防止下载被劫持）；
3. 生成权限为 `600` 的默认配置；
4. 在系统上配置 systemd（Linux）或开机脚本（随身 WiFi）实现自启。

可用环境变量：

| 变量 | 说明 | 默认 |
| :--- | :--- | :--- |
| `CC_REPO` | 覆盖仓库地址 `owner/repo` | `Elysia-SHY/CyberCompanion` |
| `CC_VERSION` | 安装指定版本号 | `latest` |
| `CC_SKIP_VERIFY` | 设为 `1` 跳过 SHA256 校验（**不推荐**） | 未设置 |

```bash
# 安装指定版本
CC_VERSION=v1.2.2 bash install.sh

# 内网镜像部署
CC_REPO=your-mirror/CyberCompanion bash install.sh
```

### 方式三：Docker 部署

```bash
git clone https://github.com/Elysia-SHY/CyberCompanion.git
cd CyberCompanion

# 推荐：使用 compose 模板（只绑本机回环、丢弃全部 capabilities、只读根文件系统）
mkdir -p data && cp config.example.json data/config.json
docker compose -f scripts/docker-compose.yml up -d

# 或者手动构建运行
docker build -t cybercompanion:latest -f scripts/Dockerfile .
docker run -d --name cybercompanion \
  -p 127.0.0.1:8088:8088 \
  -v cybercompanion-data:/data \
  --cap-drop ALL --security-opt no-new-privileges:true \
  --restart unless-stopped \
  cybercompanion:latest
```

运行镜像基于 distroless（无 shell、无包管理器），以非 root 用户（uid 65532）运行，配置与会话持久化在 `/data` 卷。

注意端口写法：`127.0.0.1:8088:8088` 只对本机开放，直接写 `8088:8088` 等于把管理面板暴露到公网。

健康检查用程序自带探针（distroless 里没有 curl/wget）：

```bash
docker inspect --format '{{.State.Health.Status}}' cybercompanion
# 也可手动执行：docker exec cybercompanion /cybercompanion -health -config /data/config.json
```

### 方式四：手动下载运行 (Mac / PC / 服务器)

前往 [Releases 页面](https://github.com/Elysia-SHY/CyberCompanion/releases) 下载对应架构的可执行文件与 `SHA256SUMS.txt`，先校验再运行：

```bash
# 校验完整性（Linux）
sha256sum -c SHA256SUMS.txt --ignore-missing

# 校验完整性（macOS）
shasum -a 256 -c SHA256SUMS.txt --ignore-missing

# macOS Apple Silicon
chmod +x cybercompanion-darwin-arm64
./cybercompanion-darwin-arm64

# Linux / 飞猫 U20
chmod +x cybercompanion-linux-arm64
./cybercompanion-linux-arm64

# Windows
cybercompanion.exe
```

启动后访问 `http://localhost:8088`（随身 WiFi 连接热点后访问 `http://192.168.88.1:8088`）进入管理面板。

---

## 首次配置流程

1. 打开 Web 面板 `http://<设备IP>:8088`。首次使用会先引导创建面板密码，创建后直接进入面板；之后每次打开输入该密码即可（Android App 会自动登录，无需输入）。
2. 在「参数设置」页填入 QQ 机器人 `AppID`、`AppSecret` 与大模型 API Key。
3. **在「参数设置」页设置主人认证口令**（`passcode`），保存后即刻生效。
4. 在 QQ 中私聊机器人，发送你刚设置的口令，即可完成主人认证，当前账号会被写入 `owners.json`。
5. 面板与 QQ 均可在后续随时修改人设、模型与口令。

> 口令认证带有限流保护：连续输错 5 次会在 10 分钟窗口后锁定该用户 30 分钟。若忘记口令，可直接编辑 `config.json` 的 `passcode` 字段后重启程序。

---

## Web 管理面板

- **状态概览**：查看设备当前网络信号、流量消耗、内存利用率与 QQ 网关连接状态。
- **人设配置**：点击预设卡片切换角色，或在输入框中直接修改 System Prompt。
- **参数设置**：修改 QQ 机器人的 AppID、Secret、模型接口地址与认证口令，保存后即时生效。密钥在界面上以 `Supe••••••••3456` 形式掩码显示。
- **运行日志**：查看 WebSocket 消息收发与大模型调用详情，日志按增量追加刷新。
- **用户与记忆**：数据概览、模型用量（含失败率与平均耗时）、用户权限调整、长期记忆的浏览/检索/增删、能力插件开关、主动消息管理，以及排障用的调试信息（模型路由实际指向哪个端点、熔断器是否在冷却、各表行数）。

### 常用配置字段

基础字段：

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `stream_reply` | `true` | 私聊流式输出开关。群聊恒为非流式；若网关不支持 SSE 会自动回退 |
| `token_budget` | `6000` | 上下文 token 预算，超预算从最早的历史开始丢弃 |
| `max_history_msgs` | `40` | 普通会话的消息条数上限（主人为其 5 倍，下限 200 条） |
| `web_port` | `8088` | 面板监听端口 |
| `enable_exec` | `false` | 远程命令总开关；开启时建议同时配置 `exec_whitelist` |
| `passcode` | 空 | 主人认证口令。留空则主人模式关闭 |

多模型路由（`providers` / `model_routing`）：

| 字段 | 说明 |
| :--- | :--- |
| `providers[].name` | 端点标识，被 `model_routing` 引用 |
| `providers[].url` | 完整的 chat completions 地址（OpenAI 兼容即可） |
| `providers[].models` | 按用途指定模型名，键为 `chat` / `complex` / `code` / `vision` / `extract` |
| `model_routing.chat` | 日常闲聊用的端点，格式 `provider` 或 `provider/model` |
| `model_routing.extract` | **记忆提炼与摘要**用的端点。这是高频后台动作，建议单独指向最便宜的模型 |
| `model_routing.complex` | 复杂推理用（可指向 `deepseek-reasoner` 这类推理模型） |

> 只填了 `oneapi_url` / `oneapi_token` / `model` 三件套而没写 `providers` 时，行为与升级前完全一致 —— 全部调用都走那一个端点。

长期记忆（`memory`）：

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `enabled` | `true` | 记忆系统总开关 |
| `recall_limit` | `6` | 每次注入上下文的记忆条数上限 |
| `extract_enabled` | `true` | 对话结束后是否自动提炼记忆 |
| `extract_batch` | `8` | 累积多少条新对话才触发一次提炼 |
| `max_per_owner` | `300` | 单个对话边界的记忆条数上限，超出后淘汰重要性最低的 |
| `min_importance` | `0.15` | 低于此重要性的记忆不注入上下文 |

群聊策略（`groups`）：

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `enabled` | `true` | 是否在群里工作 |
| `reply_chance` | `20` | 未被 @ 时的回复概率（%）。官方 API 下不会触发，见上文说明 |
| `always_reply_at` | `true` | 被 @ 或叫到名字时必定回复 |
| `isolate_memory` | `true` | 群记忆是否按群隔离。**建议保持开启** |
| `max_replies_per_minute` | `6` | 单群每分钟回复上限，防止刷屏被风控 |

主动消息（`schedule`）：

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `enabled` | `true` | 调度器总开关 |
| `check_interval_sec` | `300` | 轮询间隔（限制在 10 秒 ~ 1 小时） |
| `quiet_hours_start` / `quiet_hours_end` | `22` / `8` | 免打扰时段（跨零点正确）。时段内到期不丢弃，推迟到结束时刻 |

外部能力 MCP（`mcp`）：

| 字段 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `enabled` | `false` | 总开关。**默认关闭**：MCP 会启动外部进程并执行外部代码，须由你明确开启 |
| `servers[].command` / `args` | — | 外部服务的启动命令与参数（stdio 传输） |
| `servers[].prefix` | 服务名 | 工具注册到本地能力清单时的前缀。两个服务都有 `read_file` 是常态 |
| `servers[].min_role` | `trusted` | 调用该服务全部工具所需的最低档位。文件系统类服务建议设为 `admin` |
| `servers[].max_tools` | 24 | 该服务最多注册多少工具。清单要整体塞进提示词，挂太多会吃光上下文 |
| `servers[].env` | — | 追加的环境变量，格式 `KEY=VALUE` |
| `call_timeout_sec` | `45` | 单次工具调用超时 |

接入示例如下（把它接进来后，直接问「Download 目录里有什么」即可）：

```json
"mcp": {
  "enabled": true,
  "servers": [
    {
      "name": "filesystem",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/sdcard/Download"],
      "prefix": "fs",
      "min_role": "admin"
    }
  ]
}
```

外部服务的连接状态与工具清单可以在控制台「用户与记忆 → 调试信息」里看到；接入失败的条目会带上失败原因。

运行期还会生成：`cybercompanion.db`（SQLite：用户、消息流水、长期记忆、人格、权限、插件、调度与调用统计）、`sessions.json`（内存会话的落盘快照，0600）与 `exec_audit.log`（命令审计）。

---

## 交互指令

### 快捷指令

除指令外，这些能力也会被模型自动调用 —— 直接说「手机还有多少电」即可，无需记命令。

| 指令 | 最低权限 | 说明 |
| :--- | :--- | :--- |
| `状态` / `/status` / `info` | 可信用户 | 输出硬件信号、流量、温度、内存与存储信息 |
| `电量` / `/battery` | 可信用户 | 只看电池电量与充电状态 |
| `你记得我什么` / `/memory` | 访客 | 列出机器人记住的关于你的长期记忆 |
| `清除记忆` / `/clear` | 可信用户 | 清空**当前对话范围**的长期记忆（不会动到别处的记忆） |
| `掷骰子` / `/dice` / `roll` | 访客 | 掷骰子、随机数、多选项随机挑选 |
| `人设 <名称>` | 主人 | 切换人格预设 |
| `设定人设 <文本>` | 主人 | 自定义人格 |
| `重启` / `/reboot` | 主人 | 在支持的设备上执行系统软重启 |
| `/exec <命令>` | 主人 | 执行系统命令（需 `enable_exec` 打开，且受 `exec_whitelist` 限制） |

### 权限档位

| 档位 | 能力 |
| :--- | :--- |
| `owner` 主人 | 执行命令、重启设备、改动人格与权限 |
| `admin` 管理员 | 管理配置与记忆，但碰不到系统执行 |
| `trusted` 可信用户 | 只读查询：设备状态、记忆查看 |
| `guest` 访客 | 闲聊与识图 |

### 认证方式

初次使用时向机器人私聊发送**你在面板中设置的 `passcode`**，程序会将该用户提升为 `owner`。权限同时写入配置文件与数据库：两者都写是有意的 —— 配置文件是可手工编辑的入口，数据库是运行时的权威来源。

也可以直接在 Web 面板的「用户与记忆」里调整任何人的档位，无需口令。

> 本项目**不提供内置默认口令**。早期版本的 `复活吧我的爱人！！！elyisa` 已被移除，因为源码公开意味着该口令等同于「任何人都是主人」。如果你是从旧版本升级，程序会在加载配置时自动清空这个已知的公开口令，并要求你重新设置。

### 群聊行为

被 @ 或叫到机器人名字时必定回复，其余按 `groups.reply_chance` 概率回复，并受 `max_replies_per_minute` 限流。

> **现实约束**：QQ 官方 Bot API 只推送「被 @ 的群消息」（`GROUP_AT_MESSAGE_CREATE`），不推送普通群消息，因此 `reply_chance` 在官方协议下不会触发 —— 每一次入站都已经是点名。代码同时处理了 `GROUP_MESSAGE_CREATE` 事件，接第三方适配器（如自建 OneBot 网关）时该配置项才会真正生效。

群记忆默认与私聊、与其他群完全隔离：`isolate_memory` 关闭也不会把不同人的记忆混在一起，只是不再按群分域。

---

## 源码编译

编译需要 Go 1.21 或更高版本：

```bash
git clone https://github.com/Elysia-SHY/CyberCompanion.git
cd CyberCompanion

# 编译当前平台（带版本号注入）
go build -ldflags "-X main.version=v1.1.0 -X main.commit=$(git rev-parse --short HEAD)" \
  -o cybercompanion ./cmd/cybercompanion

# 交叉编译 macOS Apple Silicon
GOOS=darwin GOARCH=arm64 go build -o cybercompanion-darwin-arm64 ./cmd/cybercompanion

# 交叉编译 飞猫 U20
GOOS=linux GOARCH=arm64 go build -o cybercompanion-linux-arm64 ./cmd/cybercompanion

# 交叉编译 高通 410
GOOS=linux GOARCH=arm GOARM=7 go build -o cybercompanion-linux-armv7 ./cmd/cybercompanion

# 交叉编译 Windows
GOOS=windows GOARCH=amd64 go build -o cybercompanion-windows-amd64.exe ./cmd/cybercompanion
```

查看版本：

```bash
./cybercompanion -version    # 多行详情：版本、提交、构建时间、平台、Go 版本
./cybercompanion -V          # 单行摘要，便于脚本 grep

# 示例输出
# cybercompanion v1.2.2 (12d44c08) linux/amd64 built 2026-09-20T01:40:00Z
```

### 硬件信息自检

在新设备上部署前，建议先跑一遍硬件自检，确认平台识别与采集是否正常：

```bash
./cybercompanion -hardware
```

输出示例（x86 Linux 容器）：

```
========== 硬件信息 ==========
平台驱动   : Linux Standard / Raspberry Pi (amd64)
主机名     : edge-node-01
系统 / 架构: linux / amd64
---------- CPU ----------
型号       : AMD EPYC 9K65 192-Core Processor
逻辑核心   : 32
当前主频   : 2800 MHz
占用率     : 3.2%
平均负载   : 2.43 / 2.45 / 2.37
---------- 内存 ----------
物理内存   : 18004 MB / 126277 MB (14%)
---------- 存储 ----------
根分区     : 3.0 GB / 256.0 GB  (/)
---------- 系统 ----------
运行时长   : 2h 29m
内核版本   : 6.6.117-45.11.6.tl4.x86_64
---------- 温度 ----------
温度       : 本平台未提供（不再返回估算值）
---------- 网络 ----------
网络类型   : 有线以太网
本机地址   : eth0 172.24.0.5, docker0 172.17.0.1

注：以下项当前平台未提供 → temperatures
==============================
```

需要在脚本里消费这些数据时，用 JSON 模式：

```bash
./cybercompanion -hardware-json | jq '.device.details.cpu_usage'
```

---

## 持续集成与发布

本项目完全由 GitHub Actions 驱动，本地无需任何构建环境。

### `ci.yml` — 每次推送/PR 自动执行

| Job | 内容 |
| :--- | :--- |
| `lint` | `gofmt` 格式检查、`go mod tidy` 一致性、`go vet`、`govulncheck` 漏洞扫描 |
| `test` | `go test -race -covermode=atomic` 竞态检测 + 覆盖率统计，并上传覆盖率报告 |
| `build` | 6 平台交叉编译矩阵 + 二进制冒烟测试（`-version` 输出校验） |
| `android` | Gradle `assembleDebug` 编译 APK 验证 Android 工程可构建 |

### `release.yml` — 打 tag 自动发版

推送形如 `v1.1.0` 的 tag 即触发：

1. 6 平台并行编译，逐产物输出 SHA256；
2. 构建 APK（若配置了 `ANDROID_KEYSTORE_BASE64` 等 Secrets 则签名 release 包，否则回退 debug 并告警）；
3. 汇总所有产物，生成 `SHA256SUMS.txt` 与一键校验脚本 `verify.sh`；
4. 创建 GitHub Release 并附带完整下载说明。

```bash
git tag v1.1.0
git push origin v1.1.0
```

---

## 许可证

本项目采用 [GPL-3.0](LICENSE) 许可证分发。
