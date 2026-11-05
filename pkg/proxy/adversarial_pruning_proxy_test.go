package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// loadRealFixtures loads the 4 production MCP tools from test/fixtures/schemas.json.
func loadRealFixtures(t *testing.T) []mcp.Tool {
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
		t.Fatalf("expected exactly 4 tools in fixtures, got %d", len(tools))
	}
	return tools
}

// InMemClientSession simulates an in-memory client connection.
type InMemClientSession struct {
	id       string
	ctx      context.Context
	cancel   context.CancelFunc
	inbound  chan *mcp.Message
	mu       sync.Mutex
	messages []*mcp.Message
}

func NewInMemClientSession(id string) *InMemClientSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &InMemClientSession{
		id:      id,
		ctx:     ctx,
		cancel:  cancel,
		inbound: make(chan *mcp.Message, 50),
	}
}

func (s *InMemClientSession) ID() string {
	return s.id
}

func (s *InMemClientSession) Context() context.Context {
	return s.ctx
}

func (s *InMemClientSession) Send(msg *mcp.Message) error {
	s.mu.Lock()
	s.messages = append(s.messages, msg)
	s.mu.Unlock()

	select {
	case s.inbound <- msg:
		return nil
	case <-s.ctx.Done():
		return io.ErrClosedPipe
	}
}

func (s *InMemClientSession) WaitMessage(t *testing.T, timeout time.Duration) *mcp.Message {
	t.Helper()
	select {
	case msg := <-s.inbound:
		return msg
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for message from proxy to client %s", s.id)
		return nil
	}
}

// InMemUpstreamTransport simulates a live MCP upstream server serving real tools.
type InMemUpstreamTransport struct {
	mu           sync.Mutex
	tools        []mcp.Tool
	nextCursor   string
	forceError   bool
	toUpstream   chan *mcp.Message
	fromUpstream chan *mcp.Message
	closed       bool
}

func NewInMemUpstreamTransport(tools []mcp.Tool) *InMemUpstreamTransport {
	return &InMemUpstreamTransport{
		tools:        tools,
		toUpstream:   make(chan *mcp.Message, 50),
		fromUpstream: make(chan *mcp.Message, 50),
	}
}

func (u *InMemUpstreamTransport) SetNextCursor(cursor string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.nextCursor = cursor
}

func (u *InMemUpstreamTransport) SetForceError(enable bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.forceError = enable
}

func (u *InMemUpstreamTransport) Send(msg *mcp.Message) error {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return io.ErrClosedPipe
	}
	u.mu.Unlock()

	// Process message in upstream server logic
	go u.handleUpstreamRequest(msg)
	return nil
}

func (u *InMemUpstreamTransport) handleUpstreamRequest(msg *mcp.Message) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.forceError {
		errMsg := mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, "simulated upstream database failure")
		u.fromUpstream <- errMsg
		return
	}

	switch msg.Method {
	case mcp.MethodInitialize:
		initResult := mcp.InitializeResult{
			ProtocolVersion: "2024-11-05",
			ServerInfo: mcp.Implementation{
				Name:    "real-upstream-testserver",
				Version: "1.0.0",
			},
			Instructions: "Production MCP Upstream Server",
		}
		resp, _ := mcp.NewResponse(*msg.ID, initResult)
		u.fromUpstream <- resp

	case mcp.MethodToolsList:
		listResult := mcp.ListToolsResult{
			Tools:      u.tools,
			NextCursor: u.nextCursor,
		}
		resp, _ := mcp.NewResponse(*msg.ID, listResult)
		u.fromUpstream <- resp

	case mcp.MethodToolsCall:
		var callParams mcp.CallToolRequestParams
		_ = json.Unmarshal(msg.Params, &callParams)
		callResult := mcp.CallToolResult{
			Content: []mcp.ContentItem{
				{
					Type: "text",
					Text: fmt.Sprintf("executed tool %s successfully with arguments", callParams.Name),
				},
			},
		}
		resp, _ := mcp.NewResponse(*msg.ID, callResult)
		u.fromUpstream <- resp

	case mcp.MethodPing:
		resp, _ := mcp.NewResponse(*msg.ID, map[string]string{"status": "pong"})
		u.fromUpstream <- resp

	default:
		errMsg := mcp.NewErrorMessage(msg.ID, mcp.CodeMethodNotFound, "method not found: "+msg.Method)
		u.fromUpstream <- errMsg
	}
}

func (u *InMemUpstreamTransport) Receive() (*mcp.Message, error) {
	msg, ok := <-u.fromUpstream
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (u *InMemUpstreamTransport) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.closed {
		u.closed = true
		close(u.fromUpstream)
	}
	return nil
}

// TestAdversarial_ProxyPruningInterception validates end-to-end proxy interception,
// tier-based schema pruning, token savings targets, pagination preservation,
// bidirectional execution pass-through, and dynamic runtime reconfiguration.
func TestAdversarial_ProxyPruningInterception(t *testing.T) {
	tools := loadRealFixtures(t)
	rawTokens := pruner.EstimateToolsTokens(tools)

	// ------------------------------------------------------------------------
	// Test 1: Pruning Interception across all 4 Tiers
	// ------------------------------------------------------------------------
	tierScenarios := []struct {
		name         string
		pruneMode    string
		tier         pruner.PruneTier
		minSavings   float64
		expectedDesc string
	}{
		{
			name:       "Tier0_PassthroughNone",
			pruneMode:  "none",
			tier:       pruner.TierNone,
			minSavings: 0.0,
		},
		{
			name:       "Tier1_LosslessMinification",
			pruneMode:  "lossless",
			tier:       pruner.TierLossless,
			minSavings: 0.02, // >= 2% savings
		},
		{
			name:       "Tier2_DescriptionCondensation",
			pruneMode:  "compact",
			tier:       pruner.TierCompact,
			minSavings: 0.30, // >= 30% savings
		},
		{
			name:       "Tier3_ParameterPruningAggressive",
			pruneMode:  "aggressive",
			tier:       pruner.TierAggressive,
			minSavings: 0.45, // >= 45% savings
		},
	}

	for _, tc := range tierScenarios {
		t.Run(tc.name, func(t *testing.T) {
			upstream := NewInMemUpstreamTransport(tools)
			engine := pruner.New(pruner.DefaultOptions(tc.tier))

			cfg := proxy.DefaultProxyConfig()
			cfg.PruneMode = tc.pruneMode
			prx := proxy.NewProxy(cfg, upstream, engine)
			prx.Start()
			defer prx.Shutdown()

			client := NewInMemClientSession("client-session-1")
			prx.RegisterClient(client)

			// Client sends tools/list request
			reqID := mcp.NewStringID(fmt.Sprintf("req-%s", tc.pruneMode))
			req, err := mcp.NewRequest(reqID, mcp.MethodToolsList, nil)
			if err != nil {
				t.Fatalf("failed to create tools/list request: %v", err)
			}

			if err := prx.HandleClientMessage(client, req); err != nil {
				t.Fatalf("HandleClientMessage failed: %v", err)
			}

			// Receive intercepted response
			resp := client.WaitMessage(t, 2*time.Second)
			if resp.ID == nil || resp.ID.String() != reqID.String() {
				t.Fatalf("response ID mismatch: expected %v, got %v", reqID, resp.ID)
			}
			if resp.Error != nil {
				t.Fatalf("unexpected error response from proxy: %v", resp.Error)
			}

			// Unmarshal intercepted tools result
			var listResult mcp.ListToolsResult
			if err := json.Unmarshal(resp.Result, &listResult); err != nil {
				t.Fatalf("failed to unmarshal intercepted result: %v", err)
			}

			if len(listResult.Tools) != 4 {
				t.Fatalf("expected 4 intercepted tools, got %d", len(listResult.Tools))
			}

			// Assert 100% Draft-07 compliance on delivered tools
			for _, tool := range listResult.Tools {
				res := pruner.ValidateTool(tool)
				if !res.Valid {
					t.Fatalf("[%s] Delivered tool %q failed Draft-07 validation: %v", tc.name, tool.Name, res.Errors)
				}
			}

			// Measure token reduction
			prunedTokens := pruner.EstimateToolsTokens(listResult.Tools)
			var savings float64
			if rawTokens > 0 {
				savings = float64(rawTokens-prunedTokens) / float64(rawTokens)
			}

			t.Logf("[%s] RawTokens=%d PrunedTokens=%d ActualSavings=%.1f%% (MinRequired=%.1f%%)",
				tc.name, rawTokens, prunedTokens, savings*100, tc.minSavings*100)

			if savings < tc.minSavings {
				t.Fatalf("[%s] savings %.3f below minimum required %.3f", tc.name, savings, tc.minSavings)
			}

			// Structural checks per tier
			switch tc.tier {
			case pruner.TierNone:
				// $schema must still be present
				if listResult.Tools[0].InputSchema.Schema == "" {
					t.Fatalf("TierNone must preserve $schema keyword")
				}
			case pruner.TierLossless:
				// $schema must be stripped
				if listResult.Tools[0].InputSchema.Schema != "" {
					t.Fatalf("TierLossless must strip $schema keyword")
				}
				// Parameter descriptions must remain unstripped
				prop := listResult.Tools[0].InputSchema.Properties["owner"].(map[string]any)
				if _, hasDesc := prop["description"]; !hasDesc {
					t.Fatalf("TierLossless must preserve property descriptions")
				}
			case pruner.TierAggressive:
				// Self-evident parameter descriptions must be stripped
				prop := listResult.Tools[0].InputSchema.Properties["owner"].(map[string]any)
				if desc, hasDesc := prop["description"]; hasDesc && desc != "" {
					t.Fatalf("TierAggressive must strip self-evident parameter description, got: %v", desc)
				}
			}
		})
	}

	// ------------------------------------------------------------------------
	// Test 2: NextCursor Pagination Preservation in Interception
	// ------------------------------------------------------------------------
	t.Run("PaginationNextCursorPreserved", func(t *testing.T) {
		upstream := NewInMemUpstreamTransport(tools)
		upstream.SetNextCursor("cursor_token_page_abc123")

		engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
		cfg := proxy.DefaultProxyConfig()
		cfg.PruneMode = "aggressive"
		prx := proxy.NewProxy(cfg, upstream, engine)
		prx.Start()
		defer prx.Shutdown()

		client := NewInMemClientSession("client-pagination")
		prx.RegisterClient(client)

		reqID := mcp.NewStringID("req-pagination")
		req, _ := mcp.NewRequest(reqID, mcp.MethodToolsList, map[string]string{"cursor": "prev_page"})
		_ = prx.HandleClientMessage(client, req)

		resp := client.WaitMessage(t, 2*time.Second)
		var listResult mcp.ListToolsResult
		if err := json.Unmarshal(resp.Result, &listResult); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}

		if listResult.NextCursor != "cursor_token_page_abc123" {
			t.Fatalf("expected NextCursor 'cursor_token_page_abc123', got: %q", listResult.NextCursor)
		}
		if len(listResult.Tools) != 4 {
			t.Fatalf("expected 4 tools, got %d", len(listResult.Tools))
		}
	})

	// ------------------------------------------------------------------------
	// Test 3: Bidirectional Transparent Tool Call Execution
	// ------------------------------------------------------------------------
	t.Run("BidirectionalToolCallPassThrough", func(t *testing.T) {
		upstream := NewInMemUpstreamTransport(tools)
		engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))

		cfg := proxy.DefaultProxyConfig()
		prx := proxy.NewProxy(cfg, upstream, engine)
		prx.Start()
		defer prx.Shutdown()

		client := NewInMemClientSession("client-call")
		prx.RegisterClient(client)

		// 1. Get pruned tools first
		reqIDList := mcp.NewStringID("req-list-before-call")
		reqList, _ := mcp.NewRequest(reqIDList, mcp.MethodToolsList, nil)
		_ = prx.HandleClientMessage(client, reqList)
		respList := client.WaitMessage(t, 2*time.Second)
		if respList.Error != nil {
			t.Fatalf("unexpected list error: %v", respList.Error)
		}

		// 2. Client executes tools/call for query_logs with valid arguments
		reqIDCall := mcp.NewStringID("req-call-1")
		callArgs := map[string]any{
			"project_id": "proj-production-01",
			"sql":        "SELECT count(*) FROM edge_logs WHERE status >= 500",
		}
		argsBytes, _ := json.Marshal(callArgs)
		callParams := mcp.CallToolRequestParams{
			Name:      "query_logs",
			Arguments: argsBytes,
		}
		callReq, _ := mcp.NewRequest(reqIDCall, mcp.MethodToolsCall, callParams)

		if err := prx.HandleClientMessage(client, callReq); err != nil {
			t.Fatalf("HandleClientMessage tools/call failed: %v", err)
		}

		// 3. Receive call result back from proxy
		callResp := client.WaitMessage(t, 2*time.Second)
		if callResp.ID == nil || callResp.ID.String() != reqIDCall.String() {
			t.Fatalf("call response ID mismatch: expected %v, got %v", reqIDCall, callResp.ID)
		}
		if callResp.Error != nil {
			t.Fatalf("unexpected call error: %v", callResp.Error)
		}

		var callResult mcp.CallToolResult
		if err := json.Unmarshal(callResp.Result, &callResult); err != nil {
			t.Fatalf("failed to unmarshal call result: %v", err)
		}
		if len(callResult.Content) == 0 || !strings.Contains(callResult.Content[0].Text, "executed tool query_logs successfully") {
			t.Fatalf("unexpected call content: %+v", callResult.Content)
		}
	})

	// ------------------------------------------------------------------------
	// Test 4: Dynamic Runtime Tier Reconfiguration
	// ------------------------------------------------------------------------
	t.Run("DynamicRuntimeTierSwitching", func(t *testing.T) {
		upstream := NewInMemUpstreamTransport(tools)
		engine := pruner.New(pruner.DefaultOptions(pruner.TierLossless))

		cfg := proxy.DefaultProxyConfig()
		cfg.PruneMode = "lossless"
		prx := proxy.NewProxy(cfg, upstream, engine)
		prx.Start()
		defer prx.Shutdown()

		client := NewInMemClientSession("client-dynamic")
		prx.RegisterClient(client)

		// First request with TierLossless
		reqID1 := mcp.NewStringID("req-lossless")
		req1, _ := mcp.NewRequest(reqID1, mcp.MethodToolsList, nil)
		_ = prx.HandleClientMessage(client, req1)
		resp1 := client.WaitMessage(t, 2*time.Second)

		var res1 mcp.ListToolsResult
		_ = json.Unmarshal(resp1.Result, &res1)
		prop1 := res1.Tools[0].InputSchema.Properties["owner"].(map[string]any)
		if _, hasDesc := prop1["description"]; !hasDesc {
			t.Fatalf("expected description present in lossless mode")
		}

		// Dynamically switch engine to TierAggressive
		engine.SetOptions(pruner.DefaultOptions(pruner.TierAggressive))

		// Second request should instantly deliver aggressively pruned tools
		reqID2 := mcp.NewStringID("req-aggressive")
		req2, _ := mcp.NewRequest(reqID2, mcp.MethodToolsList, nil)
		_ = prx.HandleClientMessage(client, req2)
		resp2 := client.WaitMessage(t, 2*time.Second)

		var res2 mcp.ListToolsResult
		_ = json.Unmarshal(resp2.Result, &res2)
		prop2 := res2.Tools[0].InputSchema.Properties["owner"].(map[string]any)
		if desc, hasDesc := prop2["description"]; hasDesc && desc != "" {
			t.Fatalf("expected self-evident description stripped after dynamic tier switch to aggressive, got: %v", desc)
		}
	})

	// ------------------------------------------------------------------------
	// Test 5: Upstream Error Clean Passthrough
	// ------------------------------------------------------------------------
	t.Run("UpstreamErrorCleanPassthrough", func(t *testing.T) {
		upstream := NewInMemUpstreamTransport(tools)
		upstream.SetForceError(true)

		engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
		cfg := proxy.DefaultProxyConfig()
		prx := proxy.NewProxy(cfg, upstream, engine)
		prx.Start()
		defer prx.Shutdown()

		client := NewInMemClientSession("client-err")
		prx.RegisterClient(client)

		reqID := mcp.NewStringID("req-failing")
		req, _ := mcp.NewRequest(reqID, mcp.MethodToolsList, nil)
		_ = prx.HandleClientMessage(client, req)

		resp := client.WaitMessage(t, 2*time.Second)
		if resp.Error == nil {
			t.Fatalf("expected error from proxy when upstream fails, got success")
		}
		if resp.Error.Code != mcp.CodeInternalError || !strings.Contains(resp.Error.Message, "simulated upstream database failure") {
			t.Fatalf("unexpected error response: %v", resp.Error)
		}
	})
}
