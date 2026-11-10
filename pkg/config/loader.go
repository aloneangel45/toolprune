package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Standard configuration filenames and errors.
const (
	DefaultConfigFileName = "toolprune.yaml"
)

var (
	// ErrConfigNotFound is returned when no candidate configuration file can be located.
	ErrConfigNotFound = errors.New("no toolprune configuration file found")
	// ErrExplicitConfigMissing is returned when a user-specified --config file does not exist.
	ErrExplicitConfigMissing = errors.New("specified configuration file does not exist")
)

// Regex matching environment variable placeholders:
// 1. Escaped: \${VAR} or \$VAR -> literal ${VAR} / $VAR
// 2. Bracketed with default: ${VAR:-default}
// 3. Bracketed simple: ${VAR}
// 4. Unbracketed simple: $VAR
var envPlaceholderRegex = regexp.MustCompile(`(\\)?(?:\${([a-zA-Z_][a-zA-Z0-9_]*)(?::-([^}]*))?}|\$([a-zA-Z_][a-zA-Z0-9_]*))`)

// ExpandEnv substitutes environment variable references in input string.
// Supported syntaxes:
//   - `${VAR}`: expands to os.Getenv("VAR")
//   - `${VAR:-default}`: expands to os.Getenv("VAR") if set and non-empty, else "default"
//   - `$VAR`: expands to os.Getenv("VAR")
//   - `\${VAR}`: escaped, yields literal "${VAR}"
func ExpandEnv(input string) string {
	return envPlaceholderRegex.ReplaceAllStringFunc(input, func(match string) string {
		sub := envPlaceholderRegex.FindStringSubmatch(match)
		if len(sub) == 0 {
			return match
		}

		// Handle escaped \${VAR} or \$VAR
		if sub[1] == `\` {
			return match[1:]
		}

		varName := sub[2]
		if varName == "" {
			varName = sub[4]
		}

		val, exists := os.LookupEnv(varName)
		if !exists || val == "" {
			// If default value provided in ${VAR:-default}
			if sub[3] != "" {
				return sub[3]
			}
			return ""
		}
		return val
	})
}

// ExpandEnvBytes substitutes environment variable references on raw byte slices.
func ExpandEnvBytes(input []byte) []byte {
	return []byte(ExpandEnv(string(input)))
}

// Parse unmarshals raw YAML or JSON bytes into a validated Config instance.
// It automatically expands environment variables, fills defaults, and executes validation.
func Parse(data []byte) (*Config, error) {
	expanded := ExpandEnvBytes(data)

	var cfg Config
	// Initialize with defaults before unmarshaling so omitted sections retain defaults
	cfg = *DefaultConfig()

	if err := yaml.Unmarshal(expanded, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse configuration: %w", err)
	}

	cfg.ApplyDefaults()
	if err := cfg.ApplyEnvOverrides(); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// ApplyEnvOverrides applies TOOLPRUNE_* runtime environment variable overrides.
func (c *Config) ApplyEnvOverrides() error {
	if raw, ok := os.LookupEnv("TOOLPRUNE_MAX_SCHEMA_TOKENS"); ok {
		trimmed := strings.TrimSpace(raw)
		if trimmed != "" {
			n, err := strconv.Atoi(trimmed)
			if err != nil || n < 0 {
				return fmt.Errorf("invalid TOOLPRUNE_MAX_SCHEMA_TOKENS=%q: must be a non-negative integer", raw)
			}
			c.Pruning.MaxSchemaTokens = n
		}
	}
	return nil
}

// Load reads and parses a configuration file from the specified path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read configuration file %q: %w", path, err)
	}

	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("error in configuration %q: %w", path, err)
	}

	return cfg, nil
}

// ResolveConfigPath locates a configuration file using explicit path or discovery fallbacks.
//
// Search Order:
//  1. Explicit path (if provided; returns ErrExplicitConfigMissing if absent)
//  2. Current directory: ./toolprune.yaml, ./toolprune.yml, ./toolprune.json
//  3. User config directory (~/.config/toolprune/config.yaml, etc.)
//  4. User home directory (~/.toolprune.yaml, ~/.toolprune.json)
func ResolveConfigPath(explicitPath string) (string, error) {
	if explicitPath != "" {
		cleanPath := filepath.Clean(explicitPath)
		if fi, err := os.Stat(cleanPath); err != nil || fi.IsDir() {
			return "", fmt.Errorf("%w: %s", ErrExplicitConfigMissing, explicitPath)
		}
		return cleanPath, nil
	}

	candidates := candidatePaths()
	for _, cand := range candidates {
		if cand == "" {
			continue
		}
		cleanCand := filepath.Clean(cand)
		if fi, err := os.Stat(cleanCand); err == nil && !fi.IsDir() {
			return cleanCand, nil
		}
	}

	return "", ErrConfigNotFound
}

// FindConfigFile searches standard locations for a configuration file, returning empty string if not found.
func FindConfigFile() string {
	p, err := ResolveConfigPath("")
	if err != nil {
		return ""
	}
	return p
}

// LoadWithFallback attempts to locate and load configuration, returning the resolved path.
func LoadWithFallback(explicitPath string) (*Config, string, error) {
	resolvedPath, err := ResolveConfigPath(explicitPath)
	if err != nil {
		return nil, "", err
	}

	cfg, err := Load(resolvedPath)
	if err != nil {
		return nil, resolvedPath, err
	}

	return cfg, resolvedPath, nil
}

// candidatePaths computes the list of standard locations searched during discovery.
func candidatePaths() []string {
	var list []string

	// 1. Current working directory
	list = append(list,
		"toolprune.yaml",
		"toolprune.yml",
		"toolprune.json",
	)

	// 2. User config directory (e.g. ~/.config/toolprune or %APPDATA%/toolprune)
	if userConfigDir, err := os.UserConfigDir(); err == nil && userConfigDir != "" {
		toolpruneDir := filepath.Join(userConfigDir, "toolprune")
		list = append(list,
			filepath.Join(toolpruneDir, "config.yaml"),
			filepath.Join(toolpruneDir, "config.yml"),
			filepath.Join(toolpruneDir, "config.json"),
			filepath.Join(toolpruneDir, "toolprune.yaml"),
		)
	}

	// 3. User home directory (~/.toolprune.yaml)
	if userHomeDir, err := os.UserHomeDir(); err == nil && userHomeDir != "" {
		list = append(list,
			filepath.Join(userHomeDir, ".toolprune.yaml"),
			filepath.Join(userHomeDir, ".toolprune.yml"),
			filepath.Join(userHomeDir, ".toolprune.json"),
		)
	}

	return list
}

// GenerateDefaultConfig returns a standard default toolprune.yaml configuration template.
func GenerateDefaultConfig() string {
	return `# ToolPrune Configuration (toolprune.yaml)
version: "1"

server:
  transport: "stdio"
  host: "127.0.0.1"
  port: 8080
  timeout: "60s"
  inject_proxy_info: true

pruning:
  tier: "aggressive"
  max_schema_tokens: 0
  max_description_length: 120
  max_param_desc_length: 80
  strip_regex: true
  max_regex_length: 25
  strip_self_evident: true
  strict_draft07: true
  strip_code_blocks: true
  strip_html_tags: true

routing:
  default_profile: "full"
  namespace_mode: "auto"
  active_window_size: 15
  auto_promote: true
  pinned_tools:
    - "toolprune_search_tools"
    - "toolprune_activate_tools"
    - "toolprune_get_tool_schema"
  global_exclude:
    - "*_internal_*"

upstreams: []
`
}
