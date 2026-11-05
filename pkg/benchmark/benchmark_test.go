package benchmark_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
)

func TestTokenCounting(t *testing.T) {
	t.Run("empty string returns zero tokens", func(t *testing.T) {
		toks, err := benchmark.CountTokens("")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if toks != 0 {
			t.Errorf("expected 0 tokens, got %d", toks)
		}

		est := benchmark.EstimateTokens("")
		if est != 0 {
			t.Errorf("expected 0 estimated tokens, got %d", est)
		}
	})

	t.Run("basic sentence counting", func(t *testing.T) {
		text := "Hello, world! This is a test of the ToolPrune token counter."
		toks, err := benchmark.CountTokens(text)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if toks <= 0 {
			t.Errorf("expected positive token count, got %d", toks)
		}

		est := benchmark.EstimateTokens(text)
		if est <= 0 {
			t.Errorf("expected positive estimated tokens, got %d", est)
		}
	})

	t.Run("compare estimator on tool schemas", func(t *testing.T) {
		fixtures, err := benchmark.LoadFixtures()
		if err != nil {
			t.Fatalf("failed to load fixtures: %v", err)
		}

		comp, err := benchmark.CompareEstimator(fixtures)
		if err != nil {
			t.Fatalf("unexpected comparison error: %v", err)
		}

		if comp.CompactByteCount <= 0 {
			t.Errorf("expected positive compact byte count, got %d", comp.CompactByteCount)
		}
		if comp.AccuracyPercent <= 0.0 {
			t.Errorf("expected positive accuracy, got %f", comp.AccuracyPercent)
		}
		t.Logf("Estimator Comparison: Exact=%d, Estimated=%d, Delta=%d, Accuracy=%.2f%%",
			comp.ExactTokens, comp.EstimatedTokens, comp.AbsoluteDelta, comp.AccuracyPercent)
	})
}

func TestRunBenchmark(t *testing.T) {
	fixtures, err := benchmark.LoadFixtures()
	if err != nil {
		t.Fatalf("failed to load fixtures: %v", err)
	}
	if len(fixtures) != 4 {
		t.Fatalf("expected 4 fixtures, got %d", len(fixtures))
	}

	report, err := benchmark.RunBenchmark(fixtures)
	if err != nil {
		t.Fatalf("RunBenchmark failed: %v", err)
	}

	if report.ToolCount != 4 {
		t.Errorf("expected ToolCount 4, got %d", report.ToolCount)
	}
	if len(report.Tiers) != 5 {
		t.Fatalf("expected 5 tier reports, got %d", len(report.Tiers))
	}
	if len(report.Tools) != 4 {
		t.Fatalf("expected 4 tool reports, got %d", len(report.Tools))
	}

	// Verify all tiers pass Draft-07 validation
	for _, tr := range report.Tiers {
		if !tr.ValidDraft07 {
			t.Errorf("tier %s failed Draft-07 validation: %v", tr.Name, tr.ValidationErrors)
		}
		t.Logf("[%s] Tokens: %d | Savings: %.1f%% | Tok/Tool: %.1f",
			tr.Name, tr.Tokens, tr.SavingsPercent, tr.TokensPerTool)
	}

	// Verify progressive savings: Tier 0 <= Tier 1 <= Tier 2 <= Tier 3 <= Tier 4
	t0 := report.Tiers[0].Tokens
	t1 := report.Tiers[1].Tokens
	t2 := report.Tiers[2].Tokens
	t3 := report.Tiers[3].Tokens
	t4 := report.Tiers[4].Tokens

	if t1 > t0 {
		t.Errorf("Tier 1 tokens (%d) should be <= Tier 0 tokens (%d)", t1, t0)
	}
	if t2 > t1 {
		t.Errorf("Tier 2 tokens (%d) should be <= Tier 1 tokens (%d)", t2, t1)
	}
	if t3 > t2 {
		t.Errorf("Tier 3 tokens (%d) should be <= Tier 2 tokens (%d)", t3, t2)
	}
	if t4 > t3 {
		t.Errorf("Tier 4 tokens (%d) should be <= Tier 3 tokens (%d)", t4, t3)
	}

	// Verify 80% target achievement on Tier 4
	if report.Tiers[4].SavingsPercent < 80.0 {
		t.Errorf("Tier 4 savings %.1f%% does not meet the 80%% target requirement", report.Tiers[4].SavingsPercent)
	}
	if !report.Summary.Target80PercentMet {
		t.Errorf("expected Target80PercentMet to be true")
	}

	// Verify formatting
	tableOut := benchmark.FormatReportTable(report)
	if !strings.Contains(tableOut, "TOOLPRUNE BENCHMARK REPORT") {
		t.Errorf("table output missing header title")
	}
	if !strings.Contains(tableOut, "ACHIEVED (≥80%)") {
		t.Errorf("table output missing target achieved badge")
	}

	mdOut := benchmark.FormatReportMarkdown(report)
	if !strings.Contains(mdOut, "### ToolPrune Token Optimization Benchmark") {
		t.Errorf("markdown output missing header")
	}

	jsonOut, err := benchmark.FormatReportJSON(report)
	if err != nil {
		t.Fatalf("JSON format error: %v", err)
	}
	var unmarshaled benchmark.Report
	if err := json.Unmarshal([]byte(jsonOut), &unmarshaled); err != nil {
		t.Fatalf("failed to unmarshal JSON report output: %v", err)
	}
}

func TestConcurrentCounting(t *testing.T) {
	fixtures, err := benchmark.LoadFixtures()
	if err != nil {
		t.Fatalf("failed to load fixtures: %v", err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := benchmark.CountToolsTokens(fixtures)
			if err != nil {
				t.Errorf("concurrent CountToolsTokens error: %v", err)
			}
			_ = benchmark.EstimateToolsTokens(fixtures)
		}()
	}

	wg.Wait()
}
