package router_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/router"
)

// channelTransport implements router.UpstreamTransport with bidirectional channels (zero mocks).
type channelTransport struct {
	in     chan *mcp.Message
	out    chan *mcp.Message
	closed atomic.Bool
	closec chan struct{}
}

func newChannelTransport() *channelTransport {
	return &channelTransport{
		in:     make(chan *mcp.Message, 100),
		out:    make(chan *mcp.Message, 100),
		closec: make(chan struct{}),
	}
}

func (c *channelTransport) Send(msg *mcp.Message) error {
	if c.closed.Load() {
		return errors.New("transport closed")
	}
	select {
	case c.out <- msg:
		return nil
	case <-c.closec:
		return errors.New("transport closed")
	}
}

func (c *channelTransport) Receive() (*mcp.Message, error) {
	if c.closed.Load() {
		return nil, io.EOF
	}
	select {
	case msg, ok := <-c.in:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-c.closec:
		return nil, io.EOF
	}
}

func (c *channelTransport) Close() error {
	if c.closed.CompareAndSwap(false, true) {
		close(c.closec)
	}
	return nil
}

// testUpstreamHandler tracks callbacks dispatched from upstream reader loops.
type testUpstreamHandler struct {
	mu            sync.Mutex
	responses     []*mcp.Message
	notifications []*mcp.Message
	crashes       map[string]error
}

func newTestUpstreamHandler() *testUpstreamHandler {
	return &testUpstreamHandler{
		crashes: make(map[string]error),
	}
}

func (h *testUpstreamHandler) HandleUpstreamResponse(upstreamID string, msg *mcp.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responses = append(h.responses, msg)
}

func (h *testUpstreamHandler) HandleUpstreamNotification(upstreamID string, msg *mcp.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications = append(h.notifications, msg)
}

func (h *testUpstreamHandler) HandleUpstreamCrash(upstreamID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.crashes[upstreamID] = err
}

func TestMultiplexer_RegistrationAndValidation(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid upstream ID validation", func(t *testing.T) {
		mux := router.NewMultiplexer(router.NamespaceModeAuto, nil)
		defer func() { _ = mux.Close() }()

		// Empty ID fails
		err := mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: ""})
		if err == nil {
			t.Errorf("expected error registering upstream with empty ID")
		}

		// ID containing '__' fails
		err = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "invalid__id"})
		if err == nil {
			t.Errorf("expected error registering upstream with double underscore")
		}

		// Valid registration
		err = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "github"})
		if err != nil {
			t.Fatalf("RegisterUpstream failed: %v", err)
		}

		// Duplicate ID fails
		err = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "github"})
		if !errors.Is(err, router.ErrUpstreamAlreadyExists) {
			t.Errorf("expected ErrUpstreamAlreadyExists, got: %v", err)
		}
	})

	t.Run("unregister upstream removes routes and triggers callback", func(t *testing.T) {
		mux := router.NewMultiplexer(router.NamespaceModeAuto, nil)
		defer func() { _ = mux.Close() }()

		var changedCount atomic.Int32
		mux.SetOnToolsChanged(func() {
			changedCount.Add(1)
		})

		tr := newChannelTransport()
		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{
			ID:        "supabase",
			Transport: tr,
		})

		mux.SyncUpstreamTools(ctx, "supabase", []mcp.Tool{
			{Name: "query_logs", Description: "Query logs"},
		})

		if len(mux.ListAllTools()) != 1 {
			t.Fatalf("expected 1 tool registered")
		}

		// Unregister unknown server
		err := mux.UnregisterUpstream(ctx, "unknown_server")
		if !errors.Is(err, router.ErrUpstreamNotFound) {
			t.Errorf("expected ErrUpstreamNotFound, got: %v", err)
		}

		// Unregister supabase
		err = mux.UnregisterUpstream(ctx, "supabase")
		if err != nil {
			t.Fatalf("UnregisterUpstream failed: %v", err)
		}

		if len(mux.ListAllTools()) != 0 {
			t.Errorf("expected 0 tools after unregistering, got %d", len(mux.ListAllTools()))
		}
		if changedCount.Load() < 2 {
			t.Errorf("expected onToolsChanged to be called at least twice (sync + unregister)")
		}
	})
}

func TestMultiplexer_NamespaceModes(t *testing.T) {
	ctx := context.Background()

	githubTools := []mcp.Tool{
		{Name: "create_issue", Description: "Create issue"},
		{Name: "common_query", Description: "GitHub common query"},
	}
	supabaseTools := []mcp.Tool{
		{Name: "query_logs", Description: "Query logs"},
		{Name: "common_query", Description: "Supabase common query"},
	}

	t.Run("NamespaceModeAlways prefixes all tools", func(t *testing.T) {
		mux := router.NewMultiplexer(router.NamespaceModePrefixAlways, nil)
		defer func() { _ = mux.Close() }()

		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "gh", Prefix: "github"})
		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "sb", Prefix: "supabase"})

		mux.SyncUpstreamTools(ctx, "gh", githubTools)
		mux.SyncUpstreamTools(ctx, "sb", supabaseTools)

		tools := mux.ListAllTools()
		if len(tools) != 4 {
			t.Fatalf("expected 4 tools, got %d", len(tools))
		}

		names := make(map[string]bool)
		for _, tool := range tools {
			names[tool.Name] = true
		}

		expected := []string{
			"github__create_issue",
			"github__common_query",
			"supabase__query_logs",
			"supabase__common_query",
		}
		for _, exp := range expected {
			if !names[exp] {
				t.Errorf("missing expected prefixed tool %q", exp)
			}
		}
	})

	t.Run("NamespaceModeAuto uses flat names for single server and prefixes for multiple", func(t *testing.T) {
		mux := router.NewMultiplexer(router.NamespaceModeAuto, nil)
		defer func() { _ = mux.Close() }()

		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "github"})
		mux.SyncUpstreamTools(ctx, "github", githubTools)

		// With 1 server, names should remain flat
		tools := mux.ListAllTools()
		for _, tDef := range tools {
			if tDef.Name != "create_issue" && tDef.Name != "common_query" {
				t.Errorf("expected flat name under single server auto mode, got %q", tDef.Name)
			}
		}

		// Add second server -> auto switches to always prefixing
		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "supabase"})
		mux.SyncUpstreamTools(ctx, "supabase", supabaseTools)

		tools = mux.ListAllTools()
		for _, tDef := range tools {
			if !strings.HasPrefix(tDef.Name, "github__") && !strings.HasPrefix(tDef.Name, "supabase__") {
				t.Errorf("expected prefixed name under multi server auto mode, got %q", tDef.Name)
			}
		}
	})

	t.Run("NamespaceModePrefixCollision prefixes only colliding tools", func(t *testing.T) {
		mux := router.NewMultiplexer(router.NamespaceModePrefixCollision, nil)
		defer func() { _ = mux.Close() }()

		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "github"})
		_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "supabase"})

		mux.SyncUpstreamTools(ctx, "github", githubTools)
		mux.SyncUpstreamTools(ctx, "supabase", supabaseTools)

		tools := mux.ListAllTools()
		names := make(map[string]bool)
		for _, tool := range tools {
			names[tool.Name] = true
		}

		// Unique tools should be un-prefixed
		if !names["create_issue"] {
			t.Errorf("unique tool create_issue should NOT be prefixed")
		}
		if !names["query_logs"] {
			t.Errorf("unique tool query_logs should NOT be prefixed")
		}

		// Colliding common_query should be prefixed
		if !names["github__common_query"] || !names["supabase__common_query"] {
			t.Errorf("colliding tool common_query MUST be prefixed with server namespace")
		}
	})
}

func TestMultiplexer_RouteResolutionAndFallback(t *testing.T) {
	ctx := context.Background()
	mux := router.NewMultiplexer(router.NamespaceModePrefixAlways, nil)
	defer func() { _ = mux.Close() }()

	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "github"})
	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "supabase"})

	mux.SyncUpstreamTools(ctx, "github", []mcp.Tool{
		{Name: "create_issue"},
		{Name: "search"},
	})
	mux.SyncUpstreamTools(ctx, "supabase", []mcp.Tool{
		{Name: "query_logs"},
		{Name: "search"},
	})

	t.Run("exact exposed name match", func(t *testing.T) {
		route, ok := mux.ResolveRoute("github__create_issue")
		if !ok {
			t.Fatalf("expected to resolve github__create_issue")
		}
		if route.OriginalName != "create_issue" {
			t.Errorf("expected original name create_issue, got %q", route.OriginalName)
		}
		if route.UpstreamID != "github" {
			t.Errorf("expected upstream ID github, got %q", route.UpstreamID)
		}
	})

	t.Run("resilience fallback when prefix omitted on unique tool", func(t *testing.T) {
		// "create_issue" only exists on github
		route, ok := mux.ResolveRoute("create_issue")
		if !ok {
			t.Fatalf("expected fallback resolution for unique tool create_issue")
		}
		if route.UpstreamID != "github" || route.OriginalName != "create_issue" {
			t.Errorf("unexpected resolved route: %+v", route)
		}
	})

	t.Run("resilience fallback rejects ambiguous tool when prefix omitted", func(t *testing.T) {
		// "search" exists on both github and supabase
		_, ok := mux.ResolveRoute("search")
		if ok {
			t.Errorf("expected resolve to fail for ambiguous tool 'search' with omitted prefix")
		}
	})

	t.Run("unknown tool returns false", func(t *testing.T) {
		_, ok := mux.ResolveRoute("non_existent_tool")
		if ok {
			t.Errorf("expected resolve to fail for non-existent tool")
		}
	})

	t.Run("CatalogProvider interface satisfaction", func(t *testing.T) {
		tool, ok := mux.GetTool("github__create_issue")
		if !ok || tool.Name != "github__create_issue" {
			t.Errorf("GetTool failed: ok=%v, tool=%+v", ok, tool)
		}

		all := mux.GetAllTools()
		if len(all) != 4 {
			t.Errorf("expected 4 tools from GetAllTools(), got %d", len(all))
		}
	})
}

func TestMultiplexer_ReaderLoopAndFailureIsolation(t *testing.T) {
	ctx := context.Background()
	mux := router.NewMultiplexer(router.NamespaceModePrefixAlways, nil)
	defer func() { _ = mux.Close() }()

	handler := newTestUpstreamHandler()
	mux.SetMessageHandler(handler)

	tr1 := newChannelTransport()
	tr2 := newChannelTransport()

	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "server1", Transport: tr1})
	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "server2", Transport: tr2})

	waitFor := func(cond func() bool, timeout time.Duration) bool {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return cond()
	}

	t.Run("upstream response is delivered to handler", func(t *testing.T) {
		reqID := mcp.NewIntID(100)
		respMsg, _ := mcp.NewResponse(reqID, map[string]string{"result": "success"})

		// Feed into tr1
		tr1.in <- respMsg

		ok := waitFor(func() bool {
			handler.mu.Lock()
			defer handler.mu.Unlock()
			return len(handler.responses) == 1
		}, 1*time.Second)

		if !ok {
			handler.mu.Lock()
			count := len(handler.responses)
			handler.mu.Unlock()
			t.Fatalf("expected 1 response delivered, got %d", count)
		}

		handler.mu.Lock()
		defer handler.mu.Unlock()
		if handler.responses[0].ID == nil || handler.responses[0].ID.String() != "100" {
			t.Errorf("unexpected response ID: %+v", handler.responses[0].ID)
		}
	})

	t.Run("upstream notification is delivered to handler", func(t *testing.T) {
		notifMsg, _ := mcp.NewNotification(mcp.MethodNotificationsToolsListChanged, nil)

		// Feed into tr2
		tr2.in <- notifMsg

		ok := waitFor(func() bool {
			handler.mu.Lock()
			defer handler.mu.Unlock()
			return len(handler.notifications) == 1
		}, 1*time.Second)

		if !ok {
			handler.mu.Lock()
			count := len(handler.notifications)
			handler.mu.Unlock()
			t.Fatalf("expected 1 notification delivered, got %d", count)
		}

		handler.mu.Lock()
		defer handler.mu.Unlock()
		if handler.notifications[0].Method != mcp.MethodNotificationsToolsListChanged {
			t.Errorf("unexpected notification method: %s", handler.notifications[0].Method)
		}
	})

	t.Run("crash on server1 isolates failure without affecting server2", func(t *testing.T) {
		// Close server1's transport to simulate crash/EOF
		_ = tr1.Close()

		ok := waitFor(func() bool {
			handler.mu.Lock()
			defer handler.mu.Unlock()
			crashErr, crashed := handler.crashes["server1"]
			return crashed && crashErr != nil
		}, 1*time.Second)

		if !ok {
			t.Errorf("expected server1 crash to be reported to handler")
		}

		// server2 must still be fully alive and receiving messages
		reqID2 := mcp.NewIntID(200)
		respMsg2, _ := mcp.NewResponse(reqID2, map[string]string{"server2": "ok"})
		tr2.in <- respMsg2

		ok2 := waitFor(func() bool {
			handler.mu.Lock()
			defer handler.mu.Unlock()
			return len(handler.responses) == 2
		}, 1*time.Second)

		if !ok2 {
			handler.mu.Lock()
			count := len(handler.responses)
			handler.mu.Unlock()
			t.Fatalf("expected server2 to process response despite server1 crash, got %d responses", count)
		}
	})
}

func TestMultiplexer_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	mux := router.NewMultiplexer(router.NamespaceModePrefixAlways, nil)
	defer func() { _ = mux.Close() }()

	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "upstream_1"})
	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "upstream_2"})

	var wg sync.WaitGroup
	const numGoroutines = 25
	const iters = 40

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				uID := fmt.Sprintf("upstream_%d", (gid%2)+1)
				tools := []mcp.Tool{
					{Name: fmt.Sprintf("tool_%d", i%10)},
				}
				if i%5 == 0 {
					mux.SyncUpstreamTools(ctx, uID, tools)
				}
				_ = mux.ListAllTools()
				_, _ = mux.ResolveRoute(fmt.Sprintf("%s__tool_%d", uID, i%10))
				_, _ = mux.GetTool(fmt.Sprintf("%s__tool_%d", uID, i%10))
			}
		}(g)
	}

	wg.Wait()
}
