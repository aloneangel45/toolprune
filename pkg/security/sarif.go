package security

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"
)

const (
	// SARIFSchemaURI is the official OASIS SARIF 2.1.0 JSON schema URI.
	SARIFSchemaURI = "https://json.schemastore.org/sarif-2.1.0.json"
	// SARIFVersion is the OASIS SARIF specification version emitted by ToolPrune.
	SARIFVersion = "2.1.0"
)

// SARIFLog represents a valid OASIS SARIF 2.1.0 log document.
type SARIFLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

// SARIFRun represents a single analysis run in a SARIF log.
type SARIFRun struct {
	Tool    SARIFTool     `json:"tool"`
	Results []SARIFResult `json:"results"`
}

// SARIFTool describes the analysis tool that produced the SARIF results.
type SARIFTool struct {
	Driver SARIFDriver `json:"driver"`
}

// SARIFDriver describes ToolPrune and the catalog of security rules evaluated.
type SARIFDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []SARIFRule `json:"rules"`
}

// SARIFRule describes an individual ToolPrune security rule.
type SARIFRule struct {
	ID                   string                `json:"id"`
	Name                 string                `json:"name"`
	ShortDescription     SARIFMessage          `json:"shortDescription"`
	FullDescription      *SARIFMessage         `json:"fullDescription,omitempty"`
	Help                 *SARIFMessage         `json:"help,omitempty"`
	DefaultConfiguration *SARIFRuleConfig      `json:"defaultConfiguration,omitempty"`
	Properties           *SARIFRuleProperties  `json:"properties,omitempty"`
}

// SARIFRuleConfig holds the default reporting level for a rule.
type SARIFRuleConfig struct {
	Level string `json:"level"`
}

// SARIFRuleProperties attaches tags and security-severity metadata for GitHub Code Scanning.
type SARIFRuleProperties struct {
	Tags             []string `json:"tags,omitempty"`
	SecuritySeverity string   `json:"security-severity,omitempty"`
}

// SARIFMessage wraps a plain text message string.
type SARIFMessage struct {
	Text string `json:"text"`
}

// SARIFResult represents a single security finding in SARIF 2.1.0 format.
type SARIFResult struct {
	RuleID     string          `json:"ruleId"`
	Level      string          `json:"level"`
	Message    SARIFMessage    `json:"message"`
	Locations  []SARIFLocation `json:"locations,omitempty"`
	Properties map[string]any  `json:"properties,omitempty"`
}

// SARIFLocation links a finding to a physical artifact and/or logical MCP tool/field.
type SARIFLocation struct {
	PhysicalLocation *SARIFPhysicalLocation `json:"physicalLocation,omitempty"`
	LogicalLocations []SARIFLogicalLocation `json:"logicalLocations,omitempty"`
}

// SARIFPhysicalLocation identifies a file on disk.
type SARIFPhysicalLocation struct {
	ArtifactLocation SARIFArtifactLocation `json:"artifactLocation"`
}

// SARIFArtifactLocation holds a normalized URI to the audited file.
type SARIFArtifactLocation struct {
	URI string `json:"uri"`
}

// SARIFLogicalLocation identifies an MCP upstream/tool/field target.
type SARIFLogicalLocation struct {
	Name               string `json:"name"`
	FullyQualifiedName string `json:"fullyQualifiedName,omitempty"`
	Kind               string `json:"kind,omitempty"`
}

// BuildSARIF constructs a valid SARIF 2.1.0 document from ToolPrune security findings.
func BuildSARIF(findings []Finding, version string) SARIFLog {
	if version == "" {
		version = "1.4.1"
	}

	rulesMap := make(map[string]SARIFRule)
	results := make([]SARIFResult, 0, len(findings))

	for _, f := range findings {
		if _, exists := rulesMap[f.RuleID]; !exists {
			rulesMap[f.RuleID] = SARIFRule{
				ID:               f.RuleID,
				Name:             f.RuleName,
				ShortDescription: SARIFMessage{Text: ruleSummary(f.RuleID, f.RuleName)},
				Help:             &SARIFMessage{Text: f.Remediation},
				DefaultConfiguration: &SARIFRuleConfig{
					Level: severityToSARIFLevel(f.Severity),
				},
				Properties: &SARIFRuleProperties{
					Tags:             []string{"security", "mcp", f.Category},
					SecuritySeverity: severityToCVSSScore(f.Severity),
				},
			}
		}

		loc := SARIFLocation{}
		if f.FilePath != "" {
			loc.PhysicalLocation = &SARIFPhysicalLocation{
				ArtifactLocation: SARIFArtifactLocation{
					URI: NormalizeSARIFURI(f.FilePath),
				},
			}
		}
		if f.ToolName != "" || f.Upstream != "" {
			fqn := f.ToolName
			if f.Upstream != "" && f.ToolName != "" {
				fqn = f.Upstream + "::" + f.ToolName
			} else if f.Upstream != "" {
				fqn = f.Upstream
			}
			if f.Field != "" {
				fqn += "." + f.Field
			}
			name := f.ToolName
			if name == "" {
				name = f.Upstream
			}
			loc.LogicalLocations = []SARIFLogicalLocation{
				{
					Name:               name,
					FullyQualifiedName: fqn,
					Kind:               "mcp-tool",
				},
			}
		}

		res := SARIFResult{
			RuleID:  f.RuleID,
			Level:   severityToSARIFLevel(f.Severity),
			Message: SARIFMessage{Text: f.Message},
			Properties: map[string]any{
				"severity": string(f.Severity),
				"category": f.Category,
			},
		}
		if f.Evidence != "" {
			res.Properties["evidence"] = f.Evidence
		}
		if loc.PhysicalLocation != nil || len(loc.LogicalLocations) > 0 {
			res.Locations = []SARIFLocation{loc}
		}
		results = append(results, res)
	}

	rules := make([]SARIFRule, 0, len(rulesMap))
	for _, r := range rulesMap {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		return rules[i].ID < rules[j].ID
	})

	return SARIFLog{
		Schema:  SARIFSchemaURI,
		Version: SARIFVersion,
		Runs: []SARIFRun{
			{
				Tool: SARIFTool{
					Driver: SARIFDriver{
						Name:           "ToolPrune",
						Version:        version,
						InformationURI: "https://github.com/aloneangel45/toolprune",
						Rules:          rules,
					},
				},
				Results: results,
			},
		},
	}
}

// FormatSARIF renders findings as an indented SARIF 2.1.0 JSON byte slice.
func FormatSARIF(findings []Finding, version string) ([]byte, error) {
	log := BuildSARIF(findings, version)
	data, err := json.MarshalIndent(log, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func severityToSARIFLevel(s Severity) string {
	switch s {
	case SeverityCritical, SeverityHigh:
		return "error"
	case SeverityMedium:
		return "warning"
	default:
		return "note"
	}
}

func severityToCVSSScore(s Severity) string {
	switch s {
	case SeverityCritical:
		return "9.5"
	case SeverityHigh:
		return "8.0"
	case SeverityMedium:
		return "5.5"
	case SeverityLow:
		return "3.0"
	default:
		return "1.0"
	}
}

func ruleSummary(ruleID, fallback string) string {
	switch ruleID {
	case RulePromptInjection:
		return "MCP tool description contains hidden prompt-injection tags or stealth override directives"
	case RuleSensitivePathExfil:
		return "MCP tool metadata references sensitive credential or system paths"
	case RuleUnicodeSpoofing:
		return "MCP tool metadata contains concealed zero-width or BiDi Unicode characters"
	case RuleConcealedBase64:
		return "MCP tool metadata embeds a large obfuscated Base64 payload"
	case RuleCommandSmuggling:
		return "MCP tool metadata contains dangerous shell command smuggling patterns"
	case RuleRugPullMidSession:
		return "MCP tool schema mutated mid-session (possible rug-pull attack)"
	case RuleRugPullLockfileDrift:
		return "MCP tool schema SHA-256 hash drifted from toolprune.lock"
	case RuleRugPullUnpinned:
		return "MCP tool is not pinned in toolprune.lock"
	case RuleUnpinnedPackageExec:
		return "MCP server executes an unpinned npx/uvx package without version pinning"
	case RuleHardcodedSecret:
		return "MCP server configuration contains a hardcoded plaintext secret or token"
	case RuleInsecureTransport:
		return "Remote MCP server uses unencrypted HTTP instead of HTTPS"
	default:
		return fallback
	}
}

// NormalizeSARIFURI converts a local OS path (Windows drive path, UNC path, POSIX absolute
// path, or relative path) into an RFC 3986 / OASIS SARIF 2.1.0 compliant URI.
func NormalizeSARIFURI(p string) string {
	s := strings.TrimSpace(p)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "file://") || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return s
	}

	// Cross-platform slash normalization (works identically on Linux, macOS, and Windows).
	s = strings.ReplaceAll(s, "\\", "/")

	// Windows UNC path: //server/share/path -> file://server/share/path
	if strings.HasPrefix(s, "//") {
		trimmed := strings.TrimPrefix(s, "//")
		host, rest, hasRest := strings.Cut(trimmed, "/")
		encodedRest := encodeURIPathSegments(rest)
		if hasRest {
			return "file://" + host + "/" + encodedRest
		}
		return "file://" + host
	}

	// Windows drive path: C:/Users/... -> file:///C:/Users/...
	if len(s) >= 2 && isASCIIAlpha(s[0]) && s[1] == ':' {
		drive := strings.ToUpper(s[:1]) + ":"
		rest := ""
		if len(s) > 2 {
			rest = strings.TrimPrefix(s[2:], "/")
		}
		encoded := encodeURIPathSegments(rest)
		if encoded == "" {
			return "file:///" + drive + "/"
		}
		return "file:///" + drive + "/" + encoded
	}

	// POSIX absolute path: /home/user/... -> file:///home/user/...
	if strings.HasPrefix(s, "/") {
		return "file:///" + encodeURIPathSegments(strings.TrimPrefix(s, "/"))
	}

	// Relative path: strip leading ./ and percent-encode segments
	for strings.HasPrefix(s, "./") {
		s = strings.TrimPrefix(s, "./")
	}
	return encodeURIPathSegments(s)
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

func encodeURIPathSegments(pathStr string) string {
	if pathStr == "" {
		return ""
	}
	parts := strings.Split(pathStr, "/")
	for i, seg := range parts {
		if seg == "" {
			continue
		}
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}
