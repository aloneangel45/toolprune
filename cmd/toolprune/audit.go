package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/doctor"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/preset"
	"github.com/aloneangel45/toolprune/pkg/security"
)

type auditFlags struct {
	schemaFile     string
	allIDEs        bool
	dir            string
	format         string
	failOn         string
	lockfile       string
	updateLockfile bool
}

var aFlags auditFlags

var auditCmd = &cobra.Command{
	Use:   "audit [flags]",
	Short: "Audit MCP tool schemas, toolprune.yaml, and local IDE configs for security risks",
	Long: `Scans MCP tool schemas and client/gateway configurations for security threats:
  - Hidden prompt-injection tags & stealth directives (<IMPORTANT>, <SYSTEM>, silently, etc.)
  - Sensitive credential/path exfiltration references (~/.ssh, id_rsa, ~/.aws, .env, /etc/passwd)
  - Concealed zero-width/BiDi Unicode spoofing characters and large obfuscated Base64 payloads
  - Dangerous shell command smuggling patterns
  - SHA-256 rug-pull drift against toolprune.lock
  - Unpinned npx/uvx package execution, hardcoded env secrets, and insecure remote HTTP URLs

Examples:
  toolprune audit --schema-file test/fixtures/schemas.json --format sarif
  toolprune audit --all-ides --fail-on high
  toolprune audit -c toolprune.yaml --format json`,
	RunE: runAudit,
}

func init() {
	auditCmd.Flags().StringVar(&aFlags.schemaFile, "schema-file", "", "Path to JSON tool schema file ([]Tool or {tools: [...]}) to scan")
	auditCmd.Flags().BoolVar(&aFlags.allIDEs, "all-ides", false, "Discover and audit Claude Desktop, Claude Code, Cursor, VS Code, Windsurf, and Zed MCP configs")
	auditCmd.Flags().StringVar(&aFlags.dir, "dir", "", "Target project directory for project-level IDE configs (default: current working directory)")
	auditCmd.Flags().StringVar(&aFlags.format, "format", "table", "Output format: table, json, sarif")
	auditCmd.Flags().StringVar(&aFlags.failOn, "fail-on", "", "Exit non-zero if any finding meets or exceeds severity (critical, high, medium, low)")
	auditCmd.Flags().StringVar(&aFlags.lockfile, "lockfile", "", "Path to toolprune.lock for verifying/pinning static tool schemas")
	auditCmd.Flags().BoolVar(&aFlags.updateLockfile, "update-lockfile", false, "Write/refresh canonical SHA-256 tool hashes into --lockfile (default: toolprune.lock)")
}

// AuditSummary provides structured JSON output for `toolprune audit --format json`.
type AuditSummary struct {
	Version        string             `json:"version"`
	TargetsScanned []string           `json:"targetsScanned"`
	TotalFindings  int                `json:"totalFindings"`
	Counts         map[string]int     `json:"counts"`
	Findings       []security.Finding `json:"findings"`
}

func runAudit(cmd *cobra.Command, args []string) error {
	cfgPath := strings.TrimSpace(globalConfig)
	schemaFile := ""
	if cmd.Flags().Changed("schema-file") {
		schemaFile = strings.TrimSpace(aFlags.schemaFile)
	}
	allIDEs := cmd.Flags().Changed("all-ides") && aFlags.allIDEs
	dirVal := ""
	if cmd.Flags().Changed("dir") {
		dirVal = strings.TrimSpace(aFlags.dir)
	}
	format := "table"
	if cmd.Flags().Changed("format") {
		format = strings.ToLower(strings.TrimSpace(aFlags.format))
	}
	failOnRaw := ""
	if cmd.Flags().Changed("fail-on") {
		failOnRaw = strings.TrimSpace(aFlags.failOn)
	}
	lockfilePath := ""
	if cmd.Flags().Changed("lockfile") {
		lockfilePath = strings.TrimSpace(aFlags.lockfile)
	}
	updateLock := cmd.Flags().Changed("update-lockfile") && aFlags.updateLockfile

	defer func() {
		resetCommandFlags(cmd)
		globalConfig = ""
	}()

	switch format {
	case "table", "json", "sarif":
	default:
		return fmt.Errorf("unsupported audit --format %q: must be table, json, or sarif", format)
	}

	var failThreshold security.Severity
	if failOnRaw != "" {
		parsed, err := security.ParseSeverity(failOnRaw)
		if err != nil {
			return fmt.Errorf("invalid --fail-on value: %w", err)
		}
		failThreshold = parsed
	}

	if dirVal == "" {
		if wd, err := os.Getwd(); err == nil {
			dirVal = wd
		} else {
			dirVal = "."
		}
	}

	// Default behavior when no explicit target is passed: audit local toolprune.yaml if found, otherwise scan all IDEs.
	if cfgPath == "" && schemaFile == "" && !allIDEs {
		if found := config.FindConfigFile(); found != "" {
			cfgPath = found
		} else {
			allIDEs = true
		}
	}

	var targets []string
	var findings []security.Finding

	// 1. Audit toolprune.yaml if requested/discovered
	if cfgPath != "" {
		cfgFindings, err := auditToolpruneYAML(cfgPath)
		if err != nil {
			return err
		}
		targets = append(targets, cfgPath)
		findings = append(findings, cfgFindings...)
	}

	// 2. Audit static JSON tool schema file if requested
	if schemaFile != "" {
		tools, err := loadToolsFromJSONFile(schemaFile)
		if err != nil {
			return err
		}
		targets = append(targets, schemaFile)
		upstreamName := strings.TrimSuffix(filepath.Base(schemaFile), filepath.Ext(schemaFile))
		for _, f := range security.ScanTools(upstreamName, tools) {
			f.FilePath = schemaFile
			findings = append(findings, f)
		}

		if updateLock {
			if lockfilePath == "" {
				lockfilePath = security.DefaultLockfileName
			}
			lf, err := security.LoadLockfile(lockfilePath)
			if err != nil {
				lf = security.NewLockfile()
			}
			lf.Update(upstreamName, tools)
			if err := security.SaveLockfile(lockfilePath, lf); err != nil {
				return fmt.Errorf("failed to write lockfile %q: %w", lockfilePath, err)
			}
		} else if lockfilePath != "" {
			lf, err := security.LoadLockfile(lockfilePath)
			if err != nil {
				return fmt.Errorf("failed to read lockfile %q: %w", lockfilePath, err)
			}
			for _, f := range lf.Verify(upstreamName, tools) {
				f.FilePath = schemaFile
				findings = append(findings, f)
			}
		}
	}

	// 3. Discover and audit all IDE MCP configs on the host
	if allIDEs {
		ideTargets, ideFindings := auditAllIDEConfigs(dirVal)
		targets = append(targets, ideTargets...)
		findings = append(findings, ideFindings...)
	}

	// Deterministic ordering: highest severity first, then RuleID, ToolName, Field
	sort.Slice(findings, func(i, j int) bool {
		ri, rj := security.SeverityRank(findings[i].Severity), security.SeverityRank(findings[j].Severity)
		if ri != rj {
			return ri > rj
		}
		if findings[i].RuleID != findings[j].RuleID {
			return findings[i].RuleID < findings[j].RuleID
		}
		if findings[i].ToolName != findings[j].ToolName {
			return findings[i].ToolName < findings[j].ToolName
		}
		return findings[i].Message < findings[j].Message
	})

	out := cmd.OutOrStdout()
	switch format {
	case "sarif":
		sarifBytes, err := security.FormatSARIF(findings, Version)
		if err != nil {
			return fmt.Errorf("failed to render SARIF output: %w", err)
		}
		_, _ = out.Write(sarifBytes)
	case "json":
		summary := buildAuditSummary(targets, findings)
		data, err := json.MarshalIndent(summary, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON audit report: %w", err)
		}
		fmt.Fprintln(out, string(data))
	case "table":
		renderAuditTable(out, targets, findings)
	}

	if failThreshold != "" {
		triggered := 0
		for _, f := range findings {
			if security.MeetsThreshold(f.Severity, failThreshold) {
				triggered++
			}
		}
		if triggered > 0 {
			return fmt.Errorf("audit failed: %d finding(s) met or exceeded severity threshold %q", triggered, failThreshold)
		}
	}
	return nil
}

func loadToolsFromJSONFile(path string) ([]mcp.Tool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read schema file %q: %w", path, err)
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(data, &tools); err == nil {
		return tools, nil
	}
	var wrapper struct {
		Tools []mcp.Tool `json:"tools"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && wrapper.Tools != nil {
		return wrapper.Tools, nil
	}
	return nil, fmt.Errorf("failed to parse tool schemas from %q: expected JSON array of tools or {\"tools\": [...]}", path)
}

func auditToolpruneYAML(cfgPath string) ([]security.Finding, error) {
	// Read raw YAML before env expansion to distinguish literal hardcoded secrets from ${VAR} placeholders
	rawBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config %q: %w", cfgPath, err)
	}
	// Validate via standard loader
	if _, err := config.Load(cfgPath); err != nil {
		return nil, fmt.Errorf("failed to load config %q: %w", cfgPath, err)
	}

	// Parse without env substitution to preserve ${VAR} placeholders accurately
	cfgRaw, err := parseConfigWithoutEnvExpansion(rawBytes)
	if err != nil {
		return nil, err
	}

	var findings []security.Finding
	for _, up := range cfgRaw.Upstreams {
		if up.Disabled {
			continue
		}
		findings = append(findings, security.AuditServerConfig(security.ServerAuditInput{
			Source:   "toolprune.yaml",
			FilePath: cfgPath,
			Name:     up.ID,
			Command:  up.Command,
			Args:     up.Args,
			URL:      up.URL,
			Env:      up.Env,
		})...)
	}
	return findings, nil
}

func parseConfigWithoutEnvExpansion(data []byte) (*config.Config, error) {
	var cfg config.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse configuration for audit: %w", err)
	}
	return &cfg, nil
}

func auditAllIDEConfigs(projectDir string) ([]string, []security.Finding) {
	type ideSpec struct {
		name      string
		path      string
		schemaKey string
	}

	var candidates []ideSpec
	candidates = append(candidates,
		ideSpec{name: "cursor", path: filepath.Join(projectDir, ".cursor", "mcp.json"), schemaKey: "mcpServers"},
		ideSpec{name: "vscode", path: filepath.Join(projectDir, ".vscode", "mcp.json"), schemaKey: "servers"},
		ideSpec{name: "claude-code", path: filepath.Join(projectDir, ".mcp.json"), schemaKey: "mcpServers"},
	)

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			ideSpec{name: "cursor-global", path: filepath.Join(home, ".cursor", "mcp.json"), schemaKey: "mcpServers"},
			ideSpec{name: "claude-code-global", path: filepath.Join(home, ".claude.json"), schemaKey: "mcpServers"},
		)
	}
	if p, err := preset.DefaultPath(preset.ClientWindsurf); err == nil && p != "" {
		candidates = append(candidates, ideSpec{name: "windsurf", path: p, schemaKey: "mcpServers"})
	}
	if p, err := doctor.DefaultClaudeDesktopPath(); err == nil && p != "" {
		candidates = append(candidates, ideSpec{name: "claude-desktop", path: p, schemaKey: "mcpServers"})
	}
	if p, err := preset.DefaultPath(preset.ClientZed); err == nil && p != "" {
		candidates = append(candidates, ideSpec{name: "zed", path: p, schemaKey: "context_servers"})
	}

	var scanned []string
	var findings []security.Finding
	seenPaths := make(map[string]bool)

	for _, c := range candidates {
		abs := c.path
		if a, err := filepath.Abs(c.path); err == nil {
			abs = a
		}
		if seenPaths[abs] {
			continue
		}
		seenPaths[abs] = true

		fi, err := os.Stat(abs)
		if err != nil || fi.IsDir() {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		clean := preset.NormalizeJSONC(data)
		var top map[string]any
		if err := json.Unmarshal(clean, &top); err != nil || top == nil {
			continue
		}
		serversRaw, ok := top[c.schemaKey]
		if !ok || serversRaw == nil {
			continue
		}
		serversMap, ok := serversRaw.(map[string]any)
		if !ok {
			continue
		}

		scanned = append(scanned, abs)
		names := make([]string, 0, len(serversMap))
		for n := range serversMap {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, srvName := range names {
			entry, ok := serversMap[srvName].(map[string]any)
			if !ok {
				continue
			}
			in := extractServerAuditInput(c.name, abs, srvName, c.schemaKey, entry)
			findings = append(findings, security.AuditServerConfig(in)...)
		}
	}

	return scanned, findings
}

func extractServerAuditInput(source, filePath, name, schemaKey string, entry map[string]any) security.ServerAuditInput {
	in := security.ServerAuditInput{
		Source:   source,
		FilePath: filePath,
		Name:     name,
		Env:      make(map[string]string),
	}

	if schemaKey == "context_servers" {
		if u, ok := entry["url"].(string); ok {
			in.URL = u
		}
		if cmdObj, ok := entry["command"].(map[string]any); ok {
			if p, ok := cmdObj["path"].(string); ok {
				in.Command = p
			}
			in.Args = anySliceToStrings(cmdObj["args"])
			in.Env = anyMapToStringMap(cmdObj["env"])
		}
		return in
	}

	if cmd, ok := entry["command"].(string); ok {
		in.Command = cmd
	}
	in.Args = anySliceToStrings(entry["args"])
	in.Env = anyMapToStringMap(entry["env"])
	if u, ok := entry["url"].(string); ok && u != "" {
		in.URL = u
	} else if su, ok := entry["serverUrl"].(string); ok && su != "" {
		in.URL = su
	}
	return in
}

func anySliceToStrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anyMapToStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func buildAuditSummary(targets []string, findings []security.Finding) AuditSummary {
	counts := map[string]int{
		"critical": 0,
		"high":     0,
		"medium":   0,
		"low":      0,
	}
	for _, f := range findings {
		counts[string(f.Severity)]++
	}
	if findings == nil {
		findings = []security.Finding{}
	}
	if targets == nil {
		targets = []string{}
	}
	return AuditSummary{
		Version:        Version,
		TargetsScanned: targets,
		TotalFindings:  len(findings),
		Counts:         counts,
		Findings:       findings,
	}
}

func renderAuditTable(w io.Writer, targets []string, findings []security.Finding) {
	fmt.Fprintf(w, "ToolPrune Security Audit (v%s)\n", Version)
	fmt.Fprintf(w, "Targets Scanned: %d\n", len(targets))
	for _, t := range targets {
		fmt.Fprintf(w, "  - %s\n", t)
	}
	fmt.Fprintln(w)

	if len(findings) == 0 {
		fmt.Fprintln(w, "No security issues detected.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tRULE\tTARGET\tMESSAGE")
	for _, f := range findings {
		target := f.ToolName
		if target == "" {
			target = f.Upstream
		} else if f.Upstream != "" {
			target = f.Upstream + "/" + f.ToolName
		}
		if f.Field != "" {
			target += " (" + f.Field + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", strings.ToUpper(string(f.Severity)), f.RuleID, target, f.Message)
	}
	_ = tw.Flush()

	summary := buildAuditSummary(targets, findings)
	fmt.Fprintf(w, "\nSummary: %d total finding(s) [critical=%d high=%d medium=%d low=%d]\n",
		summary.TotalFindings,
		summary.Counts["critical"],
		summary.Counts["high"],
		summary.Counts["medium"],
		summary.Counts["low"],
	)
}
