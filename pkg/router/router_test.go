package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func TestRouter_UnifiedPipeline(t *testing.T) {
	ctx := context.Background()

	// Initialize real pruner engine
	pruneEngine := pruner.New()

	routerCfg := router.DefaultRouterConfig()
	routerCfg.NamespaceMode = router.NamespaceModePrefixAlways
	routerCfg.PruneMode = "aggressive"

	r := router.NewRouter(routerCfg, pruneEngine, nil)
	defer func() { _ = r.Close() }()

	// Attach filter
	filterCfg := router.DefaultFilterConfig()
	filter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	r.AttachFilter(filter)

	// Attach window
	windowCfg := router.DefaultWindowConfig()
	windowCfg.MaxActive = 5
	window, err := router.NewToolWindow(windowCfg, r.Multiplexer(), nil)
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}
	r.AttachWindow(window)

	var listChangedEmitted atomic.Bool
	r.SetNotifier(func() {
		listChangedEmitted.Store(true)
	})

	// Register real upstreams
	trGitHub := newChannelTransport()
	trSupabase := newChannelTransport()

	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "github", Transport: trGitHub})
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "supabase", Transport: trSupabase})

	githubTools := []mcp.Tool{
		{
			Name:        "create_issue",
			Description: "Create a GitHub issue with title, body, and labels.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"title": map[string]any{"type": "string", "description": "Title of issue"},
					"body":  map[string]any{"type": "string", "description": "Body of issue"},
				},
				Required: []string{"title"},
			},
		},
		{
			Name:        "delete_repo",
			Description: "Permanently delete a repository.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"repo": map[string]any{"type": "string"},
				},
				Required: []string{"repo"},
			},
		},
	}

	supabaseTools := []mcp.Tool{
		{
			Name:        "query_logs",
			Description: "Query unified project logs using SQL.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"sql": map[string]any{"type": "string", "description": "SQL statement"},
				},
				Required: []string{"sql"},
			},
		},
	}

	r.Multiplexer().SyncUpstreamTools(ctx, "github", githubTools)
	r.Multiplexer().SyncUpstreamTools(ctx, "supabase", supabaseTools)

	t.Run("HandleToolsList includes windowed tools and meta tools", func(t *testing.T) {
		reqID := mcp.NewIntID(1)
		listReq, _ := mcp.NewRequest(reqID, "tools/list", nil)

		// Initially window has no active tools, so only meta-tools should be returned
		resp, err := r.HandleToolsList(ctx, listReq)
		if err != nil {
			t.Fatalf("HandleToolsList failed: %v", err)
		}

		var listRes mcp.ListToolsResult
		if err := json.Unmarshal(resp.Result, &listRes); err != nil {
			t.Fatalf("unmarshal list result failed: %v", err)
		}

		// Verify meta tools are present
		hasSearch := false
		for _, tool := range listRes.Tools {
			if tool.Name == "toolprune_search_tools" {
				hasSearch = true
			}
		}
		if !hasSearch {
			t.Errorf("expected toolprune_search_tools in tools/list response")
		}

		// Promote a tool into active window
		_, _, err = window.Promote("github__create_issue")
		if err != nil {
			t.Fatalf("Promote failed: %v", err)
		}

		// Now tools/list must include github__create_issue
		resp, err = r.HandleToolsList(ctx, listReq)
		if err != nil {
			t.Fatalf("HandleToolsList after promote failed: %v", err)
		}
		if err := json.Unmarshal(resp.Result, &listRes); err != nil {
			t.Fatalf("unmarshal list result failed: %v", err)
		}

		hasCreateIssue := false
		for _, tool := range listRes.Tools {
			if tool.Name == "github__create_issue" {
				hasCreateIssue = true
				// Check that schema pruning was applied
				if tool.InputSchema.Type != "object" {
					t.Errorf("schema type must be object")
				}
			}
		}
		if !hasCreateIssue {
			t.Errorf("expected promoted github__create_issue in tools/list response")
		}
	})

	t.Run("RouteToolCall rewrites tool name and routes to correct transport", func(t *testing.T) {
		callParams := mcp.CallToolRequestParams{
			Name:      "github__create_issue",
			Arguments: json.RawMessage(`{"title": "Bug Report"}`),
		}
		paramsJSON, _ := json.Marshal(callParams)
		callReq, _ := mcp.NewRequest(mcp.NewIntID(42), "tools/call", json.RawMessage(paramsJSON))

		targetTr, rewrittenMsg, err := r.RouteToolCall(callReq)
		if err != nil {
			t.Fatalf("RouteToolCall failed: %v", err)
		}
		if targetTr != trGitHub {
			t.Errorf("expected target transport to be trGitHub")
		}

		// Verify parameter rewriting: Name rewritten from "github__create_issue" to "create_issue"
		var rewrittenParams mcp.CallToolRequestParams
		if err := json.Unmarshal(rewrittenMsg.Params, &rewrittenParams); err != nil {
			t.Fatalf("unmarshal rewritten params failed: %v", err)
		}
		if rewrittenParams.Name != "create_issue" {
			t.Errorf("expected rewritten name 'create_issue', got %q", rewrittenParams.Name)
		}
		if string(rewrittenParams.Arguments) != `{"title":"Bug Report"}` {
			t.Errorf("arguments were corrupted: %s", string(rewrittenParams.Arguments))
		}
	})

	t.Run("RouteToolCall auto-promotes dormant tool into window", func(t *testing.T) {
		if window.IsActive("supabase__query_logs") {
			t.Fatalf("supabase__query_logs should be dormant initially")
		}

		callParams := mcp.CallToolRequestParams{
			Name:      "supabase__query_logs",
			Arguments: json.RawMessage(`{"sql": "SELECT 1"}`),
		}
		paramsJSON, _ := json.Marshal(callParams)
		callReq, _ := mcp.NewRequest(mcp.NewIntID(43), "tools/call", json.RawMessage(paramsJSON))

		targetTr, _, err := r.RouteToolCall(callReq)
		if err != nil {
			t.Fatalf("RouteToolCall failed: %v", err)
		}
		if targetTr != trSupabase {
			t.Errorf("expected target transport to be trSupabase")
		}

		// Tool must now be active in window due to auto-promotion
		if !window.IsActive("supabase__query_logs") {
			t.Errorf("expected supabase__query_logs to be auto-promoted into window")
		}
	})

	t.Run("RouteToolCall blocks destructive tools when profile is read_only", func(t *testing.T) {
		// Switch profile to read_only
		if err := r.SetProfile(router.ProfileReadOnly); err != nil {
			t.Fatalf("SetProfile failed: %v", err)
		}

		callParams := mcp.CallToolRequestParams{
			Name:      "github__delete_repo",
			Arguments: json.RawMessage(`{"repo": "my-repo"}`),
		}
		paramsJSON, _ := json.Marshal(callParams)
		callReq, _ := mcp.NewRequest(mcp.NewIntID(44), "tools/call", json.RawMessage(paramsJSON))

		_, _, err := r.RouteToolCall(callReq)
		if err == nil {
			t.Fatalf("expected error routing delete_repo under read_only profile")
		}

		var mcpErr *mcp.JSONRPCError
		if !errors.As(err, &mcpErr) {
			t.Fatalf("expected mcp.JSONRPCError type, got %T: %v", err, err)
		}
		if mcpErr.Code != mcp.CodeMethodNotFound {
			t.Errorf("expected CodeMethodNotFound (-32601), got %d", mcpErr.Code)
		}
		if !strings.Contains(mcpErr.Message, "blocked by active profile") {
			t.Errorf("expected 'blocked by active profile' in error message, got: %s", mcpErr.Message)
		}

		// Restore profile to full
		_ = r.SetProfile(router.ProfileFull)
	})

	t.Run("RouteToolCall errors on unknown tool", func(t *testing.T) {
		callParams := mcp.CallToolRequestParams{
			Name: "unknown__tool",
		}
		paramsJSON, _ := json.Marshal(callParams)
		callReq, _ := mcp.NewRequest(mcp.NewIntID(45), "tools/call", json.RawMessage(paramsJSON))

		_, _, err := r.RouteToolCall(callReq)
		if err == nil {
			t.Fatalf("expected error for unknown tool")
		}
		var mcpErr *mcp.JSONRPCError
		if !errors.As(err, &mcpErr) || mcpErr.Code != mcp.CodeMethodNotFound {
			t.Errorf("expected CodeMethodNotFound for unknown tool, got %v", err)
		}
	})

	t.Run("HandleInternalToolCall executes search and activate meta-tools", func(t *testing.T) {
		// 1. Search meta-tool
		searchParams := mcp.CallToolRequestParams{
			Name:      "toolprune_search_tools",
			Arguments: json.RawMessage(`{"query": "issue"}`),
		}
		searchJSON, _ := json.Marshal(searchParams)
		reqID := mcp.NewIntID(99)
		searchReq, _ := mcp.NewRequest(reqID, "tools/call", json.RawMessage(searchJSON))

		resp, handled := r.HandleInternalToolCall(searchReq)
		if !handled || resp == nil {
			t.Fatalf("expected toolprune_search_tools to be handled internally")
		}

		var callRes mcp.CallToolResult
		if err := json.Unmarshal(resp.Result, &callRes); err != nil {
			t.Fatalf("unmarshal search result failed: %v", err)
		}
		if len(callRes.Content) != 1 || !strings.Contains(callRes.Content[0].Text, "github__create_issue") {
			t.Errorf("expected search result to contain github__create_issue, got: %+v", callRes.Content)
		}

		// 2. Activate meta-tool
		activateParams := mcp.CallToolRequestParams{
			Name:      "toolprune_activate_tools",
			Arguments: json.RawMessage(`{"tools": ["github__delete_repo"]}`),
		}
		actJSON, _ := json.Marshal(activateParams)
		actReq, _ := mcp.NewRequest(mcp.NewIntID(100), "tools/call", json.RawMessage(actJSON))

		resp, handled = r.HandleInternalToolCall(actReq)
		if !handled || resp == nil {
			t.Fatalf("expected toolprune_activate_tools to be handled internally")
		}
		if !window.IsActive("github__delete_repo") {
			t.Errorf("expected github__delete_repo to be activated into window")
		}

		// 3. Normal tool is not handled internally
		normalParams := mcp.CallToolRequestParams{Name: "github__create_issue"}
		normJSON, _ := json.Marshal(normalParams)
		normReq, _ := mcp.NewRequest(mcp.NewIntID(101), "tools/call", json.RawMessage(normJSON))
		_, handled = r.HandleInternalToolCall(normReq)
		if handled {
			t.Errorf("regular tool should NOT be handled internally")
		}
	})

	t.Run("concurrent router operations", func(t *testing.T) {
		var wg sync.WaitGroup
		const numGoroutines = 25
		const iters = 40

		for g := 0; g < numGoroutines; g++ {
			wg.Add(1)
			go func(gid int) {
				defer wg.Done()
				for i := 0; i < iters; i++ {
					reqID := mcp.NewIntID(int64(gid*iters + i))

					// Tools list
					listReq, _ := mcp.NewRequest(reqID, "tools/list", nil)
					_, _ = r.HandleToolsList(ctx, listReq)

					// Tool call
					callParams := mcp.CallToolRequestParams{
						Name:      "github__create_issue",
						Arguments: json.RawMessage(`{"title": "Concurrent"}`),
					}
					paramsJSON, _ := json.Marshal(callParams)
					callReq, _ := mcp.NewRequest(reqID, "tools/call", json.RawMessage(paramsJSON))
					_, _, _ = r.RouteToolCall(callReq)

					// Internal meta call
					searchParams := mcp.CallToolRequestParams{
						Name:      "toolprune_search_tools",
						Arguments: json.RawMessage(`{"query": "logs"}`),
					}
					sJSON, _ := json.Marshal(searchParams)
					sReq, _ := mcp.NewRequest(reqID, "tools/call", json.RawMessage(sJSON))
					_, _ = r.HandleInternalToolCall(sReq)

					if i%10 == 0 {
						_ = r.SetProfile(router.ProfileFull)
					}
				}
			}(g)
		}

		wg.Wait()
	})
}

func TestRouter_MetaToolsEnforceFilterAndInvoke(t *testing.T) {
	ctx := context.Background()
	pruneEngine := pruner.New()

	cfg := router.DefaultRouterConfig()
	cfg.NamespaceMode = router.NamespaceModePrefixAlways
	cfg.PruneMode = "dynamic"
	cfg.EnableCatalog = true

	r := router.NewRouter(cfg, pruneEngine, nil)
	defer func() { _ = r.Close() }()

	filterCfg := router.DefaultFilterConfig()
	filterCfg.ActiveProfile = router.ProfileReadOnly
	filter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		t.Fatalf("NewToolFilter failed: %v", err)
	}
	r.AttachFilter(filter)

	winCfg := router.DefaultWindowConfig()
	winCfg.MaxActive = 4
	win, err := router.NewToolWindow(winCfg, r.Multiplexer(), nil)
	if err != nil {
		t.Fatalf("NewToolWindow failed: %v", err)
	}
	r.AttachWindow(win)

	tr := newChannelTransport()
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "github", Transport: tr})
	r.Multiplexer().SyncUpstreamTools(ctx, "github", []mcp.Tool{
		{
			Name:        "get_issue",
			Description: "Fetch a repository issue by number.",
			InputSchema: mcp.InputSchema{
				Type:       "object",
				Properties: map[string]any{"issue_number": map[string]any{"type": "integer"}},
				Required:   []string{"issue_number"},
			},
		},
		{
			Name:        "delete_repo",
			Description: "Permanently delete a repository issue/repo.",
			InputSchema: mcp.InputSchema{
				Type:       "object",
				Properties: map[string]any{"repo": map[string]any{"type": "string"}},
				Required:   []string{"repo"},
			},
		},
	})

	// 1. toolprune_search_tools must NOT return delete_repo under read_only profile
	searchPayload, _ := json.Marshal(mcp.CallToolRequestParams{
		Name:      router.MetaToolSearch,
		Arguments: json.RawMessage(`{"query":"repo"}`),
	})
	searchReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodToolsCall, json.RawMessage(searchPayload))
	searchResp, handled := r.HandleInternalToolCall(searchReq)
	if !handled || searchResp == nil {
		t.Fatal("expected toolprune_search_tools to be handled")
	}
	var searchRes mcp.CallToolResult
	_ = json.Unmarshal(searchResp.Result, &searchRes)
	if strings.Contains(searchRes.Content[0].Text, "delete_repo") {
		t.Fatalf("SECURITY VIOLATION: toolprune_search_tools leaked blocked tool delete_repo: %s", searchRes.Content[0].Text)
	}
	if !strings.Contains(searchRes.Content[0].Text, "github__get_issue") {
		t.Fatalf("expected allowed tool github__get_issue in search results, got: %s", searchRes.Content[0].Text)
	}

	// 2. toolprune_activate_tools must NOT promote delete_repo under read_only profile
	actPayload, _ := json.Marshal(mcp.CallToolRequestParams{
		Name:      router.MetaToolActivate,
		Arguments: json.RawMessage(`{"tools":["github__delete_repo","github__get_issue"]}`),
	})
	actReq, _ := mcp.NewRequest(mcp.NewIntID(2), mcp.MethodToolsCall, json.RawMessage(actPayload))
	_, handled = r.HandleInternalToolCall(actReq)
	if !handled {
		t.Fatal("expected toolprune_activate_tools to be handled")
	}
	if win.IsActive("github__delete_repo") {
		t.Fatal("SECURITY VIOLATION: toolprune_activate_tools promoted blocked tool github__delete_repo!")
	}
	if !win.IsActive("github__get_issue") {
		t.Fatal("expected allowed tool github__get_issue to be promoted")
	}

	// 3. toolprune_get_tool_schema returns schema for allowed tool and blocks delete_repo
	schemaPayload, _ := json.Marshal(mcp.CallToolRequestParams{
		Name:      router.MetaToolGetSchema,
		Arguments: json.RawMessage(`{"tool_names":["github__get_issue","github__delete_repo"]}`),
	})
	schemaReq, _ := mcp.NewRequest(mcp.NewIntID(3), mcp.MethodToolsCall, json.RawMessage(schemaPayload))
	schemaResp, handled := r.HandleInternalToolCall(schemaReq)
	if !handled || schemaResp == nil {
		t.Fatal("expected toolprune_get_tool_schema to be handled")
	}
	var schemaRes mcp.CallToolResult
	_ = json.Unmarshal(schemaResp.Result, &schemaRes)
	if !strings.Contains(schemaRes.Content[0].Text, "issue_number") {
		t.Fatalf("expected hydrated schema for github__get_issue, got: %s", schemaRes.Content[0].Text)
	}
	if !strings.Contains(schemaRes.Content[0].Text, "Blocked by active profile") {
		t.Fatalf("expected blocked notice for github__delete_repo, got: %s", schemaRes.Content[0].Text)
	}

	// 4. toolprune_invoke rejects invalid args before upstream, and unwraps valid args cleanly
	badInvokePayload, _ := json.Marshal(mcp.CallToolRequestParams{
		Name:      router.MetaToolInvoke,
		Arguments: json.RawMessage(`{"tool_name":"github__get_issue","arguments":{"issue_number":"not_an_int"}}`),
	})
	badInvokeReq, _ := mcp.NewRequest(mcp.NewIntID(4), mcp.MethodToolsCall, json.RawMessage(badInvokePayload))
	badInvokeResp, handled := r.HandleInternalToolCall(badInvokeReq)
	if !handled || badInvokeResp == nil {
		t.Fatal("expected invalid toolprune_invoke call to be rejected before upstream")
	}
	var badInvokeRes mcp.CallToolResult
	_ = json.Unmarshal(badInvokeResp.Result, &badInvokeRes)
	if !badInvokeRes.IsError {
		t.Fatal("expected IsError=true on invalid toolprune_invoke arguments")
	}

	goodInvokePayload, _ := json.Marshal(mcp.CallToolRequestParams{
		Name:      router.MetaToolInvoke,
		Arguments: json.RawMessage(`{"tool_name":"github__get_issue","arguments":{"issue_number":42}}`),
	})
	goodInvokeReq, _ := mcp.NewRequest(mcp.NewIntID(5), mcp.MethodToolsCall, json.RawMessage(goodInvokePayload))
	_, handled = r.HandleInternalToolCall(goodInvokeReq)
	if handled {
		t.Fatal("expected valid toolprune_invoke to fall through to RouteToolCall")
	}
	targetTr, rewritten, err := r.RouteToolCall(goodInvokeReq)
	if err != nil || targetTr != tr {
		t.Fatalf("RouteToolCall for toolprune_invoke failed: %v", err)
	}
	var unwrapped mcp.CallToolRequestParams
	_ = json.Unmarshal(rewritten.Params, &unwrapped)
	if unwrapped.Name != "get_issue" {
		t.Fatalf("expected unwrapped tool name 'get_issue', got %q", unwrapped.Name)
	}
}

func TestRouterGateway_MultiUpstreamStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	pruneEngine := pruner.New()

	rCfg := router.DefaultRouterConfig()
	rCfg.NamespaceMode = router.NamespaceModePrefixAlways
	rCfg.PruneMode = "dynamic"
	rCfg.EnableCatalog = true

	r := router.NewRouter(rCfg, pruneEngine, nil)
	gw := router.NewRouterGateway(r, router.GatewayConfig{
		RequestTimeout:  5 * time.Second,
		InjectProxyInfo: true,
		ServerVersion:   "1.2.0",
	})
	defer gw.Shutdown()

	trA := newChannelTransport()
	trB := newChannelTransport()
	// Echo responses from both upstreams asynchronously
	for _, tr := range []*channelTransport{trA, trB} {
		tRef := tr
		go func() {
			for {
				select {
				case <-tRef.closec:
					return
				case msg, ok := <-tRef.out:
					if !ok {
						return
					}
					if msg != nil && msg.ID != nil {
						resp, _ := mcp.NewResponse(*msg.ID, mcp.NewToolTextResult("ok:"+string(msg.Params)))
						tRef.in <- resp
					}
				}
			}
		}()
	}

	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "gh", Transport: trA})
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "db", Transport: trB})
	r.Multiplexer().SyncUpstreamTools(ctx, "gh", []mcp.Tool{
		{
			Name:        "get_pr",
			Description: "Get pull request details.",
			InputSchema: mcp.InputSchema{
				Type:       "object",
				Properties: map[string]any{"pr": map[string]any{"type": "integer"}},
				Required:   []string{"pr"},
			},
		},
	})
	r.Multiplexer().SyncUpstreamTools(ctx, "db", []mcp.Tool{
		{
			Name:        "list_tables",
			Description: "List SQL tables.",
			InputSchema: mcp.InputSchema{Type: "object"},
		},
	})

	if err := gw.Start(); err != nil {
		t.Fatalf("gw.Start failed: %v", err)
	}

	sseSrv := proxy.NewSSEServerWithDispatcher(proxy.DefaultSSEServerConfig(), gw)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// POST /mcp tools/list
	listResp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("POST /mcp tools/list failed: %v", err)
	}
	defer listResp.Body.Close()
	sessionID := listResp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("expected Mcp-Session-Id header")
	}

	var rpcList mcp.Message
	if err := json.NewDecoder(listResp.Body).Decode(&rpcList); err != nil {
		t.Fatalf("decode tools/list response failed: %v", err)
	}
	var listRes mcp.ListToolsResult
	_ = json.Unmarshal(rpcList.Result, &listRes)
	if len(listRes.Tools) < 6 { // 2 compact tools + 4 meta-tools
		t.Fatalf("expected 2 compact tools + 4 meta-tools, got %d", len(listRes.Tools))
	}

	// POST /mcp tools/call across gh upstream
	callReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"gh__get_pr","arguments":{"pr":10}}}`))
	callReq.Header.Set("Content-Type", "application/json")
	callReq.Header.Set("Mcp-Session-Id", sessionID)
	callResp, err := http.DefaultClient.Do(callReq)
	if err != nil {
		t.Fatalf("POST /mcp tools/call failed: %v", err)
	}
	defer callResp.Body.Close()

	var rpcCall mcp.Message
	_ = json.NewDecoder(callResp.Body).Decode(&rpcCall)
	if rpcCall.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcCall.Error)
	}
	if !strings.Contains(string(rpcCall.Result), "get_pr") {
		t.Fatalf("expected upstream response containing rewritten 'get_pr', got %s", string(rpcCall.Result))
	}
}
