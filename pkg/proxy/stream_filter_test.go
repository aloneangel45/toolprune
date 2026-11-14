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

func TestStreamFilter_SchemaFingerprintCacheCloneAndOneOfNullableValidation(t *testing.T) {
	opts := pruner.DefaultOptions(pruner.TierCompact)
	opts.MaxSchemaTokens = 200
	engine := pruner.New(opts)
	sf := proxy.NewStreamFilter(proxy.StreamFilterConfig{
		PruneMode:       "compact",
		MaxSchemaTokens: 200,
	})

	toolsV1 := []mcp.Tool{
		{
			Name:        "deploy_service",
			Description: "Deploy service.",
			InputSchema: mcp.InputSchema{
				Type:       "object",
				Properties: map[string]any{"replicas": map[string]any{"type": "string"}},
				Required:   []string{"replicas"},
			},
		},
	}
	out1, _, err := sf.FilterToolsList(toolsV1, engine)
	if err != nil {
		t.Fatal(err)
	}
	// Mutate returned slice/map and ensure cached copy is unaffected
	out1[0].Name = "CORRUPTED_NAME"
	out1[0].InputSchema.Properties["replicas"] = map[string]any{"type": "boolean"}

	out1Cached, mHit, err := sf.FilterToolsList(toolsV1, engine)
	if err != nil || !mHit.CacheHit {
		t.Fatalf("expected cache hit on identical toolsV1, err=%v hit=%v", err, mHit.CacheHit)
	}
	if out1Cached[0].Name != "deploy_service" {
		t.Fatalf("caller mutation corrupted cached tool name: %q", out1Cached[0].Name)
	}

	// Change property type from "string" to "integer" with same property count (1) and required count (1).
	// Must bust FNV-1a cache AND update rawRegistry!
	toolsV2 := []mcp.Tool{
		{
			Name:        "deploy_service",
			Description: "Deploy service.",
			InputSchema: mcp.InputSchema{
				Type:       "object",
				Properties: map[string]any{"replicas": map[string]any{"type": "integer"}},
				Required:   []string{"replicas"},
			},
		},
	}
	_, mMiss, err := sf.FilterToolsList(toolsV2, engine)
	if err != nil {
		t.Fatal(err)
	}
	if mMiss.CacheHit {
		t.Fatal("expected CacheHit=false when property type changed from string to integer")
	}
	if rej := sf.InterceptToolCall("deploy_service", json.RawMessage(`{"replicas":"not_an_int"}`)); rej == nil || !rej.IsError {
		t.Fatal("expected updated rawRegistry to reject string for integer replicas")
	}

	// Verify FilterProjectedToolsList does NOT overwrite rawRegistry with stubbed schemas
	stubProjected := []mcp.Tool{
		{
			Name:        "deploy_service",
			Description: "Deploy service. Required: replicas.",
			InputSchema: mcp.InputSchema{Type: "object"},
		},
	}
	if _, _, err := sf.FilterProjectedToolsList(stubProjected, engine); err != nil {
		t.Fatal(err)
	}
	if rej := sf.InterceptToolCall("deploy_service", json.RawMessage(`{"replicas":"still_invalid"}`)); rej == nil || !rej.IsError {
		t.Fatal("FilterProjectedToolsList overwrote rawRegistry; expected invalid replicas to still be rejected")
	}

	// Verify oneOf rejects multiple matching branches, nullable required fields accept null, and RFC 6901 $ref unescapes ~1/~0
	complexSchema := mcp.InputSchema{
		Type: "object",
		Defs: map[string]any{
			"ns/item~v1": map[string]any{
				"type": "string",
				"enum": []any{"alpha", "beta"},
			},
		},
		Properties: map[string]any{
			"mode": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "number", "minimum": float64(0)},
					map[string]any{"type": "number", "maximum": float64(10)},
				},
			},
			"nullable_tag": map[string]any{
				"type": []any{"string", "null"},
			},
			"strict_name": map[string]any{
				"type": "string",
			},
			"escaped_ref": map[string]any{
				"$ref": "#/$defs/ns~1item~0v1",
			},
		},
		Required: []string{"mode", "nullable_tag", "strict_name"},
	}

	// mode=5 matches BOTH oneOf branches (>=0 and <=10) -> MUST fail oneOf!
	if vErr := proxy.ValidateToolArguments("complex_tool", complexSchema, json.RawMessage(`{"mode":5,"nullable_tag":null,"strict_name":"ok"}`)); vErr == nil {
		t.Fatal("expected oneOf validation error when value matches 2 branches")
	}
	// mode=20 matches exactly 1 branch (>=0), nullable_tag=null is explicitly allowed, escaped_ref="alpha" matches $defs["ns/item~v1"] -> MUST pass!
	if vErr := proxy.ValidateToolArguments("complex_tool", complexSchema, json.RawMessage(`{"mode":20,"nullable_tag":null,"strict_name":"ok","escaped_ref":"alpha"}`)); vErr != nil {
		t.Fatalf("expected valid oneOf + nullable required + escaped $ref to pass, got: %v", vErr.ToCallToolResult().Content[0].Text)
	}
	// escaped_ref="gamma" violates resolved $defs["ns/item~v1"] enum -> MUST fail!
	if vErr := proxy.ValidateToolArguments("complex_tool", complexSchema, json.RawMessage(`{"mode":20,"nullable_tag":null,"strict_name":"ok","escaped_ref":"gamma"}`)); vErr == nil {
		t.Fatal("expected escaped $ref enum violation to fail")
	}
	// strict_name=null on non-nullable required string -> MUST fail!
	if vErr := proxy.ValidateToolArguments("complex_tool", complexSchema, json.RawMessage(`{"mode":20,"nullable_tag":null,"strict_name":null}`)); vErr == nil {
		t.Fatal("expected non-nullable required property with null value to fail")
	}
}
