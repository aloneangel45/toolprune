package benchmark

import (
	"encoding/json"
	"fmt"
	"strings"
)

// FormatReportTable formats the benchmark report into an attractive ASCII table.
func FormatReportTable(r *Report) string {
	if r == nil {
		return ""
	}

	var sb strings.Builder

	// Header Box
	sb.WriteString("┌────────────────────────────────────────────────────────────────────────────────────────┐\n")
	sb.WriteString("│                                TOOLPRUNE BENCHMARK REPORT                              │\n")
	sb.WriteString(fmt.Sprintf("│ Tools: %-4d | Tokenizer: %-15s | Timestamp: %-26s │\n",
		r.ToolCount, r.Tokenizer, r.Timestamp.Format("2006-01-02 15:04:05 UTC")))
	sb.WriteString("├──────┬─────────────────────────┬─────────┬──────────┬──────────┬───────────────────────┤\n")
	sb.WriteString("│ TIER │ PRUNING STRATEGY        │  TOKENS │  SAVINGS │ TOK/TOOL │ DRAFT-07 COMPLIANCE   │\n")
	sb.WriteString("├──────┼─────────────────────────┼─────────┼──────────┼──────────┼───────────────────────┤\n")

	for _, t := range r.Tiers {
		compliance := "PASS"
		if !t.ValidDraft07 {
			compliance = "FAIL"
		}
		sb.WriteString(fmt.Sprintf("│  %-3s │ %-23s │ %7d │ %7.1f%% │ %8.1f │ %-21s │\n",
			tierNumber(t.Tier), t.Name, t.Tokens, t.SavingsPercent, t.TokensPerTool, compliance))
	}

	sb.WriteString("├──────┴─────────────────────────┴─────────┴──────────┴──────────┴───────────────────────┤\n")
	targetStatus := "FAIL"
	if r.Summary.Target80PercentMet {
		targetStatus = "ACHIEVED (≥80%)"
	}
	sb.WriteString(fmt.Sprintf("│ TARGET 80%% REDUCTION: %-20s | MAX SAVINGS: %5.1f%% (%d tokens)       │\n",
		targetStatus, r.Summary.MaxSavingsPercent, r.Summary.MaxTokensSaved))
	sb.WriteString("└────────────────────────────────────────────────────────────────────────────────────────┘\n\n")

	// Per-Tool Breakdown Table
	if len(r.Tools) > 0 {
		sb.WriteString("┌──────────────────────────────────┬──────┬────────┬────────┬────────┬────────┬──────────┐\n")
		sb.WriteString("│ TOOL NAME                        │  RAW │ TIER 1 │ TIER 2 │ TIER 3 │ TIER 4 │  SAVINGS │\n")
		sb.WriteString("├──────────────────────────────────┼──────┼────────┼────────┼────────┼────────┼──────────┤\n")

		for _, tool := range r.Tools {
			name := tool.ToolName
			if len(name) > 32 {
				name = name[:29] + "..."
			}
			sb.WriteString(fmt.Sprintf("│ %-32s │ %4d │ %6d │ %6d │ %6d │ %6d │ %7.1f%% │\n",
				name, tool.RawTokens, tool.Tier1Tokens, tool.Tier2Tokens, tool.Tier3Tokens, tool.Tier4Tokens, tool.MaxSavingsPercent))
		}

		sb.WriteString("└──────────────────────────────────┴──────┴────────┴────────┴────────┴────────┴──────────┘\n")
	}

	return sb.String()
}

// FormatReportMarkdown formats the benchmark report into GitHub-flavored Markdown.
func FormatReportMarkdown(r *Report) string {
	if r == nil {
		return ""
	}

	var sb strings.Builder

	sb.WriteString("### ToolPrune Token Optimization Benchmark\n\n")
	sb.WriteString(fmt.Sprintf("- **Tools Evaluated**: %d\n", r.ToolCount))
	sb.WriteString(fmt.Sprintf("- **Tokenizer**: `%s`\n", r.Tokenizer))
	sb.WriteString(fmt.Sprintf("- **Max Reduction**: **%.1f%%** (%d tokens saved)\n", r.Summary.MaxSavingsPercent, r.Summary.MaxTokensSaved))
	sb.WriteString(fmt.Sprintf("- **80%% Target Met**: %t\n\n", r.Summary.Target80PercentMet))

	sb.WriteString("| Tier | Pruning Strategy | Tokens | Bytes | Savings (%) | Tokens / Tool | Draft-07 Status |\n")
	sb.WriteString("|:----:|:-----------------|-------:|------:|------------:|--------------:|:---------------:|\n")

	for _, t := range r.Tiers {
		compliance := "✅ PASS"
		if !t.ValidDraft07 {
			compliance = "❌ FAIL"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %.1f%% | %.1f | %s |\n",
			tierNumber(t.Tier), t.Name, t.Tokens, t.Bytes, t.SavingsPercent, t.TokensPerTool, compliance))
	}

	if len(r.Tools) > 0 {
		sb.WriteString("\n#### Per-Tool Token Breakdown\n\n")
		sb.WriteString("| Tool Name | Raw | Tier 1 (Lossless) | Tier 2 (Compact) | Tier 3 (Aggressive) | Tier 4 (Catalog) | Max Savings |\n")
		sb.WriteString("|:---|---:|---:|---:|---:|---:|---:|\n")
		for _, tool := range r.Tools {
			sb.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %d | %d | **%.1f%%** |\n",
				tool.ToolName, tool.RawTokens, tool.Tier1Tokens, tool.Tier2Tokens, tool.Tier3Tokens, tool.Tier4Tokens, tool.MaxSavingsPercent))
		}
	}

	return sb.String()
}

// FormatReportJSON serializes the benchmark report to formatted JSON.
func FormatReportJSON(r *Report) (string, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tierNumber(tier any) string {
	switch fmt.Sprintf("%v", tier) {
	case "none":
		return "0"
	case "lossless":
		return "1"
	case "compact":
		return "2"
	case "aggressive":
		return "3"
	case "dynamic":
		return "4"
	default:
		return "-"
	}
}
