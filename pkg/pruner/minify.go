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

// MinifyInputSchema cleans an mcp.InputSchema struct.
func MinifyInputSchema(s mcp.InputSchema, opts MinifyOptions) mcp.InputSchema {
	out := s

	// 1. Strip $schema keyword
	if opts.StripSchemaKeyword {
		out.Schema = ""
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

	// 4. Recursively minify properties
	if len(out.Properties) > 0 {
		out.Properties = MinifySchemaMap(out.Properties, opts)
	} else if opts.CleanEmptyContainers && out.Properties != nil && len(out.Properties) == 0 {
		out.Properties = nil
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
