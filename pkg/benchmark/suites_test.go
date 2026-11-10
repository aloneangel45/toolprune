package benchmark_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

func TestRealisticEnterpriseSuite_Over90PercentReduction(t *testing.T) {
	suite := benchmark.RealisticEnterpriseSuite()
	if len(suite) < 6 {
		t.Fatalf("expected at least 6 realistic enterprise MCP tools, got %d", len(suite))
	}

	report, err := benchmark.RunBenchmarkWithOptions(suite, benchmark.BenchmarkOptions{
		UseExactTokens:        true,
		ValidateSchemas:       true,
		MaxSchemaTokens:       350,
		IncludeParamBreakdown: true,
	})
	if err != nil {
		t.Fatalf("RunBenchmarkWithOptions failed: %v", err)
	}

	for _, tr := range report.Tiers {
		if !tr.ValidDraft07 {
			t.Errorf("tier %s failed Draft-07 validation: %v", tr.Name, tr.ValidationErrors)
		}
	}
	if report.BudgetTier == nil {
		t.Fatal("expected BudgetTier report when MaxSchemaTokens=350")
	}
	if !report.BudgetTier.ValidDraft07 {
		t.Errorf("BudgetTier failed Draft-07 validation: %v", report.BudgetTier.ValidationErrors)
	}
	if report.BudgetTier.Tokens > 350 {
		t.Errorf("expected BudgetTier exact cl100k_base tokens <= 350, got %d", report.BudgetTier.Tokens)
	}

	// Verify >90% token reduction target on realistic verbose PostgreSQL + Filesystem + GitHub MCP schemas
	if report.Summary.MaxSavingsPercent < 90.0 {
		t.Errorf("expected >90%% max token reduction on realistic enterprise suite, got %.2f%% (raw=%d, tier4=%d)",
			report.Summary.MaxSavingsPercent, report.RawTokens, report.Tiers[4].Tokens)
	}
	if report.BudgetTier.SavingsPercent < 90.0 {
		t.Errorf("expected >90%% token reduction under 350-token budget, got %.2f%% (raw=%d, budget=%d)",
			report.BudgetTier.SavingsPercent, report.RawTokens, report.BudgetTier.Tokens)
	}

	t.Logf("Realistic Enterprise MCP Suite (%d tools): Raw=%d tok -> Tier3=%d tok (-%.1f%%) -> Budget(350)=%d tok (-%.1f%%) -> Tier4=%d tok (-%.1f%%)",
		report.ToolCount,
		report.RawTokens,
		report.Tiers[3].Tokens, report.Tiers[3].SavingsPercent,
		report.BudgetTier.Tokens, report.BudgetTier.SavingsPercent,
		report.Tiers[4].Tokens, report.Tiers[4].SavingsPercent,
	)

	// Ensure fixture file test/fixtures/mcp_suite_realistic.json stays valid and loadable
	fixtureCandidate := filepath.Join("..", "..", "test", "fixtures", "mcp_suite_realistic.json")
	if _, statErr := os.Stat(fixtureCandidate); os.IsNotExist(statErr) {
		if rawJSON, mErr := benchmark.MarshalRealisticEnterpriseSuiteJSON(); mErr == nil {
			_ = os.WriteFile(fixtureCandidate, append(rawJSON, '\n'), 0644)
		}
	}
	if _, statErr := os.Stat(fixtureCandidate); statErr == nil {
		loaded, loadErr := benchmark.LoadFixtures(fixtureCandidate)
		if loadErr != nil {
			t.Fatalf("failed to load mcp_suite_realistic.json: %v", loadErr)
		}
		if len(loaded) != len(suite) {
			t.Errorf("mcp_suite_realistic.json tool count = %d, want %d", len(loaded), len(suite))
		}
	}
}

func BenchmarkEnforceTokenBudget_RealisticSuite(b *testing.B) {
	suite := benchmark.RealisticEnterpriseSuite()
	opts := pruner.DefaultOptions(pruner.TierAggressive)
	engine := pruner.New(opts)
	baseTools, _, err := engine.PruneToolsWithTier(suite, pruner.TierAggressive)
	if err != nil {
		b.Fatalf("PruneToolsWithTier setup failed: %v", err)
	}
	budgetOpts := pruner.BudgetOptions{
		MaxSchemaTokens: 600,
		PinnedTools:     map[string]struct{}{"postgres__analyze_query_plan": {}},
		ContextBoostTools: map[string]float64{
			"postgres__apply_ddl_migration":     2.5,
			"filesystem__apply_multi_hunk_diff": 1.4,
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		compacted, _ := pruner.EnforceTokenBudget(baseTools, budgetOpts)
		if len(compacted) == 0 {
			b.Fatal("empty compacted slice")
		}
	}
}

func BenchmarkStreamFilter_ColdPass_RealisticSuite(b *testing.B) {
	suite := benchmark.RealisticEnterpriseSuite()
	opts := pruner.DefaultOptions(pruner.TierAggressive)
	opts.StrictDraft07 = false // Hot-path stream Benchmark (<0.5ms target)
	opts.MaxSchemaTokens = 600
	engine := pruner.New(opts)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sf := proxy.NewStreamFilter(proxy.StreamFilterConfig{
			PruneMode:       "aggressive",
			MaxSchemaTokens: 600,
		})
		sf.RecordToolCall("postgres__apply_ddl_migration")
		out, _, err := sf.FilterToolsList(suite, engine)
		if err != nil || len(out) == 0 {
			b.Fatalf("FilterToolsList failed: %v", err)
		}
	}
}

func BenchmarkStreamFilter_CacheHit_RealisticSuite(b *testing.B) {
	suite := benchmark.RealisticEnterpriseSuite()
	opts := pruner.DefaultOptions(pruner.TierAggressive)
	opts.MaxSchemaTokens = 600
	engine := pruner.New(opts)

	sf := proxy.NewStreamFilter(proxy.StreamFilterConfig{
		PruneMode:       "aggressive",
		MaxSchemaTokens: 600,
	})
	sf.RecordToolCall("postgres__apply_ddl_migration")
	if _, _, err := sf.FilterToolsList(suite, engine); err != nil {
		b.Fatalf("warmup failed: %v", err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, metrics, err := sf.FilterToolsList(suite, engine)
		if err != nil || !metrics.CacheHit || len(out) == 0 {
			b.Fatalf("expected cache hit, err=%v", err)
		}
	}
}

func BenchmarkStreamFilter_PreBindValidation(b *testing.B) {
	suite := benchmark.RealisticEnterpriseSuite()
	ddlSchema := suite[0].InputSchema
	validArgs := json.RawMessage(`{
		"migration_id": "20261110_idx",
		"database": "ledger",
		"lock_timeout_ms": 1500,
		"operations": [
			{
				"op_type": "create_table",
				"table_name": "events",
				"columns": [
					{"name": "id", "data_type": "uuid", "primary_key": true}
				]
			}
		]
	}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if vErr := proxy.ValidateToolArguments("postgres__apply_ddl_migration", ddlSchema, validArgs); vErr != nil {
			b.Fatalf("unexpected validation failure: %v", vErr)
		}
	}
}
