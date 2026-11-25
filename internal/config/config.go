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
	Colab  ColabConfig  `yaml:"runtime"`
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
	defaultListen := firstEnv("COMFELLE_DEFAULT_LISTEN", "COMFY_DEFAULT_LISTEN")
	if defaultListen == "" {
		defaultListen = "127.0.0.1:8080"
	}
	return &Config{
		Server: ServerConfig{
			Listen:             defaultListen,
			TrustProxy:         true,
			CookieSecure:       true,
			EnableFloatingDock: true,
		},
		Auth: AuthConfig{
			Username:        "admin",
			SessionTTLHours: 168,
		},
		Colab: ColabConfig{
			TokenFile:            "./token.json",
			StateFile:            "./state.json",
			DefaultGPU:           "L4",
			AuthUser:             "0",
			KeepaliveIntervalSec: 480,
		},
	}
}

// Load 从 YAML 文件加载配置，同时向下兼容旧版 `colab:` 键名，并允许通过环境变量覆盖关键项。
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %q 失败: %w", path, err)
	}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %q 失败: %w", path, err)
	}
	var top map[string]yaml.Node
	if yaml.Unmarshal(data, &top) == nil {
		_, hasRuntime := top["runtime"]
		_, hasLegacy := top["colab"]
		if hasLegacy && !hasRuntime {
			legacy := struct {
				Colab ColabConfig `yaml:"colab"`
			}{Colab: cfg.Colab}
			if err := yaml.Unmarshal(data, &legacy); err != nil {
				return nil, fmt.Errorf("解析配置文件 %q 失败: %w", path, err)
			}
			cfg.Colab = legacy.Colab
		}
	}
	applyEnvOverrides(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func applyEnvOverrides(cfg *Config) {
	if v := firstEnv("COMFELLE_LISTEN", "COMFY_PROXY_LISTEN"); v != "" {
		cfg.Server.Listen = v
	}
	if v := firstEnv("COMFELLE_USERNAME", "COMFY_PROXY_USERNAME"); v != "" {
		cfg.Auth.Username = v
	}
	if v := firstEnv("COMFELLE_PASSWORD_HASH", "COMFY_PROXY_PASSWORD_HASH"); v != "" {
		cfg.Auth.PasswordHash = v
	}
	if v := firstEnv("COMFELLE_SESSION_SECRET", "COMFY_PROXY_SESSION_SECRET"); v != "" {
		cfg.Auth.SessionSecret = v
	}
	if v := firstEnv("COMFELLE_TOKEN_FILE", "COMFY_COLAB_TOKEN_FILE"); v != "" {
		cfg.Colab.TokenFile = v
	}
	if v := firstEnv("COMFELLE_RCLONE_CONFIG", "COMFY_RCLONE_CONFIG"); v != "" {
		cfg.Colab.RcloneConfig = v
	}
	if v := firstEnv("COMFELLE_RCLONE_REMOTE", "COMFY_RCLONE_REMOTE"); v != "" {
		cfg.Colab.RcloneRemote = v
	}
	if v := firstEnv("HF_TOKEN"); v != "" {
		cfg.Colab.HFToken = v
	}
	if v := firstEnv("CIVITAI_TOKEN"); v != "" {
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
		return errors.New("auth.password_hash 不能为空，请运行 `comfelle hash-password` 生成")
	}
	if c.Auth.SessionTTLHours <= 0 {
		c.Auth.SessionTTLHours = 168
	}
	if strings.TrimSpace(c.Colab.TokenFile) == "" {
		return errors.New("runtime.token_file 不能为空")
	}
	if c.Colab.TokenFile == "./token.json" {
		if _, err := os.Stat("./token.json"); os.IsNotExist(err) {
			if _, legacyErr := os.Stat("./google_token.json"); legacyErr == nil {
				c.Colab.TokenFile = "./google_token.json"
			}
		}
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

// NormalizeGPU 规范化 GPU 名称；留空或未知值回落为 L4，显式指定 CPU/NONE 时使用 CPU。
func NormalizeGPU(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "CPU", "NONE":
		return "CPU"
	case "T4", "L4", "G4", "A100", "H100":
		return strings.ToUpper(strings.TrimSpace(raw))
	default:
		return "L4"
	}
}
