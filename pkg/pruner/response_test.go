package pruner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

func TestResponsePruner_JSONArrayHeadTail(t *testing.T) {
	items := make([]map[string]any, 100)
	for i := range items {
		items[i] = map[string]any{
			"id":      i + 1,
			"title":   fmt.Sprintf("GitHub Issue #%d: investigate context tax latency regression", i+1),
			"author":  "octocat",
			"summary": "Detailed benchmark telemetry for MCP proxy streaming.",
		}
	}
	rawJSON, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}

	rp := NewResponsePruner(ResponsePrunerOptions{
		MaxResponseTokens: 450,
		PreserveErrors:    true,
		ArrayHeadTail:     3,
		DeduplicateLogs:   true,
	})

	res := mcp.CallToolResult{
		Content: []mcp.ContentItem{{Type: "text", Text: string(rawJSON)}},
	}

	pruned, metrics := rp.PruneCallResult(res)
	if !metrics.Modified {
		t.Fatalf("expected JSON array response to be pruned")
	}
	if metrics.PrunedTokens > 450 {
		t.Errorf("expected pruned tokens <= 450, got %d (raw=%d)", metrics.PrunedTokens, metrics.RawTokens)
	}
	if metrics.PrunedTokens >= metrics.RawTokens {
		t.Errorf("expected token reduction, got raw=%d pruned=%d", metrics.RawTokens, metrics.PrunedTokens)
	}

	var decoded []any
	if err := json.Unmarshal([]byte(pruned.Content[0].Text), &decoded); err != nil {
		t.Fatalf("pruned JSON array must remain valid JSON: %v\npayload=%s", err, pruned.Content[0].Text)
	}
	// Expect 3 head + 1 marker + 3 tail = 7 elements (or smaller K if budget tight)
	if len(decoded) < 3 {
		t.Fatalf("expected head + marker + tail elements, got %d", len(decoded))
	}
	markerFound := false
	for _, elem := range decoded {
		if s, ok := elem.(string); ok && strings.Contains(s, "items omitted by ToolPrune") {
			markerFound = true
		}
	}
	if !markerFound {
		t.Errorf("expected ToolPrune omission summary marker in pruned JSON array, got: %s", pruned.Content[0].Text)
	}
}

func TestResponsePruner_DeduplicateLogsAndHeadTail(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("2026-11-21T10:00:00Z INFO starting build pipeline\n")
	for i := 0; i < 40; i++ {
		sb.WriteString(fmt.Sprintf("2026-11-21T10:00:%02dZ WARN retrying upstream connection at 0x%08x\n", i%60, 0xdead0000+i))
	}
	for i := 0; i < 60; i++ {
		sb.WriteString(fmt.Sprintf("step %d: compiling module pkg/service_%d/handler.go with full diagnostics output\n", i, i))
	}
	sb.WriteString("2026-11-21T10:02:00Z INFO build completed successfully\n")

	rp := NewResponsePruner(ResponsePrunerOptions{
		MaxResponseTokens: 250,
		PreserveErrors:    true,
		ArrayHeadTail:     5,
		DeduplicateLogs:   true,
	})

	pruned, metrics := rp.PruneCallResult(*mcp.NewToolTextResult(sb.String()))
	if !metrics.Modified {
		t.Fatalf("expected log response to be pruned")
	}
	text := pruned.Content[0].Text
	if !strings.Contains(text, "(repeated 40x)") {
		t.Errorf("expected consecutive near-identical log lines to collapse with '(repeated 40x)', got:\n%s", text)
	}
	if !strings.Contains(text, "starting build pipeline") {
		t.Errorf("expected head line to be preserved")
	}
	if !strings.Contains(text, "build completed successfully") {
		t.Errorf("expected tail line to be preserved")
	}
	if metrics.PrunedTokens > 250 {
		t.Errorf("expected pruned tokens <= 250, got %d", metrics.PrunedTokens)
	}
}

func TestResponsePruner_PreserveErrors(t *testing.T) {
	errMsg := "ERROR: SQL syntax error near 'SELEC' at line 1, column 1.\nHint: Did you mean SELECT? " + strings.Repeat("context detail ", 30)
	rp := NewResponsePruner(ResponsePrunerOptions{
		MaxResponseTokens: 100,
		PreserveErrors:    true,
		ArrayHeadTail:     5,
		DeduplicateLogs:   true,
	})

	res := mcp.NewToolExecutionErrorResult(errMsg)
	pruned, metrics := rp.PruneCallResult(*res)
	if !pruned.IsError {
		t.Fatalf("expected IsError to stay true")
	}
	// Within 2x budget (<= 256 tokens), error must remain completely untouched
	if metrics.Modified || pruned.Content[0].Text != errMsg {
		t.Errorf("expected moderate isError message to be preserved verbatim when PreserveErrors=true")
	}

	// Massive error payload (>2x budget) trims tail while keeping core error header intact
	hugeErr := "FATAL: database connection refused on port 5432\n" + strings.Repeat("stack frame line at internal/db/pool.go:123\n", 400) + "exit status 1"
	prunedHuge, hugeMetrics := rp.PruneCallResult(*mcp.NewToolExecutionErrorResult(hugeErr))
	if !prunedHuge.IsError || !hugeMetrics.Modified {
		t.Fatalf("expected massive isError payload to be trimmed while keeping IsError=true")
	}
	if !strings.Contains(prunedHuge.Content[0].Text, "FATAL: database connection refused") {
		t.Errorf("expected core error header to be preserved, got:\n%s", prunedHuge.Content[0].Text)
	}
}

func TestResponsePruner_DisabledOrUnderBudgetPassesUnchanged(t *testing.T) {
	small := *mcp.NewToolTextResult("ok")
	rp := NewResponsePruner(ResponsePrunerOptions{MaxResponseTokens: 200})
	out, metrics := rp.PruneCallResult(small)
	if metrics.Modified || out.Content[0].Text != "ok" {
		t.Fatalf("under-budget response must pass unchanged")
	}

	rpDisabled := NewResponsePruner(ResponsePrunerOptions{MaxResponseTokens: 0})
	out2, metrics2 := rpDisabled.PruneCallResult(*mcp.NewToolTextResult(strings.Repeat("x", 5000)))
	if metrics2.Modified || len(out2.Content[0].Text) != 5000 {
		t.Fatalf("disabled response pruner must pass unchanged")
	}
}
