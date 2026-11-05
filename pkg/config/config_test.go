package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/pruner"
)

func TestDuration_Unmarshal(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"Seconds", `"30s"`, 30 * time.Second, false},
		{"Minutes", `"5m"`, 5 * time.Minute, false},
		{"Empty", `""`, 0, false},
		{"Invalid", `"abc"`, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalJSON([]byte(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("UnmarshalJSON(%s) error = %v, wantErr = %v", tc.input, err, tc.wantErr)
			}
			if !tc.wantErr && d.Duration() != tc.expected {
				t.Fatalf("Duration = %v, want %v", d.Duration(), tc.expected)
			}
		})
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TP_TEST_VAR", "my-secret-key")
	t.Setenv("TP_PORT", "9090")

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Basic bracketed",
			input:    "token: ${TP_TEST_VAR}",
			expected: "token: my-secret-key",
		},
		{
			name:     "Default fallback used when unset",
			input:    "url: http://localhost:${TP_UNSET_VAR:-8080}/sse",
			expected: "url: http://localhost:8080/sse",
		},
		{
			name:     "Default fallback ignored when set",
			input:    "port: ${TP_PORT:-8080}",
			expected: "port: 9090",
		},
		{
			name:     "Escaped placeholder",
			input:    `val: \${TP_TEST_VAR}`,
			expected: "val: ${TP_TEST_VAR}",
		},
		{
			name:     "Bare variable",
			input:    "token: $TP_TEST_VAR",
			expected: "token: my-secret-key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExpandEnv(tc.input)
			if got != tc.expected {
				t.Errorf("ExpandEnv(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestParse_FullYAML(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_12345")

	yamlContent := `
version: "1"
server:
  transport: "sse"
  host: "0.0.0.0"
  port: 8888
  timeout: "45s"
pruning:
  tier: "dynamic"
  max_description_length: 100
  strip_regex: true
routing:
  default_profile: "dev"
  namespace_mode: "always"
  active_window_size: 20
  pinned_tools:
    - "custom_meta"
upstreams:
  - id: "github"
    prefix: "gh"
    transport: "stdio"
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-github"]
    env:
      GITHUB_TOKEN: "${GITHUB_TOKEN}"
  - id: "remote"
    transport: "sse"
    url: "https://api.example.com/sse"
`

	cfg, err := Parse([]byte(yamlContent))
	if err != nil {
		t.Fatalf("Parse() failed: %v", err)
	}

	if cfg.Server.Transport != "sse" || cfg.Server.Port != 8888 {
		t.Errorf("Server mismatch: %+v", cfg.Server)
	}
	if cfg.Pruning.Tier != "dynamic" || cfg.Pruning.MaxDescriptionLength != 100 {
		t.Errorf("Pruning mismatch: %+v", cfg.Pruning)
	}
	if cfg.Routing.DefaultProfile != "dev" || cfg.Routing.NamespaceMode != "always" || cfg.Routing.ActiveWindowSize != 20 {
		t.Errorf("Routing mismatch: %+v", cfg.Routing)
	}
	if len(cfg.Upstreams) != 2 {
		t.Fatalf("Expected 2 upstreams, got %d", len(cfg.Upstreams))
	}
	if cfg.Upstreams[0].Env["GITHUB_TOKEN"] != "ghp_12345" {
		t.Errorf("Env var not expanded in upstream: %v", cfg.Upstreams[0].Env)
	}
}

func TestParse_JSON(t *testing.T) {
	jsonContent := `{
		"version": "1",
		"server": {
			"transport": "stdio",
			"timeout": "30s"
		},
		"pruning": {
			"tier": "aggressive"
		},
		"upstreams": [
			{
				"id": "local_sqlite",
				"transport": "stdio",
				"command": "sqlite-mcp",
				"args": ["db.sqlite"]
			}
		]
	}`

	cfg, err := Parse([]byte(jsonContent))
	if err != nil {
		t.Fatalf("Parse(JSON) failed: %v", err)
	}

	if cfg.Server.Transport != "stdio" {
		t.Errorf("Server.Transport = %q, want 'stdio'", cfg.Server.Transport)
	}
	if cfg.Pruning.Tier != "aggressive" {
		t.Errorf("Pruning.Tier = %q, want 'aggressive'", cfg.Pruning.Tier)
	}
	if len(cfg.Upstreams) != 1 || cfg.Upstreams[0].ID != "local_sqlite" {
		t.Errorf("Upstreams mismatch: %+v", cfg.Upstreams)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name        string
		modify      func(c *Config)
		expectedErr string
	}{
		{
			name: "Invalid server transport",
			modify: func(c *Config) {
				c.Server.Transport = "grpc"
			},
			expectedErr: "server.transport \"grpc\" is invalid",
		},
		{
			name: "Invalid pruning tier",
			modify: func(c *Config) {
				c.Pruning.Tier = "extreme"
			},
			expectedErr: "pruning.tier \"extreme\" is invalid",
		},
		{
			name: "Invalid namespace mode",
			modify: func(c *Config) {
				c.Routing.NamespaceMode = "random"
			},
			expectedErr: "routing.namespace_mode \"random\" is invalid",
		},
		{
			name: "Upstream ID contains double underscore",
			modify: func(c *Config) {
				c.Upstreams = append(c.Upstreams, UpstreamConfig{
					ID:        "github__mcp",
					Transport: "stdio",
					Command:   "npx",
				})
			},
			expectedErr: "contains '__'",
		},
		{
			name: "Duplicate upstream ID",
			modify: func(c *Config) {
				c.Upstreams = append(c.Upstreams,
					UpstreamConfig{ID: "srv1", Transport: "stdio", Command: "cmd1"},
					UpstreamConfig{ID: "srv1", Transport: "stdio", Command: "cmd2"},
				)
			},
			expectedErr: "duplicate upstream id \"srv1\"",
		},
		{
			name: "Stdio upstream missing command",
			modify: func(c *Config) {
				c.Upstreams = append(c.Upstreams, UpstreamConfig{
					ID:        "srv_nocmd",
					Transport: "stdio",
					Command:   "",
				})
			},
			expectedErr: "has transport 'stdio' but command is empty",
		},
		{
			name: "SSE upstream invalid URL",
			modify: func(c *Config) {
				c.Upstreams = append(c.Upstreams, UpstreamConfig{
					ID:        "srv_sse",
					Transport: "sse",
					URL:       "ftp://invalid-scheme.com",
				})
			},
			expectedErr: "not a valid HTTP/HTTPS URL",
		},
		{
			name: "Upstream negative timeout",
			modify: func(c *Config) {
				c.Upstreams = append(c.Upstreams, UpstreamConfig{
					ID:        "srv_neg_timeout",
					Transport: "stdio",
					Command:   "git",
					Timeout:   Duration(-5 * time.Second),
				})
			},
			expectedErr: "timeout cannot be negative",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.modify(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.expectedErr) {
				t.Errorf("Error %q does not contain expected substring %q", err.Error(), tc.expectedErr)
			}
		})
	}
}

func TestResolveConfigPath(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "custom.yaml")
	if err := os.WriteFile(configPath, []byte("version: '1'"), 0o600); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	// 1. Explicit path exists
	resolved, err := ResolveConfigPath(configPath)
	if err != nil {
		t.Fatalf("ResolveConfigPath(explicit) failed: %v", err)
	}
	if resolved != filepath.Clean(configPath) {
		t.Errorf("Resolved path %q != %q", resolved, configPath)
	}

	// 2. Explicit path does not exist
	_, err = ResolveConfigPath(filepath.Join(tempDir, "non_existent.yaml"))
	if err == nil {
		t.Fatalf("Expected ErrExplicitConfigMissing, got nil")
	}

	// 3. FindConfigFile helper
	found := FindConfigFile()
	_ = found // may or may not find something depending on cwd
}

func TestConfigAdapters(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.Timeout = Duration(45 * time.Second)
	cfg.Pruning.Tier = "compact"
	cfg.Pruning.MaxDescriptionLength = 90
	cfg.Routing.DefaultProfile = "read_only"
	cfg.Routing.ActiveWindowSize = 12

	// ProxyConfig
	proxyCfg := cfg.ToProxyConfig()
	if proxyCfg.PruneMode != "compact" || proxyCfg.RequestTimeout != 45*time.Second {
		t.Errorf("ToProxyConfig mismatch: %+v", proxyCfg)
	}

	// SSEServerConfig
	sseCfg := cfg.ToSSEServerConfig()
	if sseCfg.Port != 8080 || sseCfg.Host != "127.0.0.1" {
		t.Errorf("ToSSEServerConfig mismatch: %+v", sseCfg)
	}

	// RouterConfig
	routerCfg := cfg.ToRouterConfig()
	if routerCfg.PruneMode != "compact" || routerCfg.ActiveProfile != "read_only" || routerCfg.MaxActiveTools != 12 {
		t.Errorf("ToRouterConfig mismatch: %+v", routerCfg)
	}

	// WindowConfig
	winCfg := cfg.ToWindowConfig()
	if !winCfg.Enabled || winCfg.MaxActive != 12 {
		t.Errorf("ToWindowConfig mismatch: %+v", winCfg)
	}

	// PrunerOptions
	pruneOpts := cfg.ToPrunerOptions()
	if pruneOpts.Tier != pruner.TierCompact || pruneOpts.MaxToolDescLength != 90 {
		t.Errorf("ToPrunerOptions mismatch: %+v", pruneOpts)
	}

	// SupervisorConfig
	up := UpstreamConfig{
		ID:      "test_cmd",
		Command: "git",
		Args:    []string{"status"},
		Timeout: Duration(5 * time.Second),
	}
	supCfg := up.ToSupervisorConfig(nil)
	if supCfg.Name != "test_cmd" || supCfg.GracePeriod != 5*time.Second {
		t.Errorf("ToSupervisorConfig mismatch: %+v", supCfg)
	}
}

func TestValidateHelper(t *testing.T) {
	if err := Validate(nil); err == nil {
		t.Errorf("expected error validating nil config")
	}
	if err := Validate(DefaultConfig()); err != nil {
		t.Errorf("unexpected error validating DefaultConfig: %v", err)
	}
}
