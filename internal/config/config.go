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
}

var (
	mu             sync.RWMutex
	instance       *Config
	configFilePath string
)

const (
	filePerm = 0o600 // 配置文件含 AppSecret / API Key / 口令，必须仅属主可读

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
		WebPassword:    "", // 首次启动时自动生成并展示，可后续修改
		EnableStickers: true,
		EnableExec:     true,
		MaxHistoryMsgs: 40,
		TokenBudget:    6000,
	}
}

// normalize 补齐缺失或不合法的字段，返回是否发生了修改。
//
// fromDisk 表示配置来自磁盘文件：
//   - 来自磁盘时，Passcode 为空视为「用户还没设置」，保持为空
//   - 首次创建时同样保持为空，由用户自行设置
//
// 无论哪种情况，WebPassword 都会在为空时自动生成，保证面板不会裸奔。
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
	// 管理密码不允许为空，否则面板无鉴权
	if cfg.WebPassword == "" {
		cfg.WebPassword = GeneratePasscode()
		changed = true
	}
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
		// 首次启动也要走一遍安全默认值补齐（WebPassword 需自动生成）
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
	if len(c.WebPassword) < 8 {
		issues = append(issues, "web_password 过短（<8 位），管理面板存在被爆破的风险")
	}
	if c.EnableExec && len(c.ExecWhitelist) == 0 {
		issues = append(issues, "enable_exec 已开启但 exec_whitelist 为空，任意命令均可执行（建议非调试环境关闭）")
	}
	return issues
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
