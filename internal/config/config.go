package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	DailyFile      string   `json:"daily_file"`
	Passcode       string   `json:"passcode"`
	WebPort        int      `json:"web_port"`
	Sandbox        bool     `json:"sandbox"`
	StickersDir    string   `json:"stickers_dir"`
	EnableStickers bool     `json:"enable_stickers"`
}

var (
	mu             sync.RWMutex
	instance       *Config
	configFilePath string
)

// DefaultConfig provides sane defaults for zero-config startup
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
		DailyFile:      "daily_traffic.json",
		Passcode:       "复活吧我的爱人！！！elyisa",
		WebPort:        8088,
		Sandbox:        false,
		StickersDir:    "./stickers",
		EnableStickers: true,
	}
}

// LoadConfig reads config from path, creating default if not exists
func LoadConfig(path string) (*Config, error) {
	mu.Lock()
	defer mu.Unlock()

	configFilePath = path
	cfg := DefaultConfig()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		// Try to write default template
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			_ = os.MkdirAll(dir, 0755)
		}
		data, err := json.MarshalIndent(cfg, "", "  ")
		if err == nil {
			_ = os.WriteFile(path, data, 0644)
		}
		instance = cfg
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config JSON: %w", err)
	}

	if cfg.WebPort <= 0 {
		cfg.WebPort = 8088
	}

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
	return cfg, nil
}

// Get returns the current active configuration
func Get() *Config {
	mu.RLock()
	defer mu.RUnlock()
	if instance == nil {
		return DefaultConfig()
	}
	cp := *instance
	return &cp
}

// Update modifies configuration safely and persists to disk
func Update(updater func(cfg *Config)) error {
	mu.Lock()
	defer mu.Unlock()

	if instance == nil {
		instance = DefaultConfig()
	}

	updater(instance)

	if configFilePath != "" {
		data, err := json.MarshalIndent(instance, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(configFilePath, data, 0644)
	}
	return nil
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
	return Update(func(cfg *Config) {
		for _, o := range cfg.Owners {
			if o == openid {
				return
			}
		}
		cfg.Owners = append(cfg.Owners, openid)
		if cfg.OwnersFile != "" {
			data, _ := json.MarshalIndent(cfg.Owners, "", "  ")
			_ = os.WriteFile(cfg.OwnersFile, data, 0644)
		}
	})
}

func mergeUnique(a, b []string) []string {
	m := make(map[string]bool)
	for _, v := range a {
		m[v] = true
	}
	for _, v := range b {
		m[v] = true
	}
	res := make([]string, 0, len(m))
	for k := range m {
		res = append(res, k)
	}
	return res
}
