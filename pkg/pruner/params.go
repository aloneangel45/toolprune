package pruner

import (
	"strings"
	"unicode"

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

	// Ensure root object type invariant
	if pruned.InputSchema.Type == "" {
		pruned.InputSchema.Type = "object"
	}

	// Prune properties recursively
	if pruned.InputSchema.Properties != nil {
		pruned.InputSchema.Properties = prunePropertiesMap(pruned.InputSchema.Properties, opts)
	}

	// Strip root-level decorative fields if present
	if opts.StripValidators {
		if b, ok := pruned.InputSchema.AdditionalProperties.(bool); ok && !b {
			pruned.InputSchema.AdditionalProperties = nil
		}
		pruned.InputSchema.Schema = ""
	}

	return pruned, nil
}

// PruneInputSchema prunes an mcp.InputSchema directly, returning a pruned copy.
func PruneInputSchema(schema *mcp.InputSchema, opts ParamsOptions) (*mcp.InputSchema, error) {
	if schema == nil {
		return &mcp.InputSchema{Type: "object"}, nil
	}

	copySchema := &mcp.InputSchema{
		Type:       schema.Type,
		Properties: prunePropertiesMap(schema.Properties, opts),
		Required:   append([]string(nil), schema.Required...),
	}
	if copySchema.Type == "" {
		copySchema.Type = "object"
	}
	return copySchema, nil
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

		// INVARIANT 4: Nested properties (for object types)
		case "properties":
			if nestedProps, ok := v.(map[string]any); ok {
				cleaned["properties"] = prunePropertiesMap(nestedProps, opts)
			} else {
				cleaned["properties"] = v
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

	// 3. Extract up to first grammatical sentence if within reasonable bound
	if idx := strings.IndexAny(clean, ".!?"); idx != -1 && idx <= maxLen {
		clean = clean[:idx+1]
		return clean
	}

	// 4. Rune-safe word-boundary truncation
	runes := []rune(clean)
	if len(runes) <= maxLen {
		return clean
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

// stripMarkdownLinks replaces [foo](bar) with foo.
func stripMarkdownLinks(s string) string {
	for {
		startBracket := strings.Index(s, "[")
		if startBracket == -1 {
			break
		}
		endBracket := strings.Index(s[startBracket:], "]")
		if endBracket == -1 {
			break
		}
		endBracket += startBracket

		if endBracket+1 < len(s) && s[endBracket+1] == '(' {
			endParen := strings.Index(s[endBracket+1:], ")")
			if endParen != -1 {
				endParen += endBracket + 1
				linkText := s[startBracket+1 : endBracket]
				s = s[:startBracket] + linkText + s[endParen+1:]
				continue
			}
		}
		break
	}
	return s
}
