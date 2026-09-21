# 随身 WiFi (U20 / Android) 远程调试与外部 Agent 接入指南

本文档介绍如何将你的随身 WiFi（飞猫 U20 / 展锐 Unisoc / 高通 Android 棒子）打造成面向 AI 智能体（Agent）的远程调试与执行节点，使 **Claude Desktop、Cursor、Antigravity、VSCode Roo Code / Cline、Aider** 等大模型智能体能够直接连接设备进行自动化诊断、日志监控、命令执行与热部署。

---

## 1. 硬件与系统架构概览

- **运行平台**: 飞猫 U20 / 展锐 Unisoc T7510 / 类似 5G/4G 随身 WiFi
- **操作系统**: Android 13（Linux 内核 5.15.123 64-bit ARM / `armv8l`）
- **Root 环境**: MagiskSU (`/system_ext/bin/su` -> `./magisk`)
- **网络拓扑**:
  - 默认网关 / 设备 IP: `192.168.88.1`
  - 连接方式: USB RNDIS 虚拟以太网或连接随身 WiFi 热点
- **核心开放端口**:
  - `5555`: 无线 ADB（Android Debug Bridge，已打通 Root 策略与开机自启）
  - `8088`: CyberCompanion Web 控制台与 REST API
  - `2333`: UFI-TOOLS 底层管理 Web 控制台
  - `3000`: One-API 大模型中转服务

---

## 2. 方式一：标准 MCP 协议接入（推荐）

通过 Model Context Protocol (MCP)，将随身 WiFi 封装为 Agent 可感知的标准工具集。无需在宿主机安装任何额外的 pip 依赖包（纯标准库实现）。

### 2.1 调试 MCP 服务路径
服务文件位于仓库中的：[`tools/ufi_debug_mcp.py`](../tools/ufi_debug_mcp.py)

### 2.2 客户端配置示例

#### Claude Desktop
编辑 `%APPDATA%\Claude\claude_desktop_config.json`：
```json
{
  "mcpServers": {
    "pocket_wifi_debugger": {
      "command": "python",
      "args": [
        "C:\\Users\\Administrator\\.gemini\\antigravity\\scratch\\CyberCompanion\\tools\\ufi_debug_mcp.py"
      ]
    }
  }
}
```

#### Cursor (MCP Settings)
在 Cursor 的 `Settings -> Features -> MCP` 中添加：
- **Name**: `pocket_wifi_debugger`
- **Type**: `command`
- **Command**: `python C:\Users\Administrator\.gemini\antigravity\scratch\CyberCompanion\tools\ufi_debug_mcp.py`

#### Antigravity / OpenHands 等
直接在对应的 MCP 服务配置文件中加入上述 stdio 声明即可。

### 2.3 工具列表与调用定义

| 工具名称 | 输入参数 | 功能描述 |
| :--- | :--- | :--- |
| `ufi_shell` | `command` (string), `as_root` (bool, default true) | 在随身 WiFi 上直接执行任意 Shell 命令（以 Magisk Root 身份运行） |
| `ufi_status` | *无* | 一键抓取系统负载、内存、5G 蜂窝网络射频（RSRP/RSRQ/SINR/Band/运营商）、守护进程状态 |
| `ufi_log` | `source` ("cybercompanion" / "oneapi" / "logcat" / "dmesg"), `lines` (int) | 读取设备日志流，排查应用崩溃或底层射频异常 |
| `ufi_push_file`| `local_path` (string), `remote_path` (string) | 将本地编译产物或修改脚本热推送到随身 WiFi 并赋予执行权限 |
| `ufi_pull_file`| `remote_path` (string), `local_path` (string) | 从随身 WiFi 下载文件/数据库到本地计算机分析 |
| `ufi_restart_service` | `target` ("cybercompanion" / "oneapi" / "system") | 一键重启指定 AI 模块或整机软重启 |

---

## 3. 方式二：无线 ADB 终端直连调试

适用于带有终端执行权限的本地 Agent（如 Cursor Composer、VSCode Roo Code、Cline、Aider 等）。

### 3.1 建立连接
```bash
# 确保本地已安装 platform-tools (adb)
adb connect 192.168.88.1:5555
```

### 3.2 验证 Root 权限
设备上的 ADB 用户（uid 2000）已配置永久 MagiskSU 白名单，调用 `su -c` 即可获取超级管理员权限：
```bash
adb -s 192.168.88.1:5555 shell "su -c 'id'"
# 正确输出: uid=0(root) gid=0(root) groups=0(root) context=u:r:magisk:s0
```

### 3.3 常用调试指令
```bash
# 1. 监控 5G/4G 真实基带与射频参数 (RSRP, RSRQ, SINR, Band)
adb -s 192.168.88.1:5555 shell "dumpsys telephony.registry | grep -E 'mSignalStrength|mCellInfo'"

# 2. 查看 CyberCompanion 实时运行状态与进程资源
adb -s 192.168.88.1:5555 shell "su -c 'ps -ef | grep cybercompanion'"
adb -s 192.168.88.1:5555 shell "su -c 'netstat -tlnp'"

# 3. 实时跟踪机器人运行日志
adb -s 192.168.88.1:5555 shell "su -c 'tail -f -n 50 /data/qq-bot/cybercompanion.log'"

# 4. 热更新二进制文件工作流（无需重启随身 WiFi）
adb -s 192.168.88.1:5555 push build/cybercompanion-linux-arm64 /data/local/tmp/cybercompanion
adb -s 192.168.88.1:5555 shell "su -c 'cp /data/local/tmp/cybercompanion /data/qq-bot/cybercompanion && chmod 755 /data/qq-bot/cybercompanion && pkill -9 -f /data/qq-bot/cybercompanion && sh /data/qq-bot/start.sh'"
```

---

## 4. 方式三：Web API 与控制台调试

随身 WiFi 本机启动了两个 Web 服务，可供 Agent 发起 HTTP 请求进行健康自检或界面调试：

### 4.1 CyberCompanion 控制台（端口 8088）
- **Web 仪表盘**: `http://192.168.88.1:8088`
- **健康检查探针**: `GET http://192.168.88.1:8088/api/health`
- **遥测状态数据**: `GET http://192.168.88.1:8088/api/status`（需 Session Cookie）
- **运行日志查询**: `GET http://192.168.88.1:8088/api/logs`
- **进程重载**: `POST http://192.168.88.1:8088/api/restart`

### 4.2 UFI-TOOLS 宿主控制台（端口 2333）
- **Web 界面**: `http://192.168.88.1:2333`
- **插件支持**: 已内置 CyberCompanion 管理插件与 One-API 管理插件，支持在浏览器中一键查看服务状态、启动/停止进程与查看端口。

---

## 5. 开机持久化与排障自愈

为了防止随身 WiFi 在重启、换电池或休眠后丢失调试通道，已固化以下底层机制：

1. **无线 ADB 开机守护脚本**：
   位于 `/data/adb/service.d/cybercompanion.sh`，在系统完成 `sys.boot_completed` 引导后，自动执行：
   ```sh
   setprop service.adb.tcp.port 5555
   setprop persist.service.adb.tcp.port 5555
   killall adbd 2>/dev/null
   ```
2. **Magisk 权限持久化**：
   Magisk 的 `policies` 数据库已记录：
   ```sql
   UPDATE policies SET policy=2 WHERE uid=2000;
   ```
   重启后执行 `adb shell "su -c ..."` 依然永久保持允许状态，不会出现 `Permission denied`。
