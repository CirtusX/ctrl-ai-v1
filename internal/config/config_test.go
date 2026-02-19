package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_NonexistentFile(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("Load with nonexistent file should not error: %v", err)
	}

	// Verify defaults.
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("default host: expected 127.0.0.1, got %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 3100 {
		t.Errorf("default port: expected 3100, got %d", cfg.Server.Port)
	}
	if !cfg.Streaming.Buffer {
		t.Error("default buffer: expected true")
	}
	if cfg.Streaming.BufferTimeoutMs != 30000 {
		t.Errorf("default timeout: expected 30000, got %d", cfg.Streaming.BufferTimeoutMs)
	}
	if !cfg.Dashboard.Enabled {
		t.Error("default dashboard: expected true")
	}
	if len(cfg.Providers) != 6 {
		t.Errorf("default providers: expected 6, got %d", len(cfg.Providers))
	}
	expectedProviders := map[string]string{
		"anthropic": "https://api.anthropic.com",
		"openai":    "https://api.openai.com",
		"moonshot":  "https://api.moonshot.cn",
		"qwen":      "https://dashscope.aliyuncs.com/compatible-mode",
		"minimax":   "https://api.minimax.io",
		"zhipu":     "https://open.bigmodel.cn/api",
	}
	for name, wantUpstream := range expectedProviders {
		p, ok := cfg.Providers[name]
		if !ok {
			t.Errorf("missing default provider: %s", name)
			continue
		}
		if p.Upstream != wantUpstream {
			t.Errorf("%s upstream: expected %q, got %q", name, wantUpstream, p.Upstream)
		}
	}
}

func TestLoad_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  host: "0.0.0.0"
  port: 9090
providers:
  anthropic:
    upstream: "https://api.anthropic.com"
streaming:
  buffer: false
  bufferTimeoutMs: 5000
dashboard:
  enabled: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("host: expected 0.0.0.0, got %q", cfg.Server.Host)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port: expected 9090, got %d", cfg.Server.Port)
	}
	if cfg.Streaming.Buffer {
		t.Error("buffer: expected false")
	}
	if cfg.Streaming.BufferTimeoutMs != 5000 {
		t.Errorf("timeout: expected 5000, got %d", cfg.Streaming.BufferTimeoutMs)
	}
	if cfg.Dashboard.Enabled {
		t.Error("dashboard: expected false")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(`{{{invalid yaml`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestLoad_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
server:
  port: 9090
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// Port overridden.
	if cfg.Server.Port != 9090 {
		t.Errorf("port: expected 9090, got %d", cfg.Server.Port)
	}
	// Host should retain default.
	if cfg.Server.Host != "127.0.0.1" {
		t.Errorf("host should be default 127.0.0.1, got %q", cfg.Server.Host)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "valid",
			cfg:     *applyDefaults(),
			wantErr: false,
		},
		{
			name: "empty host",
			cfg: Config{
				Server:    ServerConfig{Host: "", Port: 3100},
				Providers: map[string]ProviderConfig{"a": {Upstream: "http://x"}},
			},
			wantErr: true,
		},
		{
			name: "port 0",
			cfg: Config{
				Server:    ServerConfig{Host: "127.0.0.1", Port: 0},
				Providers: map[string]ProviderConfig{"a": {Upstream: "http://x"}},
			},
			wantErr: true,
		},
		{
			name: "port 65536",
			cfg: Config{
				Server:    ServerConfig{Host: "127.0.0.1", Port: 65536},
				Providers: map[string]ProviderConfig{"a": {Upstream: "http://x"}},
			},
			wantErr: true,
		},
		{
			name: "empty upstream",
			cfg: Config{
				Server:    ServerConfig{Host: "127.0.0.1", Port: 3100},
				Providers: map[string]ProviderConfig{"a": {Upstream: ""}},
			},
			wantErr: true,
		},
		{
			name: "negative timeout",
			cfg: Config{
				Server:    ServerConfig{Host: "127.0.0.1", Port: 3100},
				Providers: map[string]ProviderConfig{"a": {Upstream: "http://x"}},
				Streaming: StreamingConfig{BufferTimeoutMs: -1},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(&tt.cfg)
			if tt.wantErr && err == nil {
				t.Error("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestWriteDefault_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	if err := WriteDefault(path); err != nil {
		t.Fatalf("WriteDefault: %v", err)
	}

	// Verify file was created.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	// Load it back and verify defaults.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after WriteDefault: %v", err)
	}

	if cfg.Server.Port != 3100 {
		t.Errorf("roundtrip port: expected 3100, got %d", cfg.Server.Port)
	}
	if !cfg.Streaming.Buffer {
		t.Error("roundtrip buffer: expected true")
	}
}
