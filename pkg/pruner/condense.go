package pruner

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

var (
	reCodeBlock       = regexp.MustCompile("(?s)```.*?```")
	reHTMLTag         = regexp.MustCompile(`(?i)</?[a-z][a-z0-9]*(?:\s+[^>]*)?/?>`)
	reMarkdownLink    = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reMarkdownFormat  = regexp.MustCompile(`(?:\*\*|__|\*|_|~~)`)
	reMarkdownHeader  = regexp.MustCompile(`(?m)^#{1,6}\s+`)
	reTutorialBullets = regexp.MustCompile(`(?m)^\s*[-*\x{2022}]\s+`)
	reNumberedSteps   = regexp.MustCompile(`(?m)^\s*\d+[\.)]\s+`)
	reInstructionMeta = regexp.MustCompile(`(?i)\b(IMPORTANT|NOTE|WARNING|CAUTION|SELECTION PROCESS|RESPONSE FORMAT)\b\s*:`)
	reMultipleSpaces  = regexp.MustCompile(`[ \t\r\n]+`)

	// Regex format detectors
	reUUIDPattern  = regexp.MustCompile(`(?i)(?:\[[0-9a-fA-F_\\]+\]\{8\}-\[[0-9a-fA-F_\\]+\]\{4\}|\[(?:0-9a-f|a-f0-9)[^\]]*\]\{8\}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4})`)
	reISODateTime  = regexp.MustCompile(`(?i)(?:\\d\{4\}-|\d{4}-|\[0-9\]\{4\}-|-(?:0[1-9]|1[0-2])-|-(?:0\[1-9\]|1\[0-2\])-|T\.\*Z|T\.\*|T\\d\{2\}|T\d{2}:\d{2}|\bUTC\b)`)
	reEmailPattern = regexp.MustCompile(`(?i)@.*(?:\.\w{2,}|\.com|\.org)`)
	reURIPattern   = regexp.MustCompile(`(?i)https?://`)
)

// Known sentence abbreviation suffixes to guard against premature sentence splitting
var commonAbbreviations = []string{
	"e.g.", "i.e.", "etc.", "vs.", "mr.", "mrs.", "dr.", "inc.", "corp.", "co.", "v1.", "v2.",
}

// resolveCondenseOptions extracts CondenseOptions from variable arguments.
func resolveCondenseOptions(opts []any) CondenseOptions {
	if len(opts) == 0 {
		return DefaultCondenseOptions()
	}
	switch v := opts[0].(type) {
	case CondenseOptions:
		return v
	case *CondenseOptions:
		if v != nil {
			return *v
		}
	case PruneOptions:
		return v.Condense
	case *PruneOptions:
		if v != nil {
			return v.Condense
		}
	}
	return DefaultCondenseOptions()
}

// CondenseTool applies Tier 2 condensation to tool descriptions and property schemas.
func CondenseTool(tool mcp.Tool, opts ...any) mcp.Tool {
	opt := resolveCondenseOptions(opts)
	out := tool

	// 1. Condense top-level tool description
	if out.Description != "" {
		out.Description = CondenseDescription(out.Description, opt.MaxToolDescLen, opt)
	}

	// 2. Condense InputSchema description and properties
	out.InputSchema = CondenseInputSchema(out.InputSchema, opt)

	// 3. Condense OutputSchema if present
	if len(out.OutputSchema) > 0 {
		out.OutputSchema = CondenseNode(out.OutputSchema, opt)
	}

	return out
}

// CondenseToolsList applies Tier 2 condensation across an entire slice of tools.
func CondenseToolsList(tools []mcp.Tool, opts ...any) []mcp.Tool {
	opt := resolveCondenseOptions(opts)
	result := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		result[i] = CondenseTool(t, opt)
	}
	return result
}

// CondenseInputSchema condenses description and sub-schemas in an mcp.InputSchema.
func CondenseInputSchema(s mcp.InputSchema, opts CondenseOptions) mcp.InputSchema {
	out := s

	if out.Description != "" {
		out.Description = CondenseDescription(out.Description, opts.MaxToolDescLen, opts)
	}

	if len(out.Properties) > 0 {
		out.Properties = CondensePropertiesMap(out.Properties, opts)
	}

	if len(out.Defs) > 0 {
		out.Defs = CondensePropertiesMap(out.Defs, opts)
	}

	if len(out.Definitions) > 0 {
		out.Definitions = CondensePropertiesMap(out.Definitions, opts)
	}

	if len(out.PatternProperties) > 0 {
		out.PatternProperties = CondensePropertiesMap(out.PatternProperties, opts)
	}

	if len(out.DependentSchemas) > 0 {
		out.DependentSchemas = CondensePropertiesMap(out.DependentSchemas, opts)
	}

	if len(out.AnyOf) > 0 {
		out.AnyOf = condenseNodeSlice(out.AnyOf, opts)
	}

	if len(out.OneOf) > 0 {
		out.OneOf = condenseNodeSlice(out.OneOf, opts)
	}

	if len(out.AllOf) > 0 {
		out.AllOf = condenseNodeSlice(out.AllOf, opts)
	}

	if len(out.Not) > 0 {
		out.Not = CondenseNode(out.Not, opts)
	}

	if addPropsMap, ok := out.AdditionalProperties.(map[string]any); ok {
		out.AdditionalProperties = CondenseNode(addPropsMap, opts)
	}

	return out
}

// CondensePropertiesMap recursively condenses all property schemas in a map.
func CondensePropertiesMap(properties map[string]any, opts CondenseOptions) map[string]any {
	if properties == nil {
		return nil
	}

	condensed := make(map[string]any, len(properties))
	for propName, propVal := range properties {
		if nodeMap, ok := propVal.(map[string]any); ok {
			condensed[propName] = CondenseNode(nodeMap, opts)
		} else {
			condensed[propName] = propVal
		}
	}
	return condensed
}

// CondenseNode condenses a schema node, its description, and its regex pattern.
func CondenseNode(node map[string]any, opts CondenseOptions) map[string]any {
	if node == nil {
		return nil
	}

	out := make(map[string]any, len(node))
	for k, v := range node {
		out[k] = v
	}

	// 1. Condense property description
	if descVal, ok := out["description"].(string); ok && descVal != "" {
		out["description"] = CondenseDescription(descVal, opts.MaxParamDescLen, opts)
	}

	// 2. Squeeze & normalize regular expressions
	if patternVal, ok := out["pattern"].(string); ok && patternVal != "" {
		formatVal, _ := out["format"].(string)

		newPattern, newFormat, changed := NormalizeRegex(patternVal, formatVal, opts)
		if changed {
			if newPattern == "" {
				delete(out, "pattern")
			} else {
				out["pattern"] = newPattern
			}
			if newFormat != "" {
				out["format"] = newFormat
			}
		}
	}

	// 3. Recursive traversal for nested object structures and schema definitions
	for _, mapKey := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if subMap, ok := out[mapKey].(map[string]any); ok {
			out[mapKey] = CondensePropertiesMap(subMap, opts)
		}
	}

	if addPropsMap, ok := out["additionalProperties"].(map[string]any); ok {
		out["additionalProperties"] = CondenseNode(addPropsMap, opts)
	}

	if itemsVal, ok := out["items"].(map[string]any); ok {
		out["items"] = CondenseNode(itemsVal, opts)
	} else if itemsArr, ok := out["items"].([]any); ok {
		out["items"] = condenseNodeSlice(itemsArr, opts)
	}

	for _, unionKey := range []string{"allOf", "anyOf", "oneOf"} {
		if unionArr, ok := out[unionKey].([]any); ok {
			out[unionKey] = condenseNodeSlice(unionArr, opts)
		}
	}

	for _, condKey := range []string{"not", "if", "then", "else"} {
		if condMap, ok := out[condKey].(map[string]any); ok {
			out[condKey] = CondenseNode(condMap, opts)
		}
	}

	return out
}

func condenseNodeSlice(slice []any, opts CondenseOptions) []any {
	out := make([]any, len(slice))
	for i, item := range slice {
		if m, ok := item.(map[string]any); ok {
			out[i] = CondenseNode(m, opts)
		} else {
			out[i] = item
		}
	}
	return out
}

// CondenseDescription sanitizes prose markdown and extracts the core sentence.
func CondenseDescription(desc string, maxLen int, opts CondenseOptions) string {
	raw := strings.TrimSpace(desc)
	if raw == "" {
		return ""
	}

	cleaned := raw

	// Step A: Strip markdown code blocks (```...```)
	if opts.StripCodeBlocks && strings.Contains(cleaned, "```") {
		cleaned = reCodeBlock.ReplaceAllString(cleaned, "")
	}

	// Step B: Strip markdown links [Text](url) -> Text
	if strings.IndexByte(cleaned, '[') >= 0 && strings.IndexByte(cleaned, '(') >= 0 {
		cleaned = reMarkdownLink.ReplaceAllString(cleaned, "$1")
	}

	// Step C: Strip HTML tags
	if opts.StripHTMLTags && strings.IndexByte(cleaned, '<') >= 0 && strings.IndexByte(cleaned, '>') >= 0 {
		cleaned = reHTMLTag.ReplaceAllString(cleaned, "")
	}

	// Step D: Strip formatting marks (**bold**, *italic*, headers)
	if strings.IndexAny(cleaned, "*_~") >= 0 {
		cleaned = reMarkdownFormat.ReplaceAllString(cleaned, "")
	}
	if strings.IndexByte(cleaned, '#') >= 0 {
		cleaned = reMarkdownHeader.ReplaceAllString(cleaned, "")
	}

	// Step E: Strip tutorial bullet and numbered step markers
	if opts.StripTutorialLists {
		if strings.IndexAny(cleaned, "-*\u2022") >= 0 {
			cleaned = reTutorialBullets.ReplaceAllString(cleaned, "")
		}
		if strings.IndexAny(cleaned, "0123456789") >= 0 && strings.IndexAny(cleaned, ".)") >= 0 {
			cleaned = reNumberedSteps.ReplaceAllString(cleaned, "")
		}
	}

	// Step F: Strip meta-phrases (e.g. "IMPORTANT:", "Selection Process:")
	if opts.StripInstructionMeta && strings.IndexByte(cleaned, ':') >= 0 {
		cleaned = reInstructionMeta.ReplaceAllString(cleaned, "")
	}

	// Step G: Normalize spaces and newlines without regex overhead
	cleaned = collapseWhitespaceFast(cleaned)

	if cleaned == "" {
		return truncateWordBoundary(raw, maxLen)
	}

	// Step H: Extract the core semantic sentence
	if opts.ExtractFirstSentence {
		cleaned = extractFirstSentence(cleaned)
	}

	// Step I: Word-boundary truncation to maxLen
	return truncateWordBoundary(cleaned, maxLen)
}

func collapseWhitespaceFast(s string) string {
	needsCollapse := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' || (c == ' ' && (i == 0 || i+1 == len(s) || s[i+1] == ' ')) {
			needsCollapse = true
			break
		}
	}
	if !needsCollapse {
		return s
	}
	var sb strings.Builder
	sb.Grow(len(s))
	inSpace := true // trims leading whitespace automatically
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			if !inSpace {
				sb.WriteByte(' ')
				inSpace = true
			}
			continue
		}
		sb.WriteByte(c)
		inSpace = false
	}
	out := sb.String()
	if len(out) > 0 && out[len(out)-1] == ' ' {
		return out[:len(out)-1]
	}
	return out
}

// extractFirstSentence finds the first grammatical sentence avoiding common abbreviation traps.
func extractFirstSentence(text string) string {
	runes := []rune(text)
	n := len(runes)

	for i := 0; i < n; i++ {
		r := runes[i]
		if r == '.' || r == '!' || r == '?' {
			// Check only the trailing window (max 6 runes) for abbreviations
			winStart := i - 5
			if winStart < 0 {
				winStart = 0
			}
			tail := strings.ToLower(string(runes[winStart : i+1]))
			isAbbrev := false
			for _, abbrev := range commonAbbreviations {
				if strings.HasSuffix(tail, abbrev) {
					isAbbrev = true
					break
				}
			}
			if isAbbrev {
				continue
			}

			// Sentence boundary found if followed by whitespace or end of string
			if i+1 == n || unicode.IsSpace(runes[i+1]) {
				return strings.TrimSpace(string(runes[:i+1]))
			}
		}
	}

	// No clear sentence terminator found; return entire text
	return text
}

// truncateWordBoundary ensures UTF-8 rune-safe truncation cleanly between words with an ASCII ellipsis.
func truncateWordBoundary(text string, maxLen int) string {
	text = strings.TrimSpace(text)
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}

	cutoff := maxLen - 3
	lastSpace := -1
	for i := cutoff; i >= 0; i-- {
		if unicode.IsSpace(runes[i]) {
			lastSpace = i
			break
		}
	}

	if lastSpace > cutoff/2 {
		return strings.TrimRight(string(runes[:lastSpace]), " ,;:-.") + "..."
	}

	return strings.TrimRight(string(runes[:cutoff]), " ,;:-.") + "..."
}

// NormalizeRegex evaluates a regex pattern and format, returning the simplified version.
func NormalizeRegex(pattern string, currentFormat string, opts CondenseOptions) (string, string, bool) {
	if len(pattern) <= opts.MaxRegexLength {
		return pattern, currentFormat, false
	}

	// Case 1: If format is already present, pattern is completely redundant
	if currentFormat != "" {
		return "", currentFormat, true
	}

	if !opts.NormalizeRegexToFormat {
		if opts.StripVerboseRegex {
			return "", "", true
		}
		return pattern, currentFormat, false
	}

	// Case 2: UUID Pattern -> format: "uuid" (evaluated before ISO-8601 to avoid false positive collisions)
	if reUUIDPattern.MatchString(pattern) {
		return "", "uuid", true
	}

	// Case 3: ISO-8601 Date/Time Regex -> format: "date-time"
	if reISODateTime.MatchString(pattern) {
		return "", "date-time", true
	}

	// Case 4: Email Pattern -> format: "email"
	if reEmailPattern.MatchString(pattern) {
		return "", "email", true
	}

	// Case 5: URI / URL Pattern -> format: "uri"
	if reURIPattern.MatchString(pattern) {
		return "", "uri", true
	}

	// Case 6: Unrecognized verbose regex (>25 chars)
	if opts.StripVerboseRegex {
		return "", "", true
	}

	return pattern, currentFormat, false
}
