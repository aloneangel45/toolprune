// Package preset generates out-of-the-box MCP client configuration files
// (Cursor .cursor/mcp.json and Windsurf ~/.codeium/windsurf/mcp_config.json)
// that wire AI clients to a ToolPrune proxy instance.
package preset

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aloneangel45/toolprune/pkg/config"
)

// Client identifies a supported AI client whose MCP configuration ToolPrune can generate.
type Client string

const (
	// ClientCursor targets Cursor's .cursor/mcp.json project-level config.
	ClientCursor Client = "cursor"
	// ClientWindsurf targets Windsurf's ~/.codeium/windsurf/mcp_config.json global config.
	ClientWindsurf Client = "windsurf"
)

// SupportedClients lists all clients with built-in presets, in stable order.
func SupportedClients() []Client {
	return []Client{ClientCursor, ClientWindsurf}
}

// ParseClient validates a client name, returning an error listing supported values.
func ParseClient(name string) (Client, error) {
	switch Client(strings.ToLower(strings.TrimSpace(name))) {
	case ClientCursor:
		return ClientCursor, nil
	case ClientWindsurf:
		return ClientWindsurf, nil
	default:
		return "", fmt.Errorf("unsupported client %q: must be one of %s", name, clientNames())
	}
}

func clientNames() string {
	clients := SupportedClients()
	names := make([]string, len(clients))
	for i, c := range clients {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}

// DefaultPath returns the conventional on-disk location of the client's MCP config file.
// For Cursor this is the project-level .cursor/mcp.json; for Windsurf the global
// ~/.codeium/windsurf/mcp_config.json.
func DefaultPath(client Client) (string, error) {
	switch client {
	case ClientCursor:
		return filepath.Join(".cursor", "mcp.json"), nil
	case ClientWindsurf:
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("cannot determine home directory for windsurf config path: %w", err)
		}
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
	default:
		return "", fmt.Errorf("unsupported client %q", client)
	}
}

// ServerSpec describes a single MCP server entry in a client config file.
// Cursor uses {command, args, env} or {url}; Windsurf uses {serverUrl} for remote servers
// or {command, args, env} for stdio servers. ExtraFields preserves unknown sibling keys.
type ServerSpec struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	ServerURL   string            `json:"serverUrl,omitempty"`
	URL         string            `json:"url,omitempty"`
	ExtraFields map[string]any    `json:"-"`
}

// MarshalJSON preserves unknown per-server sibling fields alongside typed fields.
func (s ServerSpec) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(s.ExtraFields)+5)
	for k, v := range s.ExtraFields {
		out[k] = v
	}
	if s.Command != "" {
		out["command"] = s.Command
	}
	if len(s.Args) > 0 {
		out["args"] = s.Args
	}
	if len(s.Env) > 0 {
		out["env"] = s.Env
	}
	if s.ServerURL != "" {
		out["serverUrl"] = s.ServerURL
	}
	if s.URL != "" {
		out["url"] = s.URL
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses typed server fields while retaining unknown sibling keys in ExtraFields.
func (s *ServerSpec) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type plainSpec struct {
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		ServerURL string            `json:"serverUrl"`
		URL       string            `json:"url"`
	}
	var p plainSpec
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	s.Command = p.Command
	s.Args = p.Args
	s.Env = p.Env
	s.ServerURL = p.ServerURL
	s.URL = p.URL

	delete(raw, "command")
	delete(raw, "args")
	delete(raw, "env")
	delete(raw, "serverUrl")
	delete(raw, "url")
	if len(raw) > 0 {
		s.ExtraFields = raw
	} else {
		s.ExtraFields = nil
	}
	return nil
}

// ConfigFile is the top-level shape of both Cursor and Windsurf MCP config files:
// a JSON object with an "mcpServers" map of server name -> ServerSpec, plus any preserved top-level sibling fields.
type ConfigFile struct {
	MCPServers  map[string]ServerSpec `json:"mcpServers"`
	ExtraFields map[string]any        `json:"-"`
}

// MarshalJSON serializes mcpServers while preserving unknown top-level sibling fields.
func (c ConfigFile) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(c.ExtraFields)+1)
	for k, v := range c.ExtraFields {
		out[k] = v
	}
	if c.MCPServers == nil {
		out["mcpServers"] = map[string]ServerSpec{}
	} else {
		out["mcpServers"] = c.MCPServers
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses mcpServers while preserving unknown top-level sibling fields via map[string]any.
func (c *ConfigFile) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var wrapper struct {
		MCPServers map[string]ServerSpec `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	c.MCPServers = wrapper.MCPServers
	delete(raw, "mcpServers")
	if len(raw) > 0 {
		c.ExtraFields = raw
	} else {
		c.ExtraFields = nil
	}
	return nil
}

// Options controls preset generation.
type Options struct {
	// Client selects the target client preset (cursor or windsurf).
	Client Client
	// ServerName is the key under "mcpServers" (default: "toolprune").
	ServerName string
	// Command is the executable used to launch the ToolPrune proxy (default: "toolprune").
	Command string
	// Args are extra arguments passed to the proxy (e.g. ["proxy", "--config", "toolprune.yaml"]).
	Args []string
	// Env holds extra environment variables for the stdio server entry.
	Env map[string]string
	// ServerURL, when set, emits a remote {serverUrl} entry instead of a stdio command
	// (Windsurf-style remote connection to a ToolPrune SSE endpoint).
	ServerURL string
	// ConfigPath, when set, is appended to Args as ["proxy", "--config", <path>].
	ConfigPath string
}

// DefaultOptions returns Options targeting the client with a plain stdio proxy entry.
func DefaultOptions(client Client) Options {
	return Options{
		Client:     client,
		ServerName: "toolprune",
		Command:    "toolprune",
		Args:       []string{"proxy"},
	}
}

// serverSpec builds the ServerSpec for the configured options.
func (o Options) serverSpec() ServerSpec {
	args := append([]string(nil), o.Args...)
	if o.ConfigPath != "" {
		args = append(args, "--config", o.ConfigPath)
	}
	spec := ServerSpec{
		Env:       o.Env,
		ServerURL: o.ServerURL,
	}
	if o.ServerURL == "" {
		spec.Command = o.Command
		if len(args) == 0 {
			spec.Args = []string{"proxy"}
		} else {
			spec.Args = args
		}
	}
	return spec
}

// Generate builds the client MCP config file content for the given options.
func Generate(o Options) (*ConfigFile, error) {
	if _, err := ParseClient(string(o.Client)); err != nil {
		return nil, err
	}
	if strings.TrimSpace(o.ServerName) == "" {
		o.ServerName = "toolprune"
	}
	if o.ServerURL == "" && strings.TrimSpace(o.Command) == "" {
		o.Command = "toolprune"
	}
	return &ConfigFile{
		MCPServers: map[string]ServerSpec{
			o.ServerName: o.serverSpec(),
		},
	}, nil
}

// GenerateJSON renders the preset as indented JSON suitable for writing to disk.
func GenerateJSON(o Options) ([]byte, error) {
	cfg, err := Generate(o)
	if err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal %s preset: %w", o.Client, err)
	}
	return append(data, '\n'), nil
}

// GenerateFromConfig derives preset options from a loaded ToolPrune Config:
// the proxy is launched in standalone mode with the config file path, and the
// first enabled stdio upstream's env is NOT inherited (client env stays explicit).
func GenerateFromConfig(cfg *config.Config, client Client, configPath string) (*ConfigFile, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	o := DefaultOptions(client)
	o.ConfigPath = configPath
	o.Args = []string{"proxy"}
	return Generate(o)
}

// MergeInto reads an existing client config file (if present) via map[string]any
// and returns a new ConfigFile with the ToolPrune server entry added or replaced,
// preserving all other servers as well as unknown top-level and per-server sibling fields.
// A missing file yields a fresh ConfigFile.
func MergeInto(existingPath string, o Options) (*ConfigFile, error) {
	cfg, err := Generate(o)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(existingPath)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("failed to read existing config %q: %w", existingPath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}

	var existing ConfigFile
	if err := json.Unmarshal(data, &existing); err != nil {
		return nil, fmt.Errorf("existing config %q is not valid JSON: %w", existingPath, err)
	}
	if existing.MCPServers == nil {
		existing.MCPServers = map[string]ServerSpec{}
	}
	name := o.ServerName
	if strings.TrimSpace(name) == "" {
		name = "toolprune"
	}
	newSpec := cfg.MCPServers[name]
	if prevSpec, hasPrev := existing.MCPServers[name]; hasPrev && len(prevSpec.ExtraFields) > 0 {
		preserved := make(map[string]any, len(prevSpec.ExtraFields))
		for k, v := range prevSpec.ExtraFields {
			preserved[k] = v
		}
		for k, v := range newSpec.ExtraFields {
			preserved[k] = v
		}
		newSpec.ExtraFields = preserved
	}
	existing.MCPServers[name] = newSpec
	return &existing, nil
}

// WriteFile generates the preset (merging with any existing file at path) and
// writes it as indented JSON, creating parent directories as needed.
func WriteFile(path string, o Options) error {
	merged, err := MergeInto(path, o)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal %s preset: %w", o.Client, err)
	}
	data = append(data, '\n')
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write %s config %q: %w", o.Client, path, err)
	}
	return nil
}

// Validate checks that raw JSON bytes conform to the client MCP config schema:
// a JSON object whose "mcpServers" value is an object mapping names to server
// entries that carry either a "command" (stdio), "serverUrl" (remote), or "url" (remote).
func Validate(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("config is not a valid JSON object: %w", err)
	}
	serversRaw, ok := raw["mcpServers"]
	if !ok {
		return fmt.Errorf(`missing required "mcpServers" object`)
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil {
		return fmt.Errorf(`"mcpServers" must be an object: %w`, err)
	}
	if len(servers) == 0 {
		return fmt.Errorf(`"mcpServers" must contain at least one server`)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf(`"mcpServers" contains an empty server name`)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(servers[name], &spec); err != nil {
			return fmt.Errorf("server %q must be an object: %w", name, err)
		}
		_, hasCommand := spec["command"]
		_, hasServerURL := spec["serverUrl"]
		_, hasURL := spec["url"]
		if !hasCommand && !hasServerURL && !hasURL {
			return fmt.Errorf("server %q must define \"command\" (stdio), \"serverUrl\" (remote), or \"url\" (remote)", name)
		}
		if hasCommand {
			var cmd string
			if err := json.Unmarshal(spec["command"], &cmd); err != nil || strings.TrimSpace(cmd) == "" {
				return fmt.Errorf("server %q has an invalid or empty \"command\"", name)
			}
		}
		if hasServerURL {
			var u string
			if err := json.Unmarshal(spec["serverUrl"], &u); err != nil || strings.TrimSpace(u) == "" {
				return fmt.Errorf("server %q has an invalid or empty \"serverUrl\"", name)
			}
		}
		if hasURL {
			var u string
			if err := json.Unmarshal(spec["url"], &u); err != nil || strings.TrimSpace(u) == "" {
				return fmt.Errorf("server %q has an invalid or empty \"url\"", name)
			}
		}
		if argsRaw, ok := spec["args"]; ok {
			var args []string
			if err := json.Unmarshal(argsRaw, &args); err != nil {
				return fmt.Errorf("server %q has invalid \"args\": must be an array of strings: %w", name, err)
			}
		}
		if envRaw, ok := spec["env"]; ok {
			var env map[string]string
			if err := json.Unmarshal(envRaw, &env); err != nil {
				return fmt.Errorf("server %q has invalid \"env\": must be an object of string values: %w", name, err)
			}
		}
	}
	return nil
}
