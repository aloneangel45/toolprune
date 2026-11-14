package pruner

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// resolveParamsOptions extracts ParamsOptions from variable arguments.
func resolveParamsOptions(opts []any) ParamsOptions {
	if len(opts) == 0 {
		return DefaultParamsOptions()
	}
	switch v := opts[0].(type) {
	case ParamsOptions:
		return v
	case *ParamsOptions:
		if v != nil {
			return *v
		}
	case PruneOptions:
		return v.Params
	case *PruneOptions:
		if v != nil {
			return v.Params
		}
	}
	return DefaultParamsOptions()
}

// PruneParamsTool applies Tier 3 parameter pruning to an mcp.Tool.
func PruneParamsTool(tool mcp.Tool, opts ...any) mcp.Tool {
	opt := resolveParamsOptions(opts)
	pruned, _ := PruneToolParams(tool, opt)
	return pruned
}

// PruneToolParams applies Tier 3 parameter schema pruning to an mcp.Tool.
func PruneToolParams(tool mcp.Tool, opts ParamsOptions) (mcp.Tool, error) {
	pruned := tool

	prunedSchema, err := PruneInputSchema(&pruned.InputSchema, opts)
	if err != nil {
		return pruned, err
	}
	pruned.InputSchema = *prunedSchema

	// Optional tier-based docstring compaction inside OutputSchema
	if len(pruned.OutputSchema) > 0 {
		pruned.OutputSchema = pruneProperty(pruned.OutputSchema, "output_schema", opts)
	}

	return pruned, nil
}

// PruneInputSchema prunes an mcp.InputSchema directly, returning a pruned copy while preserving Draft-07/2020-12 fields.
func PruneInputSchema(schema *mcp.InputSchema, opts ParamsOptions) (*mcp.InputSchema, error) {
	if schema == nil {
		return &mcp.InputSchema{Type: "object"}, nil
	}

	copySchema := CloneInputSchema(*schema)
	if copySchema.Type == "" {
		copySchema.Type = "object"
	}

	if copySchema.Properties != nil {
		copySchema.Properties = prunePropertiesMap(copySchema.Properties, opts)
	}
	if copySchema.Defs != nil {
		copySchema.Defs = prunePropertiesMap(copySchema.Defs, opts)
	}
	if copySchema.Definitions != nil {
		copySchema.Definitions = prunePropertiesMap(copySchema.Definitions, opts)
	}
	if copySchema.PatternProperties != nil {
		copySchema.PatternProperties = prunePropertiesMap(copySchema.PatternProperties, opts)
	}
	if copySchema.DependentSchemas != nil {
		copySchema.DependentSchemas = prunePropertiesMap(copySchema.DependentSchemas, opts)
	}
	if len(copySchema.AnyOf) > 0 {
		copySchema.AnyOf = pruneBranchSlice(copySchema.AnyOf, "root_anyOf", opts)
	}
	if len(copySchema.OneOf) > 0 {
		copySchema.OneOf = pruneBranchSlice(copySchema.OneOf, "root_oneOf", opts)
	}
	if len(copySchema.AllOf) > 0 {
		copySchema.AllOf = pruneBranchSlice(copySchema.AllOf, "root_allOf", opts)
	}
	if copySchema.Not != nil {
		copySchema.Not = pruneProperty(copySchema.Not, "root_not", opts)
	}
	if addPropsMap, ok := copySchema.AdditionalProperties.(map[string]any); ok {
		copySchema.AdditionalProperties = pruneProperty(addPropsMap, "additional_props", opts)
	}

	// Strip root-level decorative fields if configured
	if opts.StripValidators {
		if b, ok := copySchema.AdditionalProperties.(bool); ok && !b {
			copySchema.AdditionalProperties = nil
		}
		copySchema.Schema = ""
		copySchema.ID = ""
		copySchema.Title = ""
		copySchema.Comment = ""
		copySchema.Examples = nil
	}

	return &copySchema, nil
}

func pruneBranchSlice(unionList []any, propName string, opts ParamsOptions) []any {
	cleanedUnion := make([]any, len(unionList))
	for i, branch := range unionList {
		if branchMap, ok := branch.(map[string]any); ok {
			cleanedUnion[i] = pruneProperty(branchMap, propName, opts)
		} else {
			cleanedUnion[i] = branch
		}
	}
	return cleanedUnion
}

// prunePropertiesMap processes a map of propertyName -> propertySchema.
func prunePropertiesMap(props map[string]any, opts ParamsOptions) map[string]any {
	if props == nil {
		return nil
	}

	result := make(map[string]any, len(props))
	for propName, rawProp := range props {
		propMap, ok := rawProp.(map[string]any)
		if !ok {
			result[propName] = rawProp
			continue
		}
		result[propName] = pruneProperty(propMap, propName, opts)
	}
	return result
}

// pruneProperty prunes an individual property schema map while enforcing invariants.
func pruneProperty(prop map[string]any, propName string, opts ParamsOptions) map[string]any {
	cleaned := make(map[string]any, len(prop))

	for k, v := range prop {
		switch k {
		// INVARIANT 1: Type is unconditionally preserved
		case "type":
			cleaned["type"] = v

		// INVARIANT 2: Required array is unconditionally preserved
		case "required":
			if reqList, ok := v.([]any); ok {
				cleaned["required"] = append([]any(nil), reqList...)
			} else if strList, ok := v.([]string); ok {
				cleaned["required"] = append([]string(nil), strList...)
			} else {
				cleaned["required"] = v
			}

		// INVARIANT 3: Enum is unconditionally preserved
		case "enum":
			if enumList, ok := v.([]any); ok {
				cleaned["enum"] = append([]any(nil), enumList...)
			} else if strList, ok := v.([]string); ok {
				cleaned["enum"] = append([]string(nil), strList...)
			} else {
				cleaned["enum"] = v
			}

		// INVARIANT 4: Nested properties and sub-schema maps ($defs, definitions, patternProperties, dependentSchemas)
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			if nestedProps, ok := v.(map[string]any); ok {
				cleaned[k] = prunePropertiesMap(nestedProps, opts)
			} else {
				cleaned[k] = v
			}

		// Conditional / negated sub-schemas: not, if, then, else
		case "not", "if", "then", "else":
			if subMap, ok := v.(map[string]any); ok {
				cleaned[k] = pruneProperty(subMap, propName+"_"+k, opts)
			} else {
				cleaned[k] = v
			}

		// INVARIANT 4: Array items
		case "items":
			if itemMap, ok := v.(map[string]any); ok {
				cleaned["items"] = pruneProperty(itemMap, propName+"_item", opts)
			} else if itemList, ok := v.([]any); ok {
				cleanedItems := make([]any, len(itemList))
				for i, item := range itemList {
					if subMap, ok := item.(map[string]any); ok {
						cleanedItems[i] = pruneProperty(subMap, propName+"_item", opts)
					} else {
						cleanedItems[i] = item
					}
				}
				cleaned["items"] = cleanedItems
			} else {
				cleaned["items"] = v
			}

		// Polymorphic unions: anyOf, oneOf, allOf
		case "anyOf", "oneOf", "allOf":
			if unionList, ok := v.([]any); ok {
				cleanedUnion := make([]any, len(unionList))
				for i, branch := range unionList {
					if branchMap, ok := branch.(map[string]any); ok {
						cleanedUnion[i] = pruneProperty(branchMap, propName, opts)
					} else {
						cleanedUnion[i] = branch
					}
				}
				cleaned[k] = cleanedUnion
			} else {
				cleaned[k] = v
			}

		// Intelligent Description Stripping
		case "description":
			if descStr, ok := v.(string); ok {
				if isSelfEvidentParam(propName, opts.SelfEvidentParams) {
					// Self-evident: drop description entirely
					continue
				}
				// Non-self-evident: condense to concise keyword/phrase
				condensed := truncateDescription(descStr, opts.MaxNonSelfEvidentLen)
				if condensed != "" {
					cleaned["description"] = condensed
				}
			}

		// Pattern (regex) handling
		case "pattern":
			if patternStr, ok := v.(string); ok {
				if opts.StripLongPatterns && len(patternStr) > opts.PatternMaxLen {
					// Strip bloated regexes (e.g. 270-char ISO-8601)
					continue
				}
			}
			cleaned["pattern"] = v

		// Format hints (compact format hints are preserved)
		case "format":
			cleaned["format"] = v

		// Decorative validators to strip
		case "minLength", "maxLength", "minimum", "maximum",
			"title", "$comment", "examples", "example",
			"readOnly", "writeOnly":
			if opts.StripValidators {
				continue
			}
			cleaned[k] = v

		case "additionalProperties":
			if b, ok := v.(bool); ok && !b && opts.StripValidators {
				continue
			}
			if subMap, ok := v.(map[string]any); ok {
				cleaned[k] = pruneProperty(subMap, propName+"_dict", opts)
				continue
			}
			cleaned[k] = v

		// Default handling
		case "default":
			if opts.StripValidators {
				// Strip null or empty string defaults
				if v == nil || v == "" {
					continue
				}
			}
			cleaned["default"] = v

		// Default passthrough for other unknown JSON Schema keywords
		default:
			cleaned[k] = v
		}
	}

	return cleaned
}

// isSelfEvidentParam determines whether a parameter name is universally understood.
func isSelfEvidentParam(name string, dict map[string]struct{}) bool {
	norm := normalizeParamName(name)
	if norm == "" {
		return false
	}

	if dict == nil {
		dict = GetDefaultSelfEvidentDictionary()
	}

	// 1. Exact dictionary match
	if _, exists := dict[norm]; exists {
		return true
	}

	// 2. Suffix matching for ID patterns (exact "id", snake_case "_id", camelCase "Id" or "ID")
	if norm == "id" || strings.HasSuffix(norm, "_id") || strings.HasSuffix(name, "Id") || strings.HasSuffix(name, "ID") {
		return true
	}

	// 3. Suffix matching for name patterns (exact "name", snake_case "_name", camelCase "Name")
	if norm == "name" || strings.HasSuffix(norm, "_name") || strings.HasSuffix(name, "Name") {
		return true
	}

	return false
}

// normalizeParamName strips punctuation, underscores, and lowers case.
func normalizeParamName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return s
}

// truncateDescription cleans markdown prose and truncates to maxLen runes.
func truncateDescription(desc string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}

	// 1. Remove markdown links: [link text](url) -> link text
	desc = stripMarkdownLinks(desc)

	// 2. Normalize whitespace and newlines
	fields := strings.Fields(desc)
	clean := strings.Join(fields, " ")
	if clean == "" {
		return ""
	}

	// 3. Extract up to first grammatical sentence if within reasonable rune bound
	if idx := strings.IndexAny(clean, ".!?"); idx != -1 && utf8.RuneCountInString(clean[:idx+1]) <= maxLen {
		clean = clean[:idx+1]
		return clean
	}

	// 4. Rune-safe word-boundary truncation
	runes := []rune(clean)
	if len(runes) <= maxLen {
		return clean
	}
	if maxLen <= 3 {
		return string(runes[:maxLen])
	}

	// Find the last space before maxLen-3 to avoid cutting words in half
	cutoff := maxLen - 3
	lastSpace := -1
	for i := cutoff; i >= 0; i-- {
		if unicode.IsSpace(runes[i]) {
			lastSpace = i
			break
		}
	}

	if lastSpace > maxLen/2 {
		return strings.TrimRight(string(runes[:lastSpace]), " ,;:-") + "..."
	}

	return strings.TrimRight(string(runes[:cutoff]), " ,;:-") + "..."
}

// stripMarkdownLinks replaces [foo](bar) with foo in a single linear pass,
// even when non-link brackets such as "[NOTE]" precede markdown links.
func stripMarkdownLinks(s string) string {
	if !strings.Contains(s, "[") {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		relStart := strings.IndexByte(s[i:], '[')
		if relStart == -1 {
			out.WriteString(s[i:])
			break
		}
		startBracket := i + relStart
		out.WriteString(s[i:startBracket])

		relEnd := strings.IndexByte(s[startBracket+1:], ']')
		if relEnd == -1 {
			out.WriteString(s[startBracket:])
			break
		}
		endBracket := startBracket + 1 + relEnd
		if nestedBracket := strings.LastIndexByte(s[startBracket+1:endBracket], '['); nestedBracket != -1 {
			actualStart := startBracket + 1 + nestedBracket
			out.WriteString(s[startBracket:actualStart])
			startBracket = actualStart
		}
		if endBracket+1 < len(s) && s[endBracket+1] == '(' {
			if relParen := strings.IndexByte(s[endBracket+2:], ')'); relParen != -1 {
				endParen := endBracket + 2 + relParen
				out.WriteString(s[startBracket+1 : endBracket])
				i = endParen + 1
				continue
			}
		}
		out.WriteString(s[startBracket : endBracket+1])
		i = endBracket + 1
	}
	return out.String()
}
