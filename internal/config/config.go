// Package config handles loading, validating, and writing the CtrlAI proxy
// configuration from ~/.ctrlai/config.yaml.
//
// The config defines:
//   - Server bind address (host:port)
//   - Upstream LLM provider URLs (Anthropic, OpenAI, Moonshot, Qwen, MiniMax, Zhipu, custom)
//   - Streaming behavior (buffer SSE for tool inspection)
//   - Dashboard toggle
//
// See design doc Section 3 for the full YAML schema.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the top-level CtrlAI proxy configuration.
// Loaded from ~/.ctrlai/config.yaml, with sensible defaults for fields
// that are not explicitly set.
type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Streaming StreamingConfig           `yaml:"streaming"`
	Dashboard DashboardConfig           `yaml:"dashboard"`
}

// ServerConfig defines where the proxy listens.
// Default: 127.0.0.1:3100 (loopback only — never bind to 0.0.0.0).
type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

// ProviderConfig maps a provider key (e.g. "anthropic") to its upstream URL.
// The proxy forwards requests to this URL after inspection.
type ProviderConfig struct {
	Upstream string `yaml:"upstream"`
}

// StreamingConfig controls SSE response buffering behavior.
//
// Buffer=true (default, required for security): the proxy buffers the entire
// SSE stream before forwarding to the SDK. This lets us inspect tool_use
// blocks that arrive incrementally across multiple SSE events.
//
// BufferTimeoutMs: maximum time to buffer before flushing (prevents hanging
// on stuck/slow LLM responses). Default: 30000ms (30 seconds).
type StreamingConfig struct {
	Buffer          bool `yaml:"buffer"`
	BufferTimeoutMs int  `yaml:"bufferTimeoutMs"`
}

// DashboardConfig controls the web dashboard served at /dashboard.
type DashboardConfig struct {
	Enabled bool `yaml:"enabled"`
}

// Load reads and parses config.yaml from the given path.
// If the file doesn't exist, returns defaults (not an error).
// Invalid YAML or validation failures return an error.
func Load(path string) (*Config, error) {
	cfg := applyDefaults()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// No config file — use defaults. This is normal on first run
			// before `ctrlai` interactive setup creates the file.
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return cfg, nil
}

// WriteDefault writes a default config.yaml with all fields populated
// and a comment header. Used by the first-run setup and `ctrlai config edit`
// when no config file exists yet.
func WriteDefault(path string) error {
	cfg := applyDefaults()
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling default config: %w", err)
	}

	header := `# CtrlAI Proxy Configuration
# See design doc Section 3 for details.
#
# server:
#   host: Bind address (default: 127.0.0.1, loopback only)
#   port: Listen port (default: 3100)
#
# providers:
#   <key>:
#     upstream: Full URL to the real LLM API
#
# streaming:
#   buffer: true = buffer SSE responses to inspect tool calls (required for security)
#   bufferTimeoutMs: Max buffer time before flushing (prevents hanging)
#
# dashboard:
#   enabled: Serve web UI at /dashboard on the same port

`
	return os.WriteFile(path, []byte(header+string(data)), 0o644)
}

// applyDefaults returns a Config with all fields set to their default values.
// Design doc Section 3 defines these defaults.
func applyDefaults() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 3100,
		},
		Providers: map[string]ProviderConfig{
			"anthropic": {Upstream: "https://api.anthropic.com"},
			"openai":    {Upstream: "https://api.openai.com"},
			"moonshot":  {Upstream: "https://api.moonshot.cn"},
			"qwen":      {Upstream: "https://dashscope.aliyuncs.com/compatible-mode"},
			"minimax":   {Upstream: "https://api.minimax.io"},
			"zhipu":     {Upstream: "https://open.bigmodel.cn/api"},
		},
		Streaming: StreamingConfig{
			Buffer:          true,
			BufferTimeoutMs: 30000,
		},
		Dashboard: DashboardConfig{
			Enabled: true,
		},
	}
}

// validate checks the config for logical errors after parsing.
func validate(cfg *Config) error {
	if cfg.Server.Host == "" {
		return fmt.Errorf("server.host must not be empty")
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range (1-65535)", cfg.Server.Port)
	}

	for name, p := range cfg.Providers {
		if p.Upstream == "" {
			return fmt.Errorf("provider %q: upstream URL is required", name)
		}
	}

	if cfg.Streaming.BufferTimeoutMs < 0 {
		return fmt.Errorf("streaming.bufferTimeoutMs must be non-negative")
	}

	return nil
}
