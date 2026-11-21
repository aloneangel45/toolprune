// Package security implements ToolPrune's MCP tool-poisoning scanner,
// SHA-256 rug-pull lockfile verifier, MCPGate-compatible JSONL security event logger,
// IDE/config static auditor, and SARIF 2.1.0 report exporter.
package security

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// Security enforcement modes.
const (
	ModeOff     = "off"
	ModeWarn    = "warn"
	ModeEnforce = "enforce"
	ModeUpdate  = "update"
)

// Finding severities.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
	SeverityInfo     Severity = "info"
)

// Rule IDs for ToolPrune security checks (mapped to SARIF 2.1.0 rules).
const (
	RulePromptInjection      = "TP-SEC-001"
	RuleSensitivePathExfil   = "TP-SEC-002"
	RuleUnicodeSpoofing      = "TP-SEC-003"
	RuleConcealedBase64      = "TP-SEC-004"
	RuleCommandSmuggling     = "TP-SEC-005"
	RuleRugPullMidSession    = "TP-SEC-006"
	RuleRugPullLockfileDrift = "TP-SEC-007"
	RuleRugPullUnpinned      = "TP-SEC-008"
	RuleUnpinnedPackageExec  = "TP-SEC-009"
	RuleHardcodedSecret      = "TP-SEC-010"
	RuleInsecureTransport    = "TP-SEC-011"
)

// Finding categories.
const (
	CategoryPromptInjection   = "prompt_injection"
	CategoryExfiltration      = "exfiltration"
	CategoryConcealedPayload  = "concealed_payload"
	CategoryCommandSmuggling  = "command_smuggling"
	CategoryRugPull           = "rug_pull"
	CategorySupplyChain       = "supply_chain"
	CategorySecretExposure    = "secret_exposure"
	CategoryInsecureTransport = "insecure_transport"
)

// ValidMode reports whether mode is one of off, warn, enforce, update.
func ValidMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case ModeOff, ModeWarn, ModeEnforce, ModeUpdate:
		return true
	default:
		return false
	}
}

// ParseSeverity validates and normalizes a severity threshold string.
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case string(SeverityCritical):
		return SeverityCritical, nil
	case string(SeverityHigh):
		return SeverityHigh, nil
	case string(SeverityMedium):
		return SeverityMedium, nil
	case string(SeverityLow):
		return SeverityLow, nil
	case string(SeverityInfo):
		return SeverityInfo, nil
	default:
		return "", fmt.Errorf("invalid severity %q: must be one of critical, high, medium, low, info", s)
	}
}

// SeverityRank maps severity to a numeric rank for sorting and threshold comparison.
func SeverityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// MeetsThreshold returns true when sev is at least as severe as threshold.
func MeetsThreshold(sev, threshold Severity) bool {
	return SeverityRank(sev) >= SeverityRank(threshold)
}

// Finding represents a single security violation detected in an MCP tool or configuration.
type Finding struct {
	RuleID      string   `json:"ruleId"`
	RuleName    string   `json:"ruleName"`
	Category    string   `json:"category"`
	Severity    Severity `json:"severity"`
	ToolName    string   `json:"toolName,omitempty"`
	Upstream    string   `json:"upstream,omitempty"`
	Field       string   `json:"field,omitempty"`
	FilePath    string   `json:"filePath,omitempty"`
	Message     string   `json:"message"`
	Evidence    string   `json:"evidence,omitempty"`
	Remediation string   `json:"remediation,omitempty"`
}

// MCPGateEvent is a structured JSONL security event compatible with MCPGate telemetry pipelines.
type MCPGateEvent struct {
	Timestamp     string   `json:"timestamp"`
	SchemaVersion string   `json:"schema_version"`
	EventType     string   `json:"event_type"`
	Action        string   `json:"action"`
	Upstream      string   `json:"upstream"`
	ToolName      string   `json:"tool_name,omitempty"`
	ToolHash      string   `json:"tool_hash,omitempty"`
	Severity      Severity `json:"severity,omitempty"`
	RuleID        string   `json:"rule_id,omitempty"`
	Category      string   `json:"category,omitempty"`
	Message       string   `json:"message"`
	Evidence      string   `json:"evidence,omitempty"`
}

// GuardConfig configures the runtime Security Guard attached to ToolPrune Proxy/Router.
type GuardConfig struct {
	Mode             string       // "off", "warn", "enforce", "update"
	LockfilePath     string       // Path to toolprune.lock (optional; empty disables disk lockfile)
	SecurityLogPath  string       // Optional MCPGate-compatible JSONL event log path
	BlockOnPoisoning bool         // In enforce mode, quarantine tools with high/critical poisoning findings
	Logger           *slog.Logger // Optional structured logger
}

// Guard inspects MCP tools/list responses for tool poisoning and SHA-256 rug-pull mutations.
type Guard struct {
	mu           sync.Mutex
	cfg          GuardConfig
	lockfile     *Lockfile
	seenHashes   map[string]string // key: upstream + "\x00" + toolName -> first observed sha256
	quarantined  map[string]string // key: upstream + "\x00" + toolName -> reason
	logger       *slog.Logger
	logFile      *os.File
	alertsCount  uint64
	blockedCount uint64
}

// NewGuard initializes a runtime security guard, loading any existing lockfile and opening
// the optional MCPGate JSONL security log file.
func NewGuard(cfg GuardConfig) (*Guard, error) {
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	if mode == "" {
		mode = ModeOff
	}
	if !ValidMode(mode) {
		return nil, fmt.Errorf("invalid security mode %q: must be off, warn, enforce, or update", cfg.Mode)
	}
	cfg.Mode = mode
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	g := &Guard{
		cfg:         cfg,
		seenHashes:  make(map[string]string),
		quarantined: make(map[string]string),
		logger:      cfg.Logger,
	}

	if cfg.LockfilePath != "" && cfg.Mode != ModeOff {
		lf, err := LoadLockfile(cfg.LockfilePath)
		if err != nil {
			if os.IsNotExist(err) {
				if cfg.Mode == ModeUpdate {
					g.lockfile = NewLockfile()
				}
			} else {
				return nil, fmt.Errorf("failed to load security lockfile %q: %w", cfg.LockfilePath, err)
			}
		} else {
			g.lockfile = lf
		}
	}

	if cfg.SecurityLogPath != "" && cfg.Mode != ModeOff {
		dir := filepath.Dir(cfg.SecurityLogPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("failed to create security log directory %q: %w", dir, err)
			}
		}
		f, err := os.OpenFile(cfg.SecurityLogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return nil, fmt.Errorf("failed to open security log %q: %w", cfg.SecurityLogPath, err)
		}
		if runtime.GOOS != "windows" {
			_ = f.Chmod(0600)
		}
		g.logFile = f
	}

	return g, nil
}

// Close flushes and closes any open security log file handle.
func (g *Guard) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.logFile != nil {
		syncErr := g.logFile.Sync()
		closeErr := g.logFile.Close()
		g.logFile = nil
		if closeErr != nil {
			return closeErr
		}
		return syncErr
	}
	return nil
}

// Mode returns the configured guard mode.
func (g *Guard) Mode() string {
	if g == nil {
		return ModeOff
	}
	return g.cfg.Mode
}

// Enabled reports whether the security guard is active (mode != off).
func (g *Guard) Enabled() bool {
	return g != nil && g.cfg.Mode != ModeOff
}

// IsQuarantined reports whether the named tool for an upstream was blocked in enforce mode.
// Handles both prefixed ("server__tool") and unprefixed ("tool") names so namespace
// differences never bypass quarantine enforcement.
func (g *Guard) IsQuarantined(upstream, toolName string) (string, bool) {
	if g == nil || g.cfg.Mode != ModeEnforce {
		return "", false
	}
	upstream = strings.TrimSpace(upstream)
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "", false
	}
	if upstream == "" {
		upstream = "default"
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	if reason, ok := g.quarantined[upstream+"\x00"+toolName]; ok {
		return reason, true
	}
	// Handle prefixed caller name ("server__tool") when quarantined under bare "tool"
	prefix := upstream + "__"
	if strings.HasPrefix(toolName, prefix) {
		bare := strings.TrimPrefix(toolName, prefix)
		if reason, ok := g.quarantined[upstream+"\x00"+bare]; ok {
			return reason, true
		}
	} else {
		// Handle bare caller name ("tool") when quarantined under prefixed "server__tool"
		if reason, ok := g.quarantined[upstream+"\x00"+prefix+toolName]; ok {
			return reason, true
		}
	}
	// Handle explicit "prefix__tool" when caller omitted or used a different upstream label
	if pfx, bare, hasDelim := strings.Cut(toolName, "__"); hasDelim && pfx != "" && bare != "" {
		if reason, ok := g.quarantined[pfx+"\x00"+bare]; ok {
			return reason, true
		}
		if reason, ok := g.quarantined[pfx+"\x00"+toolName]; ok {
			return reason, true
		}
	}
	return "", false
}

// IsQuarantinedAnyUpstream reports whether toolName (prefixed or bare) matches any
// quarantined tool across all upstreams in enforce mode.
func (g *Guard) IsQuarantinedAnyUpstream(toolName string) (string, bool) {
	if g == nil || g.cfg.Mode != ModeEnforce {
		return "", false
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return "", false
	}
	if pfx, bare, hasDelim := strings.Cut(toolName, "__"); hasDelim && pfx != "" && bare != "" {
		if reason, ok := g.IsQuarantined(pfx, bare); ok {
			return reason, true
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	for key, reason := range g.quarantined {
		up, name, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if name == toolName || (up+"__"+name) == toolName {
			return reason, true
		}
	}
	return "", false
}

// EvaluateTools scans an upstream's tools/list response for tool-poisoning payloads,
// verifies SHA-256 lockfile pins and mid-session rug-pull immutability, logs structured
// MCPGate JSONL events, and quarantines offending tools in enforce mode.
func (g *Guard) EvaluateTools(upstream string, tools []mcp.Tool) (allowed []mcp.Tool, findings []Finding, blocked int) {
	if g == nil || g.cfg.Mode == ModeOff {
		return tools, nil, 0
	}
	if upstream == "" {
		upstream = "default"
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	allowed = make([]mcp.Tool, 0, len(tools))
	hashes := make(map[string]string, len(tools))
	for _, t := range tools {
		hashes[t.Name] = ComputeToolHash(t)
	}

	// Lockfile update mode refreshes pins on disk while still running poisoning scans.
	if g.cfg.Mode == ModeUpdate && g.cfg.LockfilePath != "" {
		if g.lockfile == nil {
			g.lockfile = NewLockfile()
		}
		g.lockfile.Update(upstream, tools)
		if err := SaveLockfile(g.cfg.LockfilePath, g.lockfile); err != nil {
			g.logger.Error("failed to update toolprune.lock", "path", g.cfg.LockfilePath, "err", err)
		} else {
			g.emitEventLocked(MCPGateEvent{
				EventType: "lockfile_updated",
				Action:    "allow",
				Upstream:  upstream,
				Severity:  SeverityInfo,
				Message:   fmt.Sprintf("updated %d pinned tool hashes in %s", len(tools), g.cfg.LockfilePath),
			})
		}
	}

	// Build map of lockfile findings if a lockfile is loaded (for warn/enforce modes).
	lockFindingsByTool := make(map[string][]Finding)
	if g.lockfile != nil && (g.cfg.Mode == ModeWarn || g.cfg.Mode == ModeEnforce) {
		for _, lf := range g.lockfile.Verify(upstream, tools) {
			lockFindingsByTool[lf.ToolName] = append(lockFindingsByTool[lf.ToolName], lf)
		}
	}

	for _, tool := range tools {
		toolHash := hashes[tool.Name]
		toolFindings := ScanTool(upstream, tool)

		// Mid-session rug-pull mutation detection
		seenKey := upstream + "\x00" + tool.Name
		if prevHash, seen := g.seenHashes[seenKey]; seen && prevHash != toolHash {
			toolFindings = append(toolFindings, Finding{
				RuleID:      RuleRugPullMidSession,
				RuleName:    "MidSessionRugPull",
				Category:    CategoryRugPull,
				Severity:    SeverityCritical,
				ToolName:    tool.Name,
				Upstream:    upstream,
				Field:       "schema",
				Message:     fmt.Sprintf("mid-session rug-pull detected: tool %q schema hash mutated from %s to %s", tool.Name, shortHash(prevHash), shortHash(toolHash)),
				Evidence:    fmt.Sprintf("previous=%s current=%s", prevHash, toolHash),
				Remediation: "Quarantine the upstream server immediately and inspect why its tool schema mutated mid-session.",
			})
		} else if !seen {
			g.seenHashes[seenKey] = toolHash
		}

		// Append any lockfile drift/unpinned findings
		if lfs, ok := lockFindingsByTool[tool.Name]; ok {
			toolFindings = append(toolFindings, lfs...)
		}

		shouldBlock := false
		if g.cfg.Mode == ModeEnforce {
			for _, f := range toolFindings {
				if f.RuleID == RuleRugPullMidSession || f.RuleID == RuleRugPullLockfileDrift {
					shouldBlock = true
					break
				}
				if g.cfg.BlockOnPoisoning && MeetsThreshold(f.Severity, SeverityHigh) {
					shouldBlock = true
					break
				}
			}
		}

		for _, f := range toolFindings {
			findings = append(findings, f)
			g.alertsCount++
			action := "warn"
			eventType := "tool_scan"
			if f.Category == CategoryRugPull {
				eventType = "rug_pull_detected"
			}
			if shouldBlock {
				action = "block"
				eventType = "tool_quarantined"
			}
			g.logger.Warn("ToolPrune security alert",
				"action", action,
				"ruleId", f.RuleID,
				"severity", f.Severity,
				"upstream", upstream,
				"tool", tool.Name,
				"field", f.Field,
				"message", f.Message,
			)
			g.emitEventLocked(MCPGateEvent{
				EventType: eventType,
				Action:    action,
				Upstream:  upstream,
				ToolName:  tool.Name,
				ToolHash:  toolHash,
				Severity:  f.Severity,
				RuleID:    f.RuleID,
				Category:  f.Category,
				Message:   f.Message,
				Evidence:  f.Evidence,
			})
		}

		if shouldBlock {
			blocked++
			g.blockedCount++
			reason := "tool quarantined by ToolPrune security policy"
			if len(toolFindings) > 0 {
				reason = fmt.Sprintf("%s (%s)", toolFindings[0].Message, toolFindings[0].RuleID)
			}
			g.quarantined[seenKey] = reason
			continue
		}
		delete(g.quarantined, seenKey)
		allowed = append(allowed, tool)
	}

	return allowed, findings, blocked
}

func (g *Guard) emitEventLocked(ev MCPGateEvent) {
	if g.logFile == nil {
		return
	}
	ev.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	ev.SchemaVersion = "mcpgate/1.0"
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	_, _ = g.logFile.Write(append(line, '\n'))
}

// -----------------------------------------------------------------------------
// Static Tool-Poisoning Scanner
// -----------------------------------------------------------------------------

type patternRule struct {
	ruleID      string
	ruleName    string
	category    string
	severity    Severity
	regex       *regexp.Regexp
	description string
	remediation string
}

var (
	promptInjectionRules = []patternRule{
		{
			ruleID:      RulePromptInjection,
			ruleName:    "PromptInjectionTagOrDirective",
			category:    CategoryPromptInjection,
			severity:    SeverityCritical,
			regex:       regexp.MustCompile(`(?i)<\s*/?\s*(?:IMPORTANT|SYSTEM|INSTRUCTIONS?|HIDDEN|ADMIN)\b[^>]{0,120}>|\[\s*/?\s*(?:SYSTEM|INST|IMPORTANT|INSTRUCTIONS?|HIDDEN|ADMIN)\s*\]`),
			description: "hidden prompt-injection tag detected in tool metadata",
			remediation: "Remove embedded instruction tags (<IMPORTANT>, <SYSTEM>, etc.) from tool descriptions or reject untrusted MCP server.",
		},
		{
			ruleID:      RulePromptInjection,
			ruleName:    "PromptInjectionOverrideDirective",
			category:    CategoryPromptInjection,
			severity:    SeverityHigh,
			regex:       regexp.MustCompile(`(?i)\b(?:ignore\s+(?:all\s+)?previous\s+instructions|disregard\s+(?:all\s+)?(?:prior|previous)|override\s+(?:all\s+)?(?:prior|previous)\s+instructions|forget\s+(?:all\s+)?previous\s+instructions|you\s+must\s+first|before\s+(?:calling|executing|using)\s+this\s+tool|do\s+not\s+(?:mention|tell|inform|reveal)\s+(?:this\s+)?to\s+the\s+user|never\s+(?:mention|tell|inform)\s+the\s+user|without\s+(?:telling|notifying|informing)\s+the\s+user|hide\s+this\s+from\s+the\s+user)\b`),
			description: "coercive prompt-injection or stealth override directive detected",
			remediation: "Audit the MCP server source; legitimate tool schemas should never instruct the model to hide actions or override prior instructions.",
		},
		{
			ruleID:      RulePromptInjection,
			ruleName:    "PromptInjectionStealthDirective",
			category:    CategoryPromptInjection,
			severity:    SeverityHigh,
			regex:       regexp.MustCompile(`(?i)\bsilently\b`),
			description: "stealth execution directive ('silently') detected in tool metadata",
			remediation: "Review why this tool instructs silent/covert model behavior and block untrusted definitions.",
		},
	}

	sensitivePathRules = []patternRule{
		{
			ruleID:      RuleSensitivePathExfil,
			ruleName:    "SensitivePathExfiltration",
			category:    CategoryExfiltration,
			severity:    SeverityHigh,
			regex:       regexp.MustCompile(`(?i)(?:~/\.ssh|/\.ssh\b|\bid_rsa\b|\bid_ed25519\b|\bauthorized_keys\b|~/\.aws|/\.aws/credentials\b|/etc/passwd\b|/etc/shadow\b|\bcredentials\.json\b|~/\.kube/config\b|~/\.gnupg\b|\bclaude_desktop_config\.json\b)`),
			description: "reference to sensitive credential or system file path detected",
			remediation: "Block tool definitions instructing access to SSH keys, AWS credentials, /etc/passwd, or client secret stores.",
		},
		{
			ruleID:      RuleSensitivePathExfil,
			ruleName:    "DotEnvExfiltration",
			category:    CategoryExfiltration,
			severity:    SeverityHigh,
			regex:       regexp.MustCompile(`(?i)(?:^|[\s"'` + "`" + `(/\\~])\.env(?:\.local|\.prod|\.production|\.secret)?(?:$|[\s"'` + "`" + `),;:/\\])`),
			description: "reference to .env secret file detected in tool metadata",
			remediation: "Verify why the tool schema references .env files; block definitions that coerce reading environment secrets.",
		},
	}

	commandSmugglingRules = []patternRule{
		{
			ruleID:      RuleCommandSmuggling,
			ruleName:    "ShellCommandSmuggling",
			category:    CategoryCommandSmuggling,
			severity:    SeverityHigh,
			regex:       regexp.MustCompile(`(?i)(?:\b(?:curl|wget)\b[^\n|;]{0,160}\|\s*(?:sh|bash|zsh|pwsh|powershell)\b|\bbash\s+-i\s+>&\s*/dev/tcp/|\bnc\s+(?:-[a-z]+\s+)*-e\b|\brm\s+-rf\s+/|\bpowershell(?:\.exe)?\s+[^\n]{0,80}-(?:enc|encodedcommand)\b|\beval\s+\$\(|\$\(\s*(?:curl|wget)\b|\bbase64\s+(?:-d|--decode)\s*\|\s*(?:sh|bash)\b|\bpython3?\s+-c\s+["']import\s+(?:socket|os|subprocess))`),
			description: "dangerous shell command smuggling pattern detected in tool metadata",
			remediation: "Reject tool definitions containing piped remote execution, reverse shells, or encoded command execution.",
		},
	}

	base64BlobRegex = regexp.MustCompile(`[A-Za-z0-9+/]{64,}={0,2}`)
)

// ScanTools runs the tool-poisoning scanner over a slice of MCP tools.
func ScanTools(upstream string, tools []mcp.Tool) []Finding {
	var all []Finding
	for _, t := range tools {
		all = append(all, ScanTool(upstream, t)...)
	}
	return all
}

// ScanTool inspects a single MCP tool's name, title, description, and nested JSON schema fields.
func ScanTool(upstream string, tool mcp.Tool) []Finding {
	var findings []Finding
	scan := func(field, text string) {
		if text == "" {
			return
		}
		findings = append(findings, ScanText(upstream, tool.Name, field, text)...)
	}

	scan("name", tool.Name)
	scan("title", tool.Title)
	scan("description", tool.Description)
	if tool.Annotations != nil {
		scan("annotations.title", tool.Annotations.Title)
	}
	scan("inputSchema.title", tool.InputSchema.Title)
	scan("inputSchema.description", tool.InputSchema.Description)
	scan("inputSchema.$comment", tool.InputSchema.Comment)

	walkSchemaStrings(tool.InputSchema.Properties, "inputSchema.properties", 0, scan)
	walkSchemaStrings(tool.InputSchema.Defs, "inputSchema.$defs", 0, scan)
	walkSchemaStrings(tool.InputSchema.Definitions, "inputSchema.definitions", 0, scan)
	walkSchemaStrings(tool.InputSchema.PatternProperties, "inputSchema.patternProperties", 0, scan)
	walkSchemaStrings(tool.InputSchema.DependentSchemas, "inputSchema.dependentSchemas", 0, scan)
	walkSchemaStrings(tool.InputSchema.Not, "inputSchema.not", 0, scan)
	walkSchemaSliceStrings(tool.InputSchema.AnyOf, "inputSchema.anyOf", 0, scan)
	walkSchemaSliceStrings(tool.InputSchema.OneOf, "inputSchema.oneOf", 0, scan)
	walkSchemaSliceStrings(tool.InputSchema.AllOf, "inputSchema.allOf", 0, scan)
	walkSchemaSliceStrings(tool.InputSchema.Examples, "inputSchema.examples", 0, scan)
	for i, req := range tool.InputSchema.Required {
		scan(fmt.Sprintf("inputSchema.required[%d]", i), req)
	}
	if len(tool.InputSchema.DependentRequired) > 0 {
		depKeys := make([]string, 0, len(tool.InputSchema.DependentRequired))
		for k := range tool.InputSchema.DependentRequired {
			depKeys = append(depKeys, k)
		}
		sort.Strings(depKeys)
		for _, k := range depKeys {
			scan("inputSchema.dependentRequired."+k+"(key)", k)
			for i, dep := range tool.InputSchema.DependentRequired[k] {
				scan(fmt.Sprintf("inputSchema.dependentRequired.%s[%d]", k, i), dep)
			}
		}
	}
	if ap, ok := tool.InputSchema.AdditionalProperties.(map[string]any); ok {
		walkSchemaStrings(ap, "inputSchema.additionalProperties", 0, scan)
	}
	for i, icon := range tool.Icons {
		scan(fmt.Sprintf("icons[%d].src", i), icon.Src)
	}
	walkSchemaStrings(tool.OutputSchema, "outputSchema", 0, scan)

	return findings
}

const maxSchemaScanDepth = 64

func walkSchemaStrings(m map[string]any, prefix string, depth int, emit func(field, text string)) {
	if len(m) == 0 || depth > maxSchemaScanDepth {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := m[k]
		field := prefix + "." + k
		if len(k) >= 4 {
			emit(field+"(key)", k)
		}
		switch val := v.(type) {
		case string:
			if len(val) >= 4 {
				emit(field, val)
			}
		case map[string]any:
			walkSchemaStrings(val, field, depth+1, emit)
		case []any:
			walkSchemaSliceStrings(val, field, depth+1, emit)
		case []string:
			for i, item := range val {
				if len(item) >= 4 {
					emit(fmt.Sprintf("%s[%d]", field, i), item)
				}
			}
		case []map[string]any:
			for i, item := range val {
				walkSchemaStrings(item, fmt.Sprintf("%s[%d]", field, i), depth+1, emit)
			}
		}
	}
}

func walkSchemaSliceStrings(s []any, prefix string, depth int, emit func(field, text string)) {
	if len(s) == 0 || depth > maxSchemaScanDepth {
		return
	}
	for i, item := range s {
		field := fmt.Sprintf("%s[%d]", prefix, i)
		switch val := item.(type) {
		case string:
			if len(val) >= 4 {
				emit(field, val)
			}
		case map[string]any:
			walkSchemaStrings(val, field, depth+1, emit)
		case []any:
			walkSchemaSliceStrings(val, field, depth+1, emit)
		case []string:
			for j, sub := range val {
				if len(sub) >= 4 {
					emit(fmt.Sprintf("%s[%d]", field, j), sub)
				}
			}
		case []map[string]any:
			for j, sub := range val {
				walkSchemaStrings(sub, fmt.Sprintf("%s[%d]", field, j), depth+1, emit)
			}
		}
	}
}

func isConcealedUnicodeRune(r rune, idx int) bool {
	return (r >= 0x200B && r <= 0x200F) || // Zero-width space/joiners & LRM/RLM
		(r >= 0x202A && r <= 0x202E) || // BiDi embeddings & overrides
		(r >= 0x2066 && r <= 0x2069) || // BiDi isolates
		r == 0x2060 || // Word joiner
		(r == 0xFEFF && idx > 0) || // Mid-string BOM / ZWNBSP
		(r >= 0xE0000 && r <= 0xE007F) // Unicode Tags smuggling block
}

// normalizeForPoisoningScan unescapes up to two layers of HTML entities (e.g. &#x3c;IMPORTANT&#x3e;,
// &amp;lt;SYSTEM&amp;gt;) and strips concealed zero-width/BiDi/tag runes so obfuscated
// prompt-injection payloads cannot evade regex detection.
func normalizeForPoisoningScan(text string) string {
	norm := text
	for i := 0; i < 2; i++ {
		unescaped := html.UnescapeString(norm)
		if unescaped == norm {
			break
		}
		norm = unescaped
	}
	var b strings.Builder
	b.Grow(len(norm))
	for i, r := range norm {
		if isConcealedUnicodeRune(r, i) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ScanText scans a single text field belonging to a tool for all tool-poisoning indicators.
func ScanText(upstream, toolName, field, text string) []Finding {
	if text == "" {
		return nil
	}
	var findings []Finding

	normalized := normalizeForPoisoningScan(text)
	candidates := []string{text}
	if normalized != text {
		candidates = append(candidates, normalized)
	}

	matchRule := func(r patternRule) (matchedText string, loc []int) {
		for _, cand := range candidates {
			if l := r.regex.FindStringIndex(cand); l != nil {
				return cand, l
			}
		}
		return "", nil
	}

	// 1. Prompt injection tags & directives
	seenInjection := false
	for _, r := range promptInjectionRules {
		if cand, loc := matchRule(r); loc != nil {
			if seenInjection && r.ruleName == "PromptInjectionStealthDirective" {
				continue
			}
			seenInjection = true
			findings = append(findings, Finding{
				RuleID:      r.ruleID,
				RuleName:    r.ruleName,
				Category:    r.category,
				Severity:    r.severity,
				ToolName:    toolName,
				Upstream:    upstream,
				Field:       field,
				Message:     fmt.Sprintf("%s in %s.%s", r.description, toolName, field),
				Evidence:    extractSnippet(cand, loc[0], loc[1]),
				Remediation: r.remediation,
			})
		}
	}

	// 2. Sensitive path exfiltration
	for _, r := range sensitivePathRules {
		if cand, loc := matchRule(r); loc != nil {
			findings = append(findings, Finding{
				RuleID:      r.ruleID,
				RuleName:    r.ruleName,
				Category:    r.category,
				Severity:    r.severity,
				ToolName:    toolName,
				Upstream:    upstream,
				Field:       field,
				Message:     fmt.Sprintf("%s in %s.%s", r.description, toolName, field),
				Evidence:    extractSnippet(cand, loc[0], loc[1]),
				Remediation: r.remediation,
			})
			break
		}
	}

	// 3. Concealed Unicode zero-width / BiDi / Tag characters
	if badRunes := detectConcealedUnicode(text); len(badRunes) > 0 {
		codes := make([]string, 0, len(badRunes))
		for _, r := range badRunes {
			codes = append(codes, fmt.Sprintf("U+%04X", r))
			if len(codes) >= 6 {
				break
			}
		}
		findings = append(findings, Finding{
			RuleID:      RuleUnicodeSpoofing,
			RuleName:    "ConcealedUnicodeCharacters",
			Category:    CategoryConcealedPayload,
			Severity:    SeverityHigh,
			ToolName:    toolName,
			Upstream:    upstream,
			Field:       field,
			Message:     fmt.Sprintf("suspicious zero-width/BiDi/tag Unicode spoofing characters (%s) detected in %s.%s", strings.Join(codes, ", "), toolName, field),
			Evidence:    fmt.Sprintf("count=%d chars=%s", len(badRunes), strings.Join(codes, ",")),
			Remediation: "Strip hidden zero-width and BiDi override characters or quarantine the upstream MCP server.",
		})
	}

	// 4. Concealed large Base64 blobs
	if blob, decoded, ok := detectSuspiciousBase64(text); ok {
		sev := SeverityMedium
		evidence := fmt.Sprintf("base64(%d chars): %s...", len(blob), truncatePreview(blob, 36))
		if decoded != "" {
			sev = SeverityHigh
			evidence = fmt.Sprintf("decoded=%q", truncatePreview(decoded, 80))
		}
		findings = append(findings, Finding{
			RuleID:      RuleConcealedBase64,
			RuleName:    "ConcealedBase64Payload",
			Category:    CategoryConcealedPayload,
			Severity:    sev,
			ToolName:    toolName,
			Upstream:    upstream,
			Field:       field,
			Message:     fmt.Sprintf("large embedded base64 payload (%d chars) detected in %s.%s", len(blob), toolName, field),
			Evidence:    evidence,
			Remediation: "Tool descriptions must not embed obfuscated base64 instructions or binary payloads.",
		})
	}

	// 5. Command / shell smuggling
	for _, r := range commandSmugglingRules {
		if cand, loc := matchRule(r); loc != nil {
			findings = append(findings, Finding{
				RuleID:      r.ruleID,
				RuleName:    r.ruleName,
				Category:    r.category,
				Severity:    r.severity,
				ToolName:    toolName,
				Upstream:    upstream,
				Field:       field,
				Message:     fmt.Sprintf("%s in %s.%s", r.description, toolName, field),
				Evidence:    extractSnippet(cand, loc[0], loc[1]),
				Remediation: r.remediation,
			})
			break
		}
	}

	return findings
}

func detectConcealedUnicode(s string) []rune {
	var found []rune
	seen := make(map[rune]bool)
	for i, r := range s {
		if isConcealedUnicodeRune(r, i) && !seen[r] {
			seen[r] = true
			found = append(found, r)
		}
	}
	return found
}

func detectSuspiciousBase64(s string) (blob string, decodedText string, ok bool) {
	matches := base64BlobRegex.FindAllString(s, -1)
	for _, m := range matches {
		// Skip pure hex strings or simple URL path segments without base64 diversity
		if !hasBase64Diversity(m) {
			continue
		}
		// Must be valid padded or raw std base64
		var raw []byte
		var err error
		if len(m)%4 == 0 {
			raw, err = base64.StdEncoding.DecodeString(m)
		} else {
			raw, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(m, "="))
		}
		if err != nil || len(raw) < 32 {
			continue
		}
		if isMostlyPrintableASCII(raw) {
			return m, string(raw), true
		}
		return m, "", true
	}
	return "", "", false
}

func hasBase64Diversity(s string) bool {
	var hasUpper, hasLower, hasDigitOrSym bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		case (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=':
			hasDigitOrSym = true
		}
	}
	return hasUpper && hasLower && hasDigitOrSym
}

func isMostlyPrintableASCII(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	printable := 0
	for _, ch := range b {
		if ch == '\n' || ch == '\r' || ch == '\t' || (ch >= 0x20 && ch <= 0x7E) {
			printable++
		}
	}
	return float64(printable)/float64(len(b)) >= 0.85
}

func extractSnippet(text string, start, end int) string {
	const pad = 24
	s := start - pad
	if s < 0 {
		s = 0
	}
	// Align to valid UTF-8 rune boundaries
	for s > 0 && s < len(text) && (text[s]&0xC0) == 0x80 {
		s--
	}
	e := end + pad
	if e > len(text) {
		e = len(text)
	}
	for e < len(text) && (text[e]&0xC0) == 0x80 {
		e++
	}
	snippet := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text[s:e])
	return strings.TrimSpace(snippet)
}

// -----------------------------------------------------------------------------
// Static MCP Config & IDE Security Auditor Helpers
// -----------------------------------------------------------------------------

// ServerAuditInput represents a normalized MCP server entry from toolprune.yaml or an IDE config.
type ServerAuditInput struct {
	Source   string            // e.g. "toolprune.yaml", "claude-desktop", "cursor"
	FilePath string            // On-disk path of the config file
	Name     string            // Upstream or server key name
	Command  string            // Stdio command executable
	Args     []string          // Stdio command args
	URL      string            // Remote SSE / Streamable HTTP URL
	Env      map[string]string // Configured environment variables
	Headers  map[string]string // Optional HTTP headers (e.g. Authorization)
}

var (
	knownSecretValueRegex = regexp.MustCompile(`^(?:ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|sk_live_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{16,}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})$`)
	secretKeyHintRegex    = regexp.MustCompile(`(?i)(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|AUTH_HEADER|AUTHORIZATION|BEARER)`)
	envPlaceholderRe      = regexp.MustCompile(`\$\{[^}]+\}|\$[A-Za-z_][A-Za-z0-9_]*`)
	npmPinnedVersionRegex = regexp.MustCompile(`@(?:[0-9]+\.[0-9]+(?:\.[0-9]+)?(?:[-+][0-9A-Za-z.-]+)?)$`)
	uvPinnedVersionRegex  = regexp.MustCompile(`(?:==|@)[0-9]+\.[0-9]+`)
)

// AuditServerConfig checks an MCP server definition for:
//   - Unpinned npx / uvx package execution (supply-chain rug-pull risk)
//   - Hardcoded plaintext secrets / API tokens in env and headers
//   - Insecure non-loopback http:// remote MCP server URLs
func AuditServerConfig(in ServerAuditInput) []Finding {
	var findings []Finding

	// 1. Check unpinned npx / uvx package invocation (including wrapped behind toolprune proxy -- ...)
	cmd, args := unwrapToolpruneCommand(in.Command, in.Args)
	cmdBase := strings.ToLower(crossPlatformBase(cmd))
	if cmdBase == "npx" || cmdBase == "npx.cmd" || cmdBase == "npx.exe" {
		if pkg := extractNpxPackage(args); pkg != "" && !npmPinnedVersionRegex.MatchString(pkg) {
			findings = append(findings, Finding{
				RuleID:      RuleUnpinnedPackageExec,
				RuleName:    "UnpinnedNpxPackage",
				Category:    CategorySupplyChain,
				Severity:    SeverityMedium,
				Upstream:    in.Name,
				Field:       "command/args",
				FilePath:    in.FilePath,
				Message:     fmt.Sprintf("server %q (%s) executes unpinned npx package %q without an explicit version", in.Name, in.Source, pkg),
				Evidence:    sanitizeCommandEvidence(cmd, args),
				Remediation: fmt.Sprintf("Pin the package to an exact immutable version (e.g., %s@1.2.3) and enable toolprune.lock.", pkg),
			})
		}
	} else if cmdBase == "uvx" || cmdBase == "uvx.exe" {
		if pkg := extractUvxPackage(args); pkg != "" && !uvPinnedVersionRegex.MatchString(pkg) {
			findings = append(findings, Finding{
				RuleID:      RuleUnpinnedPackageExec,
				RuleName:    "UnpinnedUvxPackage",
				Category:    CategorySupplyChain,
				Severity:    SeverityMedium,
				Upstream:    in.Name,
				Field:       "command/args",
				FilePath:    in.FilePath,
				Message:     fmt.Sprintf("server %q (%s) executes unpinned uvx package %q without an explicit version", in.Name, in.Source, pkg),
				Evidence:    sanitizeCommandEvidence(cmd, args),
				Remediation: fmt.Sprintf("Pin the package to an exact version (e.g., %s==1.0.0) and enable toolprune.lock.", pkg),
			})
		}
	}

	// 2. Check hardcoded secrets in env map and HTTP headers (never emitting raw secret values)
	auditSecretMap := func(m map[string]string, fieldPrefix string) {
		if len(m) == 0 {
			return
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			val := strings.TrimSpace(m[k])
			if val == "" || isEnvPlaceholder(val) || isDummyPlaceholder(val) {
				continue
			}
			// Strip optional "Bearer " / "Token " prefix when inspecting value format
			probe := val
			for _, pfx := range []string{"bearer ", "token ", "basic "} {
				if len(val) > len(pfx) && strings.EqualFold(val[:len(pfx)], pfx) {
					probe = strings.TrimSpace(val[len(pfx):])
					break
				}
			}
			if probe == "" || isEnvPlaceholder(probe) || isDummyPlaceholder(probe) {
				continue
			}
			if knownSecretValueRegex.MatchString(probe) || (secretKeyHintRegex.MatchString(k) && len(probe) >= 8) {
				findings = append(findings, Finding{
					RuleID:      RuleHardcodedSecret,
					RuleName:    "HardcodedEnvSecret",
					Category:    CategorySecretExposure,
					Severity:    SeverityHigh,
					Upstream:    in.Name,
					Field:       fieldPrefix + k,
					FilePath:    in.FilePath,
					Message:     fmt.Sprintf("hardcoded secret or token detected in %s server %q %s%s", in.Source, in.Name, fieldPrefix, k),
					Evidence:    fmt.Sprintf("%s=%s", k, redactSecret(val)),
					Remediation: fmt.Sprintf("Replace literal value with an environment placeholder like \"${%s}\" or a secret manager.", k),
				})
			}
		}
	}
	auditSecretMap(in.Env, "env.")
	auditSecretMap(in.Headers, "headers.")

	// 3. Check insecure HTTP remote URL
	if rawURL := strings.TrimSpace(in.URL); rawURL != "" && !isEnvPlaceholder(rawURL) {
		if u, err := url.Parse(rawURL); err == nil && strings.EqualFold(u.Scheme, "http") {
			host := u.Hostname()
			if !isLoopbackHost(host) {
				findings = append(findings, Finding{
					RuleID:      RuleInsecureTransport,
					RuleName:    "InsecureHTTPRemoteURL",
					Category:    CategoryInsecureTransport,
					Severity:    SeverityHigh,
					Upstream:    in.Name,
					Field:       "url",
					FilePath:    in.FilePath,
					Message:     fmt.Sprintf("remote MCP server %q (%s) uses unencrypted HTTP transport to non-loopback host %q", in.Name, in.Source, host),
					Evidence:    sanitizeURLEvidence(u),
					Remediation: "Switch remote MCP endpoints to HTTPS (TLS) to prevent MITM tool-schema tampering.",
				})
			}
		}
	}

	return findings
}

func sanitizeCommandEvidence(cmd string, args []string) string {
	if len(args) == 0 {
		return cmd
	}
	safeArgs := make([]string, 0, len(args))
	redactNext := false
	for _, a := range args {
		if redactNext {
			redactNext = false
			safeArgs = append(safeArgs, redactSecret(a))
			continue
		}
		if eqIdx := strings.IndexByte(a, '='); eqIdx > 0 {
			k := a[:eqIdx]
			v := a[eqIdx+1:]
			if secretKeyHintRegex.MatchString(k) || knownSecretValueRegex.MatchString(strings.TrimSpace(v)) {
				safeArgs = append(safeArgs, k+"="+redactSecret(v))
				continue
			}
		}
		if strings.HasPrefix(a, "-") && secretKeyHintRegex.MatchString(a) {
			redactNext = true
			safeArgs = append(safeArgs, a)
			continue
		}
		if knownSecretValueRegex.MatchString(strings.TrimSpace(a)) {
			safeArgs = append(safeArgs, redactSecret(a))
			continue
		}
		safeArgs = append(safeArgs, a)
	}
	return fmt.Sprintf("%s %s", cmd, strings.Join(safeArgs, " "))
}

func sanitizeURLEvidence(u *url.URL) string {
	if u == nil {
		return ""
	}
	cp := *u
	if cp.User != nil {
		cp.User = url.User("***")
	}
	if cp.RawQuery != "" {
		q := cp.Query()
		for k, vals := range q {
			for i, v := range vals {
				if secretKeyHintRegex.MatchString(k) || knownSecretValueRegex.MatchString(strings.TrimSpace(v)) {
					vals[i] = "REDACTED"
				}
			}
			q[k] = vals
		}
		cp.RawQuery = q.Encode()
	}
	return cp.String()
}

func unwrapToolpruneCommand(cmd string, args []string) (string, []string) {
	base := strings.ToLower(crossPlatformBase(cmd))
	if base == "toolprune" || base == "toolprune.exe" {
		for i, a := range args {
			if a == "--" && i+1 < len(args) {
				return args[i+1], args[i+2:]
			}
		}
	}
	return cmd, args
}

func extractNpxPackage(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "-p" || a == "--package" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func extractUvxPackage(args []string) string {
	skipNext := false
	for _, a := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--from" || a == "--python" || a == "-p" || a == "--with" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func isEnvPlaceholder(val string) bool {
	if strings.HasPrefix(val, "${") && strings.HasSuffix(val, "}") {
		return true
	}
	if strings.HasPrefix(val, "$") && !strings.ContainsAny(val, " \t") {
		return true
	}
	if strings.HasPrefix(val, "${input:") {
		return true
	}
	// Handle composite values like "Bearer ${API_TOKEN}" where the secret itself is an env placeholder
	if envPlaceholderRe.MatchString(val) {
		remainder := strings.TrimSpace(envPlaceholderRe.ReplaceAllString(val, ""))
		if remainder == "" || strings.EqualFold(remainder, "bearer") || strings.EqualFold(remainder, "token") || strings.EqualFold(remainder, "basic") {
			return true
		}
	}
	return false
}

func isDummyPlaceholder(val string) bool {
	lower := strings.ToLower(val)
	for _, marker := range []string{"your_", "your-", "changeme", "replace_me", "replace-me", "example", "<your", "todo", "xxx", "placeholder", "dummy", "test-token"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// redactSecret masks secret values so raw credentials never leak into table, JSON, or SARIF output.
func redactSecret(val string) string {
	runes := []rune(strings.TrimSpace(val))
	if len(runes) <= 12 {
		return "[REDACTED]"
	}
	return string(runes[:3]) + "****[REDACTED]"
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "localhost" || h == "" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

func crossPlatformBase(p string) string {
	s := strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	s = strings.TrimRight(s, "/")
	if idx := strings.LastIndexByte(s, '/'); idx >= 0 {
		return s[idx+1:]
	}
	return s
}
