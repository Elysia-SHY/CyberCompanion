// CyberCompanion WebUI Controller
document.addEventListener('DOMContentLoaded', () => {
  let activeTab = 'overview';
  let isLogsPaused = false;
  let logRefreshTimer = null;
  let statusTimer = null;
  let currentConfig = {};
  let currentPresets = [];

  // DOM elements
  const navBtns = document.querySelectorAll('.nav-btn');
  const tabPanes = document.querySelectorAll('.tab-pane');
  const pageTitle = document.getElementById('page-title');
  const toastEl = document.getElementById('toast');

  // Navigation Switch
  navBtns.forEach(btn => {
    btn.addEventListener('click', () => {
      const target = btn.getAttribute('data-tab');
      switchTab(target);
    });
  });

  function switchTab(tabId) {
    activeTab = tabId;
    navBtns.forEach(b => b.classList.toggle('active', b.getAttribute('data-tab') === tabId));
    tabPanes.forEach(p => p.classList.toggle('active', p.id === `tab-${tabId}`));
    
    const titles = {
      overview: '仪表盘概览',
      persona: '灵魂与人设',
      config: '系统与模型配置',
      logs: '实时运行日志'
    };
    pageTitle.textContent = titles[tabId] || '控制台';

    if (tabId === 'logs') {
      fetchLogs();
    } else if (tabId === 'config') {
      fetchConfig();
    } else if (tabId === 'persona') {
      fetchPresets();
    }
  }

  // Toast Notification
  function showToast(msg) {
    toastEl.textContent = msg;
    toastEl.classList.add('show');
    setTimeout(() => {
      toastEl.classList.remove('show');
    }, 3000);
  }

  // ── 统一请求封装 ──────────────────────────────────────────────
  // 面板现在需要登录，且所有写操作都要求 CSRF 双重提交令牌。
  // 这里包装全局 fetch，避免逐处修改调用点，也保证新增请求不会漏掉令牌。
  const _rawFetch = window.fetch.bind(window);

  function readCookie(name) {
    const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'));
    return m ? decodeURIComponent(m[1]) : '';
  }

  function showLoginOverlay(message) {
    let el = document.getElementById('cc-login-overlay');
    if (!el) {
      el = document.createElement('div');
      el.id = 'cc-login-overlay';
      el.innerHTML = `
        <div class="cc-login-card">
          <div class="cc-login-title">CyberCompanion</div>
          <div class="cc-login-sub">请输入面板管理密码</div>
          <input type="password" id="cc-login-input" placeholder="管理密码" autocomplete="current-password">
          <button id="cc-login-btn">登录</button>
          <div class="cc-login-err" id="cc-login-err"></div>
          <div class="cc-login-hint">密码见 config.json 的 web_password 字段</div>
        </div>`;
      document.body.appendChild(el);

      const submit = async () => {
        const input = document.getElementById('cc-login-input');
        const errEl = document.getElementById('cc-login-err');
        const btn = document.getElementById('cc-login-btn');
        const pwd = input.value;
        if (!pwd) return;
        btn.disabled = true;
        errEl.textContent = '';
        try {
          const r = await _rawFetch('/api/login', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ password: pwd })
          });
          if (r.ok) {
            el.remove();
            location.reload();
          } else {
            let msg = '登录失败';
            try { msg = (await r.json()).error || msg; } catch (e) {}
            errEl.textContent = msg;
          }
        } catch (e) {
          errEl.textContent = '网络错误: ' + e.message;
        }
        btn.disabled = false;
      };

      document.getElementById('cc-login-btn').addEventListener('click', submit);
      document.getElementById('cc-login-input').addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') submit();
      });
    }
    el.style.display = 'flex';
    if (message) {
      document.getElementById('cc-login-err').textContent = message;
    }
    setTimeout(() => {
      const i = document.getElementById('cc-login-input');
      if (i) i.focus();
    }, 50);
  }

  // 需要登录时挂起后续轮询，避免每 3 秒刷一次 401 报错
  let authRequired = false;

  window.fetch = async function (input, init) {
    const opts = Object.assign({}, init);
    const method = (opts.method || 'GET').toUpperCase();

    if (method !== 'GET' && method !== 'HEAD') {
      const csrf = readCookie('cc_csrf');
      if (csrf) {
        opts.headers = Object.assign({}, opts.headers, { 'X-CSRF-Token': csrf });
      }
    }

    const resp = await _rawFetch(input, opts);

    if (resp.status === 401) {
      authRequired = true;
      showLoginOverlay();
    }
    return resp;
  };

  // Fetch Status
  async function fetchStatus() {
    try {
      if (authRequired) return;
      const resp = await fetch('/api/status');
      if (!resp.ok) return;
      const data = await resp.json();

      // Sidebar & Dot
      const dot = document.getElementById('qq-status-dot');
      const qqText = document.getElementById('qq-status-text');
      if (data.qq_connected) {
        dot.className = 'dot online';
        qqText.textContent = 'QQ 网关已就绪 (在线)';
      } else {
        dot.className = 'dot offline';
        qqText.textContent = 'QQ 网关断开 (重连中)';
      }

      document.getElementById('sidebar-device').textContent = data.device_info.device_type || '通用边缘设备';

      // Companion Hero
      document.getElementById('hero-name').textContent = data.bot_name || 'DEEPSEEK-CHAN';
      document.getElementById('hero-title').textContent = data.persona_title || '专属伴侣';
      document.getElementById('hero-desc').textContent = data.persona_desc || data.system_prompt;
      
      const avatarEmojis = {
        deepseek_chan: '🐳',
        elysia: '🌸',
        neko: '🐱',
        jarvis: '🤖',
        custom: '✨'
      };
      const emoji = avatarEmojis[data.active_persona] || '💙';
      document.getElementById('hero-avatar').textContent = emoji;

      // Stats
      document.getElementById('stat-device').textContent = data.device_info.device_type;
      document.getElementById('stat-os-arch').textContent = `${data.device_info.os} / ${data.device_info.arch}`;
      document.getElementById('stat-uptime').textContent = data.device_info.uptime;
      document.getElementById('stat-hostname').textContent = `主机: ${data.device_info.hostname}`;
      document.getElementById('stat-signal').textContent = data.device_info.signal_rsrp || '未提供';
      document.getElementById('stat-network-type').textContent = data.device_info.network_type || '未提供';
      document.getElementById('stat-traffic-today').textContent = data.device_info.traffic_today || '未提供';
      document.getElementById('stat-rsrp-detail').textContent = data.device_info.signal_rsrp || '未提供';

      // 详细硬件信息（所有平台统一提供，拿不到的项明确标注）
      const d = data.device_info.details || {};

      const ipsEl = document.getElementById('stat-network-ips');
      if (ipsEl) {
        ipsEl.textContent = (d.network_ips && d.network_ips.length) ? d.network_ips.join(', ') : '未检测到';
      }

      // CPU 占用条
      const cpuPct = Math.max(0, Math.min(100, Number(d.cpu_usage) || 0));
      const cpuFill = document.getElementById('cpu-fill');
      const cpuLabel = document.getElementById('cpu-label');
      if (cpuFill && cpuLabel) {
        cpuFill.style.width = `${cpuPct}%`;
        const cores = d.cpu_cores > 0 ? `${d.cpu_cores} 核 · ` : '';
        cpuLabel.textContent = `${cores}${cpuPct.toFixed(1)}%`;
      }

      // Memory bar
      if (d.memory_total_mb_ext > 0) {
        const used = d.memory_used_mb_ext;
        const total = d.memory_total_mb_ext;
        const pct = Math.min(100, Math.round((used / total) * 100));
        document.getElementById('mem-label').textContent = `${used} MB / ${total} MB (${pct}%)`;
        document.getElementById('mem-fill').style.width = `${pct}%`;
      } else {
        document.getElementById('mem-label').textContent = '本平台未提供';
        document.getElementById('mem-fill').style.width = '0%';
      }

      // Disk bar
      const diskFill = document.getElementById('disk-fill');
      const diskLabel = document.getElementById('disk-label');
      if (diskFill && diskLabel) {
        if (d.disk_total_gb > 0) {
          const pct = Math.min(100, Math.round((d.disk_used_gb / d.disk_total_gb) * 100));
          diskLabel.textContent = `${d.disk_used_gb} GB / ${d.disk_total_gb} GB (${pct}%)`;
          diskFill.style.width = `${pct}%`;
        } else {
          diskLabel.textContent = '本平台未提供';
          diskFill.style.width = '0%';
        }
      }

      // 详细规格表：只展示真实拿到的项
      const specList = document.getElementById('spec-list');
      if (specList) {
        const nz = (v) => v !== undefined && v !== null && v !== '' && v !== 0;
        const specs = [
          ['处理器型号', d.cpu_model || '未提供'],
          ['逻辑核心', d.cpu_cores > 0 ? `${d.cpu_cores} 核` : '未提供'],
          ['当前主频', d.cpu_freq_mhz > 0 ? `${d.cpu_freq_mhz} MHz` : '未提供'],
          ['平均负载', d.load_avg || '未提供'],
          ['调频策略', d.cpu_governor || '未提供'],
          ['交换分区', d.swap_total_mb > 0 ? `${d.swap_used_mb} / ${d.swap_total_mb} MB` : '未提供'],
          ['系统运行时长', d.system_uptime || '未提供'],
          ['内核版本', d.kernel || '未提供'],
          ['系统进程数', d.process_count > 0 ? `${d.process_count} 个` : '未提供'],
          ['电池', d.battery_level >= 0 ? `${d.battery_level}% (${d.battery_status || '状态未知'})` : '无电池或不可用'],
          ['运行协程', nz(d.goroutines) ? `${d.goroutines}` : '未提供'],
          ['采集时间', d.collected_at || '未提供']
        ];
        specList.innerHTML = '';
        specs.forEach(([k, v]) => {
          const row = document.createElement('div');
          row.className = 'spec-row';
          const kEl = document.createElement('span');
          kEl.className = 'spec-key';
          kEl.textContent = k;
          const vEl = document.createElement('span');
          vEl.className = 'spec-val';
          vEl.textContent = v;
          row.appendChild(kEl);
          row.appendChild(vEl);
          specList.appendChild(row);
        });
        // 明确列出本平台无法采集的项，而不是静默显示假数据
        if (d.unavailable && d.unavailable.length) {
          const note = document.createElement('div');
          note.className = 'spec-note';
          note.textContent = `本平台未提供：${d.unavailable.join('、')}`;
          specList.appendChild(note);
        }
      }

      // Temperatures
      const tempContainer = document.getElementById('temp-tags');
      tempContainer.innerHTML = '';
      if (data.device_info.temperatures && data.device_info.temperatures.length > 0) {
        data.device_info.temperatures.forEach(t => {
          const tag = document.createElement('span');
          tag.className = 'temp-tag';
          tag.textContent = t;
          tempContainer.appendChild(tag);
        });
      } else {
        const tag = document.createElement('span');
        tag.className = 'temp-tag';
        tag.textContent = '温度：本平台未提供';
        tempContainer.appendChild(tag);
      }

    } catch (err) {
      console.warn('Status fetch error:', err);
    }
  }

  // Fetch Config
  async function fetchConfig() {
    try {
      const resp = await fetch('/api/config');
      if (!resp.ok) return;
      currentConfig = await resp.json();

      document.getElementById('cfg-qq-appid').value = currentConfig.qq_appid || '';
      // 密钥类字段服务端只回传掩码（如 Supe••••••••3456）。
      // 掩码原样提交时后端会跳过写入，因此这里回填掩码是安全的。
      document.getElementById('cfg-qq-secret').value = currentConfig.qq_secret_masked || '';
      document.getElementById('cfg-qq-secret').placeholder =
        currentConfig.qq_secret_masked ? '留空则不修改' : '请输入 QQ AppSecret';
      document.getElementById('cfg-llm-url').value = currentConfig.oneapi_url || '';
      document.getElementById('cfg-llm-token').value = currentConfig.oneapi_token_masked || '';
      document.getElementById('cfg-llm-token').placeholder =
        currentConfig.oneapi_token_masked ? '留空则不修改' : '请输入大模型 API Key';
      document.getElementById('cfg-llm-model').value = currentConfig.model || '';
      // 口令不回传任何形式的值（含掩码），只提示是否已设置
      const passEl = document.getElementById('cfg-passcode');
      passEl.value = '';
      passEl.placeholder = currentConfig.passcode_set
        ? '已设置（留空则不修改）'
        : '尚未设置，请在此自定义主人认证口令';
      document.getElementById('cfg-web-port').value = currentConfig.web_port || 8088;
      document.getElementById('cfg-enable-stickers').checked = currentConfig.enable_stickers !== false;

      document.getElementById('custom-bot-name').value = currentConfig.bot_name || '';
      document.getElementById('custom-prompt-text').value = currentConfig.system_prompt || '';
    } catch (err) {
      showToast('获取配置失败: ' + err.message);
    }
  }

  // Save Config
  document.getElementById('btn-save-config').addEventListener('click', async () => {
    const payload = {
      qq_appid: document.getElementById('cfg-qq-appid').value.trim(),
      qq_secret: document.getElementById('cfg-qq-secret').value.trim(),
      oneapi_url: document.getElementById('cfg-llm-url').value.trim(),
      oneapi_token: document.getElementById('cfg-llm-token').value.trim(),
      model: document.getElementById('cfg-llm-model').value.trim(),
      passcode: document.getElementById('cfg-passcode').value.trim(),
      web_port: parseInt(document.getElementById('cfg-web-port').value, 10) || 8088,
      enable_stickers: document.getElementById('cfg-enable-stickers').checked
    };

    try {
      const resp = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload)
      });
      if (resp.ok) {
        showToast('🎉 配置保存成功！已持久化并实时应用');
        fetchStatus();
      } else {
        const text = await resp.text();
        showToast('保存失败: ' + text);
      }
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  });

  // Fetch & Render Presets
  async function fetchPresets() {
    try {
      const resp = await fetch('/api/persona');
      if (!resp.ok) return;
      const data = await resp.json();
      currentPresets = data.presets || [];
      const activeId = data.active_persona;

      const container = document.getElementById('preset-container');
      container.innerHTML = '';

      const avatarIcons = {
        deepseek_chan: '🐳',
        elysia: '🌸',
        neko: '🐱',
        jarvis: '🤖'
      };

      currentPresets.forEach(p => {
        const card = document.createElement('div');
        card.className = `preset-card ${p.id === activeId ? 'active' : ''}`;
        card.innerHTML = `
          <div class="preset-card-header">
            <span style="font-size: 24px;">${avatarIcons[p.id] || '🎭'}</span>
            <div>
              <div class="preset-card-title">${p.name}</div>
              <span class="badge badge-accent">${p.title}</span>
            </div>
          </div>
          <div class="preset-card-desc">${p.description}</div>
        `;
        card.addEventListener('click', () => selectPreset(p.id));
        container.appendChild(card);
      });
    } catch (err) {
      console.warn('Failed to fetch presets:', err);
    }
  }

  // Select Preset
  async function selectPreset(id) {
    try {
      const resp = await fetch('/api/persona', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ id })
      });
      if (resp.ok) {
        showToast('✨ 灵魂蜕变成功！人设与对话记忆已无缝切换');
        fetchPresets();
        fetchStatus();
      } else {
        showToast('切换失败');
      }
    } catch (err) {
      showToast('错误: ' + err.message);
    }
  }

  // Save Custom Persona
  document.getElementById('btn-save-custom-persona').addEventListener('click', async () => {
    const name = document.getElementById('custom-bot-name').value.trim();
    const prompt = document.getElementById('custom-prompt-text').value.trim();
    if (!prompt) {
      showToast('⚠️ 提示词内容不能为空哦');
      return;
    }

    try {
      const resp = await fetch('/api/persona', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          id: 'custom',
          name: name || '自定义伴侣',
          prompt: prompt
        })
      });
      if (resp.ok) {
        showToast('🎨 自定义人设已生效并保存！');
        fetchPresets();
        fetchStatus();
      } else {
        showToast('应用失败');
      }
    } catch (err) {
      showToast('网络异常: ' + err.message);
    }
  });

  // Fetch Logs
  async function fetchLogs() {
    if (isLogsPaused) return;
    try {
      const resp = await fetch('/api/logs');
      if (!resp.ok) return;
      const logs = await resp.json();
      const terminal = document.getElementById('terminal-view');
      terminal.textContent = logs.join('\n');
      terminal.scrollTop = terminal.scrollHeight;
    } catch (err) {
      console.warn('Log fetch error:', err);
    }
  }

  // Log Controls
  document.getElementById('btn-clear-logs').addEventListener('click', () => {
    document.getElementById('terminal-view').textContent = '';
  });

  const btnPause = document.getElementById('btn-pause-logs');
  btnPause.addEventListener('click', () => {
    isLogsPaused = !isLogsPaused;
    btnPause.textContent = isLogsPaused ? '恢复滚动' : '暂停滚动';
  });

  // Refresh & Reboot
  document.getElementById('btn-refresh').addEventListener('click', () => {
    fetchStatus();
    showToast('🔄 状态已刷新');
  });

  document.getElementById('btn-reboot').addEventListener('click', async () => {
    if (confirm('⚠️ 确定要远程重启这台设备吗？设备将在 3 秒后断开连接并在 1 分钟后恢复。')) {
      try {
        await fetch('/api/restart?confirm=yes', { method: 'POST' });
        showToast('⚡ 重启指令已发送，设备正在重启...');
      } catch (err) {
        showToast('重启请求发送失败');
      }
    }
  });

  // Init Intervals
  fetchStatus();
  statusTimer = setInterval(fetchStatus, 3000);
  logRefreshTimer = setInterval(() => {
    if (activeTab === 'logs') {
      fetchLogs();
    }
  }, 2000);
});
