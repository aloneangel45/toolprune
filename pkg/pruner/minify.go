package pruner

import (
	"encoding/json"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// MinifyTool applies Tier 1 lossless minification to a single MCP tool definition.
func MinifyTool(tool mcp.Tool, opts ...MinifyOptions) mcp.Tool {
	var opt MinifyOptions
	if len(opts) > 0 {
		opt = opts[0]
	} else {
		opt = DefaultMinifyOptions()
	}

	out := tool
	out.InputSchema = MinifyInputSchema(tool.InputSchema, opt)
	if len(out.OutputSchema) > 0 {
		out.OutputSchema = MinifyNode(out.OutputSchema, opt)
	}
	return out
}

// MinifyToolsList applies Tier 1 lossless minification across an entire slice of tools.
func MinifyToolsList(tools []mcp.Tool, opts ...MinifyOptions) []mcp.Tool {
	var opt MinifyOptions
	if len(opts) > 0 {
		opt = opts[0]
	} else {
		opt = DefaultMinifyOptions()
	}

	result := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		result[i] = MinifyTool(t, opt)
	}
	return result
}

// MinifyInputSchema cleans an mcp.InputSchema struct while preserving full Draft-07/2020-12 semantics.
func MinifyInputSchema(s mcp.InputSchema, opts MinifyOptions) mcp.InputSchema {
	out := s

	// 1. Strip decorative root metadata keywords
	if opts.StripSchemaKeyword {
		out.Schema = ""
	}
	if opts.StripTitle {
		out.Title = ""
	}
	if opts.StripID {
		out.ID = ""
	}
	if opts.StripComments {
		out.Comment = ""
	}
	if opts.StripExamples {
		out.Examples = nil
	}

	// 2. Strip additionalProperties: false if boolean
	if opts.StripAdditionalPropsFalse {
		if b, ok := out.AdditionalProperties.(bool); ok && !b {
			out.AdditionalProperties = nil
		}
	}

	// 3. Clean empty required array
	if opts.CleanEmptyContainers && len(out.Required) == 0 {
		out.Required = nil
	}

	// 4. Recursively minify properties and sub-schema maps
	if len(out.Properties) > 0 {
		out.Properties = MinifySchemaMap(out.Properties, opts)
	} else if opts.CleanEmptyContainers && out.Properties != nil {
		out.Properties = nil
	}

	if len(out.Defs) > 0 {
		out.Defs = MinifySchemaMap(out.Defs, opts)
	} else if opts.CleanEmptyContainers && out.Defs != nil {
		out.Defs = nil
	}

	if len(out.Definitions) > 0 {
		out.Definitions = MinifySchemaMap(out.Definitions, opts)
	} else if opts.CleanEmptyContainers && out.Definitions != nil {
		out.Definitions = nil
	}

	if len(out.PatternProperties) > 0 {
		out.PatternProperties = MinifySchemaMap(out.PatternProperties, opts)
	} else if opts.CleanEmptyContainers && out.PatternProperties != nil {
		out.PatternProperties = nil
	}

	if len(out.DependentSchemas) > 0 {
		out.DependentSchemas = MinifySchemaMap(out.DependentSchemas, opts)
	} else if opts.CleanEmptyContainers && out.DependentSchemas != nil {
		out.DependentSchemas = nil
	}

	if opts.CleanEmptyContainers && len(out.DependentRequired) == 0 {
		out.DependentRequired = nil
	}

	if len(out.AnyOf) > 0 {
		out.AnyOf = minifyNodeSlice(out.AnyOf, opts)
	} else if opts.CleanEmptyContainers && out.AnyOf != nil {
		out.AnyOf = nil
	}

	if len(out.OneOf) > 0 {
		out.OneOf = minifyNodeSlice(out.OneOf, opts)
	} else if opts.CleanEmptyContainers && out.OneOf != nil {
		out.OneOf = nil
	}

	if len(out.AllOf) > 0 {
		out.AllOf = minifyNodeSlice(out.AllOf, opts)
	} else if opts.CleanEmptyContainers && out.AllOf != nil {
		out.AllOf = nil
	}

	if len(out.Not) > 0 {
		out.Not = MinifyNode(out.Not, opts)
	} else if opts.CleanEmptyContainers && out.Not != nil {
		out.Not = nil
	}

	// 5. If AdditionalProperties is a schema map, recursively minify it
	if addPropsMap, ok := out.AdditionalProperties.(map[string]any); ok {
		out.AdditionalProperties = MinifyNode(addPropsMap, opts)
	}

	return out
}

// MinifySchemaMap recursively traverses a property map or schema map.
func MinifySchemaMap(properties map[string]any, opts MinifyOptions) map[string]any {
	if properties == nil {
		return nil
	}

	minified := make(map[string]any, len(properties))
	for propName, propVal := range properties {
		switch v := propVal.(type) {
		case map[string]any:
			minified[propName] = MinifyNode(v, opts)
		default:
			minified[propName] = v
		}
	}
	return minified
}

// MinifyNode minifies an individual JSON Schema AST node (map[string]any).
func MinifyNode(node map[string]any, opts MinifyOptions) map[string]any {
	if node == nil {
		return nil
	}

	out := make(map[string]any, len(node))

	for k, v := range node {
		// Strip blacklisted metadata keywords
		if opts.StripSchemaKeyword && (k == "$schema") {
			continue
		}
		if opts.StripTitle && (k == "title") {
			continue
		}
		if opts.StripID && (k == "$id" || k == "id") {
			continue
		}
		if opts.StripComments && (k == "$comment" || k == "comment") {
			continue
		}
		if opts.StripExamples && (k == "examples" || k == "example") {
			continue
		}

		// Handle additionalProperties
		if k == "additionalProperties" {
			if b, ok := v.(bool); ok && !b && opts.StripAdditionalPropsFalse {
				// Drop "additionalProperties: false"
				continue
			}
			if subMap, ok := v.(map[string]any); ok {
				// Preserve dictionary schema, recurse
				out[k] = MinifyNode(subMap, opts)
				continue
			}
		}

		// Handle empty required arrays
		if k == "required" && opts.CleanEmptyContainers {
			if arr, ok := v.([]any); ok && len(arr) == 0 {
				continue
			}
			if strArr, ok := v.([]string); ok && len(strArr) == 0 {
				continue
			}
		}

		// Recursive traversal for nested schemas
		switch k {
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			if subMap, ok := v.(map[string]any); ok {
				out[k] = MinifySchemaMap(subMap, opts)
				continue
			}

		case "items":
			if subMap, ok := v.(map[string]any); ok {
				out[k] = MinifyNode(subMap, opts)
				continue
			}
			if subArr, ok := v.([]any); ok {
				out[k] = minifyNodeSlice(subArr, opts)
				continue
			}

		case "allOf", "anyOf", "oneOf":
			if subArr, ok := v.([]any); ok {
				out[k] = minifyNodeSlice(subArr, opts)
				continue
			}

		case "not", "if", "then", "else":
			if subMap, ok := v.(map[string]any); ok {
				out[k] = MinifyNode(subMap, opts)
				continue
			}
		}

		// Default: retain field unchanged
		out[k] = v
	}

	return out
}

// minifyNodeSlice recursively minifies an array of schema objects (e.g. anyOf, oneOf).
func minifyNodeSlice(slice []any, opts MinifyOptions) []any {
	out := make([]any, len(slice))
	for i, item := range slice {
		if m, ok := item.(map[string]any); ok {
			out[i] = MinifyNode(m, opts)
		} else {
			out[i] = item
		}
	}
	return out
}

// CompactJSON serializes any value to zero-whitespace JSON bytes.
func CompactJSON(v any) ([]byte, error) {
	return json.Marshal(v)
}
