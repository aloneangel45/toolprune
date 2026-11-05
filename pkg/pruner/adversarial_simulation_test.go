package pruner_test

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// loadSchemasFixture loads the 4 real production tool schemas from test/fixtures/schemas.json.
func loadSchemasFixture(t *testing.T) []mcp.Tool {
	t.Helper()

	candidates := []string{
		filepath.Join("..", "..", "test", "fixtures", "schemas.json"),
		filepath.Join("test", "fixtures", "schemas.json"),
		`C:\Users\Arda\Desktop\open_source_hunter\test\fixtures\schemas.json`,
	}

	var data []byte
	var err error
	for _, path := range candidates {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("failed to read test fixtures from any candidate path: %v", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		t.Fatalf("failed to unmarshal test fixtures: %v", err)
	}
	if len(tools) != 4 {
		t.Fatalf("expected exactly 4 production tools in schemas.json, got %d", len(tools))
	}
	return tools
}

// ValidateArgumentPayload rigorously validates a runtime argument payload against an MCP InputSchema.
// It verifies required presence, type conformance, and enum constraints.
func ValidateArgumentPayload(schema mcp.InputSchema, args map[string]any) error {
	// 1. Check all required properties are present
	for _, req := range schema.Required {
		if _, ok := args[req]; !ok {
			return fmt.Errorf("missing required field: %q", req)
		}
	}

	// 2. Validate property types and enum constraints
	for key, val := range args {
		propRaw, exists := schema.Properties[key]
		if !exists {
			if b, ok := schema.AdditionalProperties.(bool); ok && !b {
				return fmt.Errorf("property %q not allowed by additionalProperties: false", key)
			}
			continue
		}

		propMap, ok := propRaw.(map[string]any)
		if !ok {
			continue
		}

		// Type conformance check
		if rawType, hasType := propMap["type"].(string); hasType {
			if err := checkArgType(key, val, rawType); err != nil {
				return err
			}
		}

		// Enum constraints check
		if enumRaw, hasEnum := propMap["enum"]; hasEnum {
			var allowed []string
			if enumSlice, ok := enumRaw.([]any); ok {
				for _, e := range enumSlice {
					allowed = append(allowed, fmt.Sprintf("%v", e))
				}
			} else if strSlice, ok := enumRaw.([]string); ok {
				allowed = append(allowed, strSlice...)
			}

			valStr := fmt.Sprintf("%v", val)
			found := false
			for _, a := range allowed {
				if a == valStr {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("property %q value %q is not in allowed enum %v", key, valStr, allowed)
			}
		}
	}

	return nil
}

func checkArgType(name string, val any, expectedType string) error {
	if val == nil {
		return fmt.Errorf("property %q has nil value", name)
	}
	switch expectedType {
	case "string":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("property %q expected string, got %T", name, val)
		}
	case "number":
		switch val.(type) {
		case float64, float32, int, int64, int32:
		default:
			return fmt.Errorf("property %q expected number, got %T", name, val)
		}
	case "integer":
		switch v := val.(type) {
		case int, int64, int32:
		case float64:
			if math.Floor(v) != v {
				return fmt.Errorf("property %q expected integer, got fractional float %v", name, v)
			}
		default:
			return fmt.Errorf("property %q expected integer, got %T", name, val)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("property %q expected boolean, got %T", name, val)
		}
	case "array":
		switch val.(type) {
		case []any, []string, []int, []float64:
		default:
			return fmt.Errorf("property %q expected array, got %T", name, val)
		}
	case "object":
		if _, ok := val.(map[string]any); !ok {
			return fmt.Errorf("property %q expected object, got %T", name, val)
		}
	}
	return nil
}

// Generate50ValidArguments produces 50 distinct valid argument payloads for each tool.
func Generate50ValidArguments(toolName string) []map[string]any {
	payloads := make([]map[string]any, 50)

	switch toolName {
	case "create_or_update_file":
		paths := []string{"README.md", "src/index.ts", "pkg/main.go", "docs/api.md", "config.json"}
		branches := []string{"main", "dev", "feature/prune", "release/v1.0", "hotfix/patch"}
		for i := 0; i < 50; i++ {
			p := map[string]any{
				"owner":   fmt.Sprintf("user-%d", i),
				"repo":    fmt.Sprintf("repo-%d", i),
				"path":    paths[i%len(paths)],
				"content": fmt.Sprintf("package main\n\n// Iteration %d payload data\nfunc Run() {}\n", i),
				"message": fmt.Sprintf("feat: update file on iteration %d", i),
				"branch":  branches[i%len(branches)],
			}
			if i%2 == 0 {
				p["sha"] = fmt.Sprintf("%040x", i+1)
			}
			payloads[i] = p
		}

	case "query_logs":
		for i := 0; i < 50; i++ {
			p := map[string]any{
				"project_id": fmt.Sprintf("proj-supabase-%04d", i),
				"sql":        fmt.Sprintf("SELECT count(*) FROM edge_logs WHERE status = %d LIMIT 10", 200+(i%5)),
			}
			switch i % 4 {
			case 1:
				p["iso_timestamp_start"] = "2026-11-05T00:00:00Z"
			case 2:
				p["iso_timestamp_end"] = "2026-11-05T23:59:59Z"
			case 3:
				p["iso_timestamp_start"] = "2026-11-04T12:00:00Z"
				p["iso_timestamp_end"] = "2026-11-05T12:00:00Z"
			}
			payloads[i] = p
		}

	case "create_dns_record":
		recordTypes := []string{"A", "AAAA", "CNAME", "TXT", "MX", "NS", "SRV", "CAA", "PTR", "SPF"}
		for i := 0; i < 50; i++ {
			recType := recordTypes[i%len(recordTypes)]
			content := "192.0.2.1"
			switch recType {
			case "AAAA":
				content = "2001:db8::1"
			case "CNAME":
				content = "target.example.com"
			case "TXT":
				content = "v=spf1 include:_spf.example.com ~all"
			case "MX":
				content = "mail.example.com"
			case "NS":
				content = "ns1.cloudflare.com"
			case "SRV":
				content = "0 5 5060 sipserver.example.com"
			case "CAA":
				content = "0 issue \"letsencrypt.org\""
			case "PTR":
				content = "host.example.com"
			case "SPF":
				content = "v=spf1 -all"
			}

			p := map[string]any{
				"zone_id": fmt.Sprintf("%032x", i+100),
				"type":    recType,
				"name":    fmt.Sprintf("sub-%d.domain.com", i),
				"content": content,
			}
			if i%2 == 0 {
				p["comment"] = fmt.Sprintf("Automated record %d", i)
			}
			if i%3 == 0 {
				p["confirm"] = true
			}
			if i%4 == 0 {
				p["priority"] = float64(10 * (i + 1))
			}
			if i%5 == 0 {
				p["proxied"] = true
			}
			if i%6 == 0 {
				p["ttl"] = float64(300)
			}
			payloads[i] = p
		}

	case "emulate":
		colorSchemes := []string{"dark", "light", "auto"}
		forcedColors := []string{"active", "none", "auto"}
		networkConds := []string{"Offline", "Slow 3G", "Fast 3G", "Slow 4G", "Fast 4G"}
		contrasts := []string{"more", "less", "custom", "no-preference", "auto"}
		reducedMotions := []string{"reduce", "no-preference", "auto"}

		for i := 0; i < 50; i++ {
			p := map[string]any{
				"pageId": float64(i + 1),
			}
			if i%2 == 0 {
				p["colorScheme"] = colorSchemes[i%len(colorSchemes)]
			}
			if i%3 == 0 {
				p["cpuThrottlingRate"] = float64(1 + (i % 20))
			}
			if i%4 == 0 {
				p["extraHttpHeaders"] = fmt.Sprintf(`{"X-Custom-Run":"%d"}`, i)
			}
			if i%5 == 0 {
				p["forcedColors"] = forcedColors[i%len(forcedColors)]
			}
			if i%6 == 0 {
				p["networkConditions"] = networkConds[i%len(networkConds)]
			}
			if i%7 == 0 {
				p["prefersContrast"] = contrasts[i%len(contrasts)]
			}
			if i%8 == 0 {
				p["prefersReducedMotion"] = reducedMotions[i%len(reducedMotions)]
			}
			if i%9 == 0 {
				p["userAgent"] = fmt.Sprintf("ToolPrune-Agent/1.0 (Test %d)", i)
			}
			if i%10 == 0 {
				p["viewport"] = "1920x1080x1"
			}
			if i%11 == 0 {
				p["geolocation"] = "37.7749,-122.4194"
			}
			payloads[i] = p
		}
	}

	return payloads
}

// GenerateInvalidArguments produces invalid argument payloads for testing rejection fidelity.
func GenerateInvalidArguments(toolName string) []struct {
	desc    string
	payload map[string]any
} {
	switch toolName {
	case "create_or_update_file":
		return []struct {
			desc    string
			payload map[string]any
		}{
			{"missing_owner", map[string]any{"repo": "r", "path": "p", "content": "c", "message": "m", "branch": "b"}},
			{"missing_repo", map[string]any{"owner": "o", "path": "p", "content": "c", "message": "m", "branch": "b"}},
			{"missing_path", map[string]any{"owner": "o", "repo": "r", "content": "c", "message": "m", "branch": "b"}},
			{"missing_content", map[string]any{"owner": "o", "repo": "r", "path": "p", "message": "m", "branch": "b"}},
			{"missing_message", map[string]any{"owner": "o", "repo": "r", "path": "p", "content": "c", "branch": "b"}},
			{"missing_branch", map[string]any{"owner": "o", "repo": "r", "path": "p", "content": "c", "message": "m"}},
			{"owner_as_number", map[string]any{"owner": 12345, "repo": "r", "path": "p", "content": "c", "message": "m", "branch": "b"}},
			{"repo_as_boolean", map[string]any{"owner": "o", "repo": true, "path": "p", "content": "c", "message": "m", "branch": "b"}},
			{"branch_as_array", map[string]any{"owner": "o", "repo": "r", "path": "p", "content": "c", "message": "m", "branch": []any{"main"}}},
			{"sha_as_object", map[string]any{"owner": "o", "repo": "r", "path": "p", "content": "c", "message": "m", "branch": "b", "sha": map[string]any{"hash": "123"}}},
			{"content_as_number", map[string]any{"owner": "o", "repo": "r", "path": "p", "content": 99.99, "message": "m", "branch": "b"}},
		}

	case "query_logs":
		return []struct {
			desc    string
			payload map[string]any
		}{
			{"missing_project_id", map[string]any{"sql": "SELECT 1"}},
			{"missing_sql", map[string]any{"project_id": "proj-1"}},
			{"project_id_as_boolean", map[string]any{"project_id": false, "sql": "SELECT 1"}},
			{"sql_as_number", map[string]any{"project_id": "proj-1", "sql": 12345}},
			{"iso_start_as_number", map[string]any{"project_id": "proj-1", "sql": "SELECT 1", "iso_timestamp_start": 99999}},
			{"iso_end_as_boolean", map[string]any{"project_id": "proj-1", "sql": "SELECT 1", "iso_timestamp_end": true}},
			{"iso_start_as_array", map[string]any{"project_id": "proj-1", "sql": "SELECT 1", "iso_timestamp_start": []any{"2026-01-01"}}},
		}

	case "create_dns_record":
		return []struct {
			desc    string
			payload map[string]any
		}{
			{"missing_zone_id", map[string]any{"type": "A", "name": "test", "content": "1.1.1.1"}},
			{"missing_type", map[string]any{"zone_id": "z1", "name": "test", "content": "1.1.1.1"}},
			{"missing_name", map[string]any{"zone_id": "z1", "type": "A", "content": "1.1.1.1"}},
			{"missing_content", map[string]any{"zone_id": "z1", "type": "A", "name": "test"}},
			{"invalid_type_enum_bogus", map[string]any{"zone_id": "z1", "type": "BOGUS_RECORD", "name": "test", "content": "1.1.1.1"}},
			{"invalid_type_enum_http", map[string]any{"zone_id": "z1", "type": "HTTP", "name": "test", "content": "1.1.1.1"}},
			{"confirm_as_string", map[string]any{"zone_id": "z1", "type": "A", "name": "test", "content": "1.1.1.1", "confirm": "yes"}},
			{"proxied_as_string", map[string]any{"zone_id": "z1", "type": "A", "name": "test", "content": "1.1.1.1", "proxied": "true"}},
			{"priority_as_string", map[string]any{"zone_id": "z1", "type": "A", "name": "test", "content": "1.1.1.1", "priority": "high"}},
			{"ttl_as_string", map[string]any{"zone_id": "z1", "type": "A", "name": "test", "content": "1.1.1.1", "ttl": "300s"}},
			{"zone_id_as_number", map[string]any{"zone_id": 99999, "type": "A", "name": "test", "content": "1.1.1.1"}},
		}

	case "emulate":
		return []struct {
			desc    string
			payload map[string]any
		}{
			{"missing_pageId", map[string]any{"colorScheme": "dark"}},
			{"pageId_as_string", map[string]any{"pageId": "42"}},
			{"pageId_as_boolean", map[string]any{"pageId": true}},
			{"colorScheme_invalid_enum", map[string]any{"pageId": 1.0, "colorScheme": "neon"}},
			{"forcedColors_invalid_enum", map[string]any{"pageId": 1.0, "forcedColors": "forced"}},
			{"networkConditions_invalid_enum", map[string]any{"pageId": 1.0, "networkConditions": "5G_Ultra"}},
			{"prefersContrast_invalid_enum", map[string]any{"pageId": 1.0, "prefersContrast": "maximum"}},
			{"prefersReducedMotion_invalid_enum", map[string]any{"pageId": 1.0, "prefersReducedMotion": "never"}},
			{"cpuThrottlingRate_as_string", map[string]any{"pageId": 1.0, "cpuThrottlingRate": "5x"}},
			{"extraHttpHeaders_as_number", map[string]any{"pageId": 1.0, "extraHttpHeaders": 1000}},
		}
	}
	return nil
}

// TestAdversarial_ArgumentGenerationAndValidationSimulation stress-tests model usability
// across all 4 production tools with 50 valid argument sets per tool (Zero Argument False Rejection)
// and verifies rejection fidelity against invalid payloads across Tiers 0-3.
func TestAdversarial_ArgumentGenerationAndValidationSimulation(t *testing.T) {
	rawTools := loadSchemasFixture(t)

	tiers := []struct {
		tier pruner.PruneTier
		name string
	}{
		{pruner.TierNone, "Tier0_Unpruned"},
		{pruner.TierLossless, "Tier1_Lossless"},
		{pruner.TierCompact, "Tier2_Compact"},
		{pruner.TierAggressive, "Tier3_Aggressive"},
	}

	// Pre-prune schemas for each tier
	prunedSchemasByTier := make(map[pruner.PruneTier]map[string]mcp.Tool)
	for _, tc := range tiers {
		engine := pruner.New(pruner.DefaultOptions(tc.tier))
		pruned, _, err := engine.PruneTools(rawTools)
		if err != nil {
			t.Fatalf("[%s] PruneTools failed: %v", tc.name, err)
		}
		// Assert 100% Draft-07 compliance on pruned schemas
		for _, tool := range pruned {
			res := pruner.ValidateTool(tool)
			if !res.Valid {
				t.Fatalf("[%s] Tool %s failed Draft-07 validation: %v", tc.name, tool.Name, res.Errors)
			}
		}
		m := make(map[string]mcp.Tool, len(pruned))
		for _, tool := range pruned {
			m[tool.Name] = tool
		}
		prunedSchemasByTier[tc.tier] = m
	}

	// ------------------------------------------------------------------------
	// PART 1: 50 Sets of Valid Argument Payloads per Tool (Zero False Rejection)
	// ------------------------------------------------------------------------
	t.Run("ValidArguments_ZeroFalseRejection", func(t *testing.T) {
		totalValidChecked := 0
		for _, rawTool := range rawTools {
			toolName := rawTool.Name
			validPayloads := Generate50ValidArguments(toolName)
			if len(validPayloads) != 50 {
				t.Fatalf("expected 50 valid payloads for %s, got %d", toolName, len(validPayloads))
			}

			for i, payload := range validPayloads {
				for _, tc := range tiers {
					targetTool := prunedSchemasByTier[tc.tier][toolName]
					err := ValidateArgumentPayload(targetTool.InputSchema, payload)
					if err != nil {
						t.Fatalf("[FALSE REJECTION BUG] Tool %q at %s failed on valid payload #%d: %v\nPayload: %+v",
							toolName, tc.name, i+1, err, payload)
					}
					totalValidChecked++
				}
			}
		}
		t.Logf("SUCCESS: Verified %d valid argument validations across 4 tiers with ZERO false rejections.", totalValidChecked)
	})

	// ------------------------------------------------------------------------
	// PART 2: Invalid Argument Payloads (Rejection Fidelity)
	// ------------------------------------------------------------------------
	t.Run("InvalidArguments_RejectionFidelity", func(t *testing.T) {
		totalInvalidChecked := 0
		for _, rawTool := range rawTools {
			toolName := rawTool.Name
			invalidCases := GenerateInvalidArguments(toolName)
			if len(invalidCases) == 0 {
				t.Fatalf("no invalid cases defined for %s", toolName)
			}

			for _, c := range invalidCases {
				for _, tc := range tiers {
					targetTool := prunedSchemasByTier[tc.tier][toolName]
					err := ValidateArgumentPayload(targetTool.InputSchema, c.payload)
					if err == nil {
						t.Fatalf("[LEAKAGE BUG] Tool %q at %s unexpectedly accepted invalid payload %q!\nPayload: %+v",
							toolName, tc.name, c.desc, c.payload)
					}
					totalInvalidChecked++
				}
			}
		}
		t.Logf("SUCCESS: Verified %d invalid argument rejections across 4 tiers with 100%% rejection fidelity.", totalInvalidChecked)
	})
}
