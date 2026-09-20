//<script>
(async () => {
    'use strict';

    // 检查 Root 权限
    const checkAdvanceFunc = async () => {
        try {
            const res = await runShellWithRoot('whoami');
            return res?.content && res.content.includes('root');
        } catch {
            return false;
        }
    };

    let logInterval = null;
    let autoRefresh = true;
    let isRunning = false;

    // 获取当前 host 保证端口跳转准确
    const getHost = () => window.location.hostname || '192.168.88.1';
    const COMPANION_URL = `http://${getHost()}:8088`;

    // 检测开机自启状态（非阻塞守护脚本与 ufi_tools_boot）
    const checkIsBootUp = async () => {
        try {
            const res = await runShellWithRoot("grep -q 'qq-bot/start.sh' /sdcard/ufi_tools_boot.sh && echo '1' || echo '0'");
            return res?.content?.trim() === '1';
        } catch {
            return false;
        }
    };

    // 检测伴侣服务运行状态与 QQ 网关连接状态
    const checkStatus = async () => {
        try {
            const [psRes, logRes] = await Promise.all([
                runShellWithRoot("pgrep -f '/data/qq-bot/cybercompanion'"),
                runShellWithRoot("timeout 2s awk '{print}' /data/qq-bot/cybercompanion.log | tail -n 20")
            ]);

            const pid = psRes?.content?.trim();
            const logContent = logRes?.content || '';

            const badge = document.querySelector('#companion_status_badge');
            const qqBadge = document.querySelector('#companion_qq_badge');
            const mainActionBtn = document.querySelector('#companion_main_action_btn');
            const openWebBtn = document.querySelector('#companion_open_web_btn');

            if (pid && !isNaN(parseInt(pid))) {
                isRunning = true;
                if (badge) {
                    badge.textContent = `🟢 伴侣运行中 (PID: ${pid})`;
                    badge.style.color = '#4ade80';
                }
                if (mainActionBtn) {
                    mainActionBtn.textContent = '⏹️ 停止伴侣';
                    mainActionBtn.style.background = 'rgba(239, 68, 68, 0.2)';
                }
                if (openWebBtn) openWebBtn.style.display = 'inline-block';

                // 判断 QQ 网关状态
                if (qqBadge) {
                    if (logContent.includes('READY') || logContent.includes('Bot online and READY')) {
                        qqBadge.textContent = '🌸 爱莉希雅 QQ 在线';
                        qqBadge.style.color = '#f472b6';
                    } else if (logContent.includes('Connecting')) {
                        qqBadge.textContent = '🟡 QQ 网关连接中...';
                        qqBadge.style.color = '#facc15';
                    } else {
                        qqBadge.textContent = '⚪ QQ 未就绪';
                        qqBadge.style.color = '#94a3b8';
                    }
                }
            } else {
                isRunning = false;
                if (badge) {
                    badge.textContent = '🔴 伴侣已停止';
                    badge.style.color = '#f87171';
                }
                if (qqBadge) {
                    qqBadge.textContent = '⚪ QQ 离线';
                    qqBadge.style.color = '#94a3b8';
                }
                if (mainActionBtn) {
                    mainActionBtn.textContent = '▶️ 启动伴侣';
                    mainActionBtn.style.background = 'rgba(244, 114, 182, 0.2)';
                }
                if (openWebBtn) openWebBtn.style.display = 'none';
            }
        } catch (e) {
            console.error(e);
        }
    };

    // 开机自启按钮状态同步
    const updateBootBtnState = async () => {
        const bootBtn = document.querySelector('#companion_boot_btn');
        if (!bootBtn) return;
        const isBoot = await checkIsBootUp();
        if (isBoot) {
            bootBtn.style.background = "var(--dark-btn-color-active, #f472b6)";
            bootBtn.textContent = "自启动: 已开启";
        } else {
            bootBtn.style.background = "";
            bootBtn.textContent = "自启动: 已关闭";
        }
    };

    // 读取运行日志 (根据规范使用 timeout 2s awk，严禁使用 cat)
    let prevLogText = '';
    const readLog = async () => {
        const textarea = document.querySelector("#Companion_textarea");
        if (!textarea) return;
        try {
            const res = await runShellWithRoot("timeout 2s awk '{print}' /data/qq-bot/cybercompanion.log | tail -n 60");
            const content = res?.content || '暂无日志或文件为空';
            if (content !== prevLogText) {
                prevLogText = content;
                textarea.value = content;
                textarea.scrollTop = textarea.scrollHeight;
            }
        } catch (e) {
            textarea.value = "读取日志失败：" + (e?.message || e);
        }
    };

    // 挂载到页面 DOM
    const mmContainer = document.querySelector('.functions-container');
    if (!mmContainer) return;

    // 防止重复插入
    const existingPanel = document.querySelector('#IFRAME_KANO_Companion');
    if (existingPanel) existingPanel.remove();

    mmContainer.insertAdjacentHTML("afterend", `
        <div id="IFRAME_KANO_Companion" style="width: 100%; margin-top: 10px;">
            <div class="title" style="margin: 6px 0; display: flex; align-items: center; justify-content: space-between;">
                <div style="display: flex; align-items: center; gap: 8px; flex-wrap: wrap;">
                    <strong>🌸 CyberCompanion 赛博伴侣 (爱莉希雅)</strong>
                    <span id="companion_status_badge" style="font-size: 11px; padding: 2px 6px; border-radius: 4px; background: rgba(0,0,0,0.25); font-weight: normal;">检测中...</span>
                    <span id="companion_qq_badge" style="font-size: 11px; padding: 2px 6px; border-radius: 4px; background: rgba(244,114,182,0.15); font-weight: normal;">🌸 QQ在线</span>
                </div>
                <div style="display: inline-block;" id="collapse_Companion_btn"></div>
            </div>
            <div class="collapse" id="collapse_Companion" data-name="close" style="height: 0px; overflow: hidden;">
                <div class="collapse_box">
                    <div id="Companion_info_grid" style="margin-bottom:12px; display:grid; grid-template-columns: repeat(auto-fit, minmax(130px, 1fr)); gap: 8px; font-size: 12px;">
                        <div style="padding: 8px; background: rgba(255,255,255,0.04); border-radius: 6px; border-left: 3px solid #f472b6;">
                            <div style="color: #94a3b8; font-size: 10px;">当前核心人格</div>
                            <div style="font-weight: bold; color: #f472b6; margin-top: 2px;">人之律者 · 爱莉希雅</div>
                        </div>
                        <div style="padding: 8px; background: rgba(255,255,255,0.04); border-radius: 6px; border-left: 3px solid #38bdf8;">
                            <div style="color: #94a3b8; font-size: 10px;">QQ 机器人 AppID</div>
                            <div style="font-weight: bold; margin-top: 2px;">1905643821</div>
                        </div>
                        <div style="padding: 8px; background: rgba(255,255,255,0.04); border-radius: 6px; border-left: 3px solid #a855f7;">
                            <div style="color: #94a3b8; font-size: 10px;">Web 控制台端口</div>
                            <div style="font-weight: bold; margin-top: 2px;">:8088 (v1.3.0)</div>
                        </div>
                        <div style="padding: 8px; background: rgba(255,255,255,0.04); border-radius: 6px; border-left: 3px solid #22c55e;">
                            <div style="color: #94a3b8; font-size: 10px;">LLM 驱动后端</div>
                            <div style="font-weight: bold; margin-top: 2px;">One-API :3000</div>
                        </div>
                    </div>
                    <div id="Companion_action_box" style="margin-bottom:10px;display:flex;gap:8px;flex-wrap:wrap">
                        <button id="companion_open_web_btn" class="btn" style="background: rgba(244, 114, 182, 0.25); border: 1px solid rgba(244,114,182,0.4); font-weight: bold; color: #f472b6;">🚀 打开伴侣面板 (8088)</button>
                        <button id="companion_main_action_btn" class="btn">启动伴侣</button>
                        <button id="companion_restart_btn" class="btn">🔄 重启伴侣</button>
                        <button id="companion_boot_btn" class="btn">自启动: 检测中</button>
                        <button id="companion_iptables_btn" class="btn">🛡️ 放行端口 8088</button>
                    </div>
                    <ul class="deviceList">
                        <li style="padding:10px;display: grid;grid-template-columns: 1fr;gap: 8px;">
                            <div>
                                <div class="title" style="display:flex; justify-content:space-between; align-items:center;">
                                    <span>伴侣运行日志 (cybercompanion.log)</span>
                                    <div style="display:flex; gap:6px;">
                                        <button style="margin: 0 !important;padding: 2px 8px;" onclick="window.companionReadLog()">刷新</button>
                                        <button style="margin: 0 !important;padding: 2px 8px;" onclick="window.companionClearLog()">清空日志</button>
                                        <button style="margin: 0 !important;padding: 2px 8px;" id="companion_toggle_auto_btn" onclick="window.companionToggleAutoRefresh()">自动刷新: 开</button>
                                    </div>
                                </div>
                                <textarea id="Companion_textarea" disabled style="margin-top: 6px;font-size:11px !important;border:none;padding:6px;margin:0;width:100%;height:260px;border-radius: 8px;overflow-x: hidden;background:rgba(0,0,0,0.2);color:#e2e8f0;font-family:monospace;resize:vertical;"></textarea>
                            </div>
                        </li>
                    </ul>
                </div>
            </div>
        </div>
    `);

    // 按钮事件绑定
    const openWebBtn = document.querySelector('#companion_open_web_btn');
    if (openWebBtn) {
        openWebBtn.onclick = () => {
            window.open(COMPANION_URL, '_blank');
        };
    }

    const mainActionBtn = document.querySelector('#companion_main_action_btn');
    if (mainActionBtn) {
        mainActionBtn.onclick = async () => {
            if (!(await checkAdvanceFunc())) return createToast("请启用高级功能 (Root) 后使用", "red");
            if (isRunning) {
                createToast("正在停止 CyberCompanion...", "");
                await runShellWithRoot("/system/bin/sh /data/qq-bot/stop.sh");
                createToast("伴侣服务已停止", "pink");
            } else {
                createToast("正在唤醒 CyberCompanion...", "");
                await runShellWithRoot("/system/bin/sh /data/qq-bot/start.sh");
                createToast("伴侣启动指令已发送", "green");
            }
            setTimeout(async () => {
                await checkStatus();
                await readLog();
            }, 1500);
        };
    }

    const restartBtn = document.querySelector('#companion_restart_btn');
    if (restartBtn) {
        restartBtn.onclick = async () => {
            if (!(await checkAdvanceFunc())) return createToast("请启用高级功能 (Root) 后使用", "red");
            createToast("正在重启伴侣服务...", "");
            await runShellWithRoot("/system/bin/sh /data/qq-bot/stop.sh; sleep 1; /system/bin/sh /data/qq-bot/start.sh");
            createToast("伴侣重启指令已执行", "green");
            setTimeout(async () => {
                await checkStatus();
                await readLog();
            }, 2000);
        };
    }

    const bootBtn = document.querySelector('#companion_boot_btn');
    if (bootBtn) {
        bootBtn.onclick = async () => {
            if (!(await checkAdvanceFunc())) return createToast("请启用高级功能 (Root) 后使用", "red");
            const isBoot = await checkIsBootUp();
            if (isBoot) {
                createToast("正在关闭开机自启...", "");
                await runShellWithRoot(`
                    sed -i '/qq-bot/d' /sdcard/ufi_tools_boot.sh
                    rm -f /data/adb/service.d/cybercompanion.sh
                `);
                createToast("已关闭伴侣开机自启动", "pink");
            } else {
                createToast("正在配置开机自启（安全非阻塞模式）...", "");
                await runShellWithRoot(`
                    grep -q 'qq-bot/start.sh' /sdcard/ufi_tools_boot.sh || echo '(sleep 25; sh /data/qq-bot/start.sh) >/dev/null 2>&1 &' >> /sdcard/ufi_tools_boot.sh
                    mkdir -p /data/adb/service.d
                    cat << 'EOF' > /data/adb/service.d/cybercompanion.sh
#!/system/bin/sh
(
    while [ "$(getprop sys.boot_completed)" != "1" ]; do sleep 3; done
    sleep 20
    if ! pgrep -f "/data/one-api/one-api" >/dev/null 2>&1; then [ -f /data/one-api/start.sh ] && sh /data/one-api/start.sh; fi
    sleep 5
    if ! pgrep -f "/data/qq-bot/cybercompanion" >/dev/null 2>&1; then [ -f /data/qq-bot/start.sh ] && sh /data/qq-bot/start.sh; fi
) >/dev/null 2>&1 &
exit 0
EOF
                    chmod 755 /data/adb/service.d/cybercompanion.sh
                `);
                createToast("已开启伴侣安全自启 (ufi_tools_boot + service.d 后台守护)", "green");
            }
            await updateBootBtnState();
        };
    }

    const iptablesBtn = document.querySelector('#companion_iptables_btn');
    if (iptablesBtn) {
        iptablesBtn.onclick = async () => {
            if (!(await checkAdvanceFunc())) return createToast("请启用高级功能 (Root) 后使用", "red");
            const res = await runShellWithRoot("iptables -I INPUT -p tcp --dport 8088 -j ACCEPT 2>&1");
            createToast(res.success ? "防火墙 8088 端口已放行！" : "放行失败：" + res.content, res.success ? "green" : "red");
        };
    }

    // 全局函数挂载供 HTML 按钮调用
    window.companionReadLog = async () => {
        await readLog();
        createToast("日志已刷新", "green", 1500);
    };

    window.companionClearLog = async () => {
        if (!(await checkAdvanceFunc())) return createToast("请启用高级功能 (Root) 后使用", "red");
        await runShellWithRoot("> /data/qq-bot/cybercompanion.log");
        await readLog();
        createToast("日志已清空", "green", 1500);
    };

    window.companionToggleAutoRefresh = () => {
        autoRefresh = !autoRefresh;
        const btn = document.querySelector('#companion_toggle_auto_btn');
        if (btn) btn.textContent = `自动刷新: ${autoRefresh ? '开' : '关'}`;
        if (autoRefresh) {
            startPolling();
            createToast("已开启日志自动刷新 (2s)", "green", 1500);
        } else {
            stopPolling();
            createToast("已暂停日志自动刷新", "pink", 1500);
        }
    };

    // 轮询控制
    const startPolling = () => {
        stopPolling();
        logInterval = requestInterval(async () => {
            await readLog();
            await checkStatus();
        }, 2000);
    };

    const stopPolling = () => {
        if (logInterval) {
            logInterval();
            logInterval = null;
        }
    };

    // 折叠面板绑定
    collapseGen("#collapse_Companion_btn", "#collapse_Companion", "#collapse_Companion", (state) => {
        if (state === 'open') {
            checkStatus();
            updateBootBtnState();
            readLog();
            if (autoRefresh) startPolling();
        } else {
            stopPolling();
        }
    });

    // 页面初次检查
    setTimeout(async () => {
        await checkStatus();
        await updateBootBtnState();
        if (localStorage.getItem("#collapse_Companion") === 'open') {
            await readLog();
            if (autoRefresh) startPolling();
        }
    }, 500);

    // 在常用操作按钮区添加一个入口快捷按钮
    const actionsButtons = document.querySelector('.functions-container .actions-buttons');
    if (actionsButtons && !document.querySelector('#companion_quick_jump_btn')) {
        const quickBtn = document.createElement('button');
        quickBtn.id = 'companion_quick_jump_btn';
        quickBtn.textContent = '🌸 赛博伴侣';
        quickBtn.style.border = '1px solid rgba(244, 114, 182, 0.6)';
        quickBtn.style.color = '#f472b6';
        quickBtn.onclick = () => {
            const panel = document.querySelector('#IFRAME_KANO_Companion');
            if (panel) {
                panel.scrollIntoView({ behavior: 'smooth', block: 'start' });
                const collapseBox = document.querySelector('#collapse_Companion');
                if (collapseBox && collapseBox.getAttribute('data-name') !== 'open') {
                    const toggleBtn = document.querySelector('#collapse_Companion_btn');
                    if (toggleBtn) toggleBtn.click();
                }
            }
        };
        actionsButtons.appendChild(quickBtn);
    }
})();
//</script>
