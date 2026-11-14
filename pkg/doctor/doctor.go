// Package doctor performs read-only local MCP environment, configuration,
// upstream executable/URL, and IDE client preset health checks for ToolPrune.
package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/preset"
)

// Options configures a ToolPrune doctor health check run.
type Options struct {
	Version     string
	Commit      string
	BuildDate   string
	ProjectDir  string
	DirExplicit bool
	ConfigPath  string
}

// BinaryReport summarizes the ToolPrune binary build info and PATH discoverability.
type BinaryReport struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	InPath       bool   `json:"inPath"`
	PathLocation string `json:"pathLocation,omitempty"`
	Status       string `json:"status"`
	Hint         string `json:"hint,omitempty"`
}

// UpstreamCheck summarizes health verification of a single configured upstream in toolprune.yaml.
type UpstreamCheck struct {
	ID        string `json:"id"`
	Transport string `json:"transport"`
	Target    string `json:"target"`
	Resolved  string `json:"resolved,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// ConfigReport summarizes discovery and validation of toolprune.yaml and its upstreams.
type ConfigReport struct {
	Found     bool            `json:"found"`
	Path      string          `json:"path,omitempty"`
	Valid     bool            `json:"valid"`
	Status    string          `json:"status"`
	Error     string          `json:"error,omitempty"`
	Upstreams []UpstreamCheck `json:"upstreams,omitempty"`
}

// ClientConfigCheck summarizes read-only inspection of a discovered or candidate MCP client config.
type ClientConfigCheck struct {
	Client         string   `json:"client"`
	Scope          string   `json:"scope"`
	Path           string   `json:"path"`
	Found          bool     `json:"found"`
	Status         string   `json:"status"`
	TotalServers   int      `json:"totalServers"`
	WrappedServers int      `json:"wrappedServers"`
	UnwrappedCount int      `json:"unwrappedCount"`
	UnwrappedNames []string `json:"unwrappedServers,omitempty"`
	Hint           string   `json:"hint,omitempty"`
	Warning        string   `json:"warning,omitempty"`
}

// Report is the top-level diagnostic result produced by Run.
type Report struct {
	Healthy    bool                `json:"healthy"`
	ProjectDir string              `json:"projectDir"`
	Binary     BinaryReport        `json:"binary"`
	Config     ConfigReport        `json:"config"`
	Clients    []ClientConfigCheck `json:"clients"`
	Errors     []string            `json:"errors,omitempty"`
}

// DefaultClaudeDesktopPath resolves the platform-standard Claude Desktop config path.
func DefaultClaudeDesktopPath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Claude", "claude_desktop_config.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

// IsToolpruneBinary reports whether cmdName refers to a toolprune executable.
func IsToolpruneBinary(cmdName string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(cmdName)))
	return base == "toolprune" || base == "toolprune.exe"
}

// Run executes all read-only environment, toolprune.yaml, and MCP client config health checks.
func Run(opts Options) *Report {
	projDir := strings.TrimSpace(opts.ProjectDir)
	if projDir == "" {
		if wd, err := os.Getwd(); err == nil {
			projDir = wd
		} else {
			projDir = "."
		}
	}
	if abs, err := filepath.Abs(projDir); err == nil {
		projDir = abs
	}

	version := opts.Version
	if version == "" {
		version = "dev"
	}
	commit := opts.Commit
	if commit == "" {
		commit = "dev"
	}
	buildDate := opts.BuildDate
	if buildDate == "" {
		buildDate = "unknown"
	}

	rep := &Report{
		Healthy:    true,
		ProjectDir: projDir,
		Binary:     checkBinary(version, commit, buildDate),
	}

	cfgRep, cfgErrs := checkToolpruneConfig(projDir, opts.DirExplicit, opts.ConfigPath)
	rep.Config = cfgRep
	if len(cfgErrs) > 0 {
		rep.Healthy = false
		rep.Errors = append(rep.Errors, cfgErrs...)
	}

	rep.Clients = scanClientConfigs(projDir)
	return rep
}

func checkBinary(version, commit, buildDate string) BinaryReport {
	br := BinaryReport{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
	if p, err := exec.LookPath("toolprune"); err == nil && p != "" {
		br.InPath = true
		br.PathLocation = p
		br.Status = "ok"
	} else {
		br.InPath = false
		br.Status = "warn"
		br.Hint = "Add the directory containing 'toolprune' to your PATH so MCP clients can spawn 'toolprune proxy'"
	}
	return br
}

func discoverConfigPath(projectDir string, dirExplicit bool, explicitConfig string) (path string, explicit bool, err error) {
	if strings.TrimSpace(explicitConfig) != "" {
		clean := filepath.Clean(explicitConfig)
		fi, statErr := os.Stat(clean)
		if statErr != nil || fi.IsDir() {
			return clean, true, fmt.Errorf("specified configuration file %q does not exist", explicitConfig)
		}
		return clean, true, nil
	}

	projectCandidates := []string{
		filepath.Join(projectDir, "toolprune.yaml"),
		filepath.Join(projectDir, "toolprune.yml"),
		filepath.Join(projectDir, "toolprune.json"),
	}
	for _, cand := range projectCandidates {
		if fi, statErr := os.Stat(cand); statErr == nil && !fi.IsDir() {
			return cand, false, nil
		}
	}

	if !dirExplicit {
		if found := config.FindConfigFile(); found != "" {
			return found, false, nil
		}
	}
	return "", false, nil
}

func checkToolpruneConfig(projectDir string, dirExplicit bool, explicitConfig string) (ConfigReport, []string) {
	cfgPath, _, err := discoverConfigPath(projectDir, dirExplicit, explicitConfig)
	if err != nil {
		msg := err.Error()
		return ConfigReport{
			Found:  false,
			Path:   cfgPath,
			Valid:  false,
			Status: "error",
			Error:  msg,
		}, []string{msg}
	}
	if cfgPath == "" {
		return ConfigReport{
			Found:  false,
			Valid:  true,
			Status: "not_found",
		}, nil
	}

	cfg, loadErr := config.Load(cfgPath)
	if loadErr != nil {
		msg := loadErr.Error()
		return ConfigReport{
			Found:  true,
			Path:   cfgPath,
			Valid:  false,
			Status: "error",
			Error:  msg,
		}, []string{msg}
	}

	if valErr := config.Validate(cfg); valErr != nil {
		msg := fmt.Sprintf("configuration %q failed validation: %v", cfgPath, valErr)
		return ConfigReport{
			Found:  true,
			Path:   cfgPath,
			Valid:  false,
			Status: "error",
			Error:  msg,
		}, []string{msg}
	}

	var errs []string
	checks := make([]UpstreamCheck, 0, len(cfg.Upstreams))
	for _, up := range cfg.Upstreams {
		transport := strings.ToLower(strings.TrimSpace(up.Transport))
		if transport == "" {
			transport = "stdio"
		}
		target := up.Command
		if transport == "sse" || transport == "streamable-http" {
			target = up.URL
		}

		uc := UpstreamCheck{
			ID:        up.ID,
			Transport: transport,
			Target:    target,
			Disabled:  up.Disabled,
		}

		if up.Disabled {
			uc.Status = "disabled"
			checks = append(checks, uc)
			continue
		}

		switch transport {
		case "stdio":
			resolved, lookErr := resolveUpstreamExecutable(up.Command, up.Dir, projectDir)
			if lookErr != nil {
				uc.Status = "error"
				uc.Error = fmt.Sprintf("upstream %q executable %q not found on PATH", up.ID, up.Command)
				errs = append(errs, uc.Error)
			} else {
				uc.Status = "ok"
				uc.Resolved = resolved
			}
		case "sse", "streamable-http":
			parsed, uErr := url.ParseRequestURI(up.URL)
			if uErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				uc.Status = "error"
				uc.Error = fmt.Sprintf("upstream %q URL %q is not a valid HTTP/HTTPS URL", up.ID, up.URL)
				errs = append(errs, uc.Error)
			} else {
				uc.Status = "ok"
				uc.Resolved = up.URL
			}
		default:
			uc.Status = "error"
			uc.Error = fmt.Sprintf("upstream %q has unsupported transport %q", up.ID, up.Transport)
			errs = append(errs, uc.Error)
		}

		checks = append(checks, uc)
	}

	status := "ok"
	valid := true
	errSummary := ""
	if len(errs) > 0 {
		status = "error"
		valid = false
		errSummary = strings.Join(errs, "; ")
	}

	return ConfigReport{
		Found:     true,
		Path:      cfgPath,
		Valid:     valid,
		Status:    status,
		Error:     errSummary,
		Upstreams: checks,
	}, errs
}

func resolveUpstreamExecutable(cmdName, upDir, projectDir string) (string, error) {
	trimmed := strings.TrimSpace(cmdName)
	if trimmed == "" {
		return "", fmt.Errorf("empty command")
	}
	if upDir != "" && !filepath.IsAbs(trimmed) {
		cand := filepath.Join(upDir, trimmed)
		if p, err := exec.LookPath(cand); err == nil {
			return p, nil
		}
	}
	if !filepath.IsAbs(trimmed) && strings.ContainsAny(trimmed, `/\`) {
		cand := filepath.Join(projectDir, trimmed)
		if p, err := exec.LookPath(cand); err == nil {
			return p, nil
		}
	}
	return exec.LookPath(trimmed)
}

type clientTarget struct {
	client    string
	scope     string
	path      string
	schemaKey string // "mcpServers", "servers", or "context_servers"
	wrapHint  string
}

func scanClientConfigs(projectDir string) []ClientConfigCheck {
	targets := []clientTarget{
		{
			client:    "cursor",
			scope:     "project",
			path:      filepath.Join(projectDir, ".cursor", "mcp.json"),
			schemaKey: "mcpServers",
			wrapHint:  "toolprune preset generate --client cursor --output .cursor/mcp.json --merge",
		},
		{
			client:    "vscode",
			scope:     "project",
			path:      filepath.Join(projectDir, ".vscode", "mcp.json"),
			schemaKey: "servers",
			wrapHint:  "toolprune preset generate --client vscode --output .vscode/mcp.json --merge",
		},
		{
			client:    "claude-code",
			scope:     "project",
			path:      filepath.Join(projectDir, ".mcp.json"),
			schemaKey: "mcpServers",
			wrapHint:  "toolprune preset generate --client claude-code --output .mcp.json --merge",
		},
	}

	if windsurfPath, err := preset.DefaultPath(preset.ClientWindsurf); err == nil && windsurfPath != "" {
		targets = append(targets, clientTarget{
			client:    "windsurf",
			scope:     "user",
			path:      windsurfPath,
			schemaKey: "mcpServers",
			wrapHint:  "toolprune preset generate --client windsurf --merge",
		})
	}
	if claudeDesktopPath, err := DefaultClaudeDesktopPath(); err == nil && claudeDesktopPath != "" {
		targets = append(targets, clientTarget{
			client:    "claude-desktop",
			scope:     "user",
			path:      claudeDesktopPath,
			schemaKey: "mcpServers",
			wrapHint:  "toolprune config patch-claude",
		})
	}
	if zedPath, err := preset.DefaultPath(preset.ClientZed); err == nil && zedPath != "" {
		targets = append(targets, clientTarget{
			client:    "zed",
			scope:     "user",
			path:      zedPath,
			schemaKey: "context_servers",
			wrapHint:  "toolprune preset generate --client zed --merge",
		})
	}

	results := make([]ClientConfigCheck, 0, len(targets))
	for _, t := range targets {
		results = append(results, inspectSingleClientConfig(t))
	}
	return results
}

func inspectSingleClientConfig(t clientTarget) ClientConfigCheck {
	res := ClientConfigCheck{
		Client: t.client,
		Scope:  t.scope,
		Path:   t.path,
	}

	fi, err := os.Stat(t.path)
	if err != nil || fi.IsDir() {
		res.Found = false
		res.Status = "not_found"
		return res
	}
	res.Found = true

	data, err := os.ReadFile(t.path)
	if err != nil {
		res.Status = "warn"
		res.Warning = fmt.Sprintf("unable to read file: %v", err)
		return res
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		res.Status = "ok"
		return res
	}

	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		res.Status = "warn"
		res.Warning = "file is not valid JSON"
		return res
	}

	serversRaw, hasKey := top[t.schemaKey]
	if !hasKey || serversRaw == nil {
		res.Status = "ok"
		return res
	}
	serversMap, ok := serversRaw.(map[string]any)
	if !ok {
		res.Status = "warn"
		res.Warning = fmt.Sprintf("%q is not a JSON object", t.schemaKey)
		return res
	}

	names := make([]string, 0, len(serversMap))
	for name := range serversMap {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		entry, ok := serversMap[name].(map[string]any)
		if !ok {
			continue
		}
		if t.schemaKey == "context_servers" {
			counted, wrapped := classifyZedEntry(name, entry)
			if !counted {
				continue
			}
			res.TotalServers++
			if wrapped {
				res.WrappedServers++
			} else {
				res.UnwrappedCount++
				res.UnwrappedNames = append(res.UnwrappedNames, name)
			}
		} else {
			res.TotalServers++
			if classifyStandardEntry(name, entry) {
				res.WrappedServers++
			} else {
				res.UnwrappedCount++
				res.UnwrappedNames = append(res.UnwrappedNames, name)
			}
		}
	}

	if res.UnwrappedCount > 0 {
		res.Status = "unwrapped"
		res.Hint = t.wrapHint
	} else {
		res.Status = "ok"
	}
	return res
}

func isToolpruneServerKey(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return lower == "toolprune" || strings.HasPrefix(lower, "toolprune_") || strings.HasPrefix(lower, "toolprune-")
}

func classifyStandardEntry(name string, entry map[string]any) bool {
	if isToolpruneServerKey(name) {
		return true
	}
	if cmdStr, ok := entry["command"].(string); ok {
		if IsToolpruneBinary(cmdStr) {
			return true
		}
		base := strings.ToLower(filepath.Base(strings.TrimSpace(cmdStr)))
		if base == "docker" || base == "docker.exe" {
			if rawArgs, ok := entry["args"].([]any); ok {
				for _, a := range rawArgs {
					if s, ok := a.(string); ok && strings.Contains(strings.ToLower(s), "toolprune") {
						return true
					}
				}
			}
		}
	}
	return false
}

func classifyZedEntry(name string, entry map[string]any) (counted bool, wrapped bool) {
	cmdObj, hasCmd := entry["command"].(map[string]any)
	_, hasURL := entry["url"].(string)
	if !hasCmd && !hasURL {
		// Extension-provided Zed context server with only {"settings": ...}
		return false, false
	}
	if isToolpruneServerKey(name) {
		return true, true
	}
	if hasCmd {
		if cmdPath, ok := cmdObj["path"].(string); ok && IsToolpruneBinary(cmdPath) {
			return true, true
		}
	}
	return true, false
}

// FormatReport renders a clean, actionable human-readable report to w.
func FormatReport(w io.Writer, r *Report) {
	fmt.Fprintf(w, "ToolPrune Doctor (v%s, %s/%s, %s)\n",
		r.Binary.Version, r.Binary.OS, r.Binary.Arch, r.Binary.GoVersion)
	fmt.Fprintf(w, "Project Directory: %s\n\n", r.ProjectDir)

	// 1. Binary & PATH
	fmt.Fprintln(w, "1. Binary & PATH")
	if r.Binary.InPath {
		fmt.Fprintf(w, "   [OK]   toolprune found on PATH: %s\n", r.Binary.PathLocation)
	} else {
		fmt.Fprintln(w, "   [WARN] toolprune not found on PATH")
		if r.Binary.Hint != "" {
			fmt.Fprintf(w, "          Hint: %s\n", r.Binary.Hint)
		}
	}
	fmt.Fprintln(w)

	// 2. Configuration & Upstreams
	fmt.Fprintln(w, "2. Configuration (toolprune.yaml)")
	switch r.Config.Status {
	case "not_found":
		fmt.Fprintln(w, "   [INFO] No toolprune.yaml found (optional for zero-config wrapper mode; run 'toolprune config init' for gateway mode)")
	case "error":
		if r.Config.Path != "" {
			fmt.Fprintf(w, "   [FAIL] %s\n", r.Config.Path)
		}
		if len(r.Config.Upstreams) == 0 && r.Config.Error != "" {
			fmt.Fprintf(w, "          Error: %s\n", r.Config.Error)
		}
	case "ok":
		fmt.Fprintf(w, "   [OK]   %s (%d upstreams configured)\n", r.Config.Path, len(r.Config.Upstreams))
	}

	if len(r.Config.Upstreams) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "   UPSTREAM\tTRANSPORT\tSTATUS\tDETAIL")
		for _, up := range r.Config.Upstreams {
			detail := up.Resolved
			if up.Disabled {
				detail = "disabled"
			} else if up.Error != "" {
				detail = up.Error
			} else if detail == "" {
				detail = up.Target
			}
			fmt.Fprintf(tw, "   %s\t%s\t%s\t%s\n", up.ID, up.Transport, strings.ToUpper(up.Status), detail)
		}
		_ = tw.Flush()
	}
	fmt.Fprintln(w)

	// 3. MCP Client Configurations
	fmt.Fprintln(w, "3. MCP Client Configurations (read-only scan)")
	discovered := 0
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "   CLIENT\tSCOPE\tSTATUS\tSERVERS\tWRAPPED\tUNWRAPPED\tPATH")
	for _, c := range r.Clients {
		if !c.Found {
			fmt.Fprintf(tw, "   %s\t%s\tNOT FOUND\t-\t-\t-\t%s\n", c.Client, c.Scope, c.Path)
			continue
		}
		discovered++
		fmt.Fprintf(tw, "   %s\t%s\t%s\t%d\t%d\t%d\t%s\n",
			c.Client, c.Scope, strings.ToUpper(c.Status), c.TotalServers, c.WrappedServers, c.UnwrappedCount, c.Path)
	}
	_ = tw.Flush()

	for _, c := range r.Clients {
		if c.Warning != "" {
			fmt.Fprintf(w, "   [WARN] %s (%s): %s\n", c.Client, c.Path, c.Warning)
		}
		if c.UnwrappedCount > 0 {
			fmt.Fprintf(w, "   [HINT] %s has %d unwrapped server(s) [%s]\n",
				c.Client, c.UnwrappedCount, strings.Join(c.UnwrappedNames, ", "))
			fmt.Fprintf(w, "          Wrap with: %s\n", c.Hint)
		}
	}
	if discovered == 0 {
		fmt.Fprintln(w, "   [INFO] No existing IDE MCP config files detected yet.")
	}
	fmt.Fprintln(w)

	if r.Healthy {
		fmt.Fprintln(w, "Overall Status: HEALTHY")
	} else {
		fmt.Fprintf(w, "Overall Status: UNHEALTHY (%d issue(s) found)\n", len(r.Errors))
	}
}
