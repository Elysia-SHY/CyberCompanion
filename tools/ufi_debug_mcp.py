#!/usr/bin/env python3
"""
Pocket WiFi (UFI / U20) Debugger MCP Server
Provides standard Model Context Protocol (MCP) tools for AI agents
(Claude Desktop, Cursor, Antigravity, OpenHands, etc.) to debug the pocket WiFi.
"""

import sys
import json
import subprocess
import shutil

ADB_BIN = shutil.which("adb") or "adb"
DEVICE_ADDR = "192.168.88.1:5555"

def ensure_adb_connected():
    """Ensure wireless ADB is connected."""
    try:
        res = subprocess.run(
            [ADB_BIN, "connect", DEVICE_ADDR],
            capture_output=True, text=True, timeout=5
        )
        return "connected" in res.stdout.lower() or "already connected" in res.stdout.lower()
    except Exception as e:
        return False

def adb_shell(cmd: str, as_root: bool = True) -> str:
    """Execute command on the pocket WiFi via ADB."""
    ensure_adb_connected()
    if as_root:
        # Wrap with Magisk su
        escaped = cmd.replace("'", "'\\''")
        full_cmd = [ADB_BIN, "-s", DEVICE_ADDR, "shell", f"su -c '{escaped}'"]
    else:
        full_cmd = [ADB_BIN, "-s", DEVICE_ADDR, "shell", cmd]

    try:
        proc = subprocess.run(
            full_cmd,
            capture_output=True, text=True, timeout=30
        )
        output = proc.stdout
        if proc.stderr:
            output += ("\n[STDERR]\n" + proc.stderr if output else proc.stderr)
        return output if output else "(command finished with empty output)"
    except subprocess.TimeoutExpired:
        return "[ERROR] Command timed out after 30 seconds"
    except Exception as e:
        return f"[ERROR] Failed to execute ADB command: {e}"

TOOLS = [
    {
        "name": "ufi_shell",
        "description": "在随身 WiFi 上执行 Shell 命令（默认以 root 权限执行）。可用于查看系统状态、进程、执行脚本、配置网络等。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "要执行的 Shell 命令，如 'ps -ef | grep cybercompanion' 或 'netstat -tlnp'"
                },
                "as_root": {
                    "type": "boolean",
                    "description": "是否以 Root 权限执行，默认 true",
                    "default": True
                }
            },
            "required": ["command"]
        }
    },
    {
        "name": "ufi_status",
        "description": "获取随身 WiFi 当前的核心硬件与网络状态（包括 5G/4G 信号 RSRP/SINR、CPU 占用、内存用量、温度、电池电量）。",
        "inputSchema": {
            "type": "object",
            "properties": {}
        }
    },
    {
        "name": "ufi_log",
        "description": "读取随身 WiFi 的日志输出（支持 CyberCompanion 机器人日志、One-API 日志、Android logcat、Linux 内核 dmesg）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "source": {
                    "type": "string",
                    "enum": ["cybercompanion", "oneapi", "logcat", "dmesg"],
                    "description": "日志来源，可选: cybercompanion, oneapi, logcat, dmesg",
                    "default": "cybercompanion"
                },
                "lines": {
                    "type": "integer",
                    "description": "读取最后多少行日志，默认 100 行",
                    "default": 100
                }
            }
        }
    },
    {
        "name": "ufi_push_file",
        "description": "将本地计算机上的文件上传/热推送到随身 WiFi 的指定路径（支持 root 覆盖写入）。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "local_path": {
                    "type": "string",
                    "description": "本地文件绝对路径"
                },
                "remote_path": {
                    "type": "string",
                    "description": "随身 WiFi 上的目标绝对路径，如 '/data/qq-bot/cybercompanion'"
                }
            },
            "required": ["local_path", "remote_path"]
        }
    },
    {
        "name": "ufi_pull_file",
        "description": "从随身 WiFi 下载指定文件到本地计算机。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "remote_path": {
                    "type": "string",
                    "description": "随身 WiFi 上的文件绝对路径"
                },
                "local_path": {
                    "type": "string",
                    "description": "本地保存的绝对路径"
                }
            },
            "required": ["remote_path", "local_path"]
        }
    },
    {
        "name": "ufi_restart_service",
        "description": "重启随身 WiFi 上的指定服务（如 cybercompanion、oneapi），或软重启随身 WiFi 设备系统。",
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {
                    "type": "string",
                    "enum": ["cybercompanion", "oneapi", "system"],
                    "description": "重启目标: cybercompanion (AI助手), oneapi (模型中转), system (整机重启)",
                    "default": "cybercompanion"
                }
            },
            "required": ["target"]
        }
    }
]

def handle_tool_call(name: str, args: dict) -> str:
    if name == "ufi_shell":
        cmd = args.get("command", "")
        as_root = args.get("as_root", True)
        return adb_shell(cmd, as_root)

    elif name == "ufi_status":
        script = """
        echo "=== 系统运行与负载 ==="
        uptime
        echo "=== 内存状态 ==="
        free -m || cat /proc/meminfo | head -n 5
        echo "=== 5G/4G 蜂窝网络状态 ==="
        dumpsys telephony.registry 2>/dev/null | grep -E "mSignalStrength|mCellInfo|mServiceState" | head -n 10
        echo "=== 网络接口与地址 ==="
        ip -br a
        echo "=== 核心守护进程 ==="
        ps -ef | grep -E "cybercompanion|one-api|adbd" | grep -v grep
        """
        return adb_shell(script, as_root=True)

    elif name == "ufi_log":
        source = args.get("source", "cybercompanion")
        lines = args.get("lines", 100)
        if source == "cybercompanion":
            return adb_shell(f"tail -n {lines} /data/qq-bot/cybercompanion.log 2>/dev/null || echo '未找到 /data/qq-bot/cybercompanion.log'", as_root=True)
        elif source == "oneapi":
            return adb_shell(f"tail -n {lines} /data/one-api/one-api.log 2>/dev/null || echo '未找到 /data/one-api/one-api.log'", as_root=True)
        elif source == "dmesg":
            return adb_shell(f"dmesg | tail -n {lines}", as_root=True)
        elif source == "logcat":
            return adb_shell(f"logcat -d -t {lines}", as_root=False)
        return f"Unknown log source: {source}"

    elif name == "ufi_push_file":
        local_p = args.get("local_path", "")
        remote_p = args.get("remote_path", "")
        ensure_adb_connected()
        tmp_remote = f"/data/local/tmp/_tmp_push_{abs(hash(local_p))}"
        res = subprocess.run([ADB_BIN, "-s", DEVICE_ADDR, "push", local_p, tmp_remote], capture_output=True, text=True)
        if res.returncode != 0:
            return f"[ERROR] ADB push failed: {res.stderr}"
        adb_shell(f"cp {tmp_remote} {remote_p} && chmod 755 {remote_p} && rm -f {tmp_remote}", as_root=True)
        return f"[SUCCESS] Pushed {local_p} -> {remote_p}"

    elif name == "ufi_pull_file":
        remote_p = args.get("remote_path", "")
        local_p = args.get("local_path", "")
        ensure_adb_connected()
        tmp_remote = f"/data/local/tmp/_tmp_pull_{abs(hash(remote_p))}"
        adb_shell(f"cp {remote_p} {tmp_remote} && chmod 666 {tmp_remote}", as_root=True)
        res = subprocess.run([ADB_BIN, "-s", DEVICE_ADDR, "pull", tmp_remote, local_p], capture_output=True, text=True)
        adb_shell(f"rm -f {tmp_remote}", as_root=True)
        if res.returncode != 0:
            return f"[ERROR] ADB pull failed: {res.stderr}"
        return f"[SUCCESS] Pulled {remote_p} -> {local_p}"

    elif name == "ufi_restart_service":
        target = args.get("target", "cybercompanion")
        if target == "cybercompanion":
            out = adb_shell("pkill -9 -f /data/qq-bot/cybercompanion; sleep 1; sh /data/qq-bot/start.sh; ps -ef | grep cybercompanion", as_root=True)
            return f"Restarted CyberCompanion:\n{out}"
        elif target == "oneapi":
            out = adb_shell("pkill -9 -f /data/one-api/one-api; sleep 1; sh /data/one-api/start.sh; ps -ef | grep one-api", as_root=True)
            return f"Restarted One-API:\n{out}"
        elif target == "system":
            out = adb_shell("reboot", as_root=True)
            return f"System reboot initiated."

    return f"Tool {name} not found."

def send_response(resp: dict):
    line = json.dumps(resp, ensure_ascii=False)
    sys.stdout.write(line + "\n")
    sys.stdout.flush()

def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except Exception:
            continue

        method = req.get("method")
        msg_id = req.get("id")

        if method == "initialize":
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {
                        "tools": {}
                    },
                    "serverInfo": {
                        "name": "ufi-debugger-mcp",
                        "version": "1.0.0"
                    }
                }
            })
        elif method == "notifications/initialized":
            pass
        elif method == "tools/list":
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "tools": TOOLS
                }
            })
        elif method == "tools/call":
            params = req.get("params", {})
            name = params.get("name")
            arguments = params.get("arguments", {})
            result_text = handle_tool_call(name, arguments)
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "content": [
                        {
                            "type": "text",
                            "text": result_text
                        }
                    ]
                }
            })
        elif method == "ping":
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {}
            })
        else:
            if msg_id is not None:
                send_response({
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "error": {
                        "code": -32601,
                        "message": f"Method {method} not found"
                    }
                })

if __name__ == "__main__":
    main()
