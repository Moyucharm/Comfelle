package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 表示整个服务的运行时配置。
type Config struct {
	Server ServerConfig `yaml:"server"`
	Auth   AuthConfig   `yaml:"auth"`
	Colab  ColabConfig  `yaml:"colab"`
}

type ServerConfig struct {
	Listen             string `yaml:"listen"`
	TrustProxy         bool   `yaml:"trust_proxy"`
	CookieSecure       bool   `yaml:"cookie_secure"`
	EnableFloatingDock bool   `yaml:"enable_floating_dock"`
}

type AuthConfig struct {
	Username        string `yaml:"username"`
	PasswordHash    string `yaml:"password_hash"`
	SessionSecret   string `yaml:"session_secret"`
	SessionTTLHours int    `yaml:"session_ttl_hours"`
}

type ColabConfig struct {
	TokenFile            string `yaml:"token_file"`
	StateFile            string `yaml:"state_file"`
	DefaultGPU           string `yaml:"default_gpu"`
	HighMem              bool   `yaml:"high_mem"`
	RcloneConfig         string `yaml:"rclone_config"`
	RcloneRemote         string `yaml:"rclone_remote"`
	AuthUser             string `yaml:"auth_user"`
	KeepaliveIntervalSec int    `yaml:"keepalive_interval_sec"`
	HFToken              string `yaml:"hf_token"`
	CivitaiToken         string `yaml:"civitai_token"`
}

// Default 返回带安全默认值的配置。
func Default() *Config {
	return &Config{
		Server: ServerConfig{
			Listen:             "127.0.0.1:8080",
			TrustProxy:         true,
			CookieSecure:       true,
			EnableFloatingDock: true,
		},
		Auth: AuthConfig{
			Username:        "admin",
			SessionTTLHours: 168,
		},
		Colab: ColabConfig{
			TokenFile:            "./google_token.json",
			StateFile:            "./state.json",
			DefaultGPU:           "L4",
			AuthUser:             "0",
			KeepaliveIntervalSec: 480,
		},
	}
}

// Load 从 YAML 文件加载配置，并允许通过环境变量覆盖关键项。
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %q 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %q 失败: %w", path, err)
	}
	applyEnvOverrides(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnvOverrides(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("COMFY_PROXY_LISTEN")); v != "" {
		cfg.Server.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_PROXY_USERNAME")); v != "" {
		cfg.Auth.Username = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_PROXY_PASSWORD_HASH")); v != "" {
		cfg.Auth.PasswordHash = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_PROXY_SESSION_SECRET")); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_COLAB_TOKEN_FILE")); v != "" {
		cfg.Colab.TokenFile = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_RCLONE_CONFIG")); v != "" {
		cfg.Colab.RcloneConfig = v
	}
	if v := strings.TrimSpace(os.Getenv("COMFY_RCLONE_REMOTE")); v != "" {
		cfg.Colab.RcloneRemote = v
	}
	if v := strings.TrimSpace(os.Getenv("HF_TOKEN")); v != "" {
		cfg.Colab.HFToken = v
	}
	if v := strings.TrimSpace(os.Getenv("CIVITAI_TOKEN")); v != "" {
		cfg.Colab.CivitaiToken = v
	}
}

// Validate 校验配置项合法性。
func (c *Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen 不能为空")
	}
	if strings.TrimSpace(c.Auth.Username) == "" {
		return errors.New("auth.username 不能为空")
	}
	if strings.TrimSpace(c.Auth.PasswordHash) == "" {
		return errors.New("auth.password_hash 不能为空，请运行 `colab-comfyui hash-password` 生成")
	}
	if c.Auth.SessionTTLHours <= 0 {
		c.Auth.SessionTTLHours = 168
	}
	if strings.TrimSpace(c.Colab.TokenFile) == "" {
		return errors.New("colab.token_file 不能为空")
	}
	if strings.TrimSpace(c.Colab.StateFile) == "" {
		c.Colab.StateFile = "./state.json"
	}
	if strings.TrimSpace(c.Colab.AuthUser) == "" {
		c.Colab.AuthUser = "0"
	}
	if c.Colab.KeepaliveIntervalSec < 60 {
		c.Colab.KeepaliveIntervalSec = 480
	}
	if strings.TrimSpace(c.Colab.RcloneConfig) != "" && strings.TrimSpace(c.Colab.RcloneRemote) == "" {
		c.Colab.RcloneRemote = "gdrive:Colab/ComfyUI"
	}
	c.Colab.DefaultGPU = NormalizeGPU(c.Colab.DefaultGPU)
	return nil
}

// NormalizeGPU 规范化 GPU 名称；未知值回落为 L4。
func NormalizeGPU(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "CPU", "NONE", "":
		return "CPU"
	case "T4", "L4", "G4", "A100", "H100":
		return strings.ToUpper(strings.TrimSpace(raw))
	default:
		return "L4"
	}
}
