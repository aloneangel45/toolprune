package router_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func TestSchemaStore(t *testing.T) {
	store := router.NewSchemaStore()

	t1 := mcp.Tool{
		Name:        "github_create_issue",
		Description: "Create an issue in a GitHub repository with title and body",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"title": map[string]any{"type": "string", "description": "Issue title"},
				"body":  map[string]any{"type": "string", "description": "Issue body"},
			},
			Required: []string{"title"},
		},
	}

	t2 := mcp.Tool{
		Name:        "supabase_query_logs",
		Description: "Execute a ClickHouse SQL query against Supabase unified logs",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			Required: []string{"query"},
		},
	}

	t3 := mcp.Tool{
		Name:        "cloudflare_dns_record",
		Description: "Create or update a DNS record in Cloudflare zone",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"zone_id": map[string]any{"type": "string"},
				"type":    map[string]any{"type": "string", "enum": []any{"A", "AAAA", "CNAME", "TXT"}},
			},
			Required: []string{"zone_id", "type"},
		},
	}

	cfg := router.DefaultCatalogConfig()

	store.Register("github", t1, t1, router.ProjectCompactTool(t1, cfg))
	store.Register("supabase", t2, t2, router.ProjectCompactTool(t2, cfg))
	store.Register("cloudflare", t3, t3, router.ProjectCompactTool(t3, cfg))

	t.Run("Get registered tool", func(t *testing.T) {
		tool, ok := store.Get("github_create_issue")
		if !ok || tool == nil {
			t.Fatalf("expected to find github_create_issue")
		}
		if tool.UpstreamServer != "github" {
			t.Errorf("expected server github, got %s", tool.UpstreamServer)
		}
		if tool.RawTool.Name != "github_create_issue" {
			t.Errorf("expected tool name github_create_issue, got %s", tool.RawTool.Name)
		}

		_, notFound := store.Get("unknown_tool")
		if notFound {
			t.Errorf("expected not found for unknown_tool")
		}
	})

	t.Run("ListAll preserves deterministic registration order", func(t *testing.T) {
		all := store.ListAll()
		if len(all) != 3 {
			t.Fatalf("expected 3 tools, got %d", len(all))
		}
		expectedOrder := []string{"github_create_issue", "supabase_query_logs", "cloudflare_dns_record"}
		for i, exp := range expectedOrder {
			if all[i].Name != exp {
				t.Errorf("at index %d expected %s, got %s", i, exp, all[i].Name)
			}
		}
	})

	t.Run("Search by keyword and server filter", func(t *testing.T) {
		// Search query "issue"
		res := store.Search("issue", "", 10)
		if len(res) != 1 || res[0].Name != "github_create_issue" {
			t.Fatalf("expected 1 result github_create_issue, got %d", len(res))
		}

		// Search query "clickhouse" matches description
		res = store.Search("clickhouse", "", 10)
		if len(res) != 1 || res[0].Name != "supabase_query_logs" {
			t.Fatalf("expected 1 result supabase_query_logs, got %d", len(res))
		}

		// Search with server filter
		res = store.Search("record", "cloudflare", 10)
		if len(res) != 1 || res[0].Name != "cloudflare_dns_record" {
			t.Fatalf("expected 1 result from cloudflare, got %d", len(res))
		}

		// Search with non-matching server filter
		res = store.Search("record", "github", 10)
		if len(res) != 0 {
			t.Fatalf("expected 0 results for record under github, got %d", len(res))
		}

		// Search limit clamping
		res = store.Search("", "", 1)
		if len(res) > 1 {
			t.Fatalf("expected at most 1 result with limit=1, got %d", len(res))
		}
	})

	t.Run("concurrent registration and search", func(t *testing.T) {
		var wg sync.WaitGroup
		const numGoroutines = 20

		for i := 0; i < numGoroutines; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				toolName := fmt.Sprintf("tool_%d", id)
				tool := mcp.Tool{
					Name:        toolName,
					Description: fmt.Sprintf("Description for %s with keyword searchable", toolName),
					InputSchema: mcp.InputSchema{Type: "object"},
				}
				store.Register("server", tool, tool, tool)
				_, _ = store.Get(toolName)
				_ = store.ListAll()
				_ = store.Search("searchable", "", 5)
			}(i)
		}
		wg.Wait()

		if len(store.ListAll()) < numGoroutines+3 {
			t.Errorf("expected at least %d registered tools", numGoroutines+3)
		}
	})
}

func TestValidateInvocation(t *testing.T) {
	rawTool := mcp.Tool{
		Name:        "create_user",
		Description: "Create a user account with name, age, active status, role, and tags",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "User full name",
				},
				"age": map[string]any{
					"type":        "integer",
					"description": "User age in years",
				},
				"score": map[string]any{
					"type":        "number",
					"description": "User activity score",
				},
				"active": map[string]any{
					"type":        "boolean",
					"description": "Account active flag",
				},
				"role": map[string]any{
					"type":        "string",
					"description": "User permission role",
					"enum":        []any{"admin", "member", "guest"},
				},
				"tags": map[string]any{
					"type":        "array",
					"description": "Associated user tags",
				},
				"metadata": map[string]any{
					"type":        "object",
					"description": "Arbitrary metadata object",
				},
			},
			Required: []string{"name", "role"},
		},
	}

	cached := &router.CachedTool{
		Name:       "create_user",
		RawTool:    rawTool,
		PrunedTool: rawTool,
	}

	t.Run("nil tool returns validation error", func(t *testing.T) {
		valErr := router.ValidateInvocation(nil, json.RawMessage(`{}`))
		if valErr == nil {
			t.Fatalf("expected validation error for nil tool")
		}
		if !strings.Contains(valErr.Error(), "tool not found in catalog") {
			t.Errorf("unexpected error message: %s", valErr.Error())
		}
	})

	t.Run("valid parameters pass validation", func(t *testing.T) {
		validJSON := []byte(`{
			"name": "Alice Developer",
			"role": "admin",
			"age": 30,
			"score": 98.5,
			"active": true,
			"tags": ["golang", "mcp"],
			"metadata": {"department": "engineering"}
		}`)

		valErr := router.ValidateInvocation(cached, json.RawMessage(validJSON))
		if valErr != nil {
			t.Fatalf("expected nil validation error, got: %v", valErr.Error())
		}
	})

	t.Run("missing required parameters", func(t *testing.T) {
		missingRole := []byte(`{"name": "Alice Developer"}`)
		valErr := router.ValidateInvocation(cached, json.RawMessage(missingRole))
		if valErr == nil {
			t.Fatalf("expected validation error for missing role")
		}
		if len(valErr.Issues) != 1 || !strings.Contains(valErr.Issues[0], `Missing required parameter: "role"`) {
			t.Errorf("unexpected issues: %v", valErr.Issues)
		}

		// Both required missing
		valErrBoth := router.ValidateInvocation(cached, json.RawMessage(`{}`))
		if valErrBoth == nil || len(valErrBoth.Issues) != 2 {
			t.Fatalf("expected 2 issues for empty args, got %v", valErrBoth)
		}
	})

	t.Run("invalid JSON arguments", func(t *testing.T) {
		malformedJSON := []byte(`{"name": "Alice", `)
		valErr := router.ValidateInvocation(cached, json.RawMessage(malformedJSON))
		if valErr == nil {
			t.Fatalf("expected validation error for malformed JSON")
		}
		if !strings.Contains(valErr.Issues[0], "arguments must be a valid JSON object") {
			t.Errorf("unexpected issue: %s", valErr.Issues[0])
		}
	})

	t.Run("type mismatches", func(t *testing.T) {
		// string expected, got number
		wrongTypeJSON := []byte(`{
			"name": 12345,
			"role": "admin"
		}`)
		valErr := router.ValidateInvocation(cached, json.RawMessage(wrongTypeJSON))
		if valErr == nil {
			t.Fatalf("expected validation error for string type mismatch")
		}
		if !strings.Contains(valErr.Issues[0], `Invalid type for parameter "name": expected string, got number`) {
			t.Errorf("unexpected issue: %s", valErr.Issues[0])
		}

		// integer expected, got float
		floatForIntJSON := []byte(`{
			"name": "Bob",
			"role": "member",
			"age": 25.7
		}`)
		valErrFloat := router.ValidateInvocation(cached, json.RawMessage(floatForIntJSON))
		if valErrFloat == nil {
			t.Fatalf("expected validation error for float age when integer expected")
		}

		// boolean expected, got string
		strForBoolJSON := []byte(`{
			"name": "Bob",
			"role": "member",
			"active": "true"
		}`)
		valErrBool := router.ValidateInvocation(cached, json.RawMessage(strForBoolJSON))
		if valErrBool == nil {
			t.Fatalf("expected validation error for string when boolean expected")
		}
	})

	t.Run("enum value violation", func(t *testing.T) {
		invalidEnumJSON := []byte(`{
			"name": "Charlie",
			"role": "superuser"
		}`)
		valErr := router.ValidateInvocation(cached, json.RawMessage(invalidEnumJSON))
		if valErr == nil {
			t.Fatalf("expected validation error for invalid enum value")
		}
		if !strings.Contains(valErr.Issues[0], "Allowed values: [admin member guest]") {
			t.Errorf("unexpected enum issue: %s", valErr.Issues[0])
		}
	})

	t.Run("tool with no parameters passes with empty args", func(t *testing.T) {
		emptyTool := &router.CachedTool{
			Name: "ping",
			RawTool: mcp.Tool{
				Name: "ping",
				InputSchema: mcp.InputSchema{
					Type: "object",
				},
			},
		}
		valErr := router.ValidateInvocation(emptyTool, json.RawMessage(`{}`))
		if valErr != nil {
			t.Fatalf("expected nil error for tool with empty schema, got: %v", valErr)
		}
	})

	t.Run("ValidationError formatting and ToCallResult", func(t *testing.T) {
		badArgs := []byte(`{"role": "superuser"}`)
		valErr := router.ValidateInvocation(cached, json.RawMessage(badArgs))
		if valErr == nil {
			t.Fatalf("expected validation error")
		}

		errStr := valErr.Error()
		if !strings.Contains(errStr, "ToolPrune Schema Validation Error") {
			t.Errorf("expected header in Error(), got: %s", errStr)
		}
		if !strings.Contains(errStr, "Expected Parameter Schema:") {
			t.Errorf("expected schema documentation in Error(), got: %s", errStr)
		}

		callRes := valErr.ToCallResult()
		if !callRes.IsError {
			t.Errorf("expected IsError=true in CallToolResult")
		}
		if len(callRes.Content) != 1 || callRes.Content[0].Type != "text" || !strings.Contains(callRes.Content[0].Text, "Validation Error") {
			t.Errorf("unexpected call result content: %+v", callRes.Content)
		}
	})
}
