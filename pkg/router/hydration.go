package router

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// CachedTool stores multi-tier schema variants and search metadata for a tool.
type CachedTool struct {
	Name           string
	UpstreamServer string
	RawTool        mcp.Tool // Original full upstream schema
	PrunedTool     mcp.Tool // Tier 3 pruned schema
	CompactTool    mcp.Tool // Tier 4 compact catalog schema
	Keywords       []string
	LastAccessed   time.Time
	AccessCount    uint64
}

// SchemaStore provides a thread-safe registry of all upstream tools and schemas.
type SchemaStore struct {
	mu    sync.RWMutex
	tools map[string]*CachedTool
	order []string // Preserves registration order
}

// NewSchemaStore instantiates a new thread-safe SchemaStore.
func NewSchemaStore() *SchemaStore {
	return &SchemaStore{
		tools: make(map[string]*CachedTool),
		order: make([]string, 0),
	}
}

// Register stores a tool with its raw, pruned, and compact variants.
func (s *SchemaStore) Register(server string, rawTool mcp.Tool, prunedTool mcp.Tool, compactTool mcp.Tool) *CachedTool {
	s.mu.Lock()
	defer s.mu.Unlock()

	name := rawTool.Name
	if _, exists := s.tools[name]; !exists {
		s.order = append(s.order, name)
	}

	keywords := extractKeywords(name, rawTool.Description)
	entry := &CachedTool{
		Name:           name,
		UpstreamServer: server,
		RawTool:        rawTool,
		PrunedTool:     prunedTool,
		CompactTool:    compactTool,
		Keywords:       keywords,
		LastAccessed:   time.Now(),
	}
	s.tools[name] = entry
	return entry
}

// Get retrieves a cached tool by name.
func (s *SchemaStore) Get(name string) (*CachedTool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.tools[name]
	return entry, ok
}

// ListAll returns all cached tools in deterministic registration order.
func (s *SchemaStore) ListAll() []*CachedTool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	res := make([]*CachedTool, len(s.order))
	for i, name := range s.order {
		res[i] = s.tools[name]
	}
	return res
}

// Search searches for cached tools matching query keywords and optional server filter.
func (s *SchemaStore) Search(query string, server string, limit int) []*CachedTool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 50 {
		limit = 8
	}

	q := strings.ToLower(strings.TrimSpace(query))
	tokens := strings.Fields(q)

	var matches []*CachedTool
	for _, name := range s.order {
		tool := s.tools[name]
		if server != "" && !strings.EqualFold(tool.UpstreamServer, server) {
			continue
		}

		toolNameLower := strings.ToLower(tool.Name)
		toolDescLower := strings.ToLower(tool.RawTool.Description)

		matched := false
		if strings.Contains(toolNameLower, q) || strings.Contains(toolDescLower, q) {
			matched = true
		} else {
			// Check individual search tokens
			allTokensMatch := true
			for _, tok := range tokens {
				tokMatch := false
				if strings.Contains(toolNameLower, tok) || strings.Contains(toolDescLower, tok) {
					tokMatch = true
				} else {
					for _, kw := range tool.Keywords {
						if strings.Contains(kw, tok) {
							tokMatch = true
							break
						}
					}
				}
				if !tokMatch {
					allTokensMatch = false
					break
				}
			}
			if allTokensMatch && len(tokens) > 0 {
				matched = true
			}
		}

		if matched {
			matches = append(matches, tool)
			if len(matches) >= limit {
				break
			}
		}
	}

	return matches
}

// ValidationError details parameter mismatches identified during pre-invocation validation.
type ValidationError struct {
	ToolName string
	Issues   []string
	Schema   mcp.InputSchema
}

// Error formats the validation failure into a human- and LLM-readable self-correcting message.
func (v *ValidationError) Error() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ToolPrune Schema Validation Error for %q:\n", v.ToolName))
	for _, issue := range v.Issues {
		sb.WriteString(fmt.Sprintf("• %s\n", issue))
	}
	sb.WriteString("\nExpected Parameter Schema:\n")

	// Sort property keys for deterministic output
	keys := make([]string, 0, len(v.Schema.Properties))
	for k := range v.Schema.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, propName := range keys {
		propDefAny := v.Schema.Properties[propName]
		propDef, _ := propDefAny.(map[string]any)
		pType, _ := propDef["type"].(string)
		isReq := containsString(v.Schema.Required, propName)
		reqStr := "optional"
		if isReq {
			reqStr = "required"
		}
		desc, _ := propDef["description"].(string)
		if enums, ok := propDef["enum"].([]any); ok && len(enums) > 0 {
			sb.WriteString(fmt.Sprintf("- %s (%s, %s, enum: %v): %s\n", propName, pType, reqStr, enums, desc))
		} else {
			if desc != "" {
				sb.WriteString(fmt.Sprintf("- %s (%s, %s): %s\n", propName, pType, reqStr, desc))
			} else {
				sb.WriteString(fmt.Sprintf("- %s (%s, %s)\n", propName, pType, reqStr))
			}
		}
	}
	return sb.String()
}

// ToCallResult formats the error as a standard MCP CallToolResult with IsError: true.
func (v *ValidationError) ToCallResult() mcp.CallToolResult {
	return mcp.CallToolResult{
		IsError: true,
		Content: []mcp.ContentItem{
			{
				Type: "text",
				Text: v.Error(),
			},
		},
	}
}

// ValidateInvocation performs strict schema validation of raw arguments against the cached schema.
func ValidateInvocation(tool *CachedTool, rawArgs json.RawMessage) *ValidationError {
	if tool == nil {
		return &ValidationError{
			ToolName: "unknown",
			Issues:   []string{"tool not found in catalog"},
		}
	}

	// Prefer pruned schema (preserves types/enums while stripped of fluff), or raw schema
	schema := tool.PrunedTool.InputSchema
	if len(schema.Properties) == 0 && len(schema.Required) == 0 {
		schema = tool.RawTool.InputSchema
	}
	if len(schema.Properties) == 0 && len(schema.Required) == 0 {
		return nil // No parameter constraints
	}

	var args map[string]any
	rawStr := strings.TrimSpace(string(rawArgs))
	if len(rawArgs) > 0 && rawStr != "" && rawStr != "{}" && rawStr != "null" {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return &ValidationError{
				ToolName: tool.Name,
				Issues:   []string{fmt.Sprintf("arguments must be a valid JSON object: %v", err)},
				Schema:   schema,
			}
		}
	} else {
		args = make(map[string]any)
	}

	var issues []string

	// 1. Check Required Properties
	for _, req := range schema.Required {
		val, exists := args[req]
		if !exists || val == nil {
			issues = append(issues, fmt.Sprintf("Missing required parameter: %q", req))
		}
	}

	// 2. Check Types and Enums for Present Properties
	for k, val := range args {
		propDefAny, defined := schema.Properties[k]
		if !defined {
			continue
		}
		propDef, ok := propDefAny.(map[string]any)
		if !ok {
			continue
		}

		// Type check
		if expectedType, ok := propDef["type"].(string); ok && val != nil {
			if !matchesJSONType(val, expectedType) {
				issues = append(issues, fmt.Sprintf("Invalid type for parameter %q: expected %s, got %s", k, expectedType, describeValType(val)))
			}
		}

		// Enum check
		if enums, ok := propDef["enum"].([]any); ok && len(enums) > 0 {
			if !matchesEnumVal(val, enums) {
				issues = append(issues, fmt.Sprintf("Invalid value %v for parameter %q. Allowed values: %v", val, k, enums))
			}
		}
	}

	if len(issues) > 0 {
		return &ValidationError{
			ToolName: tool.Name,
			Issues:   issues,
			Schema:   schema,
		}
	}
	return nil
}

func matchesJSONType(val any, expected string) bool {
	switch expected {
	case "string":
		_, ok := val.(string)
		return ok
	case "number":
		_, ok := val.(float64)
		return ok
	case "integer":
		f, ok := val.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "array":
		_, ok := val.([]any)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	default:
		return true
	}
}

func describeValType(val any) string {
	switch val.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", val)
	}
}

func matchesEnumVal(val any, enums []any) bool {
	valStr := fmt.Sprintf("%v", val)
	for _, e := range enums {
		if fmt.Sprintf("%v", e) == valStr {
			return true
		}
	}
	return false
}

func containsString(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

func extractKeywords(name, desc string) []string {
	combined := strings.ToLower(name + " " + desc)
	tokens := strings.FieldsFunc(combined, func(r rune) bool {
		return r == '_' || r == '-' || r == ' ' || r == '/' || r == '.' || r == ':' || r == ',' || r == '(' || r == ')' || r == '[' || r == ']' || r == '`' || r == '"' || r == '\''
	})
	seen := make(map[string]struct{}, len(tokens))
	var res []string
	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if len(t) >= 2 {
			if _, ok := seen[t]; !ok {
				seen[t] = struct{}{}
				res = append(res, t)
			}
		}
	}
	return res
}
