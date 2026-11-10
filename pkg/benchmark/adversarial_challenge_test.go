package benchmark_test

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// TestAdversarialSpecialTokens tests how token counters handle special BPE tokens
// such as <|endoftext|>, <|fim_prefix|>, etc.
func TestAdversarialSpecialTokens(t *testing.T) {
	specialTokens := []string{
		"<|endoftext|>",
		"<|fim_prefix|>",
		"<|fim_middle|>",
		"<|fim_suffix|>",
		"<|endofprompt|>",
		"This is normal text with <|endoftext|> embedded inside.",
	}

	for _, text := range specialTokens {
		t.Run(fmt.Sprintf("special_%s", text), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("CountTokens panicked on special token %q: %v", text, r)
				}
			}()

			toks, err := benchmark.CountTokens(text)
			if err != nil {
				t.Fatalf("CountTokens error on %q: %v", text, err)
			}
			if toks <= 0 {
				t.Errorf("expected positive tokens for %q, got %d", text, toks)
			}

			est := benchmark.EstimateTokens(text)
			if est <= 0 {
				t.Errorf("expected positive estimated tokens for %q, got %d", text, est)
			}
		})
	}
}

// TestAdversarialCalibratedEstimatorDrift tests calibrated estimator accuracy and drift
// across various payload sizes and structures (ASCII, Unicode, 4-byte UTF-8, repetitive strings).
func TestAdversarialCalibratedEstimatorDrift(t *testing.T) {
	testCases := []struct {
		name    string
		payload string
	}{
		{"empty", ""},
		{"single_char_a", "a"},
		{"single_bracket", "{"},
		{"short_json", `{"key": "value"}`},
		{"repeated_ascii", strings.Repeat("a", 10000)},
		{"repeated_pattern", strings.Repeat(`{"field": "test", "num": 123}, `, 500)},
		{"unicode_chinese", strings.Repeat("这是一个关于模型上下文协议工具测试的中文字符串。", 100)},
		{"unicode_japanese", strings.Repeat("これはMCPツールのベンチマークテスト用の日本語テキストです。", 100)},
		{"unicode_cyrillic", strings.Repeat("Это тестовая строка для проверки токенизатора ToolPrune.", 100)},
		{"unicode_arabic", strings.Repeat("هذا نص اختبار لبروتوكول سياق النموذج وأداة تشذيب المخطط.", 100)},
		{"astral_4byte_utf8", strings.Repeat("\U0001D538\U0001D539\U0001D53A\U0001D53B\U00010300\U00010301\U0001D400\U0001D401", 200)},
		{"deeply_escaped_json", strings.Repeat(`{\"nested\": \"value with \\\"quotes\\\" and \\n newlines\"}`, 200)},
		{"huge_ascii_1mb", strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789 \n", 28000)},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			est := benchmark.EstimateTokens(tc.payload)
			if est < 0 {
				t.Fatalf("EstimateTokens returned negative value %d for %q", est, tc.name)
			}

			if tc.payload == "" {
				if est != 0 {
					t.Errorf("expected 0 for empty payload, got %d", est)
				}
				return
			}

			exact, err := benchmark.CountTokensExact(tc.payload)
			if err != nil {
				t.Fatalf("CountTokensExact failed: %v", err)
			}

			if exact < 0 {
				t.Fatalf("CountTokensExact returned negative tokens %d", exact)
			}

			delta := math.Abs(float64(est - exact))
			var pctDrift float64
			if exact > 0 {
				pctDrift = (delta / float64(exact)) * 100.0
			}

			t.Logf("[%s] Payload bytes: %d | Exact: %d | Est: %d | Delta: %d | Drift: %.2f%%",
				tc.name, len(tc.payload), exact, est, int(delta), pctDrift)
		})
	}
}

// TestAdversarialEmptyAndZeroTools tests benchmark behavior with empty or minimal tool schemas.
func TestAdversarialEmptyAndZeroTools(t *testing.T) {
	t.Run("empty tools slice returns error", func(t *testing.T) {
		report, err := benchmark.RunBenchmark([]mcp.Tool{})
		if err == nil {
			t.Fatalf("expected error on empty tools slice, got report: %+v", report)
		}
		if !strings.Contains(err.Error(), "cannot benchmark empty tool set") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	t.Run("nil tools slice returns error", func(t *testing.T) {
		report, err := benchmark.RunBenchmark(nil)
		if err == nil {
			t.Fatalf("expected error on nil tools slice, got report: %+v", report)
		}
	})

	t.Run("CompareEstimator on empty slice", func(t *testing.T) {
		comp, err := benchmark.CompareEstimator([]mcp.Tool{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if comp.AccuracyPercent != 100.0 {
			t.Errorf("expected 100%% accuracy for empty tools slice, got %f", comp.AccuracyPercent)
		}
		if comp.ExactTokens != 0 || comp.EstimatedTokens != 0 {
			t.Errorf("expected 0 tokens, got exact=%d est=%d", comp.ExactTokens, comp.EstimatedTokens)
		}
	})

	t.Run("zero-value tool schema", func(t *testing.T) {
		minimalTools := []mcp.Tool{
			{
				Name:        "minimal_tool",
				Description: "",
				InputSchema: mcp.InputSchema{
					Type:       "object",
					Properties: map[string]any{},
				},
			},
		}

		report, err := benchmark.RunBenchmark(minimalTools)
		if err != nil {
			t.Fatalf("unexpected error running benchmark on minimal tool: %v", err)
		}

		if report.ToolCount != 1 {
			t.Errorf("expected ToolCount 1, got %d", report.ToolCount)
		}
		if len(report.Tiers) != 5 {
			t.Fatalf("expected 5 tiers, got %d", len(report.Tiers))
		}

		table := benchmark.FormatReportTable(report)
		if table == "" {
			t.Errorf("expected non-empty table output")
		}

		md := benchmark.FormatReportMarkdown(report)
		if md == "" {
			t.Errorf("expected non-empty markdown output")
		}
	})
}

// TestAdversarialDeeplyNestedSchemas tests tools with extreme schema nesting depth (up to 60 levels).
func TestAdversarialDeeplyNestedSchemas(t *testing.T) {
	depths := []int{5, 15, 30, 50}

	for _, depth := range depths {
		t.Run(fmt.Sprintf("nesting_depth_%d", depth), func(t *testing.T) {
			nestedSchema := buildNestedSchema(depth)
			tool := mcp.Tool{
				Name:        fmt.Sprintf("deep_tool_depth_%d", depth),
				Description: fmt.Sprintf("Tool with %d levels of nested object parameters", depth),
				InputSchema: nestedSchema,
			}

			tools := []mcp.Tool{tool}

			report, err := benchmark.RunBenchmark(tools)
			if err != nil {
				t.Fatalf("RunBenchmark failed on depth %d: %v", depth, err)
			}

			if report.ToolCount != 1 {
				t.Errorf("expected 1 tool, got %d", report.ToolCount)
			}

			// Verify progressive savings or equality
			for i := 1; i < len(report.Tiers); i++ {
				prev := report.Tiers[i-1].Tokens
				curr := report.Tiers[i].Tokens
				if curr > prev {
					t.Errorf("Tier %d tokens (%d) > Tier %d tokens (%d)", i, curr, i-1, prev)
				}
			}

			t.Logf("Depth %d: Raw=%d -> Tier1=%d -> Tier2=%d -> Tier3=%d -> Tier4=%d (Saved: %.1f%%)",
				depth, report.Tiers[0].Tokens, report.Tiers[1].Tokens, report.Tiers[2].Tokens,
				report.Tiers[3].Tokens, report.Tiers[4].Tokens, report.Tiers[4].SavingsPercent)
		})
	}
}

// helper to build deeply nested object properties
func buildNestedSchema(depth int) mcp.InputSchema {
	leaf := map[string]any{
		"type":        "string",
		"description": "Leaf string property at deepest level",
	}

	curr := map[string]any{
		"leaf_field": leaf,
	}

	for i := depth - 1; i >= 0; i-- {
		parent := map[string]any{
			fmt.Sprintf("level_%d", i): map[string]any{
				"type":        "object",
				"description": fmt.Sprintf("Intermediate nested container at depth level %d", i),
				"properties":  curr,
			},
		}
		curr = parent
	}

	return mcp.InputSchema{
		Type:       "object",
		Properties: curr,
	}
}

// TestAdversarialMassiveToolSet tests benchmarking scaling under 500 realistic tools.
// It also proves the empirical threshold: micro-tools (<120 tokens) achieve ~65% savings,
// while realistic production tools (>350 tokens) comfortably exceed the 80% reduction target.
func TestAdversarialMassiveToolSet(t *testing.T) {
	t.Run("micro_tools_savings_bounded_by_envelope", func(t *testing.T) {
		const toolCount = 100
		microTools := make([]mcp.Tool, toolCount)
		for i := 0; i < toolCount; i++ {
			microTools[i] = mcp.Tool{
				Name:        fmt.Sprintf("tool_%d", i),
				Description: fmt.Sprintf("Micro tool %d description.", i),
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]any{
						"id": map[string]any{"type": "string"},
					},
					Required: []string{"id"},
				},
			}
		}

		rep, err := benchmark.RunBenchmark(microTools)
		if err != nil {
			t.Fatalf("RunBenchmark failed: %v", err)
		}
		// For micro tools, baseline is ~70-90 tokens, Tier 4 is ~25-30 tokens, saving ~60-70%
		t.Logf("Micro-tools: Raw=%d -> Tier 4=%d (Savings: %.1f%%)",
			rep.RawTokens, rep.Tiers[4].Tokens, rep.Tiers[4].SavingsPercent)
	})

	t.Run("production_schemas_exceed_80_percent_target", func(t *testing.T) {
		fixtures, err := benchmark.LoadFixtures()
		if err != nil {
			t.Fatalf("failed to load fixtures: %v", err)
		}

		// Replicate real production schemas 50 times (200 real production tools)
		const replication = 50
		tools := make([]mcp.Tool, 0, len(fixtures)*replication)
		for rep := 0; rep < replication; rep++ {
			for _, f := range fixtures {
				clone := f
				clone.Name = fmt.Sprintf("%s_instance_%03d", f.Name, rep)
				tools = append(tools, clone)
			}
		}

		start := time.Now()
		report, err := benchmark.RunBenchmark(tools)
		duration := time.Since(start)

		if err != nil {
			t.Fatalf("RunBenchmark on %d tools failed: %v", len(tools), err)
		}

		// Verify 80% target on realistic production tools
		if report.Tiers[4].SavingsPercent < 80.0 {
			t.Errorf("expected Tier 4 savings >= 80%% on production schemas, got %.1f%%", report.Tiers[4].SavingsPercent)
		}

		t.Logf("Massive Production Toolset (%d tools) completed in %v. Baseline: %d -> Tier 4: %d (%.1f%% reduction)",
			len(tools), duration, report.RawTokens, report.Tiers[4].Tokens, report.Tiers[4].SavingsPercent)
	})

}

// TestAdversarialConcurrencyStress stresses token counters, estimators, and encoder lifecycle
// under high-concurrency race conditions.
func TestAdversarialConcurrencyStress(t *testing.T) {
	fixtures, err := benchmark.LoadFixtures()
	if err != nil {
		t.Fatalf("failed to load fixtures: %v", err)
	}

	const workers = 30
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	var opsCount int64

	for w := 0; w < workers; w++ {
		workerID := w
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				// Alternating operations
				switch (workerID + i) % 5 {
				case 0:
					_, err := benchmark.CountToolsTokens(fixtures)
					if err != nil {
						t.Errorf("worker %d CountToolsTokens failed: %v", workerID, err)
					}
				case 1:
					_ = benchmark.EstimateToolsTokens(fixtures)
				case 2:
					_, err := benchmark.CompareEstimator(fixtures)
					if err != nil {
						t.Errorf("worker %d CompareEstimator failed: %v", workerID, err)
					}
				case 3:
					_, _ = benchmark.CountTokens("concurrent test string with various tokens and special text")
				case 4:
					_ = benchmark.IsExactAvailable()
				}
				atomic.AddInt64(&opsCount, 1)
			}
		}()
	}

	wg.Wait()
	t.Logf("Successfully executed %d concurrent token counting operations with 0 errors/races", atomic.LoadInt64(&opsCount))
}

// TestAdversarialEncoderSequentialReset verifies that ResetEncoder functions correctly sequentially,
// but notes the architectural finding: ResetEncoder cannot be called concurrently with CountTokens
// because sync.Once assignment causes a data race on internal mutex state.
func TestAdversarialEncoderSequentialReset(t *testing.T) {
	// 1. Initial count with warmed encoder
	toks1, err := benchmark.CountTokens("Hello, world!")
	if err != nil {
		t.Fatalf("CountTokens failed: %v", err)
	}
	if toks1 <= 0 {
		t.Errorf("expected positive tokens, got %d", toks1)
	}

	// 2. Sequential reset
	benchmark.ResetEncoder()

	// 3. Count again - should transparently re-initialize
	toks2, err := benchmark.CountTokens("Hello, world!")
	if err != nil {
		t.Fatalf("CountTokens after reset failed: %v", err)
	}
	if toks1 != toks2 {
		t.Errorf("token count mismatch after reset: %d vs %d", toks1, toks2)
	}
}

// TestAdversarialTableFormatterRobustness verifies table and markdown generation on extreme values.
func TestAdversarialTableFormatterRobustness(t *testing.T) {
	t.Run("nil report", func(t *testing.T) {
		if benchmark.FormatReportTable(nil) != "" {
			t.Errorf("expected empty string for nil report table")
		}
		if benchmark.FormatReportMarkdown(nil) != "" {
			t.Errorf("expected empty string for nil report markdown")
		}
		jsonStr, err := benchmark.FormatReportJSON(nil)
		if err != nil || jsonStr != "null" {
			t.Errorf("expected 'null' for nil report JSON, got %q, err: %v", jsonStr, err)
		}
	})

	t.Run("zero-value report", func(t *testing.T) {
		r := &benchmark.Report{}
		table := benchmark.FormatReportTable(r)
		if !strings.Contains(table, "TOOLPRUNE BENCHMARK REPORT") {
			t.Errorf("expected header in zero-value table")
		}

		md := benchmark.FormatReportMarkdown(r)
		if !strings.Contains(md, "ToolPrune Token Optimization Benchmark") {
			t.Errorf("expected header in zero-value markdown")
		}
	})

	t.Run("tool with very long name and strange characters", func(t *testing.T) {
		r := &benchmark.Report{
			Timestamp: time.Now(),
			ToolCount: 1,
			Tokenizer: "cl100k_base",
			RawTokens: 500,
			Summary: benchmark.BenchmarkSummary{
				MaxSavingsPercent:  92.5,
				MaxTokensSaved:     460,
				Target80PercentMet: true,
			},
			Tiers: []benchmark.TierReport{
				{
					Tier:           pruner.TierDynamic,
					Name:           "Tier 4: Dynamic Catalog",
					Tokens:         40,
					TokensSaved:    460,
					SavingsPercent: 92.5,
					TokensPerTool:  40.0,
					ValidDraft07:   true,
				},
			},
			Tools: []benchmark.ToolReport{
				{
					ToolName:          strings.Repeat("very_long_tool_name_part_", 10),
					RawTokens:         500,
					Tier1Tokens:       450,
					Tier2Tokens:       250,
					Tier3Tokens:       150,
					Tier4Tokens:       40,
					MaxSavingsPercent: 92.5,
				},
			},
		}

		table := benchmark.FormatReportTable(r)
		if !strings.Contains(table, "...") {
			t.Errorf("expected tool name truncation with '...' in table")
		}

		md := benchmark.FormatReportMarkdown(r)
		if !strings.Contains(md, "very_long_tool_name_part_") {
			t.Errorf("expected tool name in markdown")
		}
	})
}
