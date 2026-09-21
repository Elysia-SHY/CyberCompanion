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
      logs: '实时运行日志',
      stickers: '表情包与图床',
      admin: '用户与记忆'
    };
    pageTitle.textContent = titles[tabId] || '控制台';

    if (tabId === 'logs') {
      fetchLogs();
    } else if (tabId === 'config') {
      fetchConfig();
    } else if (tabId === 'persona') {
      fetchPresets();
    } else if (tabId === 'stickers') {
      fetchStickers();
    } else if (tabId === 'admin') {
      fetchAdmin();
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

  // ── 认证引导 ────────────────────────────────────────────────
  // 首次运行（面板密码尚未创建）时显示「创建密码」而不是「请输入密码」。
  //
  // 服务端不再在后台自动生成一串随机密码写进 config.json：在没有终端的设备上
  // （Android / 随身 WiFi）用户根本看不到那个文件，只能面对一个答不对的登录框。
  let authMode = 'login'; // 'login' | 'setup'

  async function detectAuthMode() {
    try {
      const r = await _rawFetch('/api/auth-status');
      if (r.ok) {
        const d = await r.json();
        authMode = d.setup_required ? 'setup' : 'login';
      }
    } catch (e) {
      // 拿不到状态就按登录处理，保持原有行为
      authMode = 'login';
    }
  }

  async function showLoginOverlay(message) {
    // 已经弹出且模式未变时只更新提示，避免轮询每 3 秒重建一次输入框
    const existing = document.getElementById('cc-login-overlay');
    if (existing && existing.dataset.mode === authMode) {
      existing.style.display = 'flex';
      const errEl = document.getElementById('cc-login-err');
      if (errEl && message) errEl.textContent = message;
      return;
    }
    if (existing) existing.remove();
    await detectAuthMode();
    renderAuthOverlay(message);
  }

  function renderAuthOverlay(message) {
    const isSetup = authMode === 'setup';
    const el = document.createElement('div');
    el.id = 'cc-login-overlay';
    el.dataset.mode = authMode;
    el.innerHTML = `
      <div class="cc-login-card">
        <div class="cc-login-title">CyberCompanion</div>
        <div class="cc-login-sub">${isSetup ? '首次使用，请先创建面板密码' : '请输入面板管理密码'}</div>
        ${isSetup ? '<input type="password" id="cc-setup-input" placeholder="新密码（至少 8 位）" autocomplete="new-password">' : ''}
        ${isSetup ? '<input type="password" id="cc-setup-confirm" placeholder="再输入一次" autocomplete="new-password">' : ''}
        ${isSetup ? '' : '<input type="password" id="cc-login-input" placeholder="管理密码" autocomplete="current-password">'}
        <button id="cc-login-btn">${isSetup ? '创建并进入面板' : '登录'}</button>
        <div class="cc-login-err" id="cc-login-err"></div>
        <div class="cc-login-hint">${isSetup
          ? '密码保存在本机配置中，用于登录这个控制台'
          : '忘记密码：删除配置文件中的 web_password 字段后重启，即可重新创建'}</div>
      </div>`;
    document.body.appendChild(el);

    const errEl = document.getElementById('cc-login-err');
    const btn = document.getElementById('cc-login-btn');

    const submit = async () => {
      const pwd = isSetup
        ? document.getElementById('cc-setup-input').value
        : document.getElementById('cc-login-input').value;
      if (!pwd) return;
      if (isSetup && pwd !== document.getElementById('cc-setup-confirm').value) {
        errEl.textContent = '两次输入的密码不一致';
        return;
      }
      btn.disabled = true;
      errEl.textContent = '';
      try {
        const body = isSetup
          ? { password: pwd, confirm: document.getElementById('cc-setup-confirm').value }
          : { password: pwd };
        const headers = { 'Content-Type': 'application/json' };
        const csrfToken = readCookie('cc_csrf');
        if (csrfToken) headers['X-CSRF-Token'] = csrfToken;
        const r = await _rawFetch(isSetup ? '/api/setup' : '/api/login', {
          method: 'POST',
          headers: headers,
          body: JSON.stringify(body)
        });
        if (r.ok) {
          el.remove();
          authRequired = false;
          location.reload();
        } else {
          let msg = isSetup ? '创建失败' : '登录失败';
          try {
            const d = await r.json();
            msg = d.error || d.message || msg;
          } catch (e) {
            try {
              const txt = await r.text();
              if (txt) msg = txt;
            } catch (_) {}
          }
          errEl.textContent = msg;
        }
      } catch (e) {
        errEl.textContent = '网络错误: ' + e.message;
      }
      btn.disabled = false;
    };

    const inputs = isSetup
      ? ['cc-setup-input', 'cc-setup-confirm']
      : ['cc-login-input'];
    inputs.forEach(id => {
      document.getElementById(id).addEventListener('keydown', (ev) => {
        if (ev.key === 'Enter') submit();
      });
    });
    btn.addEventListener('click', submit);

    el.style.display = 'flex';
    if (message) errEl.textContent = message;
    setTimeout(() => {
      const first = document.getElementById(inputs[0]);
      if (first) first.focus();
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

  // ── 更新日志 ────────────────────────────────────────────────
  // 内容直接取自仓库 CHANGELOG.md，用户不用去 GitHub 也能看到这次更新了什么
  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[c]));
  }

  // 极简 Markdown 渲染：只处理标题、无序列表与行内代码/加粗，够展示 CHANGELOG 即可。
  // 先转义再替换，避免把日志内容当成 HTML 执行。
  function inlineMarkdown(s) {
    return escapeHtml(s)
      .replace(/`([^`]+)`/g, '<code>$1</code>')
      .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
  }

  function renderMarkdown(md) {
    const lines = String(md || '').split(/\r?\n/);
    let html = '';
    let inList = false;
    const closeList = () => { if (inList) { html += '</ul>'; inList = false; } };
    for (const raw of lines) {
      const line = raw.replace(/\s+$/, '');
      if (!line.trim()) { closeList(); continue; }
      const heading = line.match(/^(#{1,6})\s+(.*)$/);
      if (heading) {
        closeList();
        // 从 h3 起，避免与页面自身的标题层级冲突
        const level = Math.min(6, heading[1].length + 2);
        html += '<h' + level + '>' + inlineMarkdown(heading[2]) + '</h' + level + '>';
      } else if (/^[-*]\s+/.test(line)) {
        if (!inList) { html += '<ul>'; inList = true; }
        html += '<li>' + inlineMarkdown(line.replace(/^[-*]\s+/, '')) + '</li>';
      } else {
        closeList();
        html += '<p>' + inlineMarkdown(line) + '</p>';
      }
    }
    closeList();
    return html || '<p>暂无更新日志</p>';
  }

  async function showChangelog() {
    let el = document.getElementById('cc-changelog-modal');
    if (!el) {
      el = document.createElement('div');
      el.id = 'cc-changelog-modal';
      el.innerHTML = `
        <div class="cc-modal-card">
          <div class="cc-modal-head">
            <span class="cc-modal-title">更新日志</span>
            <button class="cc-modal-close" id="cc-changelog-close" aria-label="关闭">×</button>
          </div>
          <div class="cc-modal-body" id="cc-changelog-body">加载中...</div>
        </div>`;
      document.body.appendChild(el);
      el.addEventListener('click', (ev) => {
        if (ev.target === el) el.style.display = 'none';
      });
      document.getElementById('cc-changelog-close').addEventListener('click', () => {
        el.style.display = 'none';
      });
    }
    el.style.display = 'flex';
    const body = document.getElementById('cc-changelog-body');
    body.innerHTML = '加载中...';
    try {
      const r = await _rawFetch('/api/changelog');
      const d = await r.json();
      body.innerHTML = renderMarkdown(d.changelog);
      const title = document.querySelector('#cc-changelog-modal .cc-modal-title');
      if (title && d.version) title.textContent = '更新日志 · v' + d.version;
    } catch (e) {
      body.innerHTML = '<p>加载失败: ' + escapeHtml(e.message) + '</p>';
    }
  }

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
      // 详细硬件信息（所有平台统一提供，拿不到的项明确标注）
      const d = data.device_info.details || {};

      document.getElementById('stat-uptime').textContent = data.device_info.uptime;
      document.getElementById('stat-hostname').textContent = `主机: ${data.device_info.hostname}`;
      document.getElementById('stat-signal').textContent = data.device_info.signal_rsrp || '未提供';
      document.getElementById('stat-network-type').textContent = data.device_info.network_type || '未提供';
      document.getElementById('stat-traffic-today').textContent = data.device_info.traffic_today || '未提供';
      document.getElementById('stat-rsrp-detail').textContent = d.signal_detail || data.device_info.signal_rsrp || '未提供';

      const ipsEl = document.getElementById('stat-network-ips');
      if (ipsEl) {
        ipsEl.textContent = (d.network_ips && d.network_ips.length) ? d.network_ips.join(', ') : '未检测到';
      }

      // CPU 占用条
      // cpu_usage 为 -1 表示「读不到 /proc/stat，或还没有第二次采样基线」，
      // 这时显示「未提供」，而不是一个看着像真读数的 0.0%。
      const cpuFill = document.getElementById('cpu-fill');
      const cpuLabel = document.getElementById('cpu-label');
      if (cpuFill && cpuLabel) {
        const rawCpu = Number(d.cpu_usage);
        const cpuCores = d.cpu_cores > 0 ? `${d.cpu_cores} 核 · ` : '';
        if (isFinite(rawCpu) && rawCpu >= 0) {
          const cpuPct = Math.max(0, Math.min(100, rawCpu));
          cpuFill.style.width = `${cpuPct}%`;
          cpuLabel.textContent = `${cpuCores}${cpuPct.toFixed(1)}%`;
        } else {
          cpuFill.style.width = '0%';
          cpuLabel.textContent = `${cpuCores}未提供`;
        }
      }

      // 内存条
      const memFill = document.getElementById('mem-fill');
      const memLabel = document.getElementById('mem-label');
      if (memFill && memLabel) {
        if (data.device_info.memory_total_mb > 0) {
          const memPct = Math.min(100, Math.round((data.device_info.memory_used_mb / data.device_info.memory_total_mb) * 100));
          memLabel.textContent = `${data.device_info.memory_used_mb} MB / ${data.device_info.memory_total_mb} MB (${memPct}%)`;
          memFill.style.width = `${memPct}%`;
        } else {
          memLabel.textContent = '本平台未提供';
          memFill.style.width = '0%';
        }
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
        if (d.cellular_band) {
          specs.push(['蜂窝频段', d.cellular_band]);
        }
        if (d.cellular_operator) {
          specs.push(['网络运营商', d.cellular_operator]);
        }
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

      const routing = currentConfig.model_routing || {};
      document.getElementById('cfg-routing-chat').value = routing.chat || '';
      document.getElementById('cfg-routing-complex').value = routing.complex || '';
      document.getElementById('cfg-routing-vision').value = routing.vision || '';
      document.getElementById('cfg-routing-code').value = routing.code || '';
      document.getElementById('cfg-routing-extract').value = routing.extract || '';

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
      model_routing: {
        chat: document.getElementById('cfg-routing-chat').value.trim(),
        complex: document.getElementById('cfg-routing-complex').value.trim(),
        vision: document.getElementById('cfg-routing-vision').value.trim(),
        code: document.getElementById('cfg-routing-code').value.trim(),
        extract: document.getElementById('cfg-routing-extract').value.trim()
      },
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

  // 更新日志入口
  const changelogBtn = document.getElementById('btn-changelog');
  if (changelogBtn) {
    changelogBtn.addEventListener('click', showChangelog);
  }

  // 侧栏版本号改为后端注入（此前前端硬编码 v1.0.0，与实际构建版本不一致）
  (async () => {
    try {
      const r = await _rawFetch('/api/auth-status');
      if (!r.ok) return;
      const d = await r.json();
      const el = document.getElementById('sidebar-version');
      if (el && d.version) {
        const v = String(d.version);
        el.textContent = (v.startsWith('v') ? v : 'v' + v) + ' · 边缘智能';
      }
    } catch (e) {
      // 版本号拿不到就保留静态占位，不影响面板功能
    }
  })();

  // ── 表情包与图床 ──────────────────────────────────────────────
  let stkScenes = [];

  function fillSceneSelect(sel, selected) {
    sel.innerHTML = '';
    stkScenes.forEach(sc => {
      const o = document.createElement('option');
      o.value = sc.scene;
      o.textContent = (sc.label && sc.label !== sc.scene ? sc.label + ' (' + sc.scene + ')' : sc.scene) + ' · ' + sc.count;
      if (sc.scene === selected) o.selected = true;
      sel.appendChild(o);
    });
  }

  async function fetchStickers() {
    try {
      const resp = await fetch('/api/stickers');
      if (!resp.ok) return;
      const d = await resp.json();

      // 概览
      const hs = d.hostStatus || {};
      const hostEl = document.getElementById('stk-host-status');
      if (hostEl) {
        if (hs.ready) hostEl.textContent = '✅ 已配置 (' + (hs.provider || '') + ')';
        else hostEl.textContent = '⚠️ 未配置（将仅使用本地表情）';
      }
      const countEl = document.getElementById('stk-count');
      if (countEl) countEl.textContent = (d.items ? d.items.length : 0) + ' 张';

      // 设置表单
      const s = d.settings || {};
      const modeEl = document.getElementById('stk-mode');
      if (modeEl && s.mode) modeEl.value = s.mode;
      const maxEl = document.getElementById('stk-max');
      if (maxEl) maxEl.value = (s.max_per_reply > 0 ? s.max_per_reply : 1);
      const smartEl = document.getElementById('stk-smart');
      if (smartEl) smartEl.checked = !!s.smart_send;
      const kwEl = document.getElementById('stk-keyword');
      if (kwEl) kwEl.checked = !!s.keyword_send;
      const cdnEl = document.getElementById('stk-cdn');
      if (cdnEl) cdnEl.value = s.cdn_prefix || '';

      // 图床配置表单
      const ih = s.image_host || {};
      setVal('stk-ih-url', ih.upload_url);
      setVal('stk-ih-method', ih.method || 'POST');
      setVal('stk-ih-field', ih.field_name);
      setVal('stk-ih-authmode', ih.auth_mode);
      setVal('stk-ih-token', ''); // 密钥不回显，留空表示不修改
      setVal('stk-ih-path', ih.result_path);
      setVal('stk-ih-base', ih.public_base);

      // 场景与下拉
      stkScenes = d.scenes || [];
      fillSceneSelect(document.getElementById('stk-up-scene'), '');
      fillSceneSelect(document.getElementById('stk-add-scene'), '');

      // 列表
      const tbody = document.getElementById('stk-tbody');
      tbody.innerHTML = '';
      (d.items || []).forEach(it => tbody.appendChild(renderStickerRow(it)));
    } catch (err) {
      showToast('获取表情数据失败: ' + err.message);
    }
  }

  function renderStickerRow(it) {
    const tr = document.createElement('tr');
    const preview = document.createElement('td');
    if (it.source === 'url' && it.url) {
      const img = document.createElement('img');
      img.src = it.url; img.className = 'stk-thumb'; img.loading = 'lazy';
      img.onerror = () => { img.replaceWith(document.createTextNode('—')); };
      preview.appendChild(img);
    } else if (it.file) {
      const img = document.createElement('img');
      img.src = '/api/stickers/media/' + it.file; img.className = 'stk-thumb'; img.loading = 'lazy';
      img.onerror = () => { img.replaceWith(document.createTextNode('—')); };
      preview.appendChild(img);
    } else {
      preview.textContent = '—';
    }

    tr.appendChild(preview);
    tr.appendChild(cell(it.id));
    tr.appendChild(cell(it.scene));
    tr.appendChild(cell(it.source === 'url' ? '直链' : '本地'));

    const srcCell = document.createElement('td');
    const srcText = it.source === 'url' ? (it.url || '') : (it.file || '');
    const srcA = document.createElement('div');
    srcA.className = 'stk-src';
    srcA.textContent = srcText;
    srcCell.appendChild(srcA);
    tr.appendChild(srcCell);

    tr.appendChild(cell(it.note || ''));

    // 启用开关
    const enCell = document.createElement('td');
    const enChk = document.createElement('input');
    enChk.type = 'checkbox';
    enChk.checked = !!it.enabled;
    enChk.addEventListener('change', () => updateSticker(it.id, { enabled: enChk.checked }));
    enCell.appendChild(enChk);
    tr.appendChild(enCell);

    // 操作
    const opCell = document.createElement('td');
    const delBtn = document.createElement('button');
    delBtn.className = 'btn btn-danger btn-sm';
    delBtn.textContent = '删除';
    delBtn.addEventListener('click', () => deleteSticker(it.id));
    opCell.appendChild(delBtn);
    tr.appendChild(opCell);

    return tr;
  }

  function cell(text) {
    const td = document.createElement('td');
    td.textContent = text;
    return td;
  }

  function setVal(id, v) {
    const el = document.getElementById(id);
    if (el && v !== undefined && v !== null) el.value = v;
  }

  function collectStickerSettings() {
    return {
      mode: document.getElementById('stk-mode').value,
      max_per_reply: parseInt(document.getElementById('stk-max').value, 10) || 1,
      smart_send: document.getElementById('stk-smart').checked,
      keyword_send: document.getElementById('stk-keyword').checked,
      cdn_prefix: document.getElementById('stk-cdn').value.trim(),
      image_host: {
        provider: document.getElementById('stk-ih-provider').value || 'custom',
        upload_url: document.getElementById('stk-ih-url').value.trim(),
        method: document.getElementById('stk-ih-method').value || 'POST',
        field_name: document.getElementById('stk-ih-field').value.trim(),
        auth_mode: document.getElementById('stk-ih-authmode').value || 'none',
        token: document.getElementById('stk-ih-token').value.trim(),
        result_path: document.getElementById('stk-ih-path').value.trim(),
        public_base: document.getElementById('stk-ih-base').value.trim()
      }
    };
  }

  async function saveStickerSettings() {
    try {
      const resp = await fetch('/api/stickers/settings', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(collectStickerSettings())
      });
      if (resp.ok) {
        showToast('🎉 表情规则与图床配置已保存');
        fetchStickers();
      } else {
        showToast('保存失败: ' + await resp.text());
      }
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  }

  document.getElementById('btn-save-stk-settings').addEventListener('click', saveStickerSettings);
  document.getElementById('btn-save-stk-host').addEventListener('click', saveStickerSettings);

  async function testStickerHost() {
    const resultEl = document.getElementById('stk-host-test-result');
    resultEl.textContent = '测试中...';
    try {
      const resp = await fetch('/api/stickers/host-test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ image_host: collectStickerSettings().image_host })
      });
      const d = await resp.json();
      if (d.error) {
        resultEl.innerHTML = '❌ 失败 (' + (d.status_code || 0) + ')：' + escapeHtml(d.error) +
          '<br><small>原始响应：' + escapeHtml(d.raw_body || '(空)') + '</small>';
      } else {
        resultEl.innerHTML = '✅ 成功 (' + (d.elapsed_ms || 0) + 'ms)：<a href="' + escapeHtml(d.url) +
          '" target="_blank" class="link">' + escapeHtml(d.url) + '</a>' +
          (d.raw_url && d.raw_url !== d.url ? '<br><small>原始：' + escapeHtml(d.raw_url) + '</small>' : '');
      }
    } catch (err) {
      resultEl.textContent = '网络错误: ' + err.message;
    }
  }
  document.getElementById('btn-stk-host-test').addEventListener('click', testStickerHost);

  async function uploadStickers() {
    const files = document.getElementById('stk-up-files').files;
    if (!files || files.length === 0) {
      showToast('⚠️ 请先选择图片');
      return;
    }
    const fd = new FormData();
    for (const f of files) fd.append('files', f);
    fd.append('scene', document.getElementById('stk-up-scene').value);
    fd.append('note', document.getElementById('stk-up-note').value.trim());
    fd.append('target', document.getElementById('stk-up-target').value);
    fd.append('drop_local', document.getElementById('stk-up-drop').checked ? '1' : '0');
    try {
      const resp = await fetch('/api/stickers/upload', { method: 'POST', body: fd });
      const d = await resp.json();
      if (resp.ok) {
        const n = (d.created || []).length;
        let msg = '✅ 成功上传 ' + n + ' 张';
        if (d.warnings && d.warnings.length) msg += '（' + d.warnings.length + ' 个跳过）';
        showToast(msg);
        fetchStickers();
      } else {
        showToast('上传失败: ' + JSON.stringify(d));
      }
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  }
  document.getElementById('btn-stk-upload').addEventListener('click', uploadStickers);

  async function addDirectSticker() {
    const url = document.getElementById('stk-add-url').value.trim();
    if (!url) { showToast('⚠️ 请填写图床直链'); return; }
    const body = {
      id: document.getElementById('stk-add-id').value.trim(),
      scene: document.getElementById('stk-add-scene').value,
      source: 'url',
      url: url,
      enabled: true
    };
    try {
      const resp = await fetch('/api/stickers', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body)
      });
      if (resp.ok) {
        showToast('✅ 已添加直链表情');
        document.getElementById('stk-add-url').value = '';
        document.getElementById('stk-add-id').value = '';
        fetchStickers();
      } else {
        showToast('添加失败: ' + await resp.text());
      }
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  }
  document.getElementById('btn-stk-add').addEventListener('click', addDirectSticker);

  async function updateSticker(id, fields) {
    try {
      const resp = await fetch('/api/stickers', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(Object.assign({ id }, fields))
      });
      if (!resp.ok) showToast('更新失败: ' + await resp.text());
      else fetchStickers();
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  }

  async function deleteSticker(id) {
    if (!confirm('确定删除表情 ' + id + ' 吗？')) return;
    try {
      const resp = await fetch('/api/stickers?id=' + encodeURIComponent(id), { method: 'DELETE' });
      if (resp.ok) {
        showToast('🗑️ 已删除 ' + id);
        fetchStickers();
      } else {
        showToast('删除失败: ' + await resp.text());
      }
    } catch (err) {
      showToast('网络错误: ' + err.message);
    }
  }

  // ── 用户与记忆（管理面板）─────────────────────────────────────
  //
  // 这一块是数据库落地后的新增能力：用户档案、长期记忆、模型用量、
  // 能力开关、主动消息、调试信息。全部走 /api/admin/*，需要已登录面板。
  const ROLE_LABELS = { owner: '主人', admin: '管理员', trusted: '可信用户', guest: '访客' };
  const CATEGORY_LABELS = {
    preference: '偏好', fact: '事实', event: '经历',
    relation: '关系', skill: '技能', instruction: '长期要求'
  };

  let adminLoaded = false;

  async function fetchJSON(url) {
    const r = await fetch(url);
    if (!r.ok) {
      let msg = 'HTTP ' + r.status;
      try { const d = await r.json(); if (d.error) msg = d.error; } catch (e) { /* 非 JSON 响应 */ }
      throw new Error(msg);
    }
    return r.json();
  }

  async function postJSON(url, body, method) {
    const r = await fetch(url, {
      method: method || 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body)
    });
    if (!r.ok) {
      let msg = 'HTTP ' + r.status;
      try { const d = await r.json(); if (d.error) msg = d.error; } catch (e) { /* 同上 */ }
      throw new Error(msg);
    }
    return r.json();
  }

  // fetchAdmin 拉取全部管理数据。
  //
  // 用 Promise.allSettled 而不是 Promise.all：任一接口失败（比如记忆表为空、
  // 调度模块未启用）不应该让整个面板显示为错误，其余部分照常渲染。
  async function fetchAdmin() {
    if (!adminLoaded) {
      // 首次进入时回填推送目标与记忆归属者的默认值，省去用户手动复制 OpenID
      prefillAdminInputs();
    }
    adminLoaded = true;

    const results = await Promise.allSettled([
      fetchJSON('/api/admin/users'),
      fetchJSON('/api/admin/usage?days=14'),
      fetchJSON('/api/admin/plugins'),
      fetchJSON('/api/admin/schedules'),
      fetchJSON('/api/admin/debug')
    ]);

    const [users, usage, plugins, schedules, debug] = results.map(r =>
      r.status === 'fulfilled' ? r.value : null);

    if (users) renderAdminUsers(users);
    if (usage) renderAdminUsage(usage);
    if (plugins) renderAdminPlugins(plugins);
    if (schedules) renderAdminSchedules(schedules);
    if (debug) renderAdminDebug(debug);

    renderAdminStats(debug, usage);
  }

  // prefillAdminInputs 用当前唯一的 owner 预填输入框。
  function prefillAdminInputs() {
    (async () => {
      try {
        const d = await fetchJSON('/api/admin/users');
        const owner = (d.users || []).find(u => u.role === 'owner');
        if (!owner) return;
        const memOwner = document.getElementById('memory-owner');
        const schedOwner = document.getElementById('sched-owner');
        if (memOwner && !memOwner.value) memOwner.value = owner.openid;
        if (schedOwner && !schedOwner.value) schedOwner.value = owner.openid;
      } catch (e) { /* 预填失败不影响使用 */ }
    })();
  }

  function renderAdminStats(debug, usage) {
    const set = (id, v) => {
      const el = document.getElementById(id);
      if (el) el.textContent = v === undefined || v === null ? '-' : v;
    };

    const counts = (debug && debug.counts) || {};
    set('stat-users', counts.users);
    set('stat-memories', counts.memories);
    set('stat-messages', counts.messages);
    set('stat-sessions', (counts.sessions || 0) + (counts.live_sessions || 0));

    if (usage) {
      set('stat-calls', usage.calls);
      const tokens = (usage.prompt_tokens || 0) + (usage.output_tokens || 0);
      set('stat-tokens', tokens > 10000 ? (tokens / 1000).toFixed(1) + 'k' : tokens);
    }
  }

  function renderAdminUsage(usage) {
    const tbody = document.querySelector('#admin-usage-table tbody');
    if (!tbody) return;
    const rows = usage.by_model || [];
    if (rows.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">近 ' + usage.days + ' 天没有调用记录</td></tr>';
      return;
    }
    tbody.innerHTML = rows.map(m => `
      <tr>
        <td>${escapeHtml(m.model || '(默认)')}</td>
        <td>${m.calls}</td>
        <td>${m.failures > 0 ? '<span class="badge badge-warn">' + m.failures + '</span>' : '0'}</td>
        <td>${m.prompt_tokens}</td>
        <td>${m.output_tokens}</td>
        <td>${m.avg_latency_ms} ms</td>
      </tr>`).join('');
  }

  function renderAdminUsers(data) {
    const tbody = document.querySelector('#admin-users-table tbody');
    if (!tbody) return;
    const users = data.users || [];
    if (users.length === 0) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">还没有任何用户发言过</td></tr>';
      return;
    }

    tbody.innerHTML = users.map(u => {
      const opts = ['owner', 'admin', 'trusted', 'guest'].map(r =>
        `<option value="${r}"${u.role === r ? ' selected' : ''}>${ROLE_LABELS[r]}</option>`).join('');
      return `<tr>
        <td class="mono">${escapeHtml(u.openid)}</td>
        <td>${escapeHtml(u.nickname || '—')}</td>
        <td><span class="badge badge-role-${u.role}">${ROLE_LABELS[u.role] || u.role}</span></td>
        <td>${u.msg_count}</td>
        <td class="mono-sm">${escapeHtml((u.last_seen || '').replace('T', ' ').slice(0, 16)) || '—'}</td>
        <td><select class="role-select" data-openid="${escapeHtml(u.openid)}">${opts}</select></td>
      </tr>`;
    }).join('');

    tbody.querySelectorAll('.role-select').forEach(sel => {
      sel.addEventListener('change', async () => {
        const openid = sel.getAttribute('data-openid');
        try {
          const d = await postJSON('/api/admin/users/role', { openid: openid, role: sel.value });
          showToast(d.changed ? '✅ 已更新权限' : '权限未变化');
          fetchAdmin();
        } catch (e) {
          showToast('❌ 更新失败: ' + e.message);
          fetchAdmin();
        }
      });
    });
  }

  function renderAdminPlugins(data) {
    const box = document.getElementById('admin-plugins');
    if (!box) return;
    const list = data.plugins || [];
    if (list.length === 0) {
      box.innerHTML = '<div class="empty-hint">没有注册任何能力</div>';
      return;
    }
    box.innerHTML = list.map(p => `
      <div class="admin-plugin-row">
        <div class="admin-plugin-info">
          <div class="admin-plugin-name">${escapeHtml(p.name)}
            <span class="badge">${escapeHtml(p.min_role_label || p.min_role)}</span>
          </div>
          <div class="admin-plugin-desc">${escapeHtml(p.description || '')}</div>
        </div>
        <label class="switch">
          <input type="checkbox" class="plugin-toggle" data-name="${escapeHtml(p.name)}"${p.enabled ? ' checked' : ''}>
          <span class="switch-slider"></span>
        </label>
      </div>`).join('');

    box.querySelectorAll('.plugin-toggle').forEach(cb => {
      cb.addEventListener('change', async () => {
        try {
          await postJSON('/api/admin/plugins', { name: cb.getAttribute('data-name'), enabled: cb.checked });
          showToast(cb.checked ? '✅ 已启用' : '⏸ 已停用');
        } catch (e) {
          showToast('❌ 操作失败: ' + e.message);
          fetchAdmin();
        }
      });
    });
  }

  function renderAdminSchedules(data) {
    const tbody = document.querySelector('#admin-schedules-table tbody');
    if (!tbody) return;
    const list = data.schedules || [];
    if (list.length === 0) {
      tbody.innerHTML = '<tr><td colspan="7" class="empty-hint">暂无主动消息任务</td></tr>';
      return;
    }

    const KIND_LABELS = { remind: '固定文本', prompt: '模型生成', plugin: '调用能力' };
    const fmt = t => (t || '').replace('T', ' ').slice(0, 16) || '—';

    tbody.innerHTML = list.map(s => `
      <tr>
        <td>${escapeHtml(s.name)}</td>
        <td>${KIND_LABELS[s.kind] || escapeHtml(s.kind)}</td>
        <td class="mono-sm">${escapeHtml(s.cron || '一次性')}</td>
        <td class="mono-sm">${fmt(s.next_run)}</td>
        <td>${s.run_count}</td>
        <td>${s.enabled ? '<span class="badge badge-ok">启用</span>' : '<span class="badge">停用</span>'}</td>
        <td class="admin-actions">
          <button class="btn btn-sm btn-outline sched-toggle" data-id="${s.id}" data-enabled="${s.enabled}">
            ${s.enabled ? '暂停' : '启用'}
          </button>
          <button class="btn btn-sm btn-danger sched-del" data-id="${s.id}">删除</button>
        </td>
      </tr>`).join('');

    tbody.querySelectorAll('.sched-toggle').forEach(btn => {
      btn.addEventListener('click', async () => {
        const enabled = btn.getAttribute('data-enabled') !== 'true';
        try {
          await postJSON('/api/admin/schedules/toggle', { id: Number(btn.getAttribute('data-id')), enabled: enabled });
          fetchAdmin();
        } catch (e) { showToast('❌ ' + e.message); }
      });
    });
    tbody.querySelectorAll('.sched-del').forEach(btn => {
      btn.addEventListener('click', async () => {
        try {
          await postJSON('/api/admin/schedules?id=' + btn.getAttribute('data-id'), undefined, 'DELETE');
          showToast('🗑️ 已删除任务');
          fetchAdmin();
        } catch (e) { showToast('❌ ' + e.message); }
      });
    });
  }

  function renderAdminDebug(debug) {
    const el = document.getElementById('admin-debug');
    if (!el) return;
    const breakers = Object.entries(debug.breakers_open || {})
      .map(([k, v]) => k + (v ? '：冷却中' : '：正常'))
      .join('\n    ') || '（暂无记录）';
    const routing = Object.entries(debug.routing || {})
      .map(([k, v]) => k + ' → ' + v).join('\n    ') || '（未解析）';

    // MCP 外部服务：连接失败的条目也要显示，且带上原因 ——
    // 「没接上」和「接上了但没工具」是两类完全不同的问题。
    const mcp = (debug.mcp || []).map(s => {
      if (!s.connected) {
        return '    ' + s.name + '：未连接 — ' + (s.error || '未知原因');
      }
      const tools = (s.tools || []).join(', ') || '（无）';
      return '    ' + s.name + ' → ' + (s.server_name || '?') + ' ' + (s.version || '') +
        '（协议 ' + (s.protocol || '?') + '，最低权限 ' + s.min_role + '）\n' +
        '      工具：' + tools;
    }).join('\n') || '    （未启用或未配置）';

    el.textContent =
      '结构版本   : v' + debug.schema_version + '\n' +
      '数据库      : ' + debug.db_path + '\n' +
      '记忆系统    : ' + (debug.memory_enabled ? '启用' : '停用') + '\n' +
      '各表行数    : ' + JSON.stringify(debug.counts) + '\n' +
      '模型路由    :\n    ' + routing + '\n' +
      '端熔断状态  :\n    ' + breakers + '\n' +
      '已注册能力  : ' + (debug.plugins || []).join(', ') + '\n' +
      '外部 MCP    :\n' + mcp + '\n' +
      '群频率记录  : ' + debug.groups_active + ' 个群';
  }

  // ── 记忆浏览 ──
  async function fetchMemories() {
    const scope = document.getElementById('memory-scope').value;
    const owner = document.getElementById('memory-owner').value.trim();
    const query = document.getElementById('memory-query').value.trim();
    const tbody = document.querySelector('#admin-memory-table tbody');

    if (!owner) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">请先填写归属者 OpenID</td></tr>';
      return;
    }

    tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">查询中…</td></tr>';

    try {
      const url = '/api/admin/memories?scope=' + encodeURIComponent(scope) +
        '&owner=' + encodeURIComponent(owner) +
        '&q=' + encodeURIComponent(query) + '&limit=100';
      const d = await fetchJSON(url);
      const items = d.memories || [];

      if (items.length === 0) {
        tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">' +
          (query ? '没有匹配「' + escapeHtml(query) + '」的记忆' : '这个对话边界还没有记忆') + '</td></tr>';
        return;
      }

      tbody.innerHTML = items.map(m => `
        <tr>
          <td>${escapeHtml(m.content)}</td>
          <td>${CATEGORY_LABELS[m.category] || escapeHtml(m.category)}</td>
          <td>${Math.round((m.importance || 0) * 100)}%</td>
          <td>${m.source === 'manual' ? '手工' : '自动'}</td>
          <td class="mono-sm">${escapeHtml((m.created_at || '').replace('T', ' ').slice(0, 16))}</td>
          <td><button class="btn btn-sm btn-danger mem-del" data-id="${m.id}">删除</button></td>
        </tr>`).join('');

      tbody.querySelectorAll('.mem-del').forEach(btn => {
        btn.addEventListener('click', async () => {
          try {
            await postJSON('/api/admin/memories?id=' + btn.getAttribute('data-id'), undefined, 'DELETE');
            showToast('🗑️ 已删除该条记忆');
            fetchMemories();
          } catch (e) { showToast('❌ ' + e.message); }
        });
      });
    } catch (e) {
      tbody.innerHTML = '<tr><td colspan="6" class="empty-hint">查询失败：' + escapeHtml(e.message) + '</td></tr>';
    }
  }

  async function addMemory() {
    const scope = document.getElementById('memory-scope').value;
    const owner = document.getElementById('memory-owner').value.trim();
    const contentEl = document.getElementById('memory-new-content');
    const content = contentEl.value.trim();

    if (!owner || !content) {
      showToast('请填写归属者与记忆内容');
      return;
    }
    try {
      await postJSON('/api/admin/memories', {
        scope: scope, owner: owner, content: content,
        category: document.getElementById('memory-new-category').value,
        importance: 0.7
      });
      contentEl.value = '';
      showToast('✅ 已写入记忆');
      fetchMemories();
    } catch (e) { showToast('❌ ' + e.message); }
  }

  async function addSchedule() {
    const name = document.getElementById('sched-name').value.trim();
    const owner = document.getElementById('sched-owner').value.trim();
    const kind = document.getElementById('sched-kind').value;
    const value = document.getElementById('sched-text').value.trim();

    if (!name || !owner) {
      showToast('请填写任务名称与推送目标');
      return;
    }
    // 三种任务类型的「内容」落在不同字段上，这里按类型分发
    const body = {
      name: name, kind: kind, owner_id: owner, scope: 'private',
      cron: document.getElementById('sched-cron').value.trim(),
      text: kind === 'remind' ? value : '',
      prompt: kind === 'prompt' ? value : '',
      plugin: kind === 'plugin' ? value : '',
      enabled: true
    };

    try {
      await postJSON('/api/admin/schedules', body);
      showToast('✅ 已创建任务');
      ['sched-name', 'sched-text'].forEach(id => { document.getElementById(id).value = ''; });
      fetchAdmin();
    } catch (e) { showToast('❌ ' + e.message); }
  }

  // 事件绑定（只绑一次）
  const adminRefresh = document.getElementById('admin-btn-refresh');
  if (adminRefresh) adminRefresh.addEventListener('click', fetchAdmin);

  const memLoad = document.getElementById('memory-btn-load');
  if (memLoad) memLoad.addEventListener('click', fetchMemories);

  const memAdd = document.getElementById('memory-btn-add');
  if (memAdd) memAdd.addEventListener('click', addMemory);

  const schedAdd = document.getElementById('sched-btn-add');
  if (schedAdd) schedAdd.addEventListener('click', addSchedule);

  const debugBtn = document.getElementById('admin-btn-debug');
  if (debugBtn) debugBtn.addEventListener('click', () => {
    const el = document.getElementById('admin-debug');
    el.hidden = !el.hidden;
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
