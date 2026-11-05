package router

import (
	"fmt"
	"strings"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// Standard meta-tool names injected by ToolPrune.
const (
	MetaToolSearch    = "toolprune_search_tools"
	MetaToolGetSchema = "toolprune_get_tool_schema"
	MetaToolActivate  = "toolprune_activate_tools"
	MetaToolInvoke    = "toolprune_invoke"
)

// CatalogMode defines how tools are presented to the AI client in Tier 4.
type CatalogMode string

const (
	// CatalogModeFull exposes all tools with compact catalog summaries (~24 tokens/tool).
	CatalogModeFull CatalogMode = "full"

	// CatalogModeWindowed exposes top-W tools with Tier 3 schemas, and remaining as compact catalog.
	CatalogModeWindowed CatalogMode = "windowed"

	// CatalogModeMetaOnly exposes ONLY the meta-tools in tools/list (~180 tokens total).
	CatalogModeMetaOnly CatalogMode = "meta_only"
)

// CatalogConfig configures Tier 4 catalog generation and projection.
type CatalogConfig struct {
	Mode                     CatalogMode `json:"mode" yaml:"mode"`
	WindowSize               int         `json:"windowSize" yaml:"window_size"`                               // Default: 15
	MaxSummaryLength         int         `json:"maxSummaryLength" yaml:"max_summary_length"`                  // Default: 80
	IncludeRequiredInSummary bool        `json:"includeRequiredInSummary" yaml:"include_required_in_summary"` // Default: true
	EnableMetaTools          bool        `json:"enableMetaTools" yaml:"enable_meta_tools"`                    // Default: true
	EnableSearchMeta         bool        `json:"enableSearchMeta" yaml:"enable_search_meta"`                  // Default: true
	EnableSchemaMeta         bool        `json:"enableSchemaMeta" yaml:"enable_schema_meta"`                  // Default: true
	EnableActivateMeta       bool        `json:"enableActivateMeta" yaml:"enable_activate_meta"`              // Default: true
	EnableInvokeMeta         bool        `json:"enableInvokeMeta" yaml:"enable_invoke_meta"`                  // Default: false (true for MetaOnly)
}

// DefaultCatalogConfig returns standard production defaults.
func DefaultCatalogConfig() CatalogConfig {
	return CatalogConfig{
		Mode:                     CatalogModeFull,
		WindowSize:               15,
		MaxSummaryLength:         80,
		IncludeRequiredInSummary: true,
		EnableMetaTools:          true,
		EnableSearchMeta:         true,
		EnableSchemaMeta:         true,
		EnableActivateMeta:       true,
		EnableInvokeMeta:         false,
	}
}

// ProjectCompactTool condenses a full or pruned tool into an ultra-compact catalog entry.
// Schema properties and required arrays are omitted to achieve ~22-26 tokens/tool while remaining valid Draft-07.
func ProjectCompactTool(tool mcp.Tool, cfg CatalogConfig) mcp.Tool {
	desc := CleanSummaryDescription(tool.Description, cfg.MaxSummaryLength)

	// Optionally append required parameter hints: "Required: p1, p2."
	if cfg.IncludeRequiredInSummary && len(tool.InputSchema.Required) > 0 {
		reqList := strings.Join(tool.InputSchema.Required, ", ")
		if len(reqList) > 40 {
			reqList = reqList[:37] + "..."
		}
		if desc != "" && !strings.HasSuffix(desc, ".") {
			desc += "."
		}
		if desc != "" {
			desc = fmt.Sprintf("%s Required: %s.", desc, reqList)
		} else {
			desc = fmt.Sprintf("Required: %s.", reqList)
		}
	}

	return mcp.Tool{
		Name:        tool.Name,
		Description: desc,
		InputSchema: mcp.InputSchema{
			Type: "object",
			// Properties and Required are intentionally omitted or empty
			// to achieve maximum token reduction while remaining valid Draft-07
		},
	}
}

// CleanSummaryDescription extracts the first sentence and truncates to maxLen.
func CleanSummaryDescription(raw string, maxLen int) string {
	if raw == "" {
		return ""
	}
	// Strip leading markdown tags
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)

	// Remove markdown line breaks and collapses multiple spaces
	lines := strings.Split(s, "\n")
	firstLine := strings.TrimSpace(lines[0])
	if firstLine != "" {
		s = firstLine
	}

	// First sentence extraction
	for _, endChar := range []string{". ", "! ", "? "} {
		if idx := strings.Index(s, endChar); idx != -1 {
			s = s[:idx+1]
			break
		}
	}
	if strings.HasSuffix(s, ".") && !strings.HasSuffix(s, "..") {
		// Keep trailing period
	}

	// Truncate at word boundary if exceeding maxLen
	runes := []rune(s)
	if len(runes) > maxLen {
		truncated := string(runes[:maxLen])
		if lastSpace := strings.LastIndex(truncated, " "); lastSpace > maxLen/2 {
			truncated = truncated[:lastSpace]
		}
		s = truncated + "..."
	}
	return strings.TrimSpace(s)
}

// IsMetaTool checks whether a tool name belongs to the ToolPrune meta-tool suite.
func IsMetaTool(name string) bool {
	return name == MetaToolSearch || name == MetaToolGetSchema || name == MetaToolActivate || name == MetaToolInvoke
}

// GetMetaToolDefinitions returns the mcp.Tool declarations for active meta-tools.
func GetMetaToolDefinitions(cfg CatalogConfig) []mcp.Tool {
	var metaTools []mcp.Tool

	if cfg.EnableSearchMeta {
		metaTools = append(metaTools, mcp.Tool{
			Name:        MetaToolSearch,
			Description: "Search the ToolPrune catalog for tools by keyword, intent, or server category.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Search keywords or task intent (e.g. 'dns records', 'clickhouse logs')",
					},
					"server": map[string]any{
						"type":        "string",
						"description": "Optional upstream server name filter",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of tools to return (default: 8, max: 25)",
					},
				},
				Required: []string{"query"},
			},
		})
	}

	if cfg.EnableSchemaMeta {
		metaTools = append(metaTools, mcp.Tool{
			Name:        MetaToolGetSchema,
			Description: "Retrieve the complete parameter schema for specific tools from the ToolPrune catalog.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"tool_names": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
						},
						"description": "List of tool names to fetch schemas for",
					},
				},
				Required: []string{"tool_names"},
			},
		})
	}

	if cfg.EnableActivateMeta {
		metaTools = append(metaTools, mcp.Tool{
			Name:        MetaToolActivate,
			Description: "Promote tools from the dormant catalog into the active sliding window.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"tools": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "string",
						},
						"description": "List of tool names to activate into the window",
					},
				},
				Required: []string{"tools"},
			},
		})
	}

	if cfg.EnableInvokeMeta {
		metaTools = append(metaTools, mcp.Tool{
			Name:        MetaToolInvoke,
			Description: "Execute any catalog tool by name with provided arguments.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"tool_name": map[string]any{
						"type":        "string",
						"description": "Exact tool name to invoke",
					},
					"arguments": map[string]any{
						"type":        "object",
						"description": "JSON arguments matching tool's parameter schema",
					},
				},
				Required: []string{"tool_name", "arguments"},
			},
		})
	}

	return metaTools
}
