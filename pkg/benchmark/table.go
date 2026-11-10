package benchmark

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func truncateRunesForTable(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

// FormatReportTable formats the benchmark report into an ASCII table.
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
			tierNumber(t.Tier), truncateRunesForTable(t.Name, 23), t.Tokens, t.SavingsPercent, t.TokensPerTool, compliance))
	}
	if r.BudgetTier != nil {
		compliance := "PASS"
		if !r.BudgetTier.ValidDraft07 {
			compliance = "FAIL"
		}
		sb.WriteString(fmt.Sprintf("│  %-3s │ %-23s │ %7d │ %7.1f%% │ %8.1f │ %-21s │\n",
			"BGT", truncateRunesForTable(r.BudgetTier.Name, 23), r.BudgetTier.Tokens, r.BudgetTier.SavingsPercent, r.BudgetTier.TokensPerTool, compliance))
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
			name := truncateRunesForTable(tool.ToolName, 32)
			sb.WriteString(fmt.Sprintf("│ %-32s │ %4d │ %6d │ %6d │ %6d │ %6d │ %7.1f%% │\n",
				name, tool.RawTokens, tool.Tier1Tokens, tool.Tier2Tokens, tool.Tier3Tokens, tool.Tier4Tokens, tool.MaxSavingsPercent))

			for _, param := range tool.Parameters {
				reqTag := "opt"
				if param.Required {
					reqTag = "req"
				}
				pLabel := truncateRunesForTable(fmt.Sprintf("  .%s (%s)", param.Name, reqTag), 32)
				pSave := calculateSavings(param.RawTokens, param.Tier3Tokens)
				sb.WriteString(fmt.Sprintf("│ %-32s │ %4d │ %6s │ %6s │ %6d │ %6s │ %7.1f%% │\n",
					pLabel, param.RawTokens, "-", "-", param.Tier3Tokens, "-", pSave))
			}
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
		compliance := "PASS"
		if !t.ValidDraft07 {
			compliance = "FAIL"
		}
		sb.WriteString(fmt.Sprintf("| %s | %s | %d | %d | %.1f%% | %.1f | %s |\n",
			tierNumber(t.Tier), t.Name, t.Tokens, t.Bytes, t.SavingsPercent, t.TokensPerTool, compliance))
	}
	if r.BudgetTier != nil {
		compliance := "PASS"
		if !r.BudgetTier.ValidDraft07 {
			compliance = "FAIL"
		}
		sb.WriteString(fmt.Sprintf("| BGT | %s | %d | %d | %.1f%% | %.1f | %s |\n",
			r.BudgetTier.Name, r.BudgetTier.Tokens, r.BudgetTier.Bytes, r.BudgetTier.SavingsPercent, r.BudgetTier.TokensPerTool, compliance))
	}

	if len(r.Tools) > 0 {
		sb.WriteString("\n#### Per-Tool Token Breakdown\n\n")
		sb.WriteString("| Tool Name | Raw | Tier 1 (Lossless) | Tier 2 (Compact) | Tier 3 (Aggressive) | Tier 4 (Catalog) | Max Savings |\n")
		sb.WriteString("|:---|---:|---:|---:|---:|---:|---:|\n")
		for _, tool := range r.Tools {
			sb.WriteString(fmt.Sprintf("| `%s` | %d | %d | %d | %d | %d | **%.1f%%** |\n",
				tool.ToolName, tool.RawTokens, tool.Tier1Tokens, tool.Tier2Tokens, tool.Tier3Tokens, tool.Tier4Tokens, tool.MaxSavingsPercent))
			for _, param := range tool.Parameters {
				reqTag := "optional"
				if param.Required {
					reqTag = "required"
				}
				pSave := calculateSavings(param.RawTokens, param.Tier3Tokens)
				sb.WriteString(fmt.Sprintf("| `  .%s` (%s) | %d | - | - | %d | - | %.1f%% |\n",
					param.Name, reqTag, param.RawTokens, param.Tier3Tokens, pSave))
			}
		}
	}

	return sb.String()
}

// FormatReportCSV serializes the benchmark report into RFC 4180 CSV format.
func FormatReportCSV(r *Report) (string, error) {
	if r == nil {
		return "", nil
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)

	header := []string{
		"record_type", "name", "tier", "tokens", "bytes", "tokens_saved",
		"savings_percent", "tokens_per_tool", "valid_draft07",
		"raw_tokens", "tier1_tokens", "tier2_tokens", "tier3_tokens", "tier4_tokens",
	}
	if err := w.Write(header); err != nil {
		return "", err
	}

	writeTier := func(tr TierReport) error {
		return w.Write([]string{
			"tier",
			tr.Name,
			string(tr.Tier),
			strconv.Itoa(tr.Tokens),
			strconv.Itoa(tr.Bytes),
			strconv.Itoa(tr.TokensSaved),
			fmt.Sprintf("%.2f", tr.SavingsPercent),
			fmt.Sprintf("%.2f", tr.TokensPerTool),
			strconv.FormatBool(tr.ValidDraft07),
			"", "", "", "", "",
		})
	}

	for _, t := range r.Tiers {
		if err := writeTier(t); err != nil {
			return "", err
		}
	}
	if r.BudgetTier != nil {
		if err := writeTier(*r.BudgetTier); err != nil {
			return "", err
		}
	}

	for _, tool := range r.Tools {
		row := []string{
			"tool",
			tool.ToolName,
			"",
			"", "", "",
			fmt.Sprintf("%.2f", tool.MaxSavingsPercent),
			"", "",
			strconv.Itoa(tool.RawTokens),
			strconv.Itoa(tool.Tier1Tokens),
			strconv.Itoa(tool.Tier2Tokens),
			strconv.Itoa(tool.Tier3Tokens),
			strconv.Itoa(tool.Tier4Tokens),
		}
		if err := w.Write(row); err != nil {
			return "", err
		}
		for _, param := range tool.Parameters {
			pSave := calculateSavings(param.RawTokens, param.Tier3Tokens)
			pRow := []string{
				"parameter",
				tool.ToolName + "." + param.Name,
				"",
				"", "", "",
				fmt.Sprintf("%.2f", pSave),
				"", "",
				strconv.Itoa(param.RawTokens),
				"", "",
				strconv.Itoa(param.Tier3Tokens),
				"",
			}
			if err := w.Write(pRow); err != nil {
				return "", err
			}
		}
	}

	w.Flush()
	if err := w.Error(); err != nil {
		return "", err
	}
	return buf.String(), nil
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
