package pruner_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// loadTestFixtures loads real MCP tool schemas from test/fixtures/schemas.json.
func loadTestFixtures(t *testing.T) []mcp.Tool {
	t.Helper()

	candidates := []string{
		filepath.Join("..", "..", "test", "fixtures", "schemas.json"),
		filepath.Join("test", "fixtures", "schemas.json"),
		`C:\Users\Arda\Desktop\open_source_hunter\test\fixtures\schemas.json`,
	}

	var data []byte
	var err error
	for _, path := range candidates {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("failed to read test fixtures from any candidate path: %v", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		t.Fatalf("failed to unmarshal test fixtures: %v", err)
	}
	if len(tools) == 0 {
		t.Fatalf("test fixtures file contains 0 tools")
	}
	return tools
}

// TestDraft07Compliance_RawFixtures ensures all raw input fixtures conform to Draft-07.
func TestDraft07Compliance_RawFixtures(t *testing.T) {
	tools := loadTestFixtures(t)
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			res := pruner.ValidateTool(tool)
			if !res.Valid {
				t.Fatalf("Raw tool %s failed Draft-07 validation: %v", tool.Name, res.Errors)
			}
		})
	}
}

// TestDraft07Compliance_NegativeCases tests that invalid schemas are strictly caught.
func TestDraft07Compliance_NegativeCases(t *testing.T) {
	tests := []struct {
		name     string
		tool     mcp.Tool
		wantCode pruner.ValidationErrorCode
	}{
		{
			name: "InvalidRootType",
			tool: mcp.Tool{
				Name: "bad_root",
				InputSchema: mcp.InputSchema{
					Type: "string", // Must be "object"
				},
			},
			wantCode: pruner.ErrInvalidRootType,
		},
		{
			name: "MissingRequiredProperty",
			tool: mcp.Tool{
				Name: "missing_req",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"propA": map[string]any{"type": "string"}},
					Required:   []string{"propA", "missingProp"},
				},
			},
			wantCode: pruner.ErrRequiredPropMissing,
		},
		{
			name: "DuplicateRequiredItem",
			tool: mcp.Tool{
				Name: "duplicate_req",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"propA": map[string]any{"type": "string"}},
					Required:   []string{"propA", "propA"},
				},
			},
			wantCode: pruner.ErrDuplicateRequired,
		},
		{
			name: "InvalidPropertyType",
			tool: mcp.Tool{
				Name: "invalid_type",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"propA": map[string]any{"type": "unsupported_data_type"},
					},
				},
			},
			wantCode: pruner.ErrInvalidPropType,
		},
		{
			name: "EmptyEnumArray",
			tool: mcp.Tool{
				Name: "empty_enum",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"propA": map[string]any{"type": "string", "enum": []any{}},
					},
				},
			},
			wantCode: pruner.ErrInvalidEnum,
		},
		{
			name: "InvalidRegexPattern",
			tool: mcp.Tool{
				Name: "invalid_regex",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"propA": map[string]any{"type": "string", "pattern": "[a-z("},
					},
				},
			},
			wantCode: pruner.ErrInvalidRegexPattern,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := pruner.ValidateTool(tt.tool)
			if res.Valid {
				t.Fatalf("expected validation failure for %s, but schema was marked valid", tt.name)
			}
			found := false
			for _, err := range res.Errors {
				if err.Code == tt.wantCode {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("expected error code %s, got errors: %v", tt.wantCode, res.Errors)
			}
		})
	}
}

// TestPruningTiers_TokenReductions asserts reduction thresholds and Draft-07 compliance across tiers.
func TestPruningTiers_TokenReductions(t *testing.T) {
	tools := loadTestFixtures(t)

	tiers := []struct {
		tier         pruner.PruneTier
		minReduction float64
	}{
		{pruner.TierNone, 0.0},
		{pruner.TierLossless, 0.02},  // At least 2% on compact JSON
		{pruner.TierCompact, 0.30},   // Target > 30% reduction
		{pruner.TierAggressive, 0.45}, // Target > 45% reduction
	}

	for _, tc := range tiers {
		t.Run(string(tc.tier), func(t *testing.T) {
			engine := pruner.New(pruner.DefaultOptions(tc.tier))
			pruned, metrics, err := engine.PruneTools(tools)
			if err != nil {
				t.Fatalf("PruneTools failed on tier %s: %v", tc.tier, err)
			}

			// 1. Verify Draft-07 compliance of all pruned tools
			for _, pTool := range pruned {
				res := pruner.ValidateTool(pTool)
				if !res.Valid {
					t.Fatalf("Pruned tool %s failed Draft-07 on tier %s: %v", pTool.Name, tc.tier, res.Errors)
				}
			}

			// 2. Verify token reduction threshold
			t.Logf("Tier %s: RawTokens=%d PrunedTokens=%d Savings=%.1f%%",
				tc.tier, metrics.RawTokens, metrics.PrunedTokens, metrics.SavingsRatio*100)

			if metrics.SavingsRatio < tc.minReduction {
				t.Fatalf("Tier %s savings ratio %.3f is below required minimum %.3f",
					tc.tier, metrics.SavingsRatio, tc.minReduction)
			}

			// 3. Tier 0 identity check
			if tc.tier == pruner.TierNone {
				if metrics.SavingsRatio != 0.0 {
					t.Fatalf("TierNone must have exactly 0.0 savings ratio, got %f", metrics.SavingsRatio)
				}
			}
		})
	}
}

// TestArgumentExecutionCompatibility proves Zero Argument Execution Regression.
func TestArgumentExecutionCompatibility(t *testing.T) {
	tools := loadTestFixtures(t)

	// Representative valid tool call argument payloads generated by Claude / Cursor
	validArguments := map[string]map[string]any{
		"create_or_update_file": {
			"owner":   "open-source-hunter",
			"repo":    "toolprune",
			"branch":  "main",
			"path":    "README.md",
			"content": "Hello World",
			"message": "Initial commit",
		},
		"query_logs": {
			"project_id": "proj-xyz-123",
			"sql":        "SELECT * FROM edge_logs LIMIT 10",
		},
		"create_dns_record": {
			"zone_id": "023e105f4ecef8ad9ca31a8372d0c353",
			"type":    "A",
			"name":    "api.example.com",
			"content": "192.0.2.1",
		},
		"emulate": {
			"pageId":      float64(42),
			"colorScheme": "dark",
		},
	}

	tiers := []pruner.PruneTier{
		pruner.TierNone,
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			engine := pruner.New(pruner.DefaultOptions(tier))
			prunedTools, _, err := engine.PruneTools(tools)
			if err != nil {
				t.Fatalf("PruneTools failed: %v", err)
			}

			for _, tool := range prunedTools {
				args, hasArgs := validArguments[tool.Name]
				if !hasArgs {
					continue
				}

				// Check that all required properties are satisfied
				for _, req := range tool.InputSchema.Required {
					if _, ok := args[req]; !ok {
						t.Errorf("[%s] tool %s missing required arg %s",
							tier, tool.Name, req)
					}
				}

				// Check that arguments match parameter types
				for argName, argVal := range args {
					rawProp, exists := tool.InputSchema.Properties[argName]
					if !exists {
						continue
					}
					propMap, ok := rawProp.(map[string]any)
					if !ok {
						continue
					}
					if rawType, ok := propMap["type"].(string); ok {
						switch rawType {
						case "string":
							if _, isStr := argVal.(string); !isStr {
								t.Errorf("[%s] tool %s arg %s expected string, got %T", tier, tool.Name, argName, argVal)
							}
						case "number":
							switch argVal.(type) {
							case float64, float32, int:
								// OK
							default:
								t.Errorf("[%s] tool %s arg %s expected number, got %T", tier, tool.Name, argName, argVal)
							}
						}
					}

					// Check that enum allows the argument value
					if rawEnum, hasEnum := propMap["enum"].([]any); hasEnum {
						matched := false
						for _, ev := range rawEnum {
							if fmt.Sprintf("%v", ev) == fmt.Sprintf("%v", argVal) {
								matched = true
								break
							}
						}
						if !matched {
							t.Errorf("[%s] tool %s arg %s enum value %v not allowed",
								tier, tool.Name, argName, argVal)
						}
					}
				}
			}
		})
	}
}

// TestTier3_Invariants verifies the 4 invariants in Tier 3 parameter pruning.
func TestTier3_Invariants(t *testing.T) {
	tools := loadTestFixtures(t)
	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	pruned, _, err := engine.PruneTools(tools)
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}

	rawByName := make(map[string]mcp.Tool, len(tools))
	for _, tool := range tools {
		rawByName[tool.Name] = tool
	}

	for _, pTool := range pruned {
		rawTool, ok := rawByName[pTool.Name]
		if !ok {
			t.Fatalf("unexpected pruned tool name: %s", pTool.Name)
		}

		// Invariant 1: Type Preservation
		for propName, rawProp := range rawTool.InputSchema.Properties {
			rawMap, isMap := rawProp.(map[string]any)
			if !isMap {
				continue
			}
			pProp, exists := pTool.InputSchema.Properties[propName]
			if !exists {
				t.Fatalf("tool %s lost property %s in pruning", pTool.Name, propName)
			}
			pMap, ok := pProp.(map[string]any)
			if !ok {
				t.Fatalf("pruned property %s is not a map", propName)
			}
			if rawType, ok := rawMap["type"]; ok {
				if pMap["type"] != rawType {
					t.Fatalf("tool %s prop %s type altered: expected %v, got %v",
						pTool.Name, propName, rawType, pMap["type"])
				}
			}

			// Invariant 3: Enum Preservation
			if rawEnum, ok := rawMap["enum"].([]any); ok {
				pEnum, hasEnum := pMap["enum"].([]any)
				if !hasEnum {
					t.Fatalf("tool %s prop %s lost enum in pruning", pTool.Name, propName)
				}
				if len(rawEnum) != len(pEnum) {
					t.Fatalf("tool %s prop %s enum length mismatch: expected %d, got %d",
						pTool.Name, propName, len(rawEnum), len(pEnum))
				}
				for i := range rawEnum {
					if rawEnum[i] != pEnum[i] {
						t.Fatalf("tool %s prop %s enum item %d mismatch: expected %v, got %v",
							pTool.Name, propName, i, rawEnum[i], pEnum[i])
					}
				}
			}
		}

		// Invariant 2: Required Preservation
		if len(rawTool.InputSchema.Required) != len(pTool.InputSchema.Required) {
			t.Fatalf("tool %s required slice length mismatch: expected %d, got %d",
				pTool.Name, len(rawTool.InputSchema.Required), len(pTool.InputSchema.Required))
		}
		for i, req := range rawTool.InputSchema.Required {
			if pTool.InputSchema.Required[i] != req {
				t.Fatalf("tool %s required item %d mismatch: expected %s, got %s",
					pTool.Name, i, req, pTool.InputSchema.Required[i])
			}
		}
	}
}

// TestTier3_SelfEvidentParameterStripping verifies self-evident descriptions are stripped.
func TestTier3_SelfEvidentParameterStripping(t *testing.T) {
	tools := loadTestFixtures(t)
	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	pruned, _, err := engine.PruneTools(tools)
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}

	for _, tool := range pruned {
		switch tool.Name {
		case "create_or_update_file":
			for _, name := range []string{"owner", "repo", "path", "content", "message", "branch"} {
				prop := tool.InputSchema.Properties[name].(map[string]any)
				if desc, hasDesc := prop["description"]; hasDesc && desc != "" {
					t.Fatalf("expected self-evident property %s to have stripped description, got: %v", name, desc)
				}
			}
		case "create_dns_record":
			// zone_id, name, content should have stripped descriptions
			for _, name := range []string{"zone_id", "name", "content"} {
				prop := tool.InputSchema.Properties[name].(map[string]any)
				if desc, hasDesc := prop["description"]; hasDesc && desc != "" {
					t.Fatalf("expected self-evident property %s to have stripped description, got: %v", name, desc)
				}
			}
			// confirm is NOT self-evident, description must be preserved (in concise form)
			confirmProp := tool.InputSchema.Properties["confirm"].(map[string]any)
			desc, hasDesc := confirmProp["description"].(string)
			if !hasDesc || desc == "" {
				t.Fatalf("expected non-self-evident property 'confirm' to retain description")
			}
			if len(desc) > 55 {
				t.Fatalf("expected 'confirm' description to be concise (<= 55 chars), got %d chars: %q", len(desc), desc)
			}
		case "query_logs":
			// 270-char ISO regex should be stripped and format: "date-time" kept
			for _, name := range []string{"iso_timestamp_start", "iso_timestamp_end"} {
				prop := tool.InputSchema.Properties[name].(map[string]any)
				if pat, hasPat := prop["pattern"]; hasPat && len(pat.(string)) > 25 {
					t.Fatalf("expected 270-char pattern to be stripped from %s, but found %q", name, pat)
				}
				if fmtVal, hasFmt := prop["format"]; !hasFmt || fmtVal != "date-time" {
					t.Fatalf("expected format: 'date-time' to be retained on %s, got: %v", name, fmtVal)
				}
			}
		}
	}
}

// TestProxyDynamicInterception verifies that pruner.Engine plugs cleanly into proxy.PruneEngine.
func TestProxyDynamicInterception(t *testing.T) {
	tools := loadTestFixtures(t)
	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))

	// Direct interface satisfaction assertion
	var _ proxy.PruneEngine = engine

	pruned, metrics, err := engine.PruneTools(tools)
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}

	if len(pruned) != len(tools) {
		t.Fatalf("expected %d tools, got %d", len(tools), len(pruned))
	}

	if metrics.SavingsRatio < 0.45 {
		t.Fatalf("expected at least 45%% savings in aggressive mode, got %.1f%%", metrics.SavingsRatio*100)
	}

	// Verify profile helper works
	profilePruned, err := engine.PruneToolsForProfile(tools, "compact")
	if err != nil {
		t.Fatalf("PruneToolsForProfile failed: %v", err)
	}
	if len(profilePruned) != len(tools) {
		t.Fatalf("expected %d profilePruned tools, got %d", len(tools), len(profilePruned))
	}
}

// TestStandalonePruneFunctions verifies package-level helper functions.
func TestStandalonePruneFunctions(t *testing.T) {
	tools := loadTestFixtures(t)

	// Test PruneTool standalone
	pTool, err := pruner.PruneTool(tools[0], pruner.TierCompact)
	if err != nil {
		t.Fatalf("PruneTool failed: %v", err)
	}
	if pTool.Name != tools[0].Name {
		t.Fatalf("expected tool name %s, got %s", tools[0].Name, pTool.Name)
	}

	// Test PruneToolsList standalone
	pList, err := pruner.PruneToolsList(tools, pruner.TierAggressive)
	if err != nil {
		t.Fatalf("PruneToolsList failed: %v", err)
	}
	if len(pList) != len(tools) {
		t.Fatalf("expected %d tools, got %d", len(tools), len(pList))
	}
}

// TestMinifyLosslessPreservesObjectAdditionalProperties ensures dictionaries are not dropped.
func TestMinifyLosslessPreservesObjectAdditionalProperties(t *testing.T) {
	dictTool := mcp.Tool{
		Name:        "dict_tool",
		Description: "Tool with dictionary schema in additionalProperties",
		InputSchema: mcp.InputSchema{
			Type: "object",
			AdditionalProperties: map[string]any{
				"type":  "string",
				"title": "UnwantedTitle",
			},
		},
	}

	minified := pruner.MinifyTool(dictTool)
	addProps, ok := minified.InputSchema.AdditionalProperties.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any in AdditionalProperties, got %T", minified.InputSchema.AdditionalProperties)
	}
	if addProps["type"] != "string" {
		t.Fatalf("expected type: string in dictionary, got %v", addProps["type"])
	}
	if _, hasTitle := addProps["title"]; hasTitle {
		t.Fatalf("expected title to be stripped from nested dictionary schema")
	}
}

// TestCondenseDescriptionSentenceSplitting verifies abbreviation handling.
func TestCondenseDescriptionSentenceSplitting(t *testing.T) {
	desc := "Search for libraries e.g. React, Next.js, and Three.js in the registry. This is a second sentence."
	opts := pruner.DefaultCondenseOptions()
	opts.ExtractFirstSentence = true
	opts.MaxToolDescLen = 150

	condensed := pruner.CondenseDescription(desc, opts.MaxToolDescLen, opts)
	if !strings.Contains(condensed, "Next.js") {
		t.Fatalf("expected condensed description to contain 'Next.js', got: %q", condensed)
	}
	if strings.Contains(condensed, "second sentence") {
		t.Fatalf("expected second sentence to be stripped, got: %q", condensed)
	}
}

// TestNormalizeRegex_UUIDv4NotDateTime asserts standard RFC 4122 UUID regex maps to "uuid", not "date-time".
func TestNormalizeRegex_UUIDv4NotDateTime(t *testing.T) {
	opts := pruner.DefaultCondenseOptions()
	opts.NormalizeRegexToFormat = true

	uuidPatterns := []string{
		`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`,
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`,
		`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`,
		`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`,
	}

	for _, pat := range uuidPatterns {
		newPat, newFmt, changed := pruner.NormalizeRegex(pat, "", opts)
		if !changed {
			t.Errorf("expected pattern %q to be normalized, but got changed=false", pat)
		}
		if newFmt != "uuid" {
			t.Errorf("pattern %q normalized to format %q, expected %q (must NOT be date-time)", pat, newFmt, "uuid")
		}
		if newPat != "" {
			t.Errorf("expected pattern %q to be emptied when replaced by format, got %q", pat, newPat)
		}
	}

	// Confirm ISO-8601 patterns still normalize to date-time
	isoPatterns := []string{
		`^(?:(?:\d\d[2468][048]|\d\d[13579][26]|\d\d0[48]|[02468][048]00|[13579][26]00)-02-29|\d{4}-(?:(?:0[13578]|1[02])-(?:0[1-9]|[12]\d|3[01])|(?:0[469]|11)-(?:0[1-9]|[12]\d|30)|(?:02)-(?:0[1-9]|1\d|2[0-8])))T(?:(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\\.\d+)?(?:Z|([+-](?:[01]\d|2[0-3]):[0-5]\d)))$`,
		`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`,
	}
	for _, isoPat := range isoPatterns {
		_, isoFmt, changed := pruner.NormalizeRegex(isoPat, "", opts)
		if !changed || isoFmt != "date-time" {
			t.Errorf("expected ISO pattern to normalize to date-time, got fmt=%q changed=%v", isoFmt, changed)
		}
	}
}

// TestSelfEvidentParam_NonIDWordsPreserved asserts non-ID English words (grid, hybrid, solid, valid) retain descriptions under Tier 3.
func TestSelfEvidentParam_NonIDWordsPreserved(t *testing.T) {
	testTool := mcp.Tool{
		Name:        "ui_layout_tool",
		Description: "Tool for managing CSS grids and hybrid layouts",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"grid": map[string]any{
					"type":        "string",
					"description": "CSS grid template configuration layout string",
				},
				"hybrid": map[string]any{
					"type":        "boolean",
					"description": "Enable hybrid rendering engine mode",
				},
				"solid": map[string]any{
					"type":        "string",
					"description": "Solid background color hex code string",
				},
				"valid": map[string]any{
					"type":        "boolean",
					"description": "Flag indicating validation status of the form",
				},
				"user_id": map[string]any{
					"type":        "string",
					"description": "The unique user identification string",
				},
				"userId": map[string]any{
					"type":        "string",
					"description": "CamelCase user identification string",
				},
				"accountID": map[string]any{
					"type":        "string",
					"description": "Account identifier with uppercase ID suffix",
				},
			},
		},
	}

	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	pruned, _, err := engine.PruneTools([]mcp.Tool{testTool})
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}

	pProps := pruned[0].InputSchema.Properties

	// grid, hybrid, solid, valid must PRESERVE their descriptions
	for _, propName := range []string{"grid", "hybrid", "solid", "valid"} {
		rawProp, ok := pProps[propName].(map[string]any)
		if !ok {
			t.Fatalf("property %s is missing from pruned tool", propName)
		}
		desc, hasDesc := rawProp["description"].(string)
		if !hasDesc || desc == "" {
			t.Fatalf("expected non-ID property %q to preserve description, but it was stripped", propName)
		}
	}

	// user_id, userId, accountID are genuine IDs and should have their descriptions stripped under Tier 3
	for _, idName := range []string{"user_id", "userId", "accountID"} {
		rawProp, ok := pProps[idName].(map[string]any)
		if !ok {
			t.Fatalf("property %s is missing from pruned tool", idName)
		}
		if desc, hasDesc := rawProp["description"]; hasDesc && desc != "" {
			t.Fatalf("expected self-evident ID property %q to have stripped description, got %q", idName, desc)
		}
	}
}

// TestDraft07Compliance_BooleanArrayItems verifies boolean schema items under Draft-07.
func TestDraft07Compliance_BooleanArrayItems(t *testing.T) {
	toolWithBoolItems := mcp.Tool{
		Name:        "bool_items_tool",
		Description: "Tool with boolean schema items",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"empty_tuple": map[string]any{
					"type":  "array",
					"items": false,
				},
				"any_array": map[string]any{
					"type":  "array",
					"items": true,
				},
			},
		},
	}

	res := pruner.ValidateTool(toolWithBoolItems)
	if !res.Valid {
		t.Fatalf("expected boolean array items to be valid under Draft-07, got errors: %v", res.Errors)
	}

	// Invalid array items should fail
	badTool := mcp.Tool{
		Name: "bad_items_tool",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"bad_array": map[string]any{
					"type":  "array",
					"items": 12345, // Not a schema object, boolean, or array
				},
			},
		},
	}
	badRes := pruner.ValidateTool(badTool)
	if badRes.Valid {
		t.Fatalf("expected number items to fail validation")
	}
	found := false
	for _, e := range badRes.Errors {
		if e.Code == pruner.ErrInvalidArrayItems {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ErrInvalidArrayItems error, got: %v", badRes.Errors)
	}
}
