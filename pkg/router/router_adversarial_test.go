package router_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/router"
)

// TestAdversarial_Deadlock_WindowAndNotifier tests for lock inversion deadlock
// between ToolWindow (holding w.mu, acquiring n.mu via Trigger) and
// DebouncedNotifier (holding n.mu in Flush, acquiring w.mu via broadcaster calling ActiveTools).
func TestAdversarial_Deadlock_WindowAndNotifier(t *testing.T) {
	catalog := newSimpleCatalog()
	for i := 1; i <= 10; i++ {
		catalog.add(mcp.Tool{
			Name: fmt.Sprintf("tool_%d", i),
		})
	}

	cfg := router.WindowConfig{
		MaxActive:         5,
		AutoPromoteOnCall: true,
	}

	// Broadcaster queries the window (standard for an MCP server syncing state on list_changed)
	var winRef *router.ToolWindow

	notifier := router.NewDebouncedNotifier(func(msg *mcp.Message) error {
		if winRef != nil {
			// Querying active tools requires w.mu.RLock()
			_ = winRef.ActiveTools()
		}
		return nil
	}, 1*time.Millisecond)
	defer notifier.Stop()

	w, err := router.NewToolWindow(cfg, catalog, notifier)
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}
	winRef = w

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		// Goroutine A: Rapidly promote/touch tools (calls Trigger -> acquires n.mu while holding w.mu)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				toolName := fmt.Sprintf("tool_%d", (i%5)+1)
				w.RecordUsage(toolName)
				_, _, _ = w.Promote(toolName)
			}
		}()

		// Goroutine B: Rapidly flush notifier (acquires n.mu, then calls broadcaster which acquires w.mu)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				notifier.Flush()
				time.Sleep(200 * time.Microsecond)
			}
		}()

		wg.Wait()
	}()

	select {
	case <-done:
		// Completed without deadlock
	case <-time.After(3 * time.Second):
		t.Fatal("DEADLOCK DETECTED: ToolWindow (w.mu) and DebouncedNotifier (n.mu) deadlocked under concurrent promotion and Flush")
	}
}

// TestAdversarial_SelfDeadlock_NotifierReentrancy tests if broadcaster calling Promote
// during Flush causes a self-deadlock due to non-reentrant mutex.
func TestAdversarial_SelfDeadlock_NotifierReentrancy(t *testing.T) {
	catalog := newSimpleCatalog()
	catalog.add(mcp.Tool{Name: "tool_1"})
	catalog.add(mcp.Tool{Name: "tool_2"})

	cfg := router.WindowConfig{MaxActive: 5}
	var winRef *router.ToolWindow

	// Broadcaster receives notification and promotes a tool (e.g. meta-tool or follow-up tool)
	notifier := router.NewDebouncedNotifier(func(msg *mcp.Message) error {
		if winRef != nil {
			_, _, _ = winRef.Promote("tool_2")
		}
		return nil
	}, 1*time.Millisecond)

	w, _ := router.NewToolWindow(cfg, catalog, notifier)
	winRef = w

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = w.Promote("tool_1")
		notifier.Flush()
	}()

	select {
	case <-done:
		notifier.Stop()
	case <-time.After(500 * time.Millisecond):
		t.Errorf("DEADLOCK DETECTED: DebouncedNotifier.Flush invokes broadcaster under n.mu; broadcaster calls w.Promote which calls n.Trigger, attempting non-reentrant n.mu.Lock()")
	}
}

// TestAdversarial_WindowFilterBypass checks if HandleToolsList leaks tools that are blocked
// by the active profile when a window is attached.
func TestAdversarial_WindowFilterBypass(t *testing.T) {
	ctx := context.Background()
	pruneEngine := pruner.New()

	routerCfg := router.DefaultRouterConfig()
	routerCfg.NamespaceMode = router.NamespaceModePrefixAlways
	routerCfg.PruneMode = "none"

	r := router.NewRouter(routerCfg, pruneEngine, nil)
	defer func() { _ = r.Close() }()

	filterCfg := router.DefaultFilterConfig()
	filter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	r.AttachFilter(filter)

	windowCfg := router.DefaultWindowConfig()
	windowCfg.MaxActive = 5
	window, err := router.NewToolWindow(windowCfg, r.Multiplexer(), nil)
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}
	r.AttachWindow(window)

	tr := newChannelTransport()
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "db", Transport: tr})

	tools := []mcp.Tool{
		{Name: "read_query", Description: "Safe read query"},
		{Name: "delete_table", Description: "Destructive delete"},
		{Name: "drop_database", Description: "Destructive drop"},
	}
	r.Multiplexer().SyncUpstreamTools(ctx, "db", tools)

	// Promote tools into active window under Full profile
	_, _, _ = window.Promote("db__read_query")
	_, _, _ = window.Promote("db__delete_table")
	_, _, _ = window.Promote("db__drop_database")

	// Switch profile to read_only (must block delete_table and drop_database)
	if err := r.SetProfile(router.ProfileReadOnly); err != nil {
		t.Fatalf("SetProfile failed: %v", err)
	}

	// Call HandleToolsList
	reqID := mcp.NewIntID(1)
	listReq, _ := mcp.NewRequest(reqID, "tools/list", nil)
	resp, err := r.HandleToolsList(ctx, listReq)
	if err != nil {
		t.Fatalf("HandleToolsList failed: %v", err)
	}

	var listRes mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &listRes); err != nil {
		t.Fatalf("unmarshal list result failed: %v", err)
	}

	for _, tool := range listRes.Tools {
		if tool.Name == "db__delete_table" || tool.Name == "db__drop_database" {
			t.Errorf("SECURITY INVARIANT VIOLATION: Tool %q is present in HandleToolsList output under ProfileReadOnly!", tool.Name)
		}
	}
}

// TestAdversarial_PinnedToolEvictionInvariant tests whether promoting a pinned tool
// when unpinned count is at capacity causes an unpinned tool to be wrongfully evicted.
func TestAdversarial_PinnedToolEvictionInvariant(t *testing.T) {
	catalog := newSimpleCatalog()
	for i := 1; i <= 10; i++ {
		catalog.add(mcp.Tool{
			Name: fmt.Sprintf("unpinned_%d", i),
		})
	}
	catalog.add(mcp.Tool{Name: "my_pinned_tool"})

	cfg := router.WindowConfig{
		MaxActive:   2,
		PinnedTools: []string{"my_pinned_tool"},
	}
	w, err := router.NewToolWindow(cfg, catalog, nil)
	if err != nil {
		t.Fatalf("NewToolWindow failed: %v", err)
	}

	// Promote 2 unpinned tools to fill capacity
	_, _, err = w.Promote("unpinned_1")
	if err != nil {
		t.Fatalf("Promote unpinned_1 failed: %v", err)
	}
	_, _, err = w.Promote("unpinned_2")
	if err != nil {
		t.Fatalf("Promote unpinned_2 failed: %v", err)
	}

	statsBefore := w.Stats()
	if statsBefore.ActiveCount != 2 {
		t.Fatalf("expected 2 active tools before pinned promotion, got %d", statsBefore.ActiveCount)
	}

	// Now promote my_pinned_tool (which is PINNED)
	promoted, evicted, err := w.Promote("my_pinned_tool")
	if err != nil {
		t.Fatalf("Promote pinned tool failed: %v", err)
	}
	if !promoted {
		t.Fatalf("pinned tool was not promoted")
	}

	// Pinned tools do NOT take unpinned slots. Eviction should be empty (""), and unpinned_1 and unpinned_2 must still be active!
	if evicted != "" {
		t.Errorf("WINDOW INVARIANT VIOLATION: Promoting a pinned tool evicted unpinned tool %q! Pinned tools must not evict unpinned tools.", evicted)
	}
	if !w.IsActive("unpinned_1") {
		t.Errorf("WINDOW INVARIANT VIOLATION: unpinned_1 was evicted when pinned tool was promoted!")
	}
	if !w.IsActive("unpinned_2") {
		t.Errorf("WINDOW INVARIANT VIOLATION: unpinned_2 was evicted when pinned tool was promoted!")
	}
}

// TestAdversarial_ProfileReadOnlyNamespacedTools tests if ProfileReadOnly blocks
// tools named 'delete', 'drop', 'remove' without trailing underscores when prefixed.
func TestAdversarial_ProfileReadOnlyNamespacedTools(t *testing.T) {
	filterCfg := router.DefaultFilterConfig()
	filter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		t.Fatalf("NewToolFilter failed: %v", err)
	}

	_ = filter.SetProfile(router.ProfileReadOnly)

	dangerousTools := []string{
		"db__delete",
		"server__drop",
		"cloud__remove",
		"worker__destroy",
		"cache__purge",
		"process__kill",
		"git__reset",
	}

	for _, tool := range dangerousTools {
		if filter.IsAllowed(tool) {
			t.Errorf("FILTER LEAK: Tool %q is ALLOWED under ProfileReadOnly! Exclude patterns like '*_delete_*' fail to match trailing names without subsequent underscore.", tool)
		}
	}
}

// TestAdversarial_Multiplexer_RegisterCloseRace tests concurrent RegisterUpstream and Close for race panics.
func TestAdversarial_Multiplexer_RegisterCloseRace(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		mux := router.NewMultiplexer(router.NamespaceModeAuto, nil)
		var wg sync.WaitGroup

		wg.Add(2)
		go func() {
			defer wg.Done()
			tr := newChannelTransport()
			_ = mux.RegisterUpstream(context.Background(), router.UpstreamConfig{
				ID:        fmt.Sprintf("server_%d", iter),
				Transport: tr,
			})
		}()

		go func() {
			defer wg.Done()
			_ = mux.Close()
		}()

		wg.Wait()
	}
}

// TestAdversarial_MassiveConcurrencyStress tests Router, Window, Filter, Multiplexer
// under 50 parallel goroutines performing simultaneous mutations and reads.
func TestAdversarial_MassiveConcurrencyStress(t *testing.T) {
	ctx := context.Background()
	pruneEngine := pruner.New()
	routerCfg := router.DefaultRouterConfig()
	routerCfg.NamespaceMode = router.NamespaceModePrefixAlways
	routerCfg.PruneMode = "none"

	r := router.NewRouter(routerCfg, pruneEngine, nil)
	defer func() { _ = r.Close() }()

	filterCfg := router.DefaultFilterConfig()
	filter, _ := router.NewToolFilter(filterCfg)
	r.AttachFilter(filter)

	windowCfg := router.DefaultWindowConfig()
	windowCfg.MaxActive = 8
	window, _ := router.NewToolWindow(windowCfg, r.Multiplexer(), nil)
	r.AttachWindow(window)

	tr1 := newChannelTransport()
	tr2 := newChannelTransport()
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "srv1", Transport: tr1})
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{ID: "srv2", Transport: tr2})

	tools1 := make([]mcp.Tool, 20)
	for i := 0; i < 20; i++ {
		tools1[i] = mcp.Tool{
			Name:        fmt.Sprintf("tool_a_%d", i),
			Description: fmt.Sprintf("Tool A %d", i),
			InputSchema: mcp.InputSchema{Type: "object"},
		}
	}
	tools2 := make([]mcp.Tool, 20)
	for i := 0; i < 20; i++ {
		tools2[i] = mcp.Tool{
			Name:        fmt.Sprintf("tool_b_%d", i),
			Description: fmt.Sprintf("Tool B %d", i),
			InputSchema: mcp.InputSchema{Type: "object"},
		}
	}
	r.Multiplexer().SyncUpstreamTools(ctx, "srv1", tools1)
	r.Multiplexer().SyncUpstreamTools(ctx, "srv2", tools2)

	const numGoroutines = 40
	const iters = 50
	var wg sync.WaitGroup

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				// 1. HandleToolsList
				reqID := mcp.NewIntID(int64(gid*iters + i))
				listReq, _ := mcp.NewRequest(reqID, "tools/list", nil)
				_, _ = r.HandleToolsList(ctx, listReq)

				// 2. RouteToolCall
				srv := "srv1"
				if (gid+i)%2 == 0 {
					srv = "srv2"
				}
				toolName := fmt.Sprintf("%s__tool_%s_%d", srv, string(rune('a'+(gid+i)%2)), (gid+i)%20)
				callParams := mcp.CallToolRequestParams{
					Name:      toolName,
					Arguments: json.RawMessage(`{}`),
				}
				pJSON, _ := json.Marshal(callParams)
				callReq, _ := mcp.NewRequest(reqID, "tools/call", json.RawMessage(pJSON))
				_, _, _ = r.RouteToolCall(callReq)

				// 3. Window mutations
				if i%4 == 0 {
					_, _, _ = window.Promote(toolName)
				} else if i%4 == 1 {
					_ = window.Demote(toolName)
				} else if i%4 == 2 {
					window.RecordUsage(toolName)
				}

				// 4. Dynamic profile changes
				if i%15 == 0 {
					_ = r.SetProfile(router.ProfileReadOnly)
				} else if i%15 == 7 {
					_ = r.SetProfile(router.ProfileFull)
				}
			}
		}(g)
	}

	wg.Wait()
}

// TestAdversarial_MalformedRPCInputs tests router and hydration resilience against malformed inputs.
func TestAdversarial_MalformedRPCInputs(t *testing.T) {
	r := router.NewRouter(router.DefaultRouterConfig(), nil, nil)
	defer func() { _ = r.Close() }()

	malformedRequests := []*mcp.Message{
		nil,
		{JSONRPC: "2.0"},
		{JSONRPC: "2.0", Method: "tools/call", Params: nil},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(``)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`null`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`{}`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`{"name": ""}`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`{"name": 123}`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`[1, 2, 3]`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`"not-an-object"`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`{"name": "tool", "arguments": "malformed_json_without_closing`)},
		{JSONRPC: "2.0", Method: "tools/call", Params: []byte(`{"name": "\u0000\u0001\u0002"}`)},
	}

	for idx, msg := range malformedRequests {
		// RouteToolCall should never panic on malformed messages
		_, _, err := r.RouteToolCall(msg)
		if err == nil && msg != nil && len(msg.Params) > 0 {
			var p mcp.CallToolRequestParams
			if json.Unmarshal(msg.Params, &p) == nil && p.Name != "" {
				// expected
			} else {
				t.Errorf("case %d: expected error for malformed tools/call message: %+v", idx, msg)
			}
		}

		// HandleInternalToolCall should never panic on malformed messages
		_, _ = r.HandleInternalToolCall(msg)
	}

	// SchemaStore and ValidateInvocation tests
	store := router.NewSchemaStore()
	cached := store.Register("srv", mcp.Tool{
		Name: "test_tool",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"num": map[string]any{"type": "number"},
				"int": map[string]any{"type": "integer"},
				"str": map[string]any{"type": "string"},
			},
			Required: []string{"str"},
		},
	}, mcp.Tool{}, mcp.Tool{})

	// ValidateInvocation with malformed JSON
	errVal := router.ValidateInvocation(cached, json.RawMessage(`{malformed}`))
	if errVal == nil {
		t.Errorf("expected validation error for malformed json")
	}

	// ValidateInvocation with missing required
	errVal = router.ValidateInvocation(cached, json.RawMessage(`{"num": 1.5}`))
	if errVal == nil {
		t.Errorf("expected validation error for missing required parameter 'str'")
	}

	// ValidateInvocation with integer type mismatch (float for int)
	errVal = router.ValidateInvocation(cached, json.RawMessage(`{"str": "ok", "int": 3.1415}`))
	if errVal == nil {
		t.Errorf("expected validation error for float passed to integer field")
	}
}
