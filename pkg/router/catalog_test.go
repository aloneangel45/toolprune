package router_test

import (
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func TestCleanSummaryDescription(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		maxLen   int
		expected string
	}{
		{
			name:     "empty description",
			input:    "",
			maxLen:   80,
			expected: "",
		},
		{
			name:     "single sentence below maxLen",
			input:    "Create or update a single file in a GitHub repository.",
			maxLen:   80,
			expected: "Create or update a single file in a GitHub repository.",
		},
		{
			name:     "strips markdown headers and extracts first sentence",
			input:    "# Super Tool - This is the first sentence. This is the second sentence.",
			maxLen:   80,
			expected: "Super Tool - This is the first sentence.",
		},
		{
			name:     "sentence with exclamation mark",
			input:    "Deploy your application immediately! It will be live soon.",
			maxLen:   80,
			expected: "Deploy your application immediately!",
		},
		{
			name:     "sentence with question mark",
			input:    "Are you sure you want to drop the database? Proceed with caution.",
			maxLen:   80,
			expected: "Are you sure you want to drop the database?",
		},
		{
			name:     "long description truncated at word boundary",
			input:    "Runs a custom read-only ClickHouse SQL query against a Supabase project unified logs stream for filtering",
			maxLen:   40,
			expected: "Runs a custom read-only ClickHouse SQL...",
		},
		{
			name:     "multiline takes first line",
			input:    "First line summary.\nSecond line details.\nThird line notes.",
			maxLen:   80,
			expected: "First line summary.",
		},
		{
			name:     "truncation without spaces inside second half falls back to rune limit",
			input:    "SupercalifragilisticexpialidociousLongWordWithoutSpaces",
			maxLen:   20,
			expected: "Supercalifragilistic...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := router.CleanSummaryDescription(tc.input, tc.maxLen)
			if actual != tc.expected {
				t.Errorf("CleanSummaryDescription(%q, %d) = %q; want %q", tc.input, tc.maxLen, actual, tc.expected)
			}
		})
	}
}

func TestProjectCompactTool(t *testing.T) {
	cfg := router.DefaultCatalogConfig()

	rawTool := mcp.Tool{
		Name:        "github_create_pull_request",
		Description: "Create a new pull request in a specified repository with base and head branches.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"owner": map[string]any{"type": "string", "description": "Repository owner"},
				"repo":  map[string]any{"type": "string", "description": "Repository name"},
				"title": map[string]any{"type": "string", "description": "PR title"},
				"head":  map[string]any{"type": "string", "description": "Branch to merge"},
				"base":  map[string]any{"type": "string", "description": "Target branch"},
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	}

	t.Run("default config includes required parameters in summary", func(t *testing.T) {
		compact := router.ProjectCompactTool(rawTool, cfg)

		if compact.Name != rawTool.Name {
			t.Errorf("expected name %q, got %q", rawTool.Name, compact.Name)
		}
		if compact.InputSchema.Type != "object" {
			t.Errorf("expected schema type 'object', got %q", compact.InputSchema.Type)
		}
		if len(compact.InputSchema.Properties) != 0 {
			t.Errorf("expected empty properties, got %d", len(compact.InputSchema.Properties))
		}
		if len(compact.InputSchema.Required) != 0 {
			t.Errorf("expected empty required, got %d", len(compact.InputSchema.Required))
		}
		if !strings.Contains(compact.Description, "Required: owner, repo, title, head, base.") {
			t.Errorf("expected description to contain required params hint, got: %q", compact.Description)
		}
	})

	t.Run("disabled required in summary", func(t *testing.T) {
		noReqCfg := cfg
		noReqCfg.IncludeRequiredInSummary = false

		compact := router.ProjectCompactTool(rawTool, noReqCfg)
		if strings.Contains(compact.Description, "Required:") {
			t.Errorf("expected description without required hint, got: %q", compact.Description)
		}
	})

	t.Run("long required parameter list truncated with ellipsis", func(t *testing.T) {
		longTool := rawTool
		longTool.InputSchema.Required = []string{
			"param_very_long_identifier_one",
			"param_very_long_identifier_two",
			"param_very_long_identifier_three",
		}

		compact := router.ProjectCompactTool(longTool, cfg)
		if !strings.Contains(compact.Description, "...") {
			t.Errorf("expected truncated required list with ellipsis, got: %q", compact.Description)
		}
	})

	t.Run("tool with empty description and required parameters", func(t *testing.T) {
		emptyDescTool := rawTool
		emptyDescTool.Description = ""

		compact := router.ProjectCompactTool(emptyDescTool, cfg)
		if compact.Description != "Required: owner, repo, title, head, base." {
			t.Errorf("expected %q, got %q", "Required: owner, repo, title, head, base.", compact.Description)
		}
	})
}

func TestIsMetaTool(t *testing.T) {
	metaTools := []string{
		router.MetaToolSearch,
		router.MetaToolGetSchema,
		router.MetaToolActivate,
		router.MetaToolInvoke,
	}

	for _, name := range metaTools {
		if !router.IsMetaTool(name) {
			t.Errorf("expected IsMetaTool(%q) to be true", name)
		}
	}

	nonMeta := []string{
		"create_issue",
		"query_logs",
		"toolprune_custom",
		"",
	}
	for _, name := range nonMeta {
		if router.IsMetaTool(name) {
			t.Errorf("expected IsMetaTool(%q) to be false", name)
		}
	}
}

func TestGetMetaToolDefinitions(t *testing.T) {
	t.Run("default configuration includes active meta-tools", func(t *testing.T) {
		cfg := router.DefaultCatalogConfig()
		defs := router.GetMetaToolDefinitions(cfg)

		if len(defs) != 3 {
			t.Fatalf("expected 3 default meta-tools, got %d", len(defs))
		}

		names := make(map[string]bool)
		for _, d := range defs {
			names[d.Name] = true
			if d.InputSchema.Type != "object" {
				t.Errorf("meta-tool %s schema must be object", d.Name)
			}
			if len(d.InputSchema.Required) == 0 {
				t.Errorf("meta-tool %s must define required parameters", d.Name)
			}
		}

		if !names[router.MetaToolSearch] || !names[router.MetaToolGetSchema] || !names[router.MetaToolActivate] {
			t.Errorf("missing expected default meta tools: %v", names)
		}
	})

	t.Run("all meta-tools enabled including invoke", func(t *testing.T) {
		cfg := router.DefaultCatalogConfig()
		cfg.EnableInvokeMeta = true
		defs := router.GetMetaToolDefinitions(cfg)

		if len(defs) != 4 {
			t.Fatalf("expected 4 meta-tools, got %d", len(defs))
		}
	})

	t.Run("all meta-tools disabled", func(t *testing.T) {
		cfg := router.CatalogConfig{
			EnableSearchMeta:   false,
			EnableSchemaMeta:   false,
			EnableActivateMeta: false,
			EnableInvokeMeta:   false,
		}
		defs := router.GetMetaToolDefinitions(cfg)
		if len(defs) != 0 {
			t.Errorf("expected 0 meta-tools when disabled, got %d", len(defs))
		}
	})
}
