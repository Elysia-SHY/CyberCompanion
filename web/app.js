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

  // Fetch Status
  async function fetchStatus() {
    try {
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
      document.getElementById('stat-signal').textContent = data.device_info.signal_rsrp || '已连网';
      document.getElementById('stat-network-type').textContent = data.device_info.network_type || '局域网';
      document.getElementById('stat-traffic-today').textContent = data.device_info.traffic_today || '实时监测中';
      document.getElementById('stat-rsrp-detail').textContent = data.device_info.signal_rsrp || '--';

      // Memory bar
      if (data.device_info.memory_total_mb > 0) {
        const used = data.device_info.memory_used_mb;
        const total = data.device_info.memory_total_mb;
        const pct = Math.min(100, Math.round((used / total) * 100));
        document.getElementById('mem-label').textContent = `${used} MB / ${total} MB (${pct}%)`;
        document.getElementById('mem-fill').style.width = `${pct}%`;
      } else {
        document.getElementById('mem-label').textContent = '轻量常驻 (~20MB)';
        document.getElementById('mem-fill').style.width = '15%';
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
        tag.textContent = '温控：正常';
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
      document.getElementById('cfg-qq-secret').value = currentConfig.qq_secret || '';
      document.getElementById('cfg-llm-url').value = currentConfig.oneapi_url || '';
      document.getElementById('cfg-llm-token').value = currentConfig.oneapi_token || '';
      document.getElementById('cfg-llm-model').value = currentConfig.model || '';
      document.getElementById('cfg-passcode').value = currentConfig.passcode || '';
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
        await fetch('/api/restart', { method: 'POST' });
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
