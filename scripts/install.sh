#!/bin/bash
# ==============================================================================
# CyberCompanion 一键自启部署脚本
# 支持设备：随身 WiFi (U20, 高通410/210)、树莓派、Linux 服务器、Termux
#
# 环境变量：
#   CC_REPO=owner/repo   覆盖默认仓库
#   CC_VERSION=v1.1.0    安装指定版本（默认 latest）
#   CC_SKIP_VERIFY=1     跳过 SHA256 校验（不推荐）
# ==============================================================================

set -euo pipefail

REPO="${CC_REPO:-Elysia-SHY/CyberCompanion}"
VERSION="${CC_VERSION:-latest}"
INSTALL_DIR="/opt/cybercompanion"
BIN_NAME="cybercompanion"

# Detect if Android / UFI environment
if [ -d "/data/local" ] || [ -f "/system/build.prop" ]; then
    IS_ANDROID=1
    INSTALL_DIR="/data/cybercompanion"
else
    IS_ANDROID=0
fi

echo "=========================================================="
echo "    CyberCompanion 边缘硬件与多模态 AI 伴侣 一键安装器     "
echo "=========================================================="

ARCH=$(uname -m)
case "$ARCH" in
    x86_64|amd64)
        BIN_ARCH="linux-amd64"
        ;;
    aarch64|arm64)
        BIN_ARCH="linux-arm64"
        ;;
    armv7l|armhf|arm)
        BIN_ARCH="linux-armv7"
        ;;
    *)
        echo "❌ 不支持的 CPU 架构: $ARCH"
        exit 1
        ;;
esac

echo "✅ 探测到系统架构: $ARCH (使用 $BIN_ARCH)"
echo "📁 安装目录: $INSTALL_DIR"

mkdir -p "$INSTALL_DIR"
cd "$INSTALL_DIR"

# ------------------------------------------------------------------------------
# 下载工具选择
# ------------------------------------------------------------------------------
if command -v curl >/dev/null 2>&1; then
    DL() { curl -fsSL "$1" -o "$2"; }
    DL_STDOUT() { curl -fsSL "$1"; }
    HAVE_DL=1
elif command -v wget >/dev/null 2>&1; then
    DL() { wget -qO "$2" "$1"; }
    DL_STDOUT() { wget -qO- "$1"; }
    HAVE_DL=1
else
    echo "❌ 缺少 curl 或 wget 下载工具"
    exit 1
fi

# ------------------------------------------------------------------------------
# 校验工具选择：优先 sha256sum，回退 openssl / shasum / busybox
# ------------------------------------------------------------------------------
SHA256_OF() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$file" | awk '{print $NF}'
    elif command -v busybox >/dev/null 2>&1; then
        busybox sha256sum "$file" | awk '{print $1}'
    else
        echo ""
    fi
}

# ------------------------------------------------------------------------------
# 下载二进制（含 SHA256 校验 + 失败重试）
# ------------------------------------------------------------------------------
if [ ! -f "$BIN_NAME" ]; then
    BASE_URL="https://github.com/$REPO/releases"
    if [ "$VERSION" = "latest" ]; then
        BIN_URL="$BASE_URL/latest/download/$BIN_NAME-$BIN_ARCH"
        SUM_URL="$BASE_URL/latest/download/SHA256SUMS.txt"
    else
        BIN_URL="$BASE_URL/download/$VERSION/$BIN_NAME-$BIN_ARCH"
        SUM_URL="$BASE_URL/download/$VERSION/SHA256SUMS.txt"
    fi

    echo "⬇️ 正在下载 $BIN_NAME ($BIN_ARCH, $VERSION)..."
    TMP_BIN=".${BIN_NAME}.download.$$"
    rm -f "$TMP_BIN"

    ok=0
    for attempt in 1 2 3; do
        if DL "$BIN_URL" "$TMP_BIN"; then
            ok=1
            break
        fi
        echo "⚠️ 第 $attempt 次下载失败，2 秒后重试..."
        sleep 2
    done

    if [ "$ok" -ne 1 ] || [ ! -s "$TMP_BIN" ]; then
        echo "❌ 二进制下载失败：$BIN_URL"
        echo "   （若网络受限，可先手动下载后放到 $INSTALL_DIR/$BIN_NAME）"
        rm -f "$TMP_BIN"
        exit 1
    fi

    # ---- SHA256 校验：防止下载被劫持或文件损坏 ----
    if [ "${CC_SKIP_VERIFY:-0}" != "1" ]; then
        echo "🔐 正在校验文件完整性..."
        EXPECTED=""
        SUMS_FILE=".SHA256SUMS.$$"
        if DL "$SUM_URL" "$SUMS_FILE" 2>/dev/null; then
            EXPECTED=$(grep -E "[[:space:]]\*?${BIN_NAME}-${BIN_ARCH}$" "$SUMS_FILE" 2>/dev/null | awk '{print $1}' | head -n1 || true)
        fi
        rm -f "$SUMS_FILE"

        ACTUAL=$(SHA256_OF "$TMP_BIN")

        if [ -z "$ACTUAL" ]; then
            echo "⚠️ 当前系统缺少可用的 SHA256 工具，跳过校验。"
            echo "   建议安装 coreutils 后重试，或设置 CC_SKIP_VERIFY=1 显式跳过。"
        elif [ -z "$EXPECTED" ]; then
            echo "⚠️ 未能获取官方 SHA256SUMS.txt，跳过校验。"
            echo "   下载地址: $SUM_URL"
        elif [ "$EXPECTED" = "$ACTUAL" ]; then
            echo "✅ SHA256 校验通过: $ACTUAL"
        else
            echo "❌ SHA256 校验失败，文件可能被篡改或下载不完整！"
            echo "   期望: $EXPECTED"
            echo "   实际: $ACTUAL"
            rm -f "$TMP_BIN"
            exit 1
        fi
    else
        echo "⚠️ 已通过 CC_SKIP_VERIFY=1 跳过 SHA256 校验（不推荐）"
    fi

    mv "$TMP_BIN" "$BIN_NAME"
    chmod 755 "$BIN_NAME"
else
    echo "ℹ️ 已存在 $INSTALL_DIR/$BIN_NAME，跳过下载。"
fi

# ------------------------------------------------------------------------------
# 生成默认配置（口令留空，由用户在 Web 面板或 QQ 中自行设定）
# ------------------------------------------------------------------------------
if [ ! -f "config.json" ]; then
    echo "⚙️ 生成默认配置文件 config.json..."
    cat << 'EOF' > config.json
{
  "qq_appid": "",
  "qq_secret": "",
  "oneapi_url": "https://api.deepseek.com/v1/chat/completions",
  "oneapi_token": "",
  "model": "deepseek-chat",
  "bot_name": "DEEPSEEK-CHAN",
  "system_prompt": "你是一个温柔贴心的二次元日常陪伴少女。",
  "active_persona": "deepseek_chan",
  "owners": [],
  "owners_file": "owners.json",
  "passcode": "",
  "web_port": 8088,
  "web_password": "",
  "trusted_proxies": [],
  "max_history_msgs": 40,
  "token_budget": 6000,
  "enable_stickers": true,
  "enable_exec": false,
  "exec_whitelist": [],
  "stream_reply": true
}
EOF
    chmod 600 config.json
    echo "🔒 配置文件权限已收紧为 600。"
fi

# ------------------------------------------------------------------------------
# 服务配置
# ------------------------------------------------------------------------------
if [ "$IS_ANDROID" -eq 1 ]; then
    echo "📱 检测为随身 WiFi / Android 宿主环境，配置后台自启动..."
    cat << EOF > run.sh
#!/bin/sh
cd $INSTALL_DIR
exec ./$BIN_NAME -config $INSTALL_DIR/config.json >> $INSTALL_DIR/run.log 2>&1 &
EOF
    chmod 755 run.sh

    # Auto hook to common boot locations if root
    if [ -d "/data/adb/service.d" ]; then
        ln -sf "$INSTALL_DIR/run.sh" /data/adb/service.d/99-cybercompanion.sh
    fi
    ./run.sh
    echo "🎉 CyberCompanion 已在后台启动！"
else
    # Linux systemd setup
    if command -v systemctl >/dev/null 2>&1 && [ -d "/etc/systemd/system" ]; then
        echo "🐧 配置 Linux systemd 守护进程..."
        cat << EOF | sudo tee /etc/systemd/system/cybercompanion.service > /dev/null
[Unit]
Description=CyberCompanion AI Bot Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$BIN_NAME -config $INSTALL_DIR/config.json

# 重启策略：崩溃自动拉起，但频繁崩溃时不再无限重启（防止疯狂刷日志）
Restart=always
RestartSec=5
StartLimitIntervalSec=300
StartLimitBurst=5

# ── 权限收敛：以专用非特权用户运行 ──
User=cybercompanion
Group=cybercompanion

# ── 文件系统沙箱：全盘只读，仅工作目录可写 ──
ProtectSystem=strict
ReadWritePaths=$INSTALL_DIR
ProtectHome=true
PrivateTmp=true

# ── 内核与设备：禁止提权、禁止触碰内核与设备节点 ──
NoNewPrivileges=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true

# 能力集：默认不授予任何特权能力。
# 若确需通过机器人执行 reboot，取消下面两行注释即可精确授予该能力。
#AmbientCapabilities=CAP_SYS_BOOT
#CapabilityBoundingSet=CAP_SYS_BOOT

# ── 资源上限：边缘设备内存有限，避免单进程吃满 ──
MemoryMax=256M
CPUQuota=70%
TasksMax=64

StandardOutput=journal
StandardError=journal
SyslogIdentifier=cybercompanion

[Install]
WantedBy=multi-user.target
EOF
        # 专用非特权用户：机器人不需要 root，沙箱化后即使被滥用也拿不到完整 shell
        if ! id -u cybercompanion >/dev/null 2>&1; then
            echo "👤 创建专用运行账户 cybercompanion..."
            sudo useradd --system --no-create-home --shell /usr/sbin/nologin cybercompanion || true
        fi
        # 工作目录需可写（config.json / owners.json / sessions.json / 日志都在这里）
        sudo chown -R cybercompanion:cybercompanion "$INSTALL_DIR"
        sudo chmod 700 "$INSTALL_DIR"
        sudo chmod 600 "$INSTALL_DIR/config.json" 2>/dev/null || true

        sudo systemctl daemon-reload
        sudo systemctl enable cybercompanion
        sudo systemctl restart cybercompanion
        echo "🎉 Systemd 服务已激活并自启！"
    else
        nohup "$INSTALL_DIR/$BIN_NAME" -config "$INSTALL_DIR/config.json" > "$INSTALL_DIR/run.log" 2>&1 &
        echo "🎉 已通过 nohup 后台启动！"
    fi
fi

IP_ADDR=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")
echo ""
echo "=========================================================="
echo "  🌟 部署完成！"
echo "  🌐 Web 控制面板地址: http://$IP_ADDR:8088"
echo "  🔑 首次启动的登录密码打印在程序日志中，请查看 run.log"
echo "     或执行: grep -i '登录密码\\|web' $INSTALL_DIR/run.log"
echo ""
echo "  ⚠️  安全提醒："
echo "     1. 面板默认仅支持本机访问，如需外网请自行加反代 + HTTPS"
echo "     2. 请在面板中设置「主人认证口令」，之后在 QQ 中私聊机器人发送它"
echo "     3. 若无必要，请保持 \"enable_exec\": false"
echo "=========================================================="
