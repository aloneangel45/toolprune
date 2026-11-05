package pruner_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// Helper to assert that a function executes without any panics.
func assertNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("[%s] PANIC detected: %v", name, r)
		}
	}()
	fn()
}

// =========================================================================
// 1. Complex Structural Schema Challenge: 20 Levels of Nesting
// =========================================================================

func build20LevelNestedTool() mcp.Tool {
	// Build bottom-up: level 20 to level 1
	var currentChild map[string]any = map[string]any{
		"type": "string",
		"description": "Deepest leaf at level 20 with verbose markdown documentation [docs](https://example.com/leaf). " +
			"IMPORTANT: Ensure leaf integrity. Additional sentence for condensation.",
		"enum":    []any{"alpha", "beta", "gamma"},
		"pattern": `^[a-z]{5,10}$`,
	}

	for lvl := 19; lvl >= 1; lvl-- {
		parent := map[string]any{
			"type": "object",
			"description": fmt.Sprintf("Nesting tier %d with tutorial instruction:\n"+
				"- Step 1: Traverse downward.\n- Step 2: Validate child.\n```go\nvar x = %d\n```", lvl, lvl),
			"properties": map[string]any{
				fmt.Sprintf("level_%d_child", lvl+1): currentChild,
				fmt.Sprintf("level_%d_meta", lvl): map[string]any{
					"type":        "integer",
					"description": fmt.Sprintf("Metadata value for level %d", lvl),
					"minimum":     float64(lvl),
					"maximum":     float64(lvl * 100),
				},
			},
			"required": []string{fmt.Sprintf("level_%d_child", lvl+1)},
		}
		currentChild = parent
	}

	return mcp.Tool{
		Name: "pathological_20_level_nesting_tool",
		Description: "A pathological schema with 20 levels of nesting designed to challenge stack safety and recursion. " +
			"```json\n{\"nested\": true}\n```\nIMPORTANT: Do not overflow the stack.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"level_1_child": currentChild,
			},
			Required: []string{"level_1_child"},
		},
	}
}

func TestAdversarial_20LevelNesting(t *testing.T) {
	tool := build20LevelNestedTool()

	// Verify input tool is valid Draft-07
	rawRes := pruner.ValidateTool(tool)
	if !rawRes.Valid {
		t.Fatalf("Raw 20-level tool failed initial Draft-07 validation: %v", rawRes.Errors)
	}

	tiers := []pruner.PruneTier{
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			assertNoPanic(t, string(tier), func() {
				engine := pruner.New(pruner.DefaultOptions(tier))
				prunedTools, metrics, err := engine.PruneTools([]mcp.Tool{tool})
				if err != nil {
					t.Fatalf("PruneTools returned error on tier %s: %v", tier, err)
				}
				if len(prunedTools) != 1 {
					t.Fatalf("Expected 1 pruned tool, got %d", len(prunedTools))
				}

				pTool := prunedTools[0]

				// 1. Assert 100% Draft-07 compliance
				vRes := pruner.ValidateTool(pTool)
				if !vRes.Valid {
					t.Fatalf("Tier %s pruned tool failed Draft-07 validation: %v", tier, vRes.Errors)
				}

				// 2. Traverse down to verify all 20 levels exist and were not corrupted
				curr := pTool.InputSchema.Properties
				for lvl := 1; lvl < 20; lvl++ {
					childKey := fmt.Sprintf("level_%d_child", lvl)
					rawChild, ok := curr[childKey]
					if !ok {
						t.Fatalf("Tier %s: Missing %s at level %d", tier, childKey, lvl)
					}
					childMap, ok := rawChild.(map[string]any)
					if !ok {
						t.Fatalf("Tier %s: %s is not a map", tier, childKey)
					}
					props, ok := childMap["properties"].(map[string]any)
					if !ok {
						t.Fatalf("Tier %s: Missing properties in %s at level %d", tier, childKey, lvl)
					}
					curr = props
				}

				// Verify Level 20 leaf invariants
				leafKey := "level_20_child"
				rawLeaf, ok := curr[leafKey]
				if !ok {
					t.Fatalf("Tier %s: Missing leaf %s at level 20", tier, leafKey)
				}
				leafMap, ok := rawLeaf.(map[string]any)
				if !ok {
					t.Fatalf("Tier %s: Leaf %s is not a map", tier, leafKey)
				}

				// Invariant check: type string must be preserved
				if leafMap["type"] != "string" {
					t.Fatalf("Tier %s: Leaf type corrupted: expected string, got %v", tier, leafMap["type"])
				}

				// Invariant check: enum values must be preserved
				enums, hasEnums := leafMap["enum"].([]any)
				if !hasEnums || len(enums) != 3 {
					t.Fatalf("Tier %s: Leaf enums lost or corrupted: %v", tier, leafMap["enum"])
				}

				// Tier-specific behavior checks
				if tier == pruner.TierCompact || tier == pruner.TierAggressive {
					// Markdown links should be stripped
					if desc, ok := leafMap["description"].(string); ok {
						if strings.Contains(desc, "[docs]") {
							t.Fatalf("Tier %s: Markdown link was not stripped from description: %q", tier, desc)
						}
					}
				}

				// Verify JSON serialization produces valid compact JSON
				jsonBytes, err := json.Marshal(pTool)
				if err != nil {
					t.Fatalf("Tier %s: Failed to serialize pruned tool to JSON: %v", tier, err)
				}
				if len(jsonBytes) == 0 {
					t.Fatalf("Tier %s: Empty JSON serialization", tier)
				}

				t.Logf("Tier %s: 20-level nesting passed. RawTokens=%d, PrunedTokens=%d, Savings=%.1f%%",
					tier, metrics.RawTokens, metrics.PrunedTokens, metrics.SavingsRatio*100)
			})
		})
	}
}

// =========================================================================
// 2. Complex Structural Schema Challenge: Recursive $defs / definitions
// =========================================================================

func buildRecursiveDefsTool() mcp.Tool {
	return mcp.Tool{
		Name: "pathological_recursive_defs_tool",
		Description: "Schema with self-referencing and mutually recursive $defs and definitions.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"rootTree": map[string]any{
					"type": "object",
					"description": "Binary tree node root referencing $defs/treeNode",
					"$ref":        "#/$defs/treeNode",
					"$defs": map[string]any{
						"treeNode": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"value": map[string]any{
									"type":        "string",
									"description": "Node payload value",
								},
								"left": map[string]any{
									"$ref": "#/$defs/treeNode",
								},
								"right": map[string]any{
									"$ref": "#/$defs/treeNode",
								},
							},
							"required": []string{"value"},
						},
						"mutualA": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"toB": map[string]any{"$ref": "#/$defs/mutualB"},
							},
						},
						"mutualB": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"toA": map[string]any{"$ref": "#/$defs/mutualA"},
							},
						},
					},
					"definitions": map[string]any{
						"legacyNode": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"data": map[string]any{"type": "string"},
								"next": map[string]any{"$ref": "#/definitions/legacyNode"},
							},
						},
					},
				},
			},
			Required: []string{"rootTree"},
		},
	}
}

func TestAdversarial_RecursiveDefinitions(t *testing.T) {
	tool := buildRecursiveDefsTool()

	rawRes := pruner.ValidateTool(tool)
	if !rawRes.Valid {
		t.Fatalf("Raw recursive defs tool failed initial Draft-07 validation: %v", rawRes.Errors)
	}

	tiers := []pruner.PruneTier{
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			assertNoPanic(t, string(tier), func() {
				engine := pruner.New(pruner.DefaultOptions(tier))
				prunedTools, _, err := engine.PruneTools([]mcp.Tool{tool})
				if err != nil {
					t.Fatalf("PruneTools failed on tier %s: %v", tier, err)
				}
				pTool := prunedTools[0]

				// Validate Draft-07 syntax
				vRes := pruner.ValidateTool(pTool)
				if !vRes.Valid {
					t.Fatalf("Tier %s: Recursive defs tool failed validation: %v", tier, vRes.Errors)
				}

				// Check that $defs and definitions structures survived
				rootTree := pTool.InputSchema.Properties["rootTree"].(map[string]any)
				if rootTree["$ref"] != "#/$defs/treeNode" {
					t.Fatalf("Tier %s: $ref was corrupted: %v", tier, rootTree["$ref"])
				}
				defs, hasDefs := rootTree["$defs"].(map[string]any)
				if !hasDefs {
					t.Fatalf("Tier %s: $defs was lost in pruning", tier)
				}
				if _, hasTreeNode := defs["treeNode"]; !hasTreeNode {
					t.Fatalf("Tier %s: $defs/treeNode was lost", tier)
				}

				// Serialization check
				_, err = json.Marshal(pTool)
				if err != nil {
					t.Fatalf("Tier %s: Failed to serialize pruned tool with recursive $defs: %v", tier, err)
				}
			})
		})
	}
}

// =========================================================================
// 3. Complex Structural Schema Challenge: Large Polymorphic Unions
// =========================================================================

func buildLargePolymorphicUnionsTool(branchCount int) mcp.Tool {
	anyOfBranches := make([]any, branchCount)
	for i := 0; i < branchCount; i++ {
		anyOfBranches[i] = map[string]any{
			"type": "object",
			"description": fmt.Sprintf("Union branch %d of %d with markdown `code` and instructions. Sentence two.",
				i, branchCount),
			"properties": map[string]any{
				fmt.Sprintf("kind_%d", i): map[string]any{
					"type": "string",
					"enum": []any{fmt.Sprintf("variant_%d_a", i), fmt.Sprintf("variant_%d_b", i)},
				},
				"payload": map[string]any{
					"type": "string",
					"pattern": `^[0-9a-fA-F]{8}$`,
				},
			},
			"required": []string{fmt.Sprintf("kind_%d", i)},
		}
	}

	oneOfBranches := []any{
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{"type": "string", "enum": []any{"fast"}},
			},
		},
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{"type": "string", "enum": []any{"accurate"}},
			},
		},
	}

	allOfBranches := []any{
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"common_id": map[string]any{"type": "string"},
			},
		},
	}

	return mcp.Tool{
		Name: "pathological_large_unions_tool",
		Description: "Tool with large polymorphic unions: 50 anyOf branches, oneOf, and allOf combinations.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"polymorphic_payload": map[string]any{
					"type":  "object",
					"anyOf": anyOfBranches,
					"oneOf": oneOfBranches,
					"allOf": allOfBranches,
				},
			},
			Required: []string{"polymorphic_payload"},
		},
	}
}

func TestAdversarial_LargePolymorphicUnions(t *testing.T) {
	tool := buildLargePolymorphicUnionsTool(50)

	rawRes := pruner.ValidateTool(tool)
	if !rawRes.Valid {
		t.Fatalf("Raw unions tool failed initial validation: %v", rawRes.Errors)
	}

	tiers := []pruner.PruneTier{
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			assertNoPanic(t, string(tier), func() {
				engine := pruner.New(pruner.DefaultOptions(tier))
				prunedTools, metrics, err := engine.PruneTools([]mcp.Tool{tool})
				if err != nil {
					t.Fatalf("PruneTools failed on tier %s: %v", tier, err)
				}
				pTool := prunedTools[0]

				// Validate Draft-07 compliance
				vRes := pruner.ValidateTool(pTool)
				if !vRes.Valid {
					t.Fatalf("Tier %s: Large union tool failed Draft-07 validation: %v", tier, vRes.Errors)
				}

				// Verify all 50 anyOf branches exist and retain invariants
				payload := pTool.InputSchema.Properties["polymorphic_payload"].(map[string]any)
				anyOf, ok := payload["anyOf"].([]any)
				if !ok || len(anyOf) != 50 {
					t.Fatalf("Tier %s: Expected 50 anyOf branches, got %d", tier, len(anyOf))
				}

				for i, branch := range anyOf {
					bMap, ok := branch.(map[string]any)
					if !ok {
						t.Fatalf("Tier %s: Branch %d is not a map", tier, i)
					}
					props, ok := bMap["properties"].(map[string]any)
					if !ok {
						t.Fatalf("Tier %s: Branch %d missing properties", tier, i)
					}
					kindKey := fmt.Sprintf("kind_%d", i)
					kindProp, ok := props[kindKey].(map[string]any)
					if !ok {
						t.Fatalf("Tier %s: Branch %d missing %s", tier, i, kindKey)
					}
					enums, ok := kindProp["enum"].([]any)
					if !ok || len(enums) != 2 {
						t.Fatalf("Tier %s: Branch %d corrupted enums: %v", tier, i, kindProp["enum"])
					}
				}

				// Check oneOf and allOf preservation
				oneOf, ok := payload["oneOf"].([]any)
				if !ok || len(oneOf) != 2 {
					t.Fatalf("Tier %s: oneOf branches corrupted", tier)
				}
				allOf, ok := payload["allOf"].([]any)
				if !ok || len(allOf) != 1 {
					t.Fatalf("Tier %s: allOf branches corrupted", tier)
				}

				t.Logf("Tier %s: 50-branch union passed. RawTokens=%d, PrunedTokens=%d, Savings=%.1f%%",
					tier, metrics.RawTokens, metrics.PrunedTokens, metrics.SavingsRatio*100)
			})
		})
	}
}

// =========================================================================
// 4. Complex Structural Schema Challenge: Heterogeneous Array items
// =========================================================================

func buildHeterogeneousArrayTool() mcp.Tool {
	// Tuple validation (items as a list of different schemas per index)
	tupleItems := []any{
		map[string]any{"type": "string", "description": "Index 0: string name", "enum": []any{"Alice", "Bob"}},
		map[string]any{"type": "integer", "description": "Index 1: integer age", "minimum": float64(0)},
		map[string]any{"type": "boolean", "description": "Index 2: boolean flag"},
		map[string]any{
			"type": "object",
			"description": "Index 3: nested address",
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
			"required": []string{"city"},
		},
		map[string]any{
			"type": "array",
			"description": "Index 4: sub-array of numbers",
			"items": map[string]any{"type": "number"},
		},
	}

	return mcp.Tool{
		Name: "pathological_heterogeneous_array_tool",
		Description: "Tool containing heterogeneous tuple array items and nested multi-type collections.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"tuple_parameter": map[string]any{
					"type":        "array",
					"description": "A tuple parameter with 5 distinct element types.",
					"items":       tupleItems,
				},
				"union_array": map[string]any{
					"type":        "array",
					"description": "An array whose items are a polymorphic anyOf union.",
					"items": map[string]any{
						"anyOf": []any{
							map[string]any{"type": "string"},
							map[string]any{"type": "number"},
						},
					},
				},
			},
			Required: []string{"tuple_parameter"},
		},
	}
}

func TestAdversarial_HeterogeneousArrayItems(t *testing.T) {
	tool := buildHeterogeneousArrayTool()

	rawRes := pruner.ValidateTool(tool)
	if !rawRes.Valid {
		t.Fatalf("Raw heterogeneous array tool failed initial validation: %v", rawRes.Errors)
	}

	tiers := []pruner.PruneTier{
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			assertNoPanic(t, string(tier), func() {
				engine := pruner.New(pruner.DefaultOptions(tier))
				prunedTools, _, err := engine.PruneTools([]mcp.Tool{tool})
				if err != nil {
					t.Fatalf("PruneTools failed on tier %s: %v", tier, err)
				}
				pTool := prunedTools[0]

				vRes := pruner.ValidateTool(pTool)
				if !vRes.Valid {
					t.Fatalf("Tier %s: Heterogeneous array tool failed Draft-07 validation: %v", tier, vRes.Errors)
				}

				tupleProp := pTool.InputSchema.Properties["tuple_parameter"].(map[string]any)
				itemsList, ok := tupleProp["items"].([]any)
				if !ok || len(itemsList) != 5 {
					t.Fatalf("Tier %s: Expected 5 tuple items, got %v", tier, tupleProp["items"])
				}

				// Check that each position kept its specific type and enum
				item0 := itemsList[0].(map[string]any)
				if item0["type"] != "string" {
					t.Fatalf("Tier %s: Item 0 type corrupted: %v", tier, item0["type"])
				}
				enums0 := item0["enum"].([]any)
				if len(enums0) != 2 {
					t.Fatalf("Tier %s: Item 0 enums lost: %v", tier, item0["enum"])
				}

				item1 := itemsList[1].(map[string]any)
				if item1["type"] != "integer" {
					t.Fatalf("Tier %s: Item 1 type corrupted: %v", tier, item1["type"])
				}

				item2 := itemsList[2].(map[string]any)
				if item2["type"] != "boolean" {
					t.Fatalf("Tier %s: Item 2 type corrupted: %v", tier, item2["type"])
				}

				item3 := itemsList[3].(map[string]any)
				if item3["type"] != "object" {
					t.Fatalf("Tier %s: Item 3 type corrupted: %v", tier, item3["type"])
				}

				item4 := itemsList[4].(map[string]any)
				if item4["type"] != "array" {
					t.Fatalf("Tier %s: Item 4 type corrupted: %v", tier, item4["type"])
				}
			})
		})
	}
}

// =========================================================================
// 5. Negative Validation Challenge: Comprehensive Malformed Schemas
// =========================================================================

func TestAdversarial_NegativeValidation_Comprehensive(t *testing.T) {
	testCases := []struct {
		name        string
		tool        mcp.Tool
		wantCode    pruner.ValidationErrorCode
		wantErrPath string
	}{
		// Category A: Root type violations
		{
			name: "RootType_String",
			tool: mcp.Tool{
				Name:        "bad_root_string",
				InputSchema: mcp.InputSchema{Type: "string"},
			},
			wantCode:    pruner.ErrInvalidRootType,
			wantErrPath: "/inputSchema/type",
		},
		{
			name: "RootType_Array",
			tool: mcp.Tool{
				Name:        "bad_root_array",
				InputSchema: mcp.InputSchema{Type: "array"},
			},
			wantCode:    pruner.ErrInvalidRootType,
			wantErrPath: "/inputSchema/type",
		},
		{
			name: "RootType_Empty",
			tool: mcp.Tool{
				Name:        "bad_root_empty",
				InputSchema: mcp.InputSchema{Type: ""},
			},
			wantCode:    pruner.ErrInvalidRootType,
			wantErrPath: "/inputSchema/type",
		},

		// Category B: Required referential integrity & uniqueness
		{
			name: "Required_MissingProperty_TopLevel",
			tool: mcp.Tool{
				Name: "missing_req_top",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"propA": map[string]any{"type": "string"}},
					Required:   []string{"propA", "nonExistentProp"},
				},
			},
			wantCode:    pruner.ErrRequiredPropMissing,
			wantErrPath: "/inputSchema/required/nonExistentProp",
		},
		{
			name: "Required_NilPropertiesWithRequired",
			tool: mcp.Tool{
				Name: "nil_props_with_req",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: nil,
					Required:   []string{"propA"},
				},
			},
			wantCode:    pruner.ErrRequiredPropMissing,
			wantErrPath: "/inputSchema/required/propA",
		},
		{
			name: "Required_DuplicateProperty",
			tool: mcp.Tool{
				Name: "duplicate_req_top",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"propA": map[string]any{"type": "string"}},
					Required:   []string{"propA", "propA"},
				},
			},
			wantCode:    pruner.ErrDuplicateRequired,
			wantErrPath: "/inputSchema/required",
		},
		{
			name: "Required_NestedMissingProperty",
			tool: mcp.Tool{
				Name: "nested_missing_req",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"user": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string"},
							},
							"required": []string{"name", "ghost_field"},
						},
					},
				},
			},
			wantCode:    pruner.ErrRequiredPropMissing,
			wantErrPath: "/inputSchema/properties/user/required/ghost_field",
		},
		{
			name: "Required_NestedDuplicateProperty",
			tool: mcp.Tool{
				Name: "nested_duplicate_req",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"user": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{"type": "string"},
							},
							"required": []string{"name", "name"},
						},
					},
				},
			},
			wantCode:    pruner.ErrDuplicateRequired,
			wantErrPath: "/inputSchema/properties/user/required",
		},

		// Category C: Non-object property definitions
		{
			name: "PropertyDefinition_IntegerInsteadOfSchema",
			tool: mcp.Tool{
				Name: "prop_is_integer",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"invalid_prop": 42},
				},
			},
			wantCode:    pruner.ErrInvalidPropertiesType,
			wantErrPath: "/inputSchema/properties/invalid_prop",
		},
		{
			name: "PropertyDefinition_StringInsteadOfSchema",
			tool: mcp.Tool{
				Name: "prop_is_string",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"invalid_prop": "should_be_map"},
				},
			},
			wantCode:    pruner.ErrInvalidPropertiesType,
			wantErrPath: "/inputSchema/properties/invalid_prop",
		},
		{
			name: "PropertyDefinition_SliceInsteadOfSchema",
			tool: mcp.Tool{
				Name: "prop_is_slice",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{"invalid_prop": []any{"bad"}},
				},
			},
			wantCode:    pruner.ErrInvalidPropertiesType,
			wantErrPath: "/inputSchema/properties/invalid_prop",
		},

		// Category D: Invalid JSON Schema Draft-07 types
		{
			name: "InvalidType_CustomType",
			tool: mcp.Tool{
				Name: "invalid_type_custom",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"age": map[string]any{"type": "uint64"},
					},
				},
			},
			wantCode:    pruner.ErrInvalidPropType,
			wantErrPath: "/inputSchema/properties/age/type",
		},
		{
			name: "InvalidType_PythonType",
			tool: mcp.Tool{
				Name: "invalid_type_python",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"data": map[string]any{"type": "dict"},
					},
				},
			},
			wantCode:    pruner.ErrInvalidPropType,
			wantErrPath: "/inputSchema/properties/data/type",
		},
		{
			name: "InvalidType_NumericValue",
			tool: mcp.Tool{
				Name: "invalid_type_numeric",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"status": map[string]any{"type": 123},
					},
				},
			},
			wantCode:    pruner.ErrInvalidPropType,
			wantErrPath: "/inputSchema/properties/status/type",
		},
		{
			name: "InvalidType_MultiTypeWithInvalidElement",
			tool: mcp.Tool{
				Name: "invalid_multi_type",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"val": map[string]any{"type": []any{"string", "unknown_primitive"}},
					},
				},
			},
			wantCode:    pruner.ErrInvalidPropType,
			wantErrPath: "/inputSchema/properties/val/type",
		},

		// Category E: Invalid Enum definitions
		{
			name: "Enum_EmptySlice",
			tool: mcp.Tool{
				Name: "empty_enum_slice",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"choice": map[string]any{"type": "string", "enum": []any{}},
					},
				},
			},
			wantCode:    pruner.ErrInvalidEnum,
			wantErrPath: "/inputSchema/properties/choice/enum",
		},
		{
			name: "Enum_ScalarValue",
			tool: mcp.Tool{
				Name: "scalar_enum_value",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"choice": map[string]any{"type": "string", "enum": "single_string_not_slice"},
					},
				},
			},
			wantCode:    pruner.ErrInvalidEnum,
			wantErrPath: "/inputSchema/properties/choice/enum",
		},

		// Category F: Invalid regular expression patterns
		{
			name: "Pattern_UnclosedBracket",
			tool: mcp.Tool{
				Name: "unclosed_regex_bracket",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"code": map[string]any{"type": "string", "pattern": `[a-zA-Z0-9(`},
					},
				},
			},
			wantCode:    pruner.ErrInvalidRegexPattern,
			wantErrPath: "/inputSchema/properties/code/pattern",
		},
		{
			name: "Pattern_DanglingQuantifier",
			tool: mcp.Tool{
				Name: "dangling_quantifier",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"code": map[string]any{"type": "string", "pattern": `+abc`},
					},
				},
			},
			wantCode:    pruner.ErrInvalidRegexPattern,
			wantErrPath: "/inputSchema/properties/code/pattern",
		},

		// Category G: Invalid array items
		{
			name: "ArrayItems_ScalarInteger",
			tool: mcp.Tool{
				Name: "array_items_integer",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"tags": map[string]any{"type": "array", "items": 999},
					},
				},
			},
			wantCode:    pruner.ErrInvalidArrayItems,
			wantErrPath: "/inputSchema/properties/tags/items",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			res := pruner.ValidateTool(tc.tool)
			if res.Valid {
				t.Fatalf("[%s] Expected schema to be INVALID, but ValidateTool reported Valid == true", tc.name)
			}
			if len(res.Errors) == 0 {
				t.Fatalf("[%s] Valid is false but Errors slice is empty", tc.name)
			}

			// Find expected error code
			var matchedErr *pruner.ValidationError
			for _, err := range res.Errors {
				if err.Code == tc.wantCode {
					matchedErr = &err
					break
				}
			}

			if matchedErr == nil {
				t.Fatalf("[%s] Expected error code %q, but got errors: %v", tc.name, tc.wantCode, res.Errors)
			}

			// Verify error details
			if matchedErr.Path != tc.wantErrPath {
				t.Fatalf("[%s] Error path mismatch: expected %q, got %q", tc.name, tc.wantErrPath, matchedErr.Path)
			}
			if matchedErr.Message == "" {
				t.Fatalf("[%s] Error message is empty", tc.name)
			}
			if matchedErr.Expected == "" {
				t.Fatalf("[%s] Error expected description is empty", tc.name)
			}
			if matchedErr.Actual == "" {
				t.Fatalf("[%s] Error actual description is empty", tc.name)
			}

			t.Logf("[%s] Correctly rejected: %s", tc.name, matchedErr.Error())
		})
	}
}

// =========================================================================
// 6. Adversarial Edge Cases: Unicode, Large Strings, ReDoS Resistance
// =========================================================================

func TestAdversarial_UnicodeAndLargeStrings(t *testing.T) {
	// Construct schema with multibyte unicode characters and large documentation
	largeDoc := strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 200) // ~11KB
	unicodeTool := mcp.Tool{
		Name:        "unicode_edge_case_tool",
		Description: "Tool with multibyte unicode: 日本語, 中文, Türkçe (İçindekiler), emojis: 🚀🌟🔥🎉 and large doc:\n" + largeDoc,
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"unicode_param": map[string]any{
					"type": "string",
					"description": "Param with emoji truncation: 🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟🌟 and Turkish chars: ğüşiöç İĞÜŞÖÇ",
				},
				"iso_date": map[string]any{
					"type":        "string",
					"description": "Date timestamp",
					"pattern":     `^\d{4}-(?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01])T(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|[+-](?:[01]\d|2[0-3]):[0-5]\d)$`,
				},
			},
			Required: []string{"unicode_param"},
		},
	}

	tiers := []pruner.PruneTier{
		pruner.TierLossless,
		pruner.TierCompact,
		pruner.TierAggressive,
	}

	for _, tier := range tiers {
		t.Run(string(tier), func(t *testing.T) {
			assertNoPanic(t, string(tier), func() {
				engine := pruner.New(pruner.DefaultOptions(tier))
				prunedTools, _, err := engine.PruneTools([]mcp.Tool{unicodeTool})
				if err != nil {
					t.Fatalf("PruneTools failed on tier %s: %v", tier, err)
				}
				pTool := prunedTools[0]

				// Validate Draft-07
				vRes := pruner.ValidateTool(pTool)
				if !vRes.Valid {
					t.Fatalf("Tier %s: Unicode tool failed Draft-07 validation: %v", tier, vRes.Errors)
				}

				// Check valid JSON marshal without invalid UTF-8 replacement errors
				jsonBytes, err := json.Marshal(pTool)
				if err != nil {
					t.Fatalf("Tier %s: Failed to serialize unicode tool: %v", tier, err)
				}
				if len(jsonBytes) == 0 {
					t.Fatalf("Tier %s: Serialized bytes empty", tier)
				}

				// Unmarshal back to ensure roundtrip validity
				var roundtrip mcp.Tool
				if err := json.Unmarshal(jsonBytes, &roundtrip); err != nil {
					t.Fatalf("Tier %s: Failed to unmarshal JSON roundtrip: %v", tier, err)
				}
			})
		})
	}
}

// =========================================================================
// 7. Strict Draft-07 Guardrail Interception in Engine
// =========================================================================

func TestAdversarial_EngineRejectsMalformedToolWhenStrict(t *testing.T) {
	malformedTool := mcp.Tool{
		Name: "malformed_for_engine",
		InputSchema: mcp.InputSchema{
			Type: "string", // Invalid root type
		},
	}

	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	_, _, err := engine.PruneTools([]mcp.Tool{malformedTool})
	if err == nil {
		t.Fatalf("Expected engine.PruneTools to reject malformed tool under StrictDraft07, but got nil error")
	}
	if !strings.Contains(err.Error(), "failed Draft-07 validation") {
		t.Fatalf("Expected error to mention Draft-07 validation, got: %v", err)
	}
	t.Logf("Engine correctly intercepted malformed schema: %v", err)
}

// =========================================================================
// 8. Concurrency & Thread-Safety Stress Testing
// =========================================================================

func TestAdversarial_ConcurrentPruningAndReconfiguration(t *testing.T) {
	tool := build20LevelNestedTool()
	engine := pruner.New(pruner.DefaultOptions(pruner.TierLossless))

	concurrency := 30
	iterations := 20
	errChan := make(chan error, concurrency*iterations)

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// Alternately reconfigure options and prune tools
				if j%5 == 0 {
					tier := pruner.TierCompact
					if workerID%2 == 0 {
						tier = pruner.TierAggressive
					}
					engine.SetOptions(pruner.DefaultOptions(tier))
				}

				pruned, _, err := engine.PruneTools([]mcp.Tool{tool})
				if err != nil {
					errChan <- fmt.Errorf("worker %d iter %d: %w", workerID, j, err)
					return
				}
				if len(pruned) != 1 {
					errChan <- fmt.Errorf("worker %d iter %d: expected 1 tool, got %d", workerID, j, len(pruned))
					return
				}
			}
		}(i)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Fatalf("Concurrency race/panic detected: %v", err)
	}
}

// =========================================================================
// 9. Input Immutability Guarantee
// =========================================================================

func TestAdversarial_ImmutabilityGuarantee(t *testing.T) {
	originalTool := build20LevelNestedTool()
	originalJSON, err := json.Marshal(originalTool)
	if err != nil {
		t.Fatalf("Failed to marshal original tool: %v", err)
	}

	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	_, _, err = engine.PruneTools([]mcp.Tool{originalTool})
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}

	afterJSON, err := json.Marshal(originalTool)
	if err != nil {
		t.Fatalf("Failed to marshal original tool after pruning: %v", err)
	}

	if string(originalJSON) != string(afterJSON) {
		t.Fatalf("Immutability violation: original input tool was mutated during pruning!\nBefore: %s\nAfter: %s",
			string(originalJSON), string(afterJSON))
	}
}

// =========================================================================
// 10. Boundary Conditions: Zero & Empty Payloads
// =========================================================================

func TestAdversarial_ZeroAndEmptyPayloads(t *testing.T) {
	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))

	// Case A: Nil tools slice
	pruned, metrics, err := engine.PruneTools(nil)
	if err != nil {
		t.Fatalf("PruneTools(nil) failed: %v", err)
	}
	if len(pruned) != 0 {
		t.Fatalf("Expected 0 pruned tools for nil, got %d", len(pruned))
	}
	if metrics.RawTokens != 0 || metrics.PrunedTokens != 0 {
		t.Fatalf("Expected 0 tokens for nil tools, got %+v", metrics)
	}

	// Case B: Empty tools slice
	pruned, _, err = engine.PruneTools([]mcp.Tool{})
	if err != nil {
		t.Fatalf("PruneTools([]) failed: %v", err)
	}
	if len(pruned) != 0 {
		t.Fatalf("Expected 0 pruned tools for empty slice, got %d", len(pruned))
	}

	// Case C: Tool with empty properties map
	emptyPropsTool := mcp.Tool{
		Name: "empty_props",
		InputSchema: mcp.InputSchema{
			Type:       "object",
			Properties: map[string]any{},
		},
	}
	pruned, _, err = engine.PruneTools([]mcp.Tool{emptyPropsTool})
	if err != nil {
		t.Fatalf("PruneTools on empty properties failed: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("Expected 1 pruned tool, got %d", len(pruned))
	}
}

// =========================================================================
// 11. Combinatorial Stress: Deep Nesting + High Branching Factor
// =========================================================================

func TestAdversarial_DeepNestingWithBranchingFactor(t *testing.T) {
	// 10 levels deep, each level has an anyOf with 5 branches, each branch has 3 properties
	type branch map[string]any

	buildLevel := func(depth int, maxDepth int, selfFn any) map[string]any {
		if depth >= maxDepth {
			return map[string]any{
				"type": "string",
				"enum": []any{"terminal_variant_a", "terminal_variant_b"},
			}
		}

		recurse := selfFn.(func(int, int, any) map[string]any)
		branches := make([]any, 5)
		for b := 0; b < 5; b++ {
			branches[b] = map[string]any{
				"type": "object",
				"properties": map[string]any{
					fmt.Sprintf("prop_d%d_b%d", depth, b): recurse(depth+1, maxDepth, selfFn),
				},
			}
		}

		return map[string]any{
			"type":  "object",
			"anyOf": branches,
		}
	}

	var recursiveBuilder func(int, int, any) map[string]any
	recursiveBuilder = func(d, maxD int, fn any) map[string]any {
		return buildLevel(d, maxD, fn)
	}

	deepBranchingRoot := recursiveBuilder(1, 8, recursiveBuilder)

	tool := mcp.Tool{
		Name: "deep_branching_combinatorial_tool",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"combinatorial_tree": deepBranchingRoot,
			},
			Required: []string{"combinatorial_tree"},
		},
	}

	engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	pruned, metrics, err := engine.PruneTools([]mcp.Tool{tool})
	if err != nil {
		t.Fatalf("PruneTools failed on deep branching combinatorial schema: %v", err)
	}
	if len(pruned) != 1 {
		t.Fatalf("Expected 1 pruned tool, got %d", len(pruned))
	}

	vRes := pruner.ValidateTool(pruned[0])
	if !vRes.Valid {
		t.Fatalf("Deep branching combinatorial schema failed validation: %v", vRes.Errors)
	}

	t.Logf("Combinatorial stress passed: RawTokens=%d, PrunedTokens=%d, Savings=%.1f%%",
		metrics.RawTokens, metrics.PrunedTokens, metrics.SavingsRatio*100)
}
