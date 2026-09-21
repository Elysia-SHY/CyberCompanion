package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	QQAppID        string   `json:"qq_appid"`
	QQSecret       string   `json:"qq_secret"`
	OneAPIURL      string   `json:"oneapi_url"`
	OneAPIToken    string   `json:"oneapi_token"`
	Model          string   `json:"model"`
	BotName        string   `json:"bot_name"`
	SystemPrompt   string   `json:"system_prompt"`
	ActivePersona  string   `json:"active_persona"`
	Owners         []string `json:"owners"`
	OwnersFile     string   `json:"owners_file"`
	Passcode       string   `json:"passcode"`
	WebPort        int      `json:"web_port"`
	WebPassword    string   `json:"web_password"`
	TrustedProxies []string `json:"trusted_proxies,omitempty"`
	EnableStickers bool     `json:"enable_stickers"`
	MaxHistoryMsgs int      `json:"max_history_msgs"`
	TokenBudget    int      `json:"token_budget"`
	ExecWhitelist  []string `json:"exec_whitelist,omitempty"`
	EnableExec     bool     `json:"enable_exec"`
	// StreamReply 开启私聊流式输出：边生成边发，避免长回复长时间无反馈。
	// 群聊始终走一次性发送（分段追加会刷屏并可能触发风控）。
	StreamReply bool `json:"stream_reply"`

	// ── 多模型 Provider（优化建议书第十一、十二节）──────────────────────
	//
	// Providers 为空时行为与升级前完全一致：全部调用都走上面的
	// oneapi_url / oneapi_token / model 三件套。填了 providers 之后，
	// 三件套退化为「默认端点」，具体用途由 ModelRouting 决定。
	Providers []ProviderSpec `json:"providers,omitempty"`
	Routing   ModelRouting   `json:"model_routing,omitempty"`

	// ── 长期记忆（优化建议书第三节）────────────────────────────────────
	Memory MemorySettings `json:"memory,omitempty"`

	// ── 群聊策略（优化建议书第九节）────────────────────────────────────
	Groups GroupSettings `json:"groups,omitempty"`

	// ── 主动消息（优化建议书第十五节）──────────────────────────────────
	Schedule ScheduleSettings `json:"schedule,omitempty"`

	// ── MCP 外部能力（优化建议书第七节）────────────────────────────────
	//
	// MCP 让本程序能够接入生态里已有的能力服务（文件系统、Home Assistant、
	// 数据库、浏览器…），而不必为每一个都写一遍插件。远端 server 暴露的
	// 工具会被包装成本地插件注册进同一个能力清单，对模型完全透明。
	MCP MCPSettings `json:"mcp,omitempty"`
}

// MCPServerSpec 描述一个外部 MCP 服务进程。
//
// 传输方式只支持 stdio：这是 MCP 生态里最普遍、最不依赖网络配置的一种，
// 也最契合本项目「跑在设备本体的单二进制」定位 —— 远端 HTTP/SSE 传输
// 既要有网，又要处理鉴权与证书，收益在低功耗场景下并不明显。
type MCPServerSpec struct {
	// Name 是服务标识，同时用作工具名前缀的来源
	Name string `json:"name"`
	// Command 是可执行文件路径或命令名
	Command string `json:"command"`
	// Args 是传给命令的参数
	Args []string `json:"args,omitempty"`
	// Env 是追加的环境变量，格式 KEY=VALUE
	Env []string `json:"env,omitempty"`
	// Prefix 是注册到本地能力清单时给工具名加的前缀。
	// 留空则用 Name。加前缀是必要的：两个 server 都有 read_file 是常态。
	Prefix string `json:"prefix,omitempty"`
	// TimeoutSec 是单次调用的超时，留空用全局值
	TimeoutSec int `json:"timeout_sec,omitempty"`
	// MaxTools 限制该服务注册的工具数量，防止一个庞大 server 淹没提示词
	MaxTools int `json:"max_tools,omitempty"`
	// MinRole 是调用该服务全部工具所需的最低权限档位，默认 trusted。
	//
	// 这个字段的存在是因为 MCP 服务的能力差别极大：一个天气服务谁都能问，
	// 一个文件系统服务却等同于把磁盘交出去。统一按最低档放行是不负责任的，
	// 统一按最高档要求又会让普通服务没法用。
	MinRole string `json:"min_role,omitempty"`
	// Enabled 为 false 时跳过该服务
	Enabled *bool `json:"enabled,omitempty"`
}

// MCPSettings 是 MCP 子系统的全局配置。
type MCPSettings struct {
	// Enabled 是总开关，默认关闭。
	//
	// 与记忆、调度这些默认开启的功能不同：MCP 会启动外部进程、执行外部代码，
	// 属于「必须由用户明确开启」的能力，不该因为升级就悄悄跑起来。
	Enabled *bool `json:"enabled,omitempty"`
	// Servers 是全部外部服务
	Servers []MCPServerSpec `json:"servers,omitempty"`
	// InitTimeoutSec 是握手超时
	InitTimeoutSec int `json:"init_timeout_sec,omitempty"`
	// CallTimeoutSec 是单次工具调用超时
	CallTimeoutSec int `json:"call_timeout_sec,omitempty"`
	// MaxToolsPerServer 是单个服务的工具数上限（全局兜底值）
	MaxToolsPerServer int `json:"max_tools_per_server,omitempty"`
}

// MCPEnabled 报告是否启用 MCP（默认关闭）。
func (m MCPSettings) MCPEnabled() bool { return BoolOr(m.Enabled, false) }

// InitTimeout 返回握手超时。
func (m MCPSettings) InitTimeout() time.Duration {
	if m.InitTimeoutSec <= 0 {
		return 20 * time.Second
	}
	return time.Duration(m.InitTimeoutSec) * time.Second
}

// CallTimeout 返回调用超时。
func (m MCPSettings) CallTimeout() time.Duration {
	if m.CallTimeoutSec <= 0 {
		return 45 * time.Second
	}
	return time.Duration(m.CallTimeoutSec) * time.Second
}

// MaxTools 返回单个服务的工具数上限。
//
// 默认 24：工具清单要整体塞进系统提示词，一个 server 挂上几百个工具
// 会把上下文吃光，也会让模型的选择变得不稳定。
func (m MCPSettings) MaxTools() int {
	if m.MaxToolsPerServer <= 0 {
		return 24
	}
	return m.MaxToolsPerServer
}

// ProviderSpec 描述一个 OpenAI 兼容的模型服务端点。
//
// 不为每家服务商写适配器：DeepSeek、OpenAI、Gemini 的兼容模式、Ollama、
// 各类中转网关都提供 /chat/completions，差异只在地址、密钥与模型名。
type ProviderSpec struct {
	// Name 是端点标识，被 ModelRouting 引用
	Name string `json:"name"`
	// URL 是完整的 chat completions 地址
	URL string `json:"url"`
	// Token 为 Bearer 凭据；本地 Ollama 之类可留空
	Token string `json:"token,omitempty"`
	// DefaultModel 是该端点未按用途指定模型时的默认值
	DefaultModel string `json:"default_model,omitempty"`
	// Models 按用途指定模型名，键为 chat/complex/code/vision/extract
	Models map[string]string `json:"models,omitempty"`
}

// ModelRouting 把「用途」映射到「哪个端点的哪个模型」。
//
// 取值格式：「providerName」或「providerName/modelName」。
// 留空表示该用途回落到默认端点。
type ModelRouting struct {
	Chat    string `json:"chat,omitempty"`
	Complex string `json:"complex,omitempty"`
	Code    string `json:"code,omitempty"`
	Vision  string `json:"vision,omitempty"`
	// Extract 用于记忆提炼与摘要：这是高频后台动作，建议指向最便宜的模型
	Extract string `json:"extract,omitempty"`
}

// MemorySettings 是长期记忆系统的配置。
type MemorySettings struct {
	// Enabled 为记忆系统总开关
	Enabled *bool `json:"enabled,omitempty"`
	// RecallLimit 每次注入上下文的记忆条数
	RecallLimit int `json:"recall_limit,omitempty"`
	// ExtractEnabled 是否在对话结束后自动提炼记忆
	ExtractEnabled *bool `json:"extract_enabled,omitempty"`
	// ExtractBatch 累积多少条新对话才触发一次提炼
	ExtractBatch int `json:"extract_batch,omitempty"`
	// SummaryThreshold 会话累计消息超过此值时生成摘要
	SummaryThreshold int `json:"summary_threshold,omitempty"`
	// MaxPerOwner 单个分域的记忆条数上限
	MaxPerOwner int `json:"max_per_owner,omitempty"`
	// MinImportance 低于此重要性的记忆不注入上下文
	MinImportance float64 `json:"min_importance,omitempty"`
	// InjectPrompt 是否把检索到的记忆注入系统提示词
	InjectPrompt *bool `json:"inject_prompt,omitempty"`
}

// GroupSettings 是群聊行为策略。
type GroupSettings struct {
	// ReplyChance 未被 @ 时的回复概率（0~100）。
	// 群聊里每条消息都回会迅速招致反感，因此默认压低。
	ReplyChance int `json:"reply_chance,omitempty"`
	// AlwaysReplyAt 被 @ 或提到机器人名字时是否必定回复
	AlwaysReplyAt *bool `json:"always_reply_at,omitempty"`
	// IsolateMemory 群记忆是否按群隔离（强烈建议保持开启）
	IsolateMemory *bool `json:"isolate_memory,omitempty"`
	// Enabled 是否在群里工作
	Enabled *bool `json:"enabled,omitempty"`
	// MaxRepliesPerMinute 单群每分钟回复上限，防止刷屏被风控
	MaxRepliesPerMinute int `json:"max_replies_per_minute,omitempty"`
}

// ScheduleSettings 是主动消息系统的配置。
type ScheduleSettings struct {
	// Enabled 为调度器总开关
	Enabled *bool `json:"enabled,omitempty"`
	// CheckIntervalSec 调度器轮询间隔（秒）
	CheckIntervalSec int `json:"check_interval_sec,omitempty"`
	// QuietHoursStart / End 免打扰时段（0~23 小时）。
	// 主动推送在深夜把人吵醒，是这类功能最容易被关掉的原因。
	QuietHoursStart int `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd   int `json:"quiet_hours_end,omitempty"`
}

var (
	mu             sync.RWMutex
	instance       *Config
	configFilePath string
)

const (
	filePerm = 0o600 // 配置文件含 AppSecret / API Key / 口令，必须仅属主可读

	// minPasswordLen 面板密码最小长度。与 Validate 里的阈值保持一致，
	// 避免两处各写一个数字后逐渐分叉。
	minPasswordLen = 8
	maxPasswordLen = 128

	// legacyDefaultPasscode 是历史版本内置的硬编码主人口令。
	// 由于源码公开，该口令等同于「任何人都是主人」，加载时会被清空并要求重新设置。
	legacyDefaultPasscode = "复活吧我的爱人！！！elyisa"
)

// GeneratePasscode 生成一个高熵随机口令。
// 之前版本硬编码 "复活吧我的爱人！！！elyisa"，任何读过源码的人都能直接拿到 owner 权限。
func GeneratePasscode() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	const length = 16
	var sb strings.Builder
	sb.Grow(length)
	for i := 0; i < length; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			// crypto/rand 失败属于系统级异常，退化为时间派生值保证仍不可预测
			n = big.NewInt(time.Now().UnixNano() % int64(len(alphabet)))
		}
		sb.WriteByte(alphabet[n.Int64()])
	}
	return sb.String()
}

// DefaultConfig provides sane defaults for zero-config startup
//
// 注意 Passcode 默认为空：它是「主人认证口令」，由用户自行设置。
// 空口令意味着主人模式默认关闭 —— 这比内置一个所有人都知道的口令安全得多。
func DefaultConfig() *Config {
	return &Config{
		QQAppID:        "",
		QQSecret:       "",
		OneAPIURL:      "https://api.deepseek.com/v1/chat/completions",
		OneAPIToken:    "",
		Model:          "deepseek-chat",
		BotName:        "DEEPSEEK-CHAN",
		SystemPrompt:   "你是一个温柔贴心的二次元日常陪伴少女。",
		ActivePersona:  "deepseek_chan",
		Owners:         []string{},
		OwnersFile:     "owners.json",
		Passcode:       "", // 由用户在 WebUI 或配置文件中自定义
		WebPort:        8088,
		WebPassword:    "", // 首次打开面板时由用户创建，不在后台自动生成
		EnableStickers: true,
		EnableExec:     true,
		MaxHistoryMsgs: 40,
		TokenBudget:    6000,
		// 流式对体验提升明显（长回复不再"石沉大海"），默认开启。
		// 若网关不支持 SSE，llm 包会自动回退到非流式，无需用户干预。
		StreamReply: true,
	}
}

// normalize 补齐缺失或不合法的字段，返回是否发生了修改。
//
// fromDisk 表示配置来自磁盘文件：
//   - 来自磁盘时，Passcode 为空视为「用户还没设置」，保持为空
//   - 首次创建时同样保持为空，由用户自行设置
//
// WebPassword 同样保持为空，由面板引导用户创建（见下方说明）。
func normalize(cfg *Config, fromDisk bool) bool {
	changed := false

	if cfg.WebPort <= 0 {
		cfg.WebPort = 8088
		changed = true
	}
	// 历史版本内置的硬编码主人口令等同于「人人都是主人」，直接清空要求重新设置
	if cfg.Passcode == legacyDefaultPasscode {
		cfg.Passcode = ""
		changed = true
	}
	// 管理密码为空表示「尚未完成初始化」，由面板首次打开时引导用户创建。
	//
	// 旧行为是在这里自动生成一串随机密码写进 config.json。这在有终端的机器上
	// 只是麻烦，但在 Android / 随身 WiFi 这类没有 shell 的设备上是致命的：
	// 用户打不开 config.json，打开面板只会看到一个永远答不对的登录框。
	// 改为由用户自己创建后，密码就成了用户真正知道的东西。
	//
	// 未设置密码期间不存在裸奔窗口：/api/login 会拒绝空密码登录，
	// 其余 /api/* 均要求已登录会话，因此此时无人能读写任何配置。

	if cfg.MaxHistoryMsgs <= 0 {
		cfg.MaxHistoryMsgs = 40
		changed = true
	}
	if cfg.TokenBudget <= 0 {
		cfg.TokenBudget = 6000
		changed = true
	}
	if cfg.OwnersFile == "" {
		cfg.OwnersFile = "owners.json"
		changed = true
	}
	_ = fromDisk
	return changed
}

// LoadConfig reads config from path, creating default if not exists
func LoadConfig(path string) (*Config, error) {
	mu.Lock()
	defer mu.Unlock()

	configFilePath = path

	if _, err := os.Stat(path); os.IsNotExist(err) {
		cfg := DefaultConfig()
		// 首次启动同样走一遍默认值补齐（端口、历史条数等）
		normalize(cfg, false)
		if err := persistLocked(cfg); err != nil {
			return nil, fmt.Errorf("failed to write default config: %w", err)
		}
		instance = cfg
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		// 尝试从备份恢复：写入中断是旧版本最常见的故障原因
		if bak, bakErr := os.ReadFile(path + ".bak"); bakErr == nil {
			recovered := DefaultConfig()
			if err2 := json.Unmarshal(bak, recovered); err2 == nil {
				instance = recovered
				return recovered, fmt.Errorf("配置损坏，已从 %s.bak 恢复（原始错误: %w）", path, err)
			}
		}
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	// 兼容旧配置：缺失字段补齐为安全默认值
	normalized := normalize(cfg, true)

	// Load owners from owners_file if specified
	if cfg.OwnersFile != "" {
		if ownerData, err := os.ReadFile(cfg.OwnersFile); err == nil {
			var fileOwners []string
			if err := json.Unmarshal(ownerData, &fileOwners); err == nil {
				cfg.Owners = mergeUnique(cfg.Owners, fileOwners)
			}
		}
	}

	instance = cfg

	if normalized {
		if err := persistLocked(cfg); err != nil {
			return cfg, fmt.Errorf("config normalized but failed to persist: %w", err)
		}
	}

	// 配置文件权限过宽时自动收紧（旧版本以 0644 创建，同机其他用户可读全部密钥）
	tightenPerm(path)
	tightenPerm(cfg.OwnersFile)

	return cfg, nil
}

// tightenPerm 将权限过宽的文件收紧到 0600
func tightenPerm(path string) {
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 != 0 {
		_ = os.Chmod(path, filePerm)
	}
}

// Get returns a snapshot of the current active configuration.
// 返回值是深拷贝，调用方修改它不会影响全局状态。
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	if instance == nil {
		return DefaultConfig()
	}
	cp := *instance
	cp.Owners = append([]string(nil), instance.Owners...)
	cp.TrustedProxies = append([]string(nil), instance.TrustedProxies...)
	cp.ExecWhitelist = append([]string(nil), instance.ExecWhitelist...)
	return &cp
}

// SetForTest 供单元测试注入配置快照。
func SetForTest(cfg *Config) {
	mu.Lock()
	defer mu.Unlock()
	instance = cfg
}

// Update modifies configuration safely and persists to disk atomically
func Update(updater func(cfg *Config)) error {
	mu.Lock()
	defer mu.Unlock()

	if instance == nil {
		instance = DefaultConfig()
	}
	updater(instance)
	return persistLocked(instance)
}

// persistLocked 必须在持有 mu 写锁时调用
func persistLocked(cfg *Config) error {
	if configFilePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	// 先备份上一版，解析失败时可用来自动恢复
	if old, err := os.ReadFile(configFilePath); err == nil && len(old) > 0 {
		_ = WriteFileAtomic(configFilePath+".bak", old, filePerm)
	}
	return WriteFileAtomic(configFilePath, data, filePerm)
}

// ValidateWebPassword 校验面板密码是否可用，返回空字符串表示通过。
//
// 只做最基本的长度与空白检查：这是本机单用户面板，
// 强制大小写数字符号反而会让用户把密码记在便签上，得不偿失。
func ValidateWebPassword(pwd string) string {
	switch {
	case pwd == "":
		return "密码不能为空"
	case len(pwd) < minPasswordLen:
		return fmt.Sprintf("密码至少需要 %d 位", minPasswordLen)
	case len(pwd) > maxPasswordLen:
		return fmt.Sprintf("密码过长（上限 %d 位）", maxPasswordLen)
	case strings.TrimSpace(pwd) != pwd:
		return "密码首尾不能包含空格或换行"
	}
	return ""
}

// IsOwner checks if openid is in the owner list
func (c *Config) IsOwner(openid string) bool {
	if openid == "" {
		return false
	}
	for _, o := range c.Owners {
		if o == openid {
			return true
		}
	}
	return false
}

// AddOwner appends an owner openid if not present
func AddOwner(openid string) error {
	if openid == "" {
		return fmt.Errorf("empty openid")
	}
	return Update(func(cfg *Config) {
		for _, o := range cfg.Owners {
			if o == openid {
				return
			}
		}
		cfg.Owners = append(cfg.Owners, openid)
		if cfg.OwnersFile != "" {
			data, _ := json.MarshalIndent(cfg.Owners, "", "  ")
			_ = WriteFileAtomic(cfg.OwnersFile, data, filePerm)
		}
	})
}

// SaveOwners 供外部在批量变更 owner 后统一落盘
func SaveOwners(owners []string, ownersFile string) error {
	if ownersFile == "" {
		return nil
	}
	data, err := json.MarshalIndent(owners, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(ownersFile, data, filePerm)
}

func mergeUnique(a, b []string) []string {
	m := make(map[string]bool, len(a)+len(b))
	for _, v := range a {
		if v != "" {
			m[v] = true
		}
	}
	for _, v := range b {
		if v != "" {
			m[v] = true
		}
	}
	res := make([]string, 0, len(m))
	for k := range m {
		res = append(res, k)
	}
	return res
}

// Validate 在启动阶段做一次配置体检，返回人类可读的问题列表
func (c *Config) Validate() []string {
	var issues []string
	if c.QQAppID == "" {
		issues = append(issues, "qq_appid 未配置，机器人网关不会上线")
	}
	if c.QQSecret == "" {
		issues = append(issues, "qq_secret 未配置，无法获取 access_token")
	}
	if c.OneAPIToken == "" {
		issues = append(issues, "oneapi_token 未配置，LLM 调用会失败")
	}
	u := strings.ToLower(c.OneAPIURL)
	if c.OneAPIURL == "" {
		issues = append(issues, "oneapi_url 未配置")
	} else if !strings.HasPrefix(u, "https://") &&
		!strings.HasPrefix(u, "http://127.0.0.1") &&
		!strings.HasPrefix(u, "http://localhost") {
		issues = append(issues, "oneapi_url 使用非 HTTPS，API Key 将以明文传输")
	}
	if len(c.Passcode) == 0 {
		issues = append(issues, "passcode 未设置，主人模式（硬件状态 / 系统控制 / 长期记忆）不会开启。可在 WebUI「安全设置」或配置文件中自定义")
	} else if len(c.Passcode) < 8 {
		issues = append(issues, "passcode 过短（<8 位），存在被暴力猜解的风险")
	}
	if c.WebPassword == "" {
		issues = append(issues, "面板密码尚未创建，首次打开控制台时会引导设置")
	} else if len(c.WebPassword) < minPasswordLen {
		issues = append(issues, "web_password 过短（<8 位），管理面板存在被爆破的风险")
	}
	if c.EnableExec && len(c.ExecWhitelist) == 0 {
		issues = append(issues, "enable_exec 已开启但 exec_whitelist 为空，任意命令均可执行（建议非调试环境关闭）")
	}

	// 多 Provider 体检：路由指向不存在的端点时，用户只会看到「模型不回复」，
	// 却完全不知道原因，因此必须在启动阶段就点明。
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			issues = append(issues, "providers 中存在未命名的端点，该端点无法被 model_routing 引用")
			continue
		}
		if seen[p.Name] {
			issues = append(issues, fmt.Sprintf("providers 中端点名重复：%s（后者会覆盖前者）", p.Name))
		}
		seen[p.Name] = true
		if p.URL == "" {
			issues = append(issues, fmt.Sprintf("providers.%s 未配置 url", p.Name))
		}
		if p.Token == "" && !strings.Contains(p.URL, "127.0.0.1") && !strings.Contains(p.URL, "localhost") {
			issues = append(issues, fmt.Sprintf("providers.%s 未配置 token，远程端点会鉴权失败", p.Name))
		}
	}

	for purpose, target := range map[string]string{
		"chat": c.Routing.Chat, "complex": c.Routing.Complex,
		"code": c.Routing.Code, "vision": c.Routing.Vision,
		"extract": c.Routing.Extract,
	} {
		if target == "" {
			continue
		}
		name := target
		if idx := strings.Index(target, "/"); idx >= 0 {
			name = target[:idx]
			if target[idx+1:] == "" {
				issues = append(issues, fmt.Sprintf("model_routing.%s 的模型名为空（应为 provider/model）", purpose))
			}
		}
		if len(c.Providers) > 0 && !seen[name] {
			issues = append(issues, fmt.Sprintf("model_routing.%s 指向未定义的端点 %q", purpose, name))
		}
	}

	// 记忆与提炼：提炼走模型调用，指到一个没配好的端点会让记忆系统静默失效
	if c.Memory.MemoryEnabled() && c.Memory.MemoryExtractEnabled() && c.OneAPIToken == "" && len(c.Providers) == 0 {
		issues = append(issues, "记忆提炼已开启，但没有任何可用的模型端点（oneapi_token 为空且未配置 providers），自动记忆不会生效")
	}

	return issues
}

// ─── 新增配置段的默认值与归一化 ────────────────────────────────────────────────
//
// 这些字段用指针表达「未设置」：JSON 里的 bool 无法区分「显式填了 false」
// 和「压根没填」。若用值类型，老配置文件里缺字段时会被当成 false，
// 于是升级后记忆系统、群聊回复一律静默关闭，用户只会觉得「这版怎么变笨了」。

// BoolOr 取指针布尔的真值，未设置时返回默认值。
func BoolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// 记忆系统的默认取向：开启，但克制。
const (
	defaultMemoryRecallLimit      = 6
	defaultMemoryExtractBatch     = 8
	defaultMemorySummaryThreshold = 40
	defaultMemoryMaxPerOwner      = 300
	defaultMemoryMinImportance    = 0.15
)

// MemoryEnabled 报告记忆系统是否启用（默认启用）。
func (m MemorySettings) MemoryEnabled() bool { return BoolOr(m.Enabled, true) }

// MemoryExtractEnabled 报告是否自动提炼记忆（默认启用）。
func (m MemorySettings) MemoryExtractEnabled() bool { return BoolOr(m.ExtractEnabled, true) }

// MemoryInjectPrompt 报告是否把记忆注入提示词（默认启用）。
func (m MemorySettings) MemoryInjectPrompt() bool { return BoolOr(m.InjectPrompt, true) }

// RecallLimitOr 返回检索条数上限。
func (m MemorySettings) RecallLimitOr() int {
	if m.RecallLimit <= 0 {
		return defaultMemoryRecallLimit
	}
	return m.RecallLimit
}

// ExtractBatchOr 返回触发提炼的消息条数阈值。
func (m MemorySettings) ExtractBatchOr() int {
	if m.ExtractBatch <= 0 {
		return defaultMemoryExtractBatch
	}
	return m.ExtractBatch
}

// SummaryThresholdOr 返回触发摘要的消息条数阈值。
func (m MemorySettings) SummaryThresholdOr() int {
	if m.SummaryThreshold <= 0 {
		return defaultMemorySummaryThreshold
	}
	return m.SummaryThreshold
}

// MaxPerOwnerOr 返回单分域记忆上限。
func (m MemorySettings) MaxPerOwnerOr() int {
	if m.MaxPerOwner <= 0 {
		return defaultMemoryMaxPerOwner
	}
	return m.MaxPerOwner
}

// MinImportanceOr 返回注入过滤的重要性下限。
func (m MemorySettings) MinImportanceOr() float64 {
	if m.MinImportance <= 0 {
		return defaultMemoryMinImportance
	}
	return m.MinImportance
}

// 群聊默认策略：默认在群里工作，但只在被 @ 时必定回复，其余按概率。
const (
	defaultGroupReplyChance = 20
	defaultGroupMaxPerMin   = 6
)

// GroupEnabled 报告是否在群聊中工作（默认启用）。
func (g GroupSettings) GroupEnabled() bool { return BoolOr(g.Enabled, true) }

// AlwaysReplyAtOr 报告被 @ 时是否必定回复（默认是）。
func (g GroupSettings) AlwaysReplyAtOr() bool { return BoolOr(g.AlwaysReplyAt, true) }

// IsolateMemoryOr 报告群记忆是否隔离（默认是）。
//
// 默认必须为 true：把 A 群的聊天内容带到 B 群去，是这类机器人最严重的隐私事故。
func (g GroupSettings) IsolateMemoryOr() bool { return BoolOr(g.IsolateMemory, true) }

// ReplyChanceOr 返回未被 @ 时的回复概率（百分比）。
func (g GroupSettings) ReplyChanceOr() int {
	if g.ReplyChance <= 0 {
		return defaultGroupReplyChance
	}
	if g.ReplyChance > 100 {
		return 100
	}
	return g.ReplyChance
}

// MaxRepliesPerMinuteOr 返回单群每分钟回复上限。
func (g GroupSettings) MaxRepliesPerMinuteOr() int {
	if g.MaxRepliesPerMinute <= 0 {
		return defaultGroupMaxPerMin
	}
	return g.MaxRepliesPerMinute
}

// 调度器默认：开启，5 分钟轮询一次，22:00–08:00 免打扰。
const (
	defaultScheduleInterval = 300
)

// ScheduleEnabled 报告调度器是否启用（默认启用）。
func (s ScheduleSettings) ScheduleEnabled() bool { return BoolOr(s.Enabled, true) }

// CheckIntervalOr 返回轮询间隔，限制在 10 秒 ~ 1 小时之间。
//
// 下界防止把设备 CPU 空转成筛子，上界保证提醒不会迟到太久。
func (s ScheduleSettings) CheckIntervalOr() int {
	v := s.CheckIntervalSec
	if v <= 0 {
		return defaultScheduleInterval
	}
	if v < 10 {
		return 10
	}
	if v > 3600 {
		return 3600
	}
	return v
}

// QuietHours 返回免打扰时段，起止相同表示不设免打扰。
func (s ScheduleSettings) QuietHours() (start, end int, set bool) {
	start, end = s.QuietHoursStart, s.QuietHoursEnd
	if start == end {
		return 0, 0, false
	}
	if start < 0 || start > 23 {
		start = 0
	}
	if end < 0 || end > 23 {
		end = 0
	}
	return start, end, true
}

// ProviderByName 按名字查找 Provider 配置。
func (c *Config) ProviderByName(name string) (ProviderSpec, bool) {
	for _, p := range c.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return ProviderSpec{}, false
}

// ConfigDir 返回配置文件所在目录，供日志/审计文件定位使用
func ConfigDir() string {
	if configFilePath == "" {
		return "."
	}
	d := filepath.Dir(configFilePath)
	if d == "" {
		return "."
	}
	return d
}
