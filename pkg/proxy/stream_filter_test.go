package proxy_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

func TestStreamFilter_ContextBoostCacheAndPreBindingValidation(t *testing.T) {
	rawTools := []mcp.Tool{
		{
			Name:        "postgres__apply_migration",
			Description: "Apply transactional DDL migration steps against a PostgreSQL schema with safety locks.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Defs: map[string]any{
					"MigrationStep": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"sql":       map[string]any{"type": "string", "minLength": float64(5)},
							"lock_mode": map[string]any{"type": "string", "enum": []any{"ACCESS_EXCLUSIVE", "SHARE_UPDATE_EXCLUSIVE"}},
						},
						"required":             []string{"sql", "lock_mode"},
						"additionalProperties": false,
					},
				},
				Properties: map[string]any{
					"schema": map[string]any{"type": "string"},
					"steps": map[string]any{
						"type":     "array",
						"minItems": float64(1),
						"items":    map[string]any{"$ref": "#/$defs/MigrationStep"},
					},
					"timeout_sec": map[string]any{
						"type":    "integer",
						"minimum": float64(1),
						"maximum": float64(300),
					},
				},
				Required:             []string{"schema", "steps"},
				AdditionalProperties: false,
			},
		},
		{
			Name:        "postgres__inspect_tables",
			Description: "Inspect PostgreSQL table metadata, indexes, and foreign keys across schemas.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"schema": map[string]any{"type": "string"},
				},
				Required: []string{"schema"},
			},
		},
		{
			Name:        "github__archive_repo",
			Description: "Archive a cold GitHub repository with large verbose documentation payload.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"owner":   map[string]any{"type": "string", "description": strings.Repeat("Owner organization name. ", 20)},
					"repo":    map[string]any{"type": "string", "description": strings.Repeat("Repository slug name. ", 20)},
					"confirm": map[string]any{"type": "boolean"},
				},
				Required: []string{"owner", "repo", "confirm"},
			},
		},
	}

	opts := pruner.DefaultOptions(pruner.TierAggressive)
	opts.MaxSchemaTokens = 160
	engine := pruner.New(opts)

	sf := proxy.NewStreamFilter(proxy.StreamFilterConfig{
		PruneMode:       "aggressive",
		MaxSchemaTokens: 160,
	})

	// 1. Cold tools/list pass
	compacted1, m1, err := sf.FilterToolsList(rawTools, engine)
	if err != nil {
		t.Fatalf("FilterToolsList failed: %v", err)
	}
	if m1.CacheHit {
		t.Fatal("expected cold pass CacheHit=false")
	}
	if len(compacted1) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(compacted1))
	}

	// 2. Immediate repeat tools/list hits FNV-1a fast-path cache
	_, m2, err := sf.FilterToolsList(rawTools, engine)
	if err != nil {
		t.Fatalf("second FilterToolsList failed: %v", err)
	}
	if !m2.CacheHit {
		t.Fatal("expected second pass CacheHit=true")
	}

	// 3. Invalid tools/call against nested $ref array item is caught pre-binding with violated sub-schema
	invalidArgs := json.RawMessage(`{"schema":"public","steps":[{"sql":"SELECT 1","lock_mode":"INVALID_LOCK"}]}`)
	errRes := sf.InterceptToolCall("postgres__apply_migration", invalidArgs)
	if errRes == nil || !errRes.IsError {
		t.Fatalf("expected pre-binding validation error for invalid nested enum, got: %+v", errRes)
	}
	errText := errRes.Content[0].Text
	if !strings.Contains(errText, "$.steps[0].lock_mode") || !strings.Contains(errText, "ACCESS_EXCLUSIVE") {
		t.Fatalf("expected self-correcting path and enum sub-schema in error, got: %s", errText)
	}

	// 4. Valid tools/call passes pre-binding validation with nil error result
	validArgs := json.RawMessage(`{"schema":"public","steps":[{"sql":"ALTER TABLE users ADD COLUMN active boolean","lock_mode":"SHARE_UPDATE_EXCLUSIVE"}],"timeout_sec":30}`)
	if okRes := sf.InterceptToolCall("postgres__apply_migration", validArgs); okRes != nil {
		t.Fatalf("expected valid call to pass pre-binding check, got: %s", okRes.Content[0].Text)
	}

	// 5. Domain affinity: postgres__inspect_tables also receives domain boost after postgres__apply_migration calls
	scores, sig := sf.ComputeBoostScores(rawTools)
	if sig == 0 {
		t.Fatal("expected non-zero context signature after calls")
	}
	if scores["postgres__apply_migration"] <= scores["github__archive_repo"] {
		t.Fatalf("expected postgres__apply_migration boost > github__archive_repo, got %v", scores)
	}
	if scores["postgres__inspect_tables"] <= 0 {
		t.Fatalf("expected sibling domain boost for postgres__inspect_tables, got %v", scores["postgres__inspect_tables"])
	}
}
