package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/router"
)

// Duration wraps time.Duration to provide seamless YAML/JSON string parsing (e.g. "60s", "2m").
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// String formats the duration as a human-readable string.
func (d Duration) String() string {
	return time.Duration(d).String()
}

// UnmarshalText implements encoding.TextUnmarshaler for string parsing.
func (d *Duration) UnmarshalText(text []byte) error {
	str := strings.TrimSpace(string(text))
	if str == "" {
		*d = 0
		return nil
	}
	dur, err := time.ParseDuration(str)
	if err != nil {
		return fmt.Errorf("invalid duration format %q: %w", str, err)
	}
	*d = Duration(dur)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// UnmarshalJSON implements json.Unmarshaler supporting both string ("60s") and numeric nanoseconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		return d.UnmarshalText([]byte(s))
	}
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		*d = Duration(time.Duration(n))
		return nil
	}
	return errors.New("invalid duration: expected duration string or nanosecond integer")
}

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	return d.UnmarshalText([]byte(value.Value))
}

// ServerConfig specifies settings for the client-facing proxy transport.
type ServerConfig struct {
	Transport       string   `json:"transport" yaml:"transport"`               // "stdio" or "sse" (default: "stdio")
	Host            string   `json:"host" yaml:"host"`                         // SSE bind host (default: "127.0.0.1")
	Port            int      `json:"port" yaml:"port"`                         // SSE bind port (default: 8080)
	BasePath        string   `json:"basePath" yaml:"base_path"`                // SSE endpoint prefix (e.g. "/mcp")
	CORSOrigins     []string `json:"corsOrigins" yaml:"cors_origins"`          // Allowed CORS origins (default: ["*"])
	Timeout         Duration `json:"timeout" yaml:"timeout"`                   // Request timeout (default: "60s")
	InjectProxyInfo bool     `json:"injectProxyInfo" yaml:"inject_proxy_info"` // Inject ToolPrune banner in init (default: true)
}

// UpstreamConfig defines connection parameters for an upstream MCP server.
type UpstreamConfig struct {
	ID          string            `json:"id" yaml:"id"`                               // Unique upstream server ID (e.g. "github")
	Prefix      string            `json:"prefix,omitempty" yaml:"prefix,omitempty"`   // Namespace prefix (defaults to ID)
	Transport   string            `json:"transport" yaml:"transport"`                 // "stdio" or "sse" (default: "stdio")
	Command     string            `json:"command,omitempty" yaml:"command,omitempty"` // Executable for stdio
	Args        []string          `json:"args,omitempty" yaml:"args,omitempty"`       // Arguments for stdio command
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`         // Extra environment variables
	Dir         string            `json:"dir,omitempty" yaml:"dir,omitempty"`         // Working directory for command
	URL         string            `json:"url,omitempty" yaml:"url,omitempty"`         // Endpoint URL for SSE transport
	Timeout     Duration          `json:"timeout,omitempty" yaml:"timeout,omitempty"` // Upstream timeout override
	Disabled    bool              `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	Description string            `json:"description,omitempty" yaml:"description,omitempty"`
}

// PruningConfig specifies schema optimization parameters for Tiers 0-4 and RFC #1 token budgeting.
type PruningConfig struct {
	Tier                 string `json:"tier" yaml:"tier"`                                               // "none"|"lossless"|"compact"|"aggressive"|"dynamic"
	MaxSchemaTokens      int    `json:"maxSchemaTokens,omitempty" yaml:"max_schema_tokens,omitempty"`   // RFC #1 maximum token budget for tools/list (0 = unlimited)
	MaxDescriptionLength int    `json:"maxDescriptionLength" yaml:"max_description_length"`             // Top-level tool description rune limit (default: 120)
	MaxParamDescLength   int    `json:"maxParamDescLength" yaml:"max_param_desc_length"`     // Parameter description rune limit (default: 80)
	StripRegex           bool   `json:"stripRegex" yaml:"strip_regex"`                       // Strip verbose regex patterns (default: true)
	MaxRegexLength       int    `json:"maxRegexLength" yaml:"max_regex_length"`             // Regex length threshold before compression (default: 25)
	StripSelfEvident     bool   `json:"stripSelfEvident" yaml:"strip_self_evident"`         // Strip self-evident parameter descriptions (default: true)
	StrictDraft07        bool   `json:"strictDraft07" yaml:"strict_draft07"`                 // Enforce valid Draft-07 JSON Schema (default: true)
	StripCodeBlocks      bool   `json:"stripCodeBlocks" yaml:"strip_code_blocks"`           // Strip markdown code blocks (default: true)
	StripHTMLTags        bool   `json:"stripHtmlTags" yaml:"strip_html_tags"`               // Strip HTML tags (default: true)
}

// ProfileConfig defines tool inclusion and exclusion globs for a named profile.
type ProfileConfig struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description"`
	Include     []string `json:"include" yaml:"include"`
	Exclude     []string `json:"exclude" yaml:"exclude"`
}

// RoutingConfig holds settings for tool profiles, namespacing, and active windowing.
type RoutingConfig struct {
	DefaultProfile   string                   `json:"defaultProfile" yaml:"default_profile"`     // Active profile on startup (default: "full")
	NamespaceMode    string                   `json:"namespaceMode" yaml:"namespace_mode"`       // "auto"|"always"|"on_collision"|"none" (default: "auto")
	ActiveWindowSize int                      `json:"activeWindowSize" yaml:"active_window_size"` // Active sliding window capacity (default: 15)
	AutoPromote      bool                     `json:"autoPromote" yaml:"auto_promote"`           // Auto-promote tool to window on call (default: true)
	PinnedTools      []string                 `json:"pinnedTools" yaml:"pinned_tools"`           // Tools immune to LRU eviction
	GlobalExclude    []string                 `json:"globalExclude" yaml:"global_exclude"`       // Patterns excluded globally
	CaseInsensitive  bool                     `json:"caseInsensitive" yaml:"case_insensitive"`   // Case-insensitive filtering (default: true)
	Profiles         map[string]ProfileConfig `json:"profiles" yaml:"profiles"`                   // Named profile configurations
}

// Config represents the top-level configuration for ToolPrune.
type Config struct {
	Version   string           `json:"version" yaml:"version"`
	Server    ServerConfig     `json:"server" yaml:"server"`
	Pruning   PruningConfig    `json:"pruning" yaml:"pruning"`
	Routing   RoutingConfig    `json:"routing" yaml:"routing"`
	Upstreams []UpstreamConfig `json:"upstreams" yaml:"upstreams"`
}

// DefaultServerConfig returns production defaults for the server block.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Transport:       "stdio",
		Host:            "127.0.0.1",
		Port:            8080,
		BasePath:        "",
		CORSOrigins:     []string{"*"},
		Timeout:         Duration(60 * time.Second),
		InjectProxyInfo: true,
	}
}

// DefaultPruningConfig returns production defaults for pruning settings.
func DefaultPruningConfig() PruningConfig {
	return PruningConfig{
		Tier:                 "aggressive",
		MaxDescriptionLength: 120,
		MaxParamDescLength:   80,
		StripRegex:           true,
		MaxRegexLength:       25,
		StripSelfEvident:     true,
		StrictDraft07:        true,
		StripCodeBlocks:      true,
		StripHTMLTags:        true,
	}
}

// DefaultRoutingConfig returns production defaults for routing and windowing.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		DefaultProfile:   "full",
		NamespaceMode:    "auto",
		ActiveWindowSize: 15,
		AutoPromote:      true,
		PinnedTools: []string{
			"toolprune_search_tools",
			"toolprune_activate_tools",
			"toolprune_get_tool_schema",
		},
		GlobalExclude:   []string{"*_internal_*"},
		CaseInsensitive: true,
		Profiles:        make(map[string]ProfileConfig),
	}
}

// DefaultConfig returns a fully populated Config initialized with standard production defaults.
func DefaultConfig() *Config {
	return &Config{
		Version:   "1",
		Server:    DefaultServerConfig(),
		Pruning:   DefaultPruningConfig(),
		Routing:   DefaultRoutingConfig(),
		Upstreams: make([]UpstreamConfig, 0),
	}
}

// ApplyDefaults fills in missing fields with sensible defaults without overriding set values.
func (c *Config) ApplyDefaults() {
	if c.Version == "" {
		c.Version = "1"
	}

	// Server defaults
	if c.Server.Transport == "" {
		c.Server.Transport = "stdio"
	}
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if len(c.Server.CORSOrigins) == 0 {
		c.Server.CORSOrigins = []string{"*"}
	}
	if c.Server.Timeout == 0 {
		c.Server.Timeout = Duration(60 * time.Second)
	}

	// Pruning defaults
	if c.Pruning.Tier == "" {
		c.Pruning.Tier = "aggressive"
	}
	if c.Pruning.MaxDescriptionLength <= 0 {
		c.Pruning.MaxDescriptionLength = 120
	}
	if c.Pruning.MaxParamDescLength <= 0 {
		c.Pruning.MaxParamDescLength = 80
	}
	if c.Pruning.MaxRegexLength <= 0 {
		c.Pruning.MaxRegexLength = 25
	}

	// Routing defaults
	if c.Routing.DefaultProfile == "" {
		c.Routing.DefaultProfile = "full"
	}
	if c.Routing.NamespaceMode == "" {
		c.Routing.NamespaceMode = "auto"
	}
	if c.Routing.ActiveWindowSize <= 0 {
		c.Routing.ActiveWindowSize = 15
	}
	if len(c.Routing.PinnedTools) == 0 {
		c.Routing.PinnedTools = []string{
			"toolprune_search_tools",
			"toolprune_activate_tools",
			"toolprune_get_tool_schema",
		}
	}
	if c.Routing.Profiles == nil {
		c.Routing.Profiles = make(map[string]ProfileConfig)
	}

	// Upstream defaults
	for i := range c.Upstreams {
		if c.Upstreams[i].Transport == "" {
			c.Upstreams[i].Transport = "stdio"
		}
		if c.Upstreams[i].Prefix == "" {
			c.Upstreams[i].Prefix = c.Upstreams[i].ID
		}
	}
}

// ValidationError records one or more schema/configuration validation failures.
type ValidationError struct {
	Errors []string
}

// Error formats the collected validation failures.
func (v *ValidationError) Error() string {
	if len(v.Errors) == 0 {
		return "no validation errors"
	}
	if len(v.Errors) == 1 {
		return "configuration error: " + v.Errors[0]
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d configuration validation errors:\n", len(v.Errors)))
	for _, err := range v.Errors {
		sb.WriteString("  - ")
		sb.WriteString(err)
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Validate verifies that all configuration values adhere to semantic and operational rules.
func (c *Config) Validate() error {
	var errs []string

	// 1. Server validation
	transport := strings.ToLower(strings.TrimSpace(c.Server.Transport))
	if transport != "stdio" && transport != "sse" {
		errs = append(errs, fmt.Sprintf("server.transport %q is invalid: must be 'stdio' or 'sse'", c.Server.Transport))
	}
	if transport == "sse" {
		if c.Server.Port < 0 || c.Server.Port > 65535 {
			errs = append(errs, fmt.Sprintf("server.port %d is out of range (0-65535)", c.Server.Port))
		}
		if strings.TrimSpace(c.Server.Host) == "" {
			errs = append(errs, "server.host cannot be empty when transport is 'sse'")
		}
	}
	if time.Duration(c.Server.Timeout) < 0 {
		errs = append(errs, "server.timeout cannot be negative")
	}

	// 2. Pruning validation
	validTiers := map[string]bool{
		"none":       true,
		"lossless":   true,
		"compact":    true,
		"aggressive": true,
		"dynamic":    true,
	}
	tier := strings.ToLower(strings.TrimSpace(c.Pruning.Tier))
	if !validTiers[tier] {
		errs = append(errs, fmt.Sprintf("pruning.tier %q is invalid: must be one of none, lossless, compact, aggressive, dynamic", c.Pruning.Tier))
	}
	if c.Pruning.MaxDescriptionLength < 0 {
		errs = append(errs, "pruning.max_description_length cannot be negative")
	}
	if c.Pruning.MaxParamDescLength < 0 {
		errs = append(errs, "pruning.max_param_desc_length cannot be negative")
	}
	if c.Pruning.MaxRegexLength < 0 {
		errs = append(errs, "pruning.max_regex_length cannot be negative")
	}
	if c.Pruning.MaxSchemaTokens < 0 {
		errs = append(errs, "pruning.max_schema_tokens cannot be negative")
	}

	// 3. Routing validation
	validModes := map[string]bool{
		"auto":         true,
		"always":       true,
		"on_collision": true,
		"none":         true,
	}
	mode := strings.ToLower(strings.TrimSpace(c.Routing.NamespaceMode))
	if !validModes[mode] {
		errs = append(errs, fmt.Sprintf("routing.namespace_mode %q is invalid: must be auto, always, on_collision, or none", c.Routing.NamespaceMode))
	}
	if c.Routing.ActiveWindowSize < 0 {
		errs = append(errs, "routing.active_window_size cannot be negative")
	}

	// Check default profile existence
	defProf := strings.TrimSpace(c.Routing.DefaultProfile)
	builtInProfiles := map[string]bool{
		"full":      true,
		"read_only": true,
		"dev":       true,
		"minimal":   true,
	}
	if defProf != "" && !builtInProfiles[defProf] {
		if _, ok := c.Routing.Profiles[defProf]; !ok {
			errs = append(errs, fmt.Sprintf("routing.default_profile %q is neither a built-in profile nor defined in routing.profiles", defProf))
		}
	}

	// Validate custom profile patterns
	for profName, prof := range c.Routing.Profiles {
		if strings.TrimSpace(profName) == "" {
			errs = append(errs, "routing.profiles key cannot be empty")
		}
		for _, inc := range prof.Include {
			if _, err := regexp.Compile(router.GlobToRegex(inc)); err != nil {
				errs = append(errs, fmt.Sprintf("profile %q include pattern %q is invalid regex/glob: %v", profName, inc, err))
			}
		}
		for _, exc := range prof.Exclude {
			if _, err := regexp.Compile(router.GlobToRegex(exc)); err != nil {
				errs = append(errs, fmt.Sprintf("profile %q exclude pattern %q is invalid regex/glob: %v", profName, exc, err))
			}
		}
	}

	// 4. Upstream server validation
	seenIDs := make(map[string]bool, len(c.Upstreams))
	for i, up := range c.Upstreams {
		if strings.TrimSpace(up.ID) == "" {
			errs = append(errs, fmt.Sprintf("upstreams[%d].id cannot be empty", i))
			continue
		}
		if strings.Contains(up.ID, "__") {
			errs = append(errs, fmt.Sprintf("upstreams[%d].id %q contains '__', which is reserved for namespace routing", i, up.ID))
		}
		if seenIDs[up.ID] {
			errs = append(errs, fmt.Sprintf("duplicate upstream id %q found at index %d", up.ID, i))
		}
		seenIDs[up.ID] = true

		if strings.Contains(up.Prefix, "__") {
			errs = append(errs, fmt.Sprintf("upstreams[%d].prefix %q cannot contain '__'", i, up.Prefix))
		}

		upTransport := strings.ToLower(strings.TrimSpace(up.Transport))
		if upTransport != "stdio" && upTransport != "sse" {
			errs = append(errs, fmt.Sprintf("upstream %q transport %q is invalid: must be 'stdio' or 'sse'", up.ID, up.Transport))
		}

		if upTransport == "stdio" && strings.TrimSpace(up.Command) == "" {
			errs = append(errs, fmt.Sprintf("upstream %q has transport 'stdio' but command is empty", up.ID))
		}

		if upTransport == "sse" {
			if strings.TrimSpace(up.URL) == "" {
				errs = append(errs, fmt.Sprintf("upstream %q has transport 'sse' but url is empty", up.ID))
			} else {
				parsedURL, err := url.ParseRequestURI(up.URL)
				if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
					errs = append(errs, fmt.Sprintf("upstream %q url %q is not a valid HTTP/HTTPS URL", up.ID, up.URL))
				}
			}
		}

		if time.Duration(up.Timeout) < 0 {
			errs = append(errs, fmt.Sprintf("upstream %q timeout cannot be negative", up.ID))
		}
	}

	if len(errs) > 0 {
		return &ValidationError{Errors: errs}
	}
	return nil
}

// Validate verifies a Config instance for validity.
func Validate(cfg *Config) error {
	if cfg == nil {
		return errors.New("configuration cannot be nil")
	}
	return cfg.Validate()
}

// ---------------------------------------------------------------------
// Adapter Methods: Exporting to proxy, router, and pruner packages
// ---------------------------------------------------------------------

// ToProxyConfig converts configuration to a proxy.ProxyConfig instance.
func (c *Config) ToProxyConfig() proxy.ProxyConfig {
	return proxy.ProxyConfig{
		PruneMode:              c.Pruning.Tier,
		MaxSchemaTokens:        c.Pruning.MaxSchemaTokens,
		PinnedTools:            append([]string(nil), c.Routing.PinnedTools...),
		RequestTimeout:         time.Duration(c.Server.Timeout),
		TimeoutSweeperInterval: 0, // Automatically dynamically calibrated
		InjectProxyInfo:        c.Server.InjectProxyInfo,
	}
}

// ToSSEServerConfig converts configuration to a proxy.SSEServerConfig instance.
func (c *Config) ToSSEServerConfig() proxy.SSEServerConfig {
	return proxy.SSEServerConfig{
		Host:            c.Server.Host,
		Port:            c.Server.Port,
		BasePath:        c.Server.BasePath,
		CORSOrigins:     c.Server.CORSOrigins,
		KeepAlivePeriod: 15 * time.Second,
		MaxBodySize:     10 * 1024 * 1024,
	}
}

// ToRouterConfig converts configuration to a router.RouterConfig instance.
func (c *Config) ToRouterConfig() router.RouterConfig {
	return router.RouterConfig{
		NamespaceMode:  router.NamespaceMode(c.Routing.NamespaceMode),
		PruneMode:      c.Pruning.Tier,
		ActiveProfile:  c.Routing.DefaultProfile,
		MaxActiveTools: c.Routing.ActiveWindowSize,
		EnableCatalog:  c.Pruning.Tier == "dynamic",
	}
}

// ToFilterConfig converts configuration to a router.FilterConfig instance.
func (c *Config) ToFilterConfig() router.FilterConfig {
	profiles := make(map[string]router.ProfileConfig, len(c.Routing.Profiles))
	for k, v := range c.Routing.Profiles {
		name := v.Name
		if name == "" {
			name = k
		}
		profiles[k] = router.ProfileConfig{
			Name:        name,
			Description: v.Description,
			Include:     v.Include,
			Exclude:     v.Exclude,
		}
	}

	return router.FilterConfig{
		ActiveProfile:   c.Routing.DefaultProfile,
		CaseInsensitive: c.Routing.CaseInsensitive,
		GlobalExclude:   c.Routing.GlobalExclude,
		Profiles:        profiles,
	}
}

// ToWindowConfig converts configuration to a router.WindowConfig instance.
func (c *Config) ToWindowConfig() router.WindowConfig {
	return router.WindowConfig{
		Enabled:           c.Routing.ActiveWindowSize > 0,
		MaxActive:         c.Routing.ActiveWindowSize,
		AutoPromoteOnCall: c.Routing.AutoPromote,
		PinnedTools:       c.Routing.PinnedTools,
	}
}

// ToPrunerOptions converts configuration to a pruner.PruneOptions instance.
func (c *Config) ToPrunerOptions() pruner.PruneOptions {
	opts := pruner.DefaultPruneOptions(pruner.PruneTier(c.Pruning.Tier))
	if c.Pruning.MaxSchemaTokens > 0 {
		opts.MaxSchemaTokens = c.Pruning.MaxSchemaTokens
	}
	if c.Pruning.MaxDescriptionLength > 0 {
		opts.MaxToolDescLength = c.Pruning.MaxDescriptionLength
		opts.Condense.MaxToolDescLen = c.Pruning.MaxDescriptionLength
	}
	if c.Pruning.MaxParamDescLength > 0 {
		opts.MaxParamDescLength = c.Pruning.MaxParamDescLength
		opts.Condense.MaxParamDescLen = c.Pruning.MaxParamDescLength
		opts.Params.MaxNonSelfEvidentLen = c.Pruning.MaxParamDescLength
	}
	if c.Pruning.MaxRegexLength > 0 {
		opts.StripRegexThresholdLen = c.Pruning.MaxRegexLength
		opts.Condense.MaxRegexLength = c.Pruning.MaxRegexLength
		opts.Params.PatternMaxLen = c.Pruning.MaxRegexLength
	}
	opts.Condense.StripVerboseRegex = c.Pruning.StripRegex
	opts.Params.StripLongPatterns = c.Pruning.StripRegex
	opts.StripSelfEvident = c.Pruning.StripSelfEvident
	opts.StrictDraft07 = c.Pruning.StrictDraft07
	opts.Condense.StripCodeBlocks = c.Pruning.StripCodeBlocks
	opts.Condense.StripHTMLTags = c.Pruning.StripHTMLTags
	return opts
}

// ToSupervisorConfig creates a proxy.SupervisorConfig for an upstream stdio server.
func (u *UpstreamConfig) ToSupervisorConfig(logger *slog.Logger) proxy.SupervisorConfig {
	gracePeriod := 2 * time.Second
	if u.Timeout > 0 {
		gracePeriod = time.Duration(u.Timeout)
	}
	return proxy.SupervisorConfig{
		Name:         u.ID,
		Command:      u.Command,
		Args:         u.Args,
		Env:          u.Env,
		Dir:          u.Dir,
		GracePeriod:  gracePeriod,
		Logger:       logger,
		RingCapacity: 100,
	}
}
