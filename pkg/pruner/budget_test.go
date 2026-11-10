package pruner_test

import (
	"testing"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

func TestEnforceTokenBudget_ProgressivePassesAndCl100kCompliance(t *testing.T) {
	tools := loadTestFixtures(t)
	rawExact, err := benchmark.CountToolsTokens(tools)
	if err != nil {
		t.Fatalf("CountToolsTokens failed: %v", err)
	}
	if rawExact < 1000 {
		t.Fatalf("expected raw fixture tokens >= 1000, got %d", rawExact)
	}

	budgets := []int{
		int(float64(rawExact) * 0.70),
		int(float64(rawExact) * 0.45),
		int(float64(rawExact) * 0.25),
		int(float64(rawExact) * 0.12),
	}

	for _, budget := range budgets {
		compacted, metrics := pruner.EnforceTokenBudget(tools, pruner.BudgetOptions{
			MaxSchemaTokens: budget,
			PinnedTools:     map[string]struct{}{tools[0].Name: {}},
			ContextBoostTools: map[string]float64{
				tools[1].Name: 1.5,
			},
		})

		if len(compacted) != len(tools) {
			t.Fatalf("budget=%d: expected %d tools, got %d", budget, len(tools), len(compacted))
		}

		for _, ct := range compacted {
			vRes := pruner.ValidateTool(ct)
			if !vRes.Valid {
				t.Fatalf("budget=%d tool=%s failed Draft-07 validation: %v", budget, ct.Name, vRes.Errors)
			}
		}

		exactCompacted, err := benchmark.CountToolsTokens(compacted)
		if err != nil {
			t.Fatalf("CountToolsTokens(compacted) failed: %v", err)
		}
		t.Logf("budget=%d pass=%d estTokens=%d exactCl100k=%d projected=%d",
			budget, metrics.PassReached, metrics.CompactedToken, exactCompacted, len(metrics.ProjectedTools))

		if exactCompacted > budget {
			t.Fatalf("real cl100k_base tokens (%d) exceeded MaxSchemaTokens (%d) at pass %d",
				exactCompacted, budget, metrics.PassReached)
		}
	}
}

func TestEnforceTokenBudget_ReachableDefsRetainedAndUnreachablePruned(t *testing.T) {
	tool := mcp.Tool{
		Name:        "postgres_migrate",
		Description: "Apply multi-step DDL migration with column definitions and constraints.",
		InputSchema: mcp.InputSchema{
			Type:   "object",
			Schema: "https://json-schema.org/draft/2020-12/schema",
			Defs: map[string]any{
				"ColumnDef": map[string]any{
					"type":        "object",
					"description": "Column definition with nested ConstraintRef",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string", "minLength": float64(1)},
						"constraint": map[string]any{"$ref": "#/$defs/ConstraintDef"},
					},
					"required": []string{"name"},
				},
				"ConstraintDef": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind": map[string]any{"type": "string", "enum": []any{"PRIMARY_KEY", "UNIQUE", "CHECK"}},
					},
					"required": []string{"kind"},
				},
				"DeadUnusedDef": map[string]any{
					"type":        "object",
					"description": "Unreferenced bloat definition that should be removed in Pass 1",
					"properties": map[string]any{
						"unused_field": map[string]any{"type": "string"},
					},
				},
			},
			Properties: map[string]any{
				"columns": map[string]any{
					"type":     "array",
					"minItems": float64(1),
					"maxItems": float64(64),
					"items":    map[string]any{"$ref": "#/$defs/ColumnDef"},
				},
				"timeout_ms": map[string]any{
					"type":    "integer",
					"minimum": float64(100),
					"maximum": float64(60000),
				},
			},
			Required:             []string{"columns"},
			AdditionalProperties: false,
		},
	}

	// Set budget tight enough to trigger Pass 3/4 without stubbing single tool
	compacted, metrics := pruner.EnforceTokenBudget([]mcp.Tool{tool}, pruner.BudgetOptions{
		MaxSchemaTokens: 220,
	})
	if metrics.PassReached < 1 || metrics.PassReached >= 5 {
		t.Fatalf("expected PassReached in 1..4, got %d", metrics.PassReached)
	}
	got := compacted[0]
	if _, ok := got.InputSchema.Defs["ColumnDef"]; !ok {
		t.Fatal("reachable $defs/ColumnDef was stripped")
	}
	if _, ok := got.InputSchema.Defs["ConstraintDef"]; !ok {
		t.Fatal("transitively reachable $defs/ConstraintDef was stripped")
	}
	if _, ok := got.InputSchema.Defs["DeadUnusedDef"]; ok {
		t.Fatal("unreachable $defs/DeadUnusedDef should have been pruned in Pass 1")
	}

	cols := got.InputSchema.Properties["columns"].(map[string]any)
	if cols["minItems"] != float64(1) || cols["maxItems"] != float64(64) {
		t.Fatalf("minItems/maxItems lost: %+v", cols)
	}
	timeout := got.InputSchema.Properties["timeout_ms"].(map[string]any)
	if timeout["minimum"] != float64(100) || timeout["maximum"] != float64(60000) {
		t.Fatalf("minimum/maximum lost: %+v", timeout)
	}
	if got.InputSchema.AdditionalProperties != false {
		t.Fatalf("additionalProperties: false lost: %v", got.InputSchema.AdditionalProperties)
	}
}
