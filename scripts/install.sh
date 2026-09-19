#!/bin/bash
# ==============================================================================
# CyberCompanion 一键自启部署脚本
# 支持设备：随身 WiFi (U20, 高通410/210)、树莓派、Linux 服务器、Termux
# ==============================================================================

set -e

REPO="Elysia-SHY/CyberCompanion"
INSTALL_DIR="/opt/cybercompanion"

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

# Download binary if not present locally
if [ ! -f "cybercompanion" ]; then
    echo "⬇️ 正在下载最新版本的 CyberCompanion ($BIN_ARCH)..."
    DOWNLOAD_URL="https://github.com/$REPO/releases/latest/download/cybercompanion-$BIN_ARCH"
    if command -v curl >/dev/null 2>&1; then
        curl -sL "$DOWNLOAD_URL" -o cybercompanion
    elif command -v wget >/dev/null 2>&1; then
        wget -qO cybercompanion "$DOWNLOAD_URL"
    else
        echo "❌ 缺少 curl 或 wget 下载工具"
        exit 1
    fi
    chmod +x cybercompanion
fi

# Create default config if missing
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
  "daily_file": "daily_traffic.json",
  "passcode": "复活吧我的爱人！！！elyisa",
  "web_port": 8088,
  "sandbox": false,
  "stickers_dir": "./stickers",
  "enable_stickers": true
}
EOF
fi

# Service Configuration
if [ "$IS_ANDROID" -eq 1 ]; then
    echo "📱 检测为随身 WiFi / Android 宿主环境，配置后台自启动..."
    cat << EOF > run.sh
#!/bin/sh
cd $INSTALL_DIR
exec ./cybercompanion -config $INSTALL_DIR/config.json >> $INSTALL_DIR/run.log 2>&1 &
EOF
    chmod +x run.sh

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
After=network.target

[Service]
Type=simple
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/cybercompanion -config $INSTALL_DIR/config.json
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        sudo systemctl daemon-reload
        sudo systemctl enable cybercompanion
        sudo systemctl restart cybercompanion
        echo "🎉 Systemd 服务已激活并自启！"
    else
        nohup "$INSTALL_DIR/cybercompanion" -config "$INSTALL_DIR/config.json" > "$INSTALL_DIR/run.log" 2>&1 &
        echo "🎉 已通过 nohup 后台启动！"
    fi
fi

IP_ADDR=$(hostname -I 2>/dev/null | awk '{print $1}' || echo "127.0.0.1")
echo ""
echo "=========================================================="
echo "  🌟 部署完成！"
echo "  🌐 Web 控制面板地址: http://$IP_ADDR:8088"
echo "  💡 登录面板配置 QQ AppID、Secret 和 API Key 即刻唤醒！"
echo "=========================================================="
