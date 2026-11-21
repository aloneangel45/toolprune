package pruner

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// ResponsePrunerOptions configures deterministic tools/call output pruning.
type ResponsePrunerOptions struct {
	MaxResponseTokens int  // Hard token ceiling per tools/call response (0 = disabled)
	PreserveErrors    bool // Keep isError: true error payloads intact unless grossly over budget (default: true)
	ArrayHeadTail     int  // Number of leading and trailing items preserved when trimming JSON arrays (default: 5)
	DeduplicateLogs   bool // Collapse consecutive identical or near-identical lines as "(repeated Nx)" (default: true)
}

// DefaultResponsePrunerOptions returns safe production defaults for response pruning.
func DefaultResponsePrunerOptions() ResponsePrunerOptions {
	return ResponsePrunerOptions{
		MaxResponseTokens: 0,
		PreserveErrors:    true,
		ArrayHeadTail:     5,
		DeduplicateLogs:   true,
	}
}

// ResponsePruner deterministically trims oversized MCP tools/call payloads
// while preserving JSON array structure, error messages, and log signal.
type ResponsePruner struct {
	opts ResponsePrunerOptions
}

// NewResponsePruner instantiates a deterministic ResponsePruner.
func NewResponsePruner(opts ResponsePrunerOptions) *ResponsePruner {
	if opts.ArrayHeadTail <= 0 {
		opts.ArrayHeadTail = 5
	}
	return &ResponsePruner{opts: opts}
}

// EstimateTextTokens estimates BPE token count for arbitrary text/JSON content
// using the calibrated 4.45 bytes/token factor.
func EstimateTextTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	t := int(math.Round(float64(len(s)) / 4.45))
	if t < 1 {
		return 1
	}
	return t
}

func estimateCallResultTokens(res mcp.CallToolResult) int {
	total := 0
	for _, item := range res.Content {
		if item.Text != "" {
			total += EstimateTextTokens(item.Text)
		}
		if item.Resource != nil && item.Resource.Text != "" {
			total += EstimateTextTokens(item.Resource.Text)
		}
	}
	return total
}

// PruneCallResult enforces the configured MaxResponseTokens budget on a CallToolResult.
func (rp *ResponsePruner) PruneCallResult(res mcp.CallToolResult) (mcp.CallToolResult, proxy.ResponsePruneMetrics) {
	rawTokens := estimateCallResultTokens(res)
	if rp == nil || rp.opts.MaxResponseTokens <= 0 || len(res.Content) == 0 {
		return res, proxy.ResponsePruneMetrics{
			RawTokens:    rawTokens,
			PrunedTokens: rawTokens,
			Modified:     false,
		}
	}

	budget := rp.opts.MaxResponseTokens
	if rawTokens <= budget {
		return res, proxy.ResponsePruneMetrics{
			RawTokens:    rawTokens,
			PrunedTokens: rawTokens,
			Modified:     false,
		}
	}

	// Clone content slice to avoid mutating caller memory
	out := mcp.CallToolResult{
		IsError: res.IsError,
		Content: make([]mcp.ContentItem, len(res.Content)),
	}
	for i, item := range res.Content {
		cp := item
		if item.Resource != nil {
			resCp := *item.Resource
			cp.Resource = &resCp
		}
		out.Content[i] = cp
	}

	// Preserve isError: true messages intact unless they massively exceed budget (>2x).
	if out.IsError && rp.opts.PreserveErrors {
		hardErrorCap := budget * 2
		if hardErrorCap < 256 {
			hardErrorCap = 256
		}
		if rawTokens <= hardErrorCap {
			return out, proxy.ResponsePruneMetrics{
				RawTokens:    rawTokens,
				PrunedTokens: rawTokens,
				Modified:     false,
			}
		}
		// Keep generous error head+tail preserving the primary error header and exit context
		errBudget := budget
		if errBudget < 128 {
			errBudget = 128
		}
		modified := rp.pruneContentItems(out.Content, errBudget, true)
		prunedTokens := estimateCallResultTokens(out)
		return out, proxy.ResponsePruneMetrics{
			RawTokens:    rawTokens,
			PrunedTokens: prunedTokens,
			Modified:     modified,
		}
	}

	modified := rp.pruneContentItems(out.Content, budget, false)
	prunedTokens := estimateCallResultTokens(out)
	return out, proxy.ResponsePruneMetrics{
		RawTokens:    rawTokens,
		PrunedTokens: prunedTokens,
		Modified:     modified,
	}
}

func (rp *ResponsePruner) pruneContentItems(items []mcp.ContentItem, totalBudget int, isError bool) bool {
	// Identify prunable text slots and compute total raw tokens across them.
	type slot struct {
		idx       int
		isResText bool
		rawTokens int
	}
	var slots []slot
	totalRaw := 0
	for i := range items {
		if items[i].Text != "" {
			tok := EstimateTextTokens(items[i].Text)
			slots = append(slots, slot{idx: i, rawTokens: tok})
			totalRaw += tok
		}
		if items[i].Resource != nil && items[i].Resource.Text != "" {
			tok := EstimateTextTokens(items[i].Resource.Text)
			slots = append(slots, slot{idx: i, isResText: true, rawTokens: tok})
			totalRaw += tok
		}
	}
	if len(slots) == 0 || totalRaw <= totalBudget {
		return false
	}

	minSlot := totalBudget / len(slots)
	if minSlot > 16 {
		minSlot = 16
	}
	if minSlot < 1 {
		minSlot = 1
	}

	modified := false
	for _, s := range slots {
		// Allocate budget proportionally to raw size without letting a per-slot floor exceed totalBudget
		slotBudget := int(math.Floor(float64(totalBudget) * float64(s.rawTokens) / float64(totalRaw)))
		if slotBudget < minSlot {
			slotBudget = minSlot
		}
		if slotBudget > totalBudget {
			slotBudget = totalBudget
		}
		if s.rawTokens <= slotBudget {
			continue
		}

		var orig string
		if s.isResText {
			orig = items[s.idx].Resource.Text
		} else {
			orig = items[s.idx].Text
		}

		pruned := rp.pruneTextPayload(orig, slotBudget, isError)
		if pruned != orig {
			modified = true
			if s.isResText {
				items[s.idx].Resource.Text = pruned
			} else {
				items[s.idx].Text = pruned
			}
		}
	}
	return modified
}

func (rp *ResponsePruner) pruneTextPayload(text string, maxTokens int, isError bool) string {
	if EstimateTextTokens(text) <= maxTokens {
		return text
	}

	trimmed := strings.TrimSpace(text)

	// 1. JSON array content: preserve first K and last K elements with deterministic omission marker
	if !isError && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		if compacted, ok := rp.pruneJSONArray(trimmed, maxTokens); ok {
			return compacted
		}
	}

	// 2. JSON object with large array fields: compact embedded arrays first
	if !isError && strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		if compacted, ok := rp.pruneJSONObjectArrays(trimmed, maxTokens); ok {
			return compacted
		}
	}

	// 3. Multi-line text / logs / stack traces: deduplicate consecutive identical or near-identical lines
	working := text
	if rp.opts.DeduplicateLogs && strings.Contains(working, "\n") {
		working = DeduplicateConsecutiveLines(working)
		if EstimateTextTokens(working) <= maxTokens {
			return working
		}
	}

	// 4. Deterministic head + tail truncation staying within the token ceiling
	return truncateHeadTail(working, maxTokens, isError)
}

func (rp *ResponsePruner) pruneJSONArray(raw string, maxTokens int) (string, bool) {
	var arr []any
	if err := json.Unmarshal([]byte(raw), &arr); err != nil {
		return "", false
	}
	if len(arr) == 0 {
		return raw, true
	}

	kStart := rp.opts.ArrayHeadTail
	if kStart <= 0 {
		kStart = 5
	}

	// Try shrinking K from configured ArrayHeadTail down to 1 so output stays valid JSON within budget.
	for k := kStart; k >= 1; k-- {
		if len(arr) <= 2*k {
			continue
		}
		omitted := len(arr) - 2*k
		compact := make([]any, 0, 2*k+1)
		compact = append(compact, arr[:k]...)
		compact = append(compact, fmt.Sprintf("[... %d items omitted by ToolPrune ...]", omitted))
		compact = append(compact, arr[len(arr)-k:]...)

		encoded, err := json.Marshal(compact)
		if err != nil {
			return "", false
		}
		outStr := string(encoded)
		if EstimateTextTokens(outStr) <= maxTokens {
			return outStr, true
		}
		if k == 1 {
			// Even K=1 exceeds budget due to huge individual elements; fall back to head+tail text trim on compacted JSON
			return truncateHeadTail(outStr, maxTokens, false), true
		}
	}

	// Array was small in element count (<=2) but elements themselves are huge
	encoded, err := json.Marshal(arr)
	if err != nil {
		return "", false
	}
	return truncateHeadTail(string(encoded), maxTokens, false), true
}

const maxJSONPruneDepth = 32

func hasPrunableJSONArray(v any, depth int) bool {
	if depth > maxJSONPruneDepth {
		return false
	}
	switch val := v.(type) {
	case []any:
		if len(val) > 2 {
			return true
		}
		for _, item := range val {
			if hasPrunableJSONArray(item, depth+1) {
				return true
			}
		}
	case map[string]any:
		for _, sub := range val {
			if hasPrunableJSONArray(sub, depth+1) {
				return true
			}
		}
	}
	return false
}

func compactNestedJSONArrays(v any, k int, depth int) (any, bool) {
	if depth > maxJSONPruneDepth {
		return v, false
	}
	switch val := v.(type) {
	case []any:
		trimmed := false
		if len(val) > 2*k {
			omitted := len(val) - 2*k
			compact := make([]any, 0, 2*k+1)
			for _, item := range val[:k] {
				sub, _ := compactNestedJSONArrays(item, k, depth+1)
				compact = append(compact, sub)
			}
			compact = append(compact, fmt.Sprintf("[... %d items omitted by ToolPrune ...]", omitted))
			for _, item := range val[len(val)-k:] {
				sub, _ := compactNestedJSONArrays(item, k, depth+1)
				compact = append(compact, sub)
			}
			return compact, true
		}
		out := make([]any, len(val))
		for i, item := range val {
			sub, subTrimmed := compactNestedJSONArrays(item, k, depth+1)
			out[i] = sub
			if subTrimmed {
				trimmed = true
			}
		}
		return out, trimmed

	case map[string]any:
		out := make(map[string]any, len(val))
		trimmed := false
		for key, subVal := range val {
			sub, subTrimmed := compactNestedJSONArrays(subVal, k, depth+1)
			out[key] = sub
			if subTrimmed {
				trimmed = true
			}
		}
		return out, trimmed
	default:
		return v, false
	}
}

func (rp *ResponsePruner) pruneJSONObjectArrays(raw string, maxTokens int) (string, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || len(obj) == 0 {
		return "", false
	}

	if !hasPrunableJSONArray(obj, 0) {
		return "", false
	}

	kStart := rp.opts.ArrayHeadTail
	if kStart <= 0 {
		kStart = 5
	}

	lastCompacted := ""
	for k := kStart; k >= 1; k-- {
		compacted, trimmedAny := compactNestedJSONArrays(obj, k, 0)
		if !trimmedAny {
			continue
		}
		encoded, err := json.Marshal(compacted)
		if err != nil {
			return "", false
		}
		outStr := string(encoded)
		lastCompacted = outStr
		if EstimateTextTokens(outStr) <= maxTokens {
			return outStr, true
		}
	}
	if lastCompacted != "" {
		return truncateHeadTail(lastCompacted, maxTokens, false), true
	}
	return "", false
}

// Normalization regexes for near-identical log line deduplication (timestamps, request IDs, hex addresses).
var (
	logTimestampRegex = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b|\b\d{2}:\d{2}:\d{2}(?:\.\d+)?\b`)
	logHexAddrRegex   = regexp.MustCompile(`\b0x[0-9a-fA-F]{4,}\b`)
)

func normalizeLogLineForDedup(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return ""
	}
	s = logTimestampRegex.ReplaceAllString(s, "<TS>")
	s = logHexAddrRegex.ReplaceAllString(s, "<HEX>")
	return s
}

// DeduplicateConsecutiveLines collapses consecutive identical or near-identical lines
// into a single representative line annotated with " (repeated Nx)".
func DeduplicateConsecutiveLines(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < 2 {
		return input
	}

	out := make([]string, 0, len(lines))
	i := 0
	for i < len(lines) {
		norm := normalizeLogLineForDedup(lines[i])
		if norm == "" {
			// Collapse consecutive blank lines quietly
			out = append(out, lines[i])
			i++
			for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
				i++
			}
			continue
		}

		j := i + 1
		for j < len(lines) && normalizeLogLineForDedup(lines[j]) == norm {
			j++
		}
		count := j - i
		if count > 1 {
			out = append(out, fmt.Sprintf("%s (repeated %dx)", lines[i], count))
		} else {
			out = append(out, lines[i])
		}
		i = j
	}
	return strings.Join(out, "\n")
}

// truncateHeadTail performs deterministic, UTF-8 rune-safe head+tail truncation
// keeping ~60% head / ~40% tail (or ~70% head / ~30% tail for errors) strictly within maxTokens.
func truncateHeadTail(text string, maxTokens int, isError bool) string {
	if maxTokens <= 0 {
		return text
	}
	if EstimateTextTokens(text) <= maxTokens {
		return text
	}

	// Exact byte ceiling that keeps EstimateTextTokens <= maxTokens (accounting for math.Round).
	maxBytes := int(math.Floor((float64(maxTokens) + 0.49) * 4.45))
	if maxBytes < 4 {
		maxBytes = 4
	}
	targetBytes := int(math.Floor(float64(maxTokens) * 4.2))
	if targetBytes > maxBytes {
		targetBytes = maxBytes
	}
	if targetBytes < 4 {
		targetBytes = maxBytes
	}

	headRatio := 0.60
	if isError {
		headRatio = 0.70
	}

	marker := "\n[... %d lines omitted by ToolPrune ...]\n"
	if isError {
		marker = "\n[... %d error tail lines omitted by ToolPrune ...]\n"
	}

	lines := strings.Split(text, "\n")
	if len(lines) >= 6 && targetBytes > 100 {
		// Reserve marker budget (~56 bytes)
		usableBytes := targetBytes - 60
		if usableBytes >= 40 {
			headBudget := int(float64(usableBytes) * headRatio)
			tailBudget := usableBytes - headBudget

			var headLines []string
			headUsed := 0
			headIdx := 0
			for headIdx < len(lines) {
				cost := len(lines[headIdx]) + 1
				if len(headLines) > 0 && headUsed+cost > headBudget {
					break
				}
				if len(headLines) == 0 && cost > headBudget {
					break
				}
				headLines = append(headLines, lines[headIdx])
				headUsed += cost
				headIdx++
			}

			var tailLines []string
			tailUsed := 0
			tailIdx := len(lines) - 1
			for tailIdx >= headIdx {
				cost := len(lines[tailIdx]) + 1
				if len(tailLines) > 0 && tailUsed+cost > tailBudget {
					break
				}
				if len(tailLines) == 0 && cost > tailBudget {
					break
				}
				tailLines = append(tailLines, lines[tailIdx])
				tailUsed += cost
				tailIdx--
			}

			// Reverse tailLines back to original order
			for l, r := 0, len(tailLines)-1; l < r; l, r = l+1, r-1 {
				tailLines[l], tailLines[r] = tailLines[r], tailLines[l]
			}

			omitted := (tailIdx - headIdx) + 1
			if len(headLines) > 0 && len(tailLines) > 0 && omitted > 0 {
				candidate := strings.Join(headLines, "\n") + fmt.Sprintf(marker, omitted) + strings.Join(tailLines, "\n")
				if EstimateTextTokens(candidate) <= maxTokens {
					return candidate
				}
			}
		}
	}

	// Single-line, tiny-budget, or multi-byte UTF-8 rune-safe head+tail fallback
	charMarker := " [... omitted by ToolPrune ...] "
	if isError && maxBytes >= 140 {
		charMarker = " [... error tail omitted by ToolPrune ...] "
	} else if maxBytes < 100 {
		charMarker = " [...] "
	}

	availBytes := maxBytes - len(charMarker)
	if availBytes < 8 {
		// Ultra-tiny budget: emit a strict UTF-8 rune-safe prefix + ellipsis within maxBytes.
		ellipsis := "..."
		prefixBudget := maxBytes - len(ellipsis)
		if prefixBudget <= 0 {
			return utf8PrefixBytes(text, maxBytes)
		}
		return utf8PrefixBytes(text, prefixBudget) + ellipsis
	}

	headByteBudget := int(math.Floor(float64(availBytes) * headRatio))
	tailByteBudget := availBytes - headByteBudget

	runes := []rune(text)
	headEnd := 0
	headBytes := 0
	for headEnd < len(runes) {
		rl := utf8.RuneLen(runes[headEnd])
		if rl < 0 {
			rl = 3 // utf8.RuneError replacement bytes
		}
		if headBytes+rl > headByteBudget {
			break
		}
		headBytes += rl
		headEnd++
	}

	tailStart := len(runes)
	tailBytes := 0
	for tailStart > headEnd {
		rl := utf8.RuneLen(runes[tailStart-1])
		if rl < 0 {
			rl = 3
		}
		if tailBytes+rl > tailByteBudget {
			break
		}
		tailBytes += rl
		tailStart--
	}

	// Guarantee strict compliance with EstimateTextTokens <= maxTokens even on boundary rounding
	res := string(runes[:headEnd]) + charMarker + string(runes[tailStart:])
	for EstimateTextTokens(res) > maxTokens && (headEnd > 0 || tailStart < len(runes)) {
		if tailStart < len(runes) && (headEnd <= 1 || tailBytes >= headBytes) {
			rl := utf8.RuneLen(runes[tailStart])
			if rl < 0 {
				rl = 3
			}
			tailBytes -= rl
			tailStart++
		} else if headEnd > 0 {
			headEnd--
			rl := utf8.RuneLen(runes[headEnd])
			if rl < 0 {
				rl = 3
			}
			headBytes -= rl
		}
		res = string(runes[:headEnd]) + charMarker + string(runes[tailStart:])
	}
	return res
}

// utf8PrefixBytes returns the longest valid UTF-8 prefix of s whose byte length <= maxBytes.
func utf8PrefixBytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) == 0 {
		return ""
	}
	if len(s) <= maxBytes && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		rl := utf8.RuneLen(r)
		if rl < 0 {
			rl = 3
		}
		if used+rl > maxBytes {
			break
		}
		b.WriteRune(r)
		used += rl
	}
	return b.String()
}
