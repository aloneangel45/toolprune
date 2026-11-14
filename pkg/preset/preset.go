// Package preset generates out-of-the-box MCP client configuration files
// (Cursor, Windsurf, VS Code, Claude Code, and Zed) that wire AI clients
// to a ToolPrune proxy instance while preserving unknown sibling keys.
package preset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
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
	// ClientVSCode targets VS Code's .vscode/mcp.json project-level config.
	ClientVSCode Client = "vscode"
	// ClientClaudeCode targets Claude Code's .mcp.json project-level config.
	ClientClaudeCode Client = "claude-code"
	// ClientZed targets Zed's settings.json context_servers config.
	ClientZed Client = "zed"
)

// SupportedClients lists all clients with built-in presets, in stable order.
func SupportedClients() []Client {
	return []Client{
		ClientCursor,
		ClientWindsurf,
		ClientVSCode,
		ClientClaudeCode,
		ClientZed,
	}
}

// ParseClient validates a client name, returning an error listing supported values.
func ParseClient(name string) (Client, error) {
	switch Client(strings.ToLower(strings.TrimSpace(name))) {
	case ClientCursor:
		return ClientCursor, nil
	case ClientWindsurf:
		return ClientWindsurf, nil
	case ClientVSCode:
		return ClientVSCode, nil
	case ClientClaudeCode:
		return ClientClaudeCode, nil
	case ClientZed:
		return ClientZed, nil
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
func DefaultPath(client Client) (string, error) {
	home, homeErr := os.UserHomeDir()
	return defaultPathForEnv(client, runtime.GOOS, os.Getenv("APPDATA"), home, homeErr)
}

func defaultPathForEnv(client Client, goos, appData, home string, homeErr error) (string, error) {
	switch client {
	case ClientCursor:
		return filepath.Join(".cursor", "mcp.json"), nil
	case ClientWindsurf:
		if homeErr != nil || home == "" {
			return "", fmt.Errorf("cannot determine home directory for windsurf config path: %w", homeErr)
		}
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
	case ClientVSCode:
		return filepath.Join(".vscode", "mcp.json"), nil
	case ClientClaudeCode:
		return ".mcp.json", nil
	case ClientZed:
		if goos == "windows" && strings.TrimSpace(appData) != "" {
			return filepath.Join(appData, "Zed", "settings.json"), nil
		}
		if homeErr != nil || home == "" {
			return "", fmt.Errorf("cannot determine home directory for zed config path: %w", homeErr)
		}
		return filepath.Join(home, ".config", "zed", "settings.json"), nil
	default:
		return "", fmt.Errorf("unsupported client %q", client)
	}
}

// ServerSpec describes a single MCP server entry in Cursor, Windsurf, VS Code, or Claude Code configs.
// ExtraFields preserves unknown per-server sibling keys.
type ServerSpec struct {
	Type        string            `json:"type,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	ServerURL   string            `json:"serverUrl,omitempty"`
	URL         string            `json:"url,omitempty"`
	ExtraFields map[string]any    `json:"-"`
}

// MarshalJSON preserves unknown per-server sibling fields alongside typed fields.
func (s ServerSpec) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(s.ExtraFields)+6)
	for k, v := range s.ExtraFields {
		out[k] = v
	}
	if s.Type != "" {
		out["type"] = s.Type
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
	if raw == nil {
		return fmt.Errorf("server entry must be a JSON object")
	}
	type plainSpec struct {
		Type      string            `json:"type"`
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
	s.Type = p.Type
	s.Command = p.Command
	s.Args = p.Args
	s.Env = p.Env
	s.ServerURL = p.ServerURL
	s.URL = p.URL

	delete(raw, "type")
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

// ZedCommandSpec represents Zed's {"path": "...", "args": [...], "env": {...}} command object.
type ZedCommandSpec struct {
	Path        string            `json:"path,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	ExtraFields map[string]any    `json:"-"`
}

// MarshalJSON preserves unknown fields inside Zed's command object.
func (z ZedCommandSpec) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(z.ExtraFields)+3)
	for k, v := range z.ExtraFields {
		out[k] = v
	}
	if z.Path != "" {
		out["path"] = z.Path
	}
	if len(z.Args) > 0 {
		out["args"] = z.Args
	}
	if len(z.Env) > 0 {
		out["env"] = z.Env
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses Zed's command object while retaining unknown sibling keys.
func (z *ZedCommandSpec) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("zed command entry must be a JSON object")
	}
	type plainZedCmd struct {
		Path string            `json:"path"`
		Args []string          `json:"args"`
		Env  map[string]string `json:"env"`
	}
	var p plainZedCmd
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	z.Path = p.Path
	z.Args = p.Args
	z.Env = p.Env

	delete(raw, "path")
	delete(raw, "args")
	delete(raw, "env")
	if len(raw) > 0 {
		z.ExtraFields = raw
	} else {
		z.ExtraFields = nil
	}
	return nil
}

// ZedServerSpec describes a single entry inside Zed's "context_servers" object.
type ZedServerSpec struct {
	Command     *ZedCommandSpec `json:"command,omitempty"`
	URL         string          `json:"url,omitempty"`
	ExtraFields map[string]any  `json:"-"`
}

// MarshalJSON serializes a Zed context_servers entry while preserving sibling keys like "settings".
func (z ZedServerSpec) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(z.ExtraFields)+2)
	for k, v := range z.ExtraFields {
		out[k] = v
	}
	if z.Command != nil {
		out["command"] = z.Command
	}
	if z.URL != "" {
		out["url"] = z.URL
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses a Zed context_servers entry while retaining unknown sibling keys.
func (z *ZedServerSpec) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("zed context server entry must be a JSON object")
	}
	type plainZedServer struct {
		Command *ZedCommandSpec `json:"command"`
		URL     string          `json:"url"`
	}
	var p plainZedServer
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	z.Command = p.Command
	z.URL = p.URL

	delete(raw, "command")
	delete(raw, "url")
	if len(raw) > 0 {
		z.ExtraFields = raw
	} else {
		z.ExtraFields = nil
	}
	return nil
}

// ConfigFile represents the top-level MCP client configuration document across all supported IDEs:
//   - Cursor / Windsurf / Claude Code: top-level "mcpServers"
//   - VS Code: top-level "servers" (preserving sibling keys such as "inputs")
//   - Zed: top-level "context_servers" (preserving all other Zed settings.json keys)
type ConfigFile struct {
	Client         Client                   `json:"-"`
	MCPServers     map[string]ServerSpec    `json:"mcpServers,omitempty"`
	Servers        map[string]ServerSpec    `json:"servers,omitempty"`
	ContextServers map[string]ZedServerSpec `json:"context_servers,omitempty"`
	ExtraFields    map[string]any           `json:"-"`
}

// HasServer reports whether the named server is present in whichever server map ConfigFile holds.
func (c *ConfigFile) HasServer(name string) bool {
	if c == nil {
		return false
	}
	if c.MCPServers != nil {
		if _, ok := c.MCPServers[name]; ok {
			return true
		}
	}
	if c.Servers != nil {
		if _, ok := c.Servers[name]; ok {
			return true
		}
	}
	if c.ContextServers != nil {
		if _, ok := c.ContextServers[name]; ok {
			return true
		}
	}
	return false
}

// MarshalJSON serializes the client-appropriate server map while preserving unknown top-level sibling fields.
func (c ConfigFile) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(c.ExtraFields)+1)
	for k, v := range c.ExtraFields {
		out[k] = v
	}
	switch c.Client {
	case ClientVSCode:
		if c.Servers == nil {
			out["servers"] = map[string]ServerSpec{}
		} else {
			out["servers"] = c.Servers
		}
	case ClientZed:
		if c.ContextServers == nil {
			out["context_servers"] = map[string]ZedServerSpec{}
		} else {
			out["context_servers"] = c.ContextServers
		}
	case ClientCursor, ClientWindsurf, ClientClaudeCode:
		if c.MCPServers == nil {
			out["mcpServers"] = map[string]ServerSpec{}
		} else {
			out["mcpServers"] = c.MCPServers
		}
	default:
		switch {
		case c.Servers != nil && c.MCPServers == nil && c.ContextServers == nil:
			out["servers"] = c.Servers
		case c.ContextServers != nil && c.MCPServers == nil && c.Servers == nil:
			out["context_servers"] = c.ContextServers
		case c.MCPServers == nil:
			out["mcpServers"] = map[string]ServerSpec{}
		default:
			out["mcpServers"] = c.MCPServers
		}
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses the client-appropriate server map while preserving all other top-level keys in ExtraFields.
// Accepts both strict JSON and JSONC (line/block comments, trailing commas, UTF-8 BOM).
func (c *ConfigFile) UnmarshalJSON(data []byte) error {
	clean := NormalizeJSONC(data)
	var raw map[string]any
	if err := json.Unmarshal(clean, &raw); err != nil {
		return err
	}
	if raw == nil {
		return fmt.Errorf("config must be a JSON object")
	}

	switch c.Client {
	case ClientVSCode:
		if _, ok := raw["servers"]; ok {
			var wrapper struct {
				Servers map[string]ServerSpec `json:"servers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.Servers = wrapper.Servers
			delete(raw, "servers")
		}
	case ClientZed:
		if _, ok := raw["context_servers"]; ok {
			var wrapper struct {
				ContextServers map[string]ZedServerSpec `json:"context_servers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.ContextServers = wrapper.ContextServers
			delete(raw, "context_servers")
		}
	case ClientCursor, ClientWindsurf, ClientClaudeCode:
		if _, ok := raw["mcpServers"]; ok {
			var wrapper struct {
				MCPServers map[string]ServerSpec `json:"mcpServers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.MCPServers = wrapper.MCPServers
			delete(raw, "mcpServers")
		}
	default:
		if _, ok := raw["mcpServers"]; ok {
			var wrapper struct {
				MCPServers map[string]ServerSpec `json:"mcpServers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.MCPServers = wrapper.MCPServers
			delete(raw, "mcpServers")
		} else if _, ok := raw["servers"]; ok {
			var wrapper struct {
				Servers map[string]ServerSpec `json:"servers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.Servers = wrapper.Servers
			c.Client = ClientVSCode
			delete(raw, "servers")
		} else if _, ok := raw["context_servers"]; ok {
			var wrapper struct {
				ContextServers map[string]ZedServerSpec `json:"context_servers"`
			}
			if err := json.Unmarshal(clean, &wrapper); err != nil {
				return err
			}
			c.ContextServers = wrapper.ContextServers
			c.Client = ClientZed
			delete(raw, "context_servers")
		}
	}

	if len(raw) > 0 {
		c.ExtraFields = raw
	} else {
		c.ExtraFields = nil
	}
	return nil
}

// Options controls preset generation.
type Options struct {
	// Client selects the target client preset (cursor, windsurf, vscode, claude-code, zed).
	Client Client
	// ServerName is the key under the client's servers map (default: "toolprune").
	ServerName string
	// Command is the executable used to launch the ToolPrune proxy (default: "toolprune").
	Command string
	// Args are extra arguments passed to the proxy (e.g. ["proxy", "--config", "toolprune.yaml"]).
	Args []string
	// Env holds extra environment variables for the stdio server entry.
	Env map[string]string
	// ServerURL, when set, emits a remote server entry instead of a stdio command.
	ServerURL string
	// ConfigPath, when set, is appended to Args as ["--config", <path>].
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

func (o Options) resolvedArgs() []string {
	args := append([]string(nil), o.Args...)
	if len(args) == 0 {
		args = []string{"proxy"}
	}
	if o.ConfigPath != "" {
		args = append(args, "--config", o.ConfigPath)
	}
	return args
}

// serverSpec builds the standard ServerSpec for Cursor, Windsurf, VS Code, or Claude Code.
func (o Options) serverSpec(client Client) ServerSpec {
	switch client {
	case ClientVSCode, ClientClaudeCode:
		if o.ServerURL != "" {
			return ServerSpec{
				Type: "sse",
				URL:  o.ServerURL,
				Env:  o.Env,
			}
		}
		return ServerSpec{
			Type:    "stdio",
			Command: o.Command,
			Args:    o.resolvedArgs(),
			Env:     o.Env,
		}
	default:
		if o.ServerURL != "" {
			return ServerSpec{
				Env:       o.Env,
				ServerURL: o.ServerURL,
			}
		}
		return ServerSpec{
			Command: o.Command,
			Args:    o.resolvedArgs(),
			Env:     o.Env,
		}
	}
}

func (o Options) zedServerSpec() ZedServerSpec {
	if o.ServerURL != "" {
		return ZedServerSpec{
			URL: o.ServerURL,
		}
	}
	return ZedServerSpec{
		Command: &ZedCommandSpec{
			Path: o.Command,
			Args: o.resolvedArgs(),
			Env:  o.Env,
		},
	}
}

// Generate builds the client MCP config file content for the given options.
func Generate(o Options) (*ConfigFile, error) {
	client, err := ParseClient(string(o.Client))
	if err != nil {
		return nil, err
	}
	o.Client = client
	if strings.TrimSpace(o.ServerName) == "" {
		o.ServerName = "toolprune"
	}
	if o.ServerURL == "" && strings.TrimSpace(o.Command) == "" {
		o.Command = "toolprune"
	}

	switch client {
	case ClientVSCode:
		return &ConfigFile{
			Client: ClientVSCode,
			Servers: map[string]ServerSpec{
				o.ServerName: o.serverSpec(ClientVSCode),
			},
		}, nil
	case ClientZed:
		return &ConfigFile{
			Client: ClientZed,
			ContextServers: map[string]ZedServerSpec{
				o.ServerName: o.zedServerSpec(),
			},
		}, nil
	default:
		return &ConfigFile{
			Client: client,
			MCPServers: map[string]ServerSpec{
				o.ServerName: o.serverSpec(client),
			},
		}, nil
	}
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

func mergeExtraFields(prev, next map[string]any) map[string]any {
	if len(prev) == 0 && len(next) == 0 {
		return nil
	}
	out := make(map[string]any, len(prev)+len(next))
	for k, v := range prev {
		out[k] = v
	}
	for k, v := range next {
		out[k] = v
	}
	return out
}

// MergeInto reads an existing client config file (if present) and returns a new ConfigFile
// with the ToolPrune server entry added or replaced, preserving all other servers as well as
// unknown top-level and per-server sibling fields (including all Zed settings.json keys and VS Code inputs).
// A missing or empty file yields a fresh ConfigFile.
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
	clean := NormalizeJSONC(data)
	if len(bytes.TrimSpace(clean)) == 0 {
		return cfg, nil
	}

	existing := ConfigFile{Client: cfg.Client}
	if err := json.Unmarshal(clean, &existing); err != nil {
		return nil, fmt.Errorf("existing config %q is not valid JSON: %w", existingPath, err)
	}

	name := o.ServerName
	if strings.TrimSpace(name) == "" {
		name = "toolprune"
	}

	switch cfg.Client {
	case ClientVSCode:
		if existing.Servers == nil {
			existing.Servers = map[string]ServerSpec{}
		}
		newSpec := cfg.Servers[name]
		if prevSpec, hasPrev := existing.Servers[name]; hasPrev {
			newSpec.ExtraFields = mergeExtraFields(prevSpec.ExtraFields, newSpec.ExtraFields)
		}
		existing.Servers[name] = newSpec

	case ClientZed:
		if existing.ContextServers == nil {
			existing.ContextServers = map[string]ZedServerSpec{}
		}
		newSpec := cfg.ContextServers[name]
		if prevSpec, hasPrev := existing.ContextServers[name]; hasPrev {
			newSpec.ExtraFields = mergeExtraFields(prevSpec.ExtraFields, newSpec.ExtraFields)
			if prevSpec.Command != nil && newSpec.Command != nil {
				newSpec.Command.ExtraFields = mergeExtraFields(prevSpec.Command.ExtraFields, newSpec.Command.ExtraFields)
			}
		}
		existing.ContextServers[name] = newSpec

	default:
		if existing.MCPServers == nil {
			existing.MCPServers = map[string]ServerSpec{}
		}
		newSpec := cfg.MCPServers[name]
		if prevSpec, hasPrev := existing.MCPServers[name]; hasPrev {
			newSpec.ExtraFields = mergeExtraFields(prevSpec.ExtraFields, newSpec.ExtraFields)
		}
		existing.MCPServers[name] = newSpec
	}

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
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %q: %w", dir, err)
		}
	}
	if err := atomicWritePresetFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write %s config %q: %w", o.Client, path, err)
	}
	return nil
}

func atomicWritePresetFile(path string, data []byte, defaultPerm os.FileMode) error {
	perm := defaultPerm
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".toolprune-preset-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// Validate checks that raw JSON bytes conform to a supported MCP client config schema:
//   - "mcpServers" (Cursor, Windsurf, Claude Code)
//   - "servers" (VS Code)
//   - "context_servers" (Zed settings.json)
func Validate(data []byte) error {
	raw, err := unmarshalTopLevelObject(data)
	if err != nil {
		return err
	}
	if serversRaw, ok := raw["mcpServers"]; ok {
		return validateStandardServers(serversRaw, "mcpServers")
	}
	if serversRaw, ok := raw["servers"]; ok {
		return validateStandardServers(serversRaw, "servers")
	}
	if serversRaw, ok := raw["context_servers"]; ok {
		return validateZedContextServers(serversRaw)
	}
	return fmt.Errorf(`missing required "mcpServers", "servers", or "context_servers" object`)
}

// ValidateForClient checks that raw JSON bytes strictly conform to the given client's MCP config schema.
func ValidateForClient(client Client, data []byte) error {
	c, err := ParseClient(string(client))
	if err != nil {
		return err
	}
	raw, err := unmarshalTopLevelObject(data)
	if err != nil {
		return err
	}
	switch c {
	case ClientVSCode:
		serversRaw, ok := raw["servers"]
		if !ok {
			return fmt.Errorf(`missing required "servers" object for vscode config`)
		}
		return validateStandardServers(serversRaw, "servers")
	case ClientZed:
		serversRaw, ok := raw["context_servers"]
		if !ok {
			return fmt.Errorf(`missing required "context_servers" object for zed settings.json`)
		}
		return validateZedContextServers(serversRaw)
	default:
		serversRaw, ok := raw["mcpServers"]
		if !ok {
			return fmt.Errorf(`missing required "mcpServers" object for %s config`, c)
		}
		return validateStandardServers(serversRaw, "mcpServers")
	}
}

// ValidateFile reads a client MCP config file from disk and validates its schema.
// When an explicit client is provided, validation enforces that client's top-level schema;
// otherwise client schema conventions are inferred from the file path or auto-detected.
func ValidateFile(path string, client ...Client) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read %q: %w", path, err)
	}
	if len(client) > 0 && strings.TrimSpace(string(client[0])) != "" {
		return ValidateForClient(client[0], data)
	}
	if inferred, ok := inferClientFromPath(path); ok {
		return ValidateForClient(inferred, data)
	}
	return Validate(data)
}

func inferClientFromPath(p string) (Client, bool) {
	trimmed := strings.TrimSpace(p)
	if trimmed == "" {
		return "", false
	}
	slashPath := pathpkg.Clean(strings.ReplaceAll(trimmed, `\`, `/`))
	base := pathpkg.Base(slashPath)
	parent := pathpkg.Base(pathpkg.Dir(slashPath))

	switch {
	case parent == ".vscode" && base == "mcp.json":
		return ClientVSCode, true
	case parent == ".cursor" && base == "mcp.json":
		return ClientCursor, true
	case base == ".mcp.json":
		return ClientClaudeCode, true
	case base == "mcp_config.json":
		return ClientWindsurf, true
	case base == "settings.json" && strings.EqualFold(parent, "zed"):
		return ClientZed, true
	default:
		return "", false
	}
}

func unmarshalTopLevelObject(data []byte) (map[string]json.RawMessage, error) {
	clean := NormalizeJSONC(data)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(clean, &raw); err != nil {
		return nil, fmt.Errorf("config is not a valid JSON object: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("config is not a valid JSON object")
	}
	return raw, nil
}

func validateStandardServers(serversRaw json.RawMessage, keyName string) error {
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil {
		return fmt.Errorf("%q must be an object: %w", keyName, err)
	}
	if servers == nil {
		return fmt.Errorf("%q must be an object", keyName)
	}
	if len(servers) == 0 {
		return fmt.Errorf("%q must contain at least one server", keyName)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%q contains an empty server name", keyName)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(servers[name], &spec); err != nil || spec == nil {
			if err != nil {
				return fmt.Errorf("server %q must be an object: %w", name, err)
			}
			return fmt.Errorf("server %q must be an object", name)
		}

		_, hasCommand := spec["command"]
		_, hasServerURL := spec["serverUrl"]
		_, hasURL := spec["url"]

		if typeRaw, hasType := spec["type"]; hasType {
			var srvType string
			if err := json.Unmarshal(typeRaw, &srvType); err != nil || strings.TrimSpace(srvType) == "" {
				return fmt.Errorf("server %q has an invalid or empty \"type\"", name)
			}
			switch strings.ToLower(strings.TrimSpace(srvType)) {
			case "stdio":
				if !hasCommand {
					return fmt.Errorf("server %q with type %q must define \"command\"", name, srvType)
				}
			case "sse", "http", "streamable-http":
				if !hasURL && !hasServerURL {
					return fmt.Errorf("server %q with type %q must define \"url\" or \"serverUrl\"", name, srvType)
				}
			default:
				return fmt.Errorf("server %q has unsupported \"type\" %q: must be stdio, sse, http, or streamable-http", name, srvType)
			}
		}

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

func validateZedContextServers(serversRaw json.RawMessage) error {
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(serversRaw, &servers); err != nil {
		return fmt.Errorf(`"context_servers" must be an object: %w`, err)
	}
	if servers == nil {
		return fmt.Errorf(`"context_servers" must be an object`)
	}
	if len(servers) == 0 {
		return fmt.Errorf(`"context_servers" must contain at least one server`)
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf(`"context_servers" contains an empty server name`)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(servers[name], &spec); err != nil || spec == nil {
			if err != nil {
				return fmt.Errorf("context server %q must be an object: %w", name, err)
			}
			return fmt.Errorf("context server %q must be an object", name)
		}

		cmdRaw, hasCommand := spec["command"]
		urlRaw, hasURL := spec["url"]
		if !hasCommand && !hasURL {
			return fmt.Errorf("context server %q must define \"command\" ({path, args, env}) or \"url\"", name)
		}
		if hasCommand {
			var cmdObj map[string]json.RawMessage
			if err := json.Unmarshal(cmdRaw, &cmdObj); err != nil || cmdObj == nil {
				return fmt.Errorf("context server %q \"command\" must be an object with \"path\"", name)
			}
			pathRaw, hasPath := cmdObj["path"]
			if !hasPath {
				return fmt.Errorf("context server %q \"command\" is missing required \"path\"", name)
			}
			var cmdPath string
			if err := json.Unmarshal(pathRaw, &cmdPath); err != nil || strings.TrimSpace(cmdPath) == "" {
				return fmt.Errorf("context server %q \"command.path\" must be a non-empty string", name)
			}
			if argsRaw, ok := cmdObj["args"]; ok {
				var args []string
				if err := json.Unmarshal(argsRaw, &args); err != nil {
					return fmt.Errorf("context server %q \"command.args\" must be an array of strings: %w", name, err)
				}
			}
			if envRaw, ok := cmdObj["env"]; ok {
				var env map[string]string
				if err := json.Unmarshal(envRaw, &env); err != nil {
					return fmt.Errorf("context server %q \"command.env\" must be an object of string values: %w", name, err)
				}
			}
		}
		if hasURL {
			var u string
			if err := json.Unmarshal(urlRaw, &u); err != nil || strings.TrimSpace(u) == "" {
				return fmt.Errorf("context server %q has an invalid or empty \"url\"", name)
			}
		}
	}
	return nil
}
