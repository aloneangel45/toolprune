package pruner_test

import (
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

func TestStripMarkdownLinks_WithPrecedingNonLinkBrackets(t *testing.T) {
	tools := []mcp.Tool{
		{
			Name:        "search_docs",
			Description: "Search documentation.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"lucene_expression": map[string]any{
						"type":        "string",
						"description": "[NOTE] Syntax follows [Lucene Query](https://lucene.apache.org/query) and [API Guide](https://example.com/guide).",
					},
				},
				Required: []string{"lucene_expression"},
			},
		},
	}

	opts := pruner.DefaultOptions(pruner.TierAggressive)
	opts.Params.MaxNonSelfEvidentLen = 200
	engine := pruner.New(opts)

	pruned, _, err := engine.PruneTools(tools)
	if err != nil {
		t.Fatalf("PruneTools failed: %v", err)
	}
	propMap := pruned[0].InputSchema.Properties["lucene_expression"].(map[string]any)
	desc, _ := propMap["description"].(string)

	if strings.Contains(desc, "https://") {
		t.Errorf("expected markdown link URLs to be stripped even after [NOTE], got: %q", desc)
	}
	if !strings.Contains(desc, "[NOTE]") || !strings.Contains(desc, "Lucene Query") || !strings.Contains(desc, "API Guide") {
		t.Errorf("expected [NOTE] and link text preserved, got: %q", desc)
	}
}
