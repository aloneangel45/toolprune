package pruner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

	rpNeg := NewResponsePruner(ResponsePrunerOptions{MaxResponseTokens: -50})
	out3, metrics3 := rpNeg.PruneCallResult(*mcp.NewToolTextResult(strings.Repeat("y", 1000)))
	if metrics3.Modified || len(out3.Content[0].Text) != 1000 {
		t.Fatalf("negative MaxResponseTokens must disable pruning cleanly")
	}
}

func TestResponsePruner_UTF8MultiByteRuneSafetyAndTinyBudgets(t *testing.T) {
	// 40 4-byte emojis = 160 bytes (~36 tokens), previously bypassed truncation due to rune-vs-byte mismatch!
	emojiPayload := strings.Repeat("🚀🔥🎯💎", 10)
	// CJK + Turkish multi-byte string
	multiBytePayload := "Başlangıç: " + strings.Repeat("日本語テストデータğüşiöç", 40) + " :Bitiş"

	for _, budget := range []int{20, 10, 5} {
		rp := NewResponsePruner(ResponsePrunerOptions{
			MaxResponseTokens: budget,
			PreserveErrors:    true,
			ArrayHeadTail:     3,
			DeduplicateLogs:   true,
		})

		for _, payload := range []string{emojiPayload, multiBytePayload} {
			pruned, m := rp.PruneCallResult(*mcp.NewToolTextResult(payload))
			text := pruned.Content[0].Text
			if !utf8.ValidString(text) {
				t.Fatalf("budget=%d produced invalid UTF-8 bytes!", budget)
			}
			if !m.Modified {
				t.Errorf("budget=%d: expected multi-byte payload (raw=%d tok) to be pruned", budget, m.RawTokens)
			}
			if m.PrunedTokens > budget {
				t.Errorf("budget=%d: pruned tokens %d exceeded hard budget (text=%q)", budget, m.PrunedTokens, text)
			}
		}
	}

	// Multi-slot response with tiny budget must not blow past totalBudget
	rpMulti := NewResponsePruner(ResponsePrunerOptions{MaxResponseTokens: 20})
	multiRes := mcp.CallToolResult{
		Content: []mcp.ContentItem{
			{Type: "text", Text: strings.Repeat("alpha ", 60)},
			{Type: "text", Text: strings.Repeat("beta ", 60)},
			{Type: "text", Text: strings.Repeat("gamma ", 60)},
		},
	}
	prunedMulti, mMulti := rpMulti.PruneCallResult(multiRes)
	if !mMulti.Modified || mMulti.PrunedTokens > 20 {
		t.Errorf("multi-slot tiny budget violated: modified=%t pruned=%d (want <= 20)", mMulti.Modified, mMulti.PrunedTokens)
	}
	for i, item := range prunedMulti.Content {
		if !utf8.ValidString(item.Text) {
			t.Errorf("slot %d produced invalid UTF-8", i)
		}
	}
}

func TestResponsePruner_NestedJSONObjectArrays(t *testing.T) {
	items := make([]map[string]any, 400)
	for i := range items {
		items[i] = map[string]any{
			"id":     i + 1,
			"status": "open",
			"detail": fmt.Sprintf("Nested item payload #%d with contextual description", i+1),
		}
	}
	nested := map[string]any{
		"ok": true,
		"data": map[string]any{
			"total": len(items),
			"items": items,
		},
	}
	raw, err := json.Marshal(nested)
	if err != nil {
		t.Fatal(err)
	}

	rp := NewResponsePruner(ResponsePrunerOptions{
		MaxResponseTokens: 350,
		PreserveErrors:    true,
		ArrayHeadTail:     3,
		DeduplicateLogs:   true,
	})

	pruned, m := rp.PruneCallResult(*mcp.NewToolTextResult(string(raw)))
	if !m.Modified {
		t.Fatalf("expected nested JSON object array to be pruned")
	}
	if m.PrunedTokens > 350 {
		t.Errorf("expected pruned tokens <= 350, got %d", m.PrunedTokens)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(pruned.Content[0].Text), &decoded); err != nil {
		t.Fatalf("nested JSON object must remain valid JSON after pruning: %v\npayload=%s", err, pruned.Content[0].Text)
	}
	dataObj, ok := decoded["data"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested 'data' object to be preserved, got %+v", decoded)
	}
	arr, ok := dataObj["items"].([]any)
	if !ok || len(arr) >= 20 {
		t.Fatalf("expected compacted nested 'items' array, got len=%d", len(arr))
	}
	if !strings.Contains(pruned.Content[0].Text, "items omitted by ToolPrune") {
		t.Errorf("expected omission summary marker inside nested array, got: %s", pruned.Content[0].Text)
	}
}
