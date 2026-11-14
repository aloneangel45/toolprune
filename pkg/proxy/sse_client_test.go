package proxy_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

func TestSSEClientTransport_ClassicSSEAndStreamableHTTP(t *testing.T) {
	mockUpstream := NewMockUpstream()
	go func() {
		for msg := range mockUpstream.outbound {
			if msg == nil || msg.ID == nil {
				continue
			}
			var res any
			switch msg.Method {
			case mcp.MethodInitialize:
				res = mcp.InitializeResult{
					ProtocolVersion: mcp.LatestProtocolVersion,
					ServerInfo:      mcp.Implementation{Name: "RemoteTestServer", Version: "1.0.0"},
				}
			case mcp.MethodToolsList:
				res = mcp.ListToolsResult{
					Tools: []mcp.Tool{
						{
							Name:        "remote_query",
							Description: "Execute remote analytics query.",
							InputSchema: mcp.InputSchema{
								Type:       "object",
								Properties: map[string]any{"sql": map[string]any{"type": "string"}},
								Required:   []string{"sql"},
							},
						},
					},
				}
			default:
				res = map[string]any{"ok": true}
			}
			resp, _ := mcp.NewResponse(*msg.ID, res)
			mockUpstream.PushToProxy(resp)
		}
	}()

	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	t.Run("classic_sse_endpoint", func(t *testing.T) {
		client, err := proxy.NewSSEClientTransport(proxy.SSEClientConfig{
			Name:           "remote_sse",
			URL:            ts.URL + "/sse",
			Headers:        proxy.ExtractHTTPHeadersFromEnv(map[string]string{"BEARER_TOKEN": "test-secret"}),
			ConnectTimeout: 5 * time.Second,
			RequestTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewSSEClientTransport failed: %v", err)
		}
		defer func() { _ = client.Close() }()

		if err := client.Start(context.Background()); err != nil {
			t.Fatalf("client.Start failed: %v", err)
		}

		req, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodToolsList, nil)
		if err := client.Send(req); err != nil {
			t.Fatalf("client.Send failed: %v", err)
		}

		resp, err := client.Receive()
		if err != nil {
			t.Fatalf("client.Receive failed: %v", err)
		}
		var listRes mcp.ListToolsResult
		if err := json.Unmarshal(resp.Result, &listRes); err != nil {
			t.Fatalf("unmarshal tools/list failed: %v", err)
		}
		if len(listRes.Tools) != 1 || listRes.Tools[0].Name != "remote_query" {
			t.Fatalf("unexpected tools from remote SSE server: %+v", listRes.Tools)
		}
	})

	t.Run("streamable_http_mcp_endpoint", func(t *testing.T) {
		client, err := proxy.NewSSEClientTransport(proxy.SSEClientConfig{
			Name:           "remote_mcp",
			URL:            ts.URL + "/mcp",
			ConnectTimeout: 5 * time.Second,
			RequestTimeout: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewSSEClientTransport failed: %v", err)
		}
		defer func() { _ = client.Close() }()

		if err := client.Start(context.Background()); err != nil {
			t.Fatalf("client.Start (/mcp) failed: %v", err)
		}

		req, _ := mcp.NewRequest(mcp.NewIntID(2), mcp.MethodToolsList, nil)
		if err := client.Send(req); err != nil {
			t.Fatalf("client.Send (/mcp) failed: %v", err)
		}

		resp, err := client.Receive()
		if err != nil {
			t.Fatalf("client.Receive (/mcp) failed: %v", err)
		}
		if resp.ID == nil || resp.ID.String() != "2" {
			t.Fatalf("expected response id=2, got %+v", resp.ID)
		}
	})
}

func TestSSEClientTransport_StartupLifecycleHeadersAndConcurrency(t *testing.T) {
	// 1. Reject hostless URLs cleanly
	if _, err := proxy.NewSSEClientTransport(proxy.SSEClientConfig{URL: "http://"}); err == nil {
		t.Fatal("expected NewSSEClientTransport to reject hostless URL 'http://'")
	}

	// 2. ExtractHTTPHeadersFromEnv handles leading/trailing whitespace & multibyte keys safely
	headers := proxy.ExtractHTTPHeadersFromEnv(map[string]string{
		"  HTTP_HEADER_X_TRACE_ID  ": "trace-123",
		"  bearer_token ":            "tok-abc",
		"İ_HTTP_HEADER_IGNORED":      "val",
	})
	if headers["X-TRACE-ID"] != "trace-123" {
		t.Errorf("expected X-TRACE-ID='trace-123', got %v", headers)
	}
	if headers["Authorization"] != "Bearer tok-abc" {
		t.Errorf("expected Authorization='Bearer tok-abc', got %v", headers)
	}

	// 3. Verify 405 POST-first Streamable HTTP does not leak watcher goroutines
	postOnlySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp, _ := mcp.NewResponse(mcp.NewIntID(1), map[string]any{"ok": true})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer postOnlySrv.Close()

	runtime.GC()
	time.Sleep(20 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()

	longParentCtx, longParentCancel := context.WithCancel(context.Background())
	defer longParentCancel()

	for i := 0; i < 30; i++ {
		tr, err := proxy.NewSSEClientTransport(proxy.SSEClientConfig{
			Name: "post_only",
			URL:  postOnlySrv.URL + "/mcp",
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Start(longParentCtx); err != nil {
			t.Fatalf("Start on 405 endpoint failed: %v", err)
		}
		// Concurrent Send + Close stress check (race-safe, must never panic on closed channel)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			req, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodPing, nil)
			_ = tr.Send(req)
		}()
		go func() {
			defer wg.Done()
			_ = tr.Close()
		}()
		wg.Wait()
	}

	time.Sleep(50 * time.Millisecond)
	afterGoroutines := runtime.NumGoroutine()
	if afterGoroutines > baseGoroutines+6 {
		t.Errorf("goroutine leak detected across 30 POST-first Start/Close cycles: before=%d after=%d", baseGoroutines, afterGoroutines)
	}

	// 4. Short-lived startupCtx cancelled after Start() succeeds must NOT kill active /sse stream
	mockUp := NewMockUpstream()
	go func() {
		for msg := range mockUp.outbound {
			if msg != nil && msg.ID != nil {
				resp, _ := mcp.NewResponse(*msg.ID, map[string]any{"pong": true})
				mockUp.PushToProxy(resp)
			}
		}
	}()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUp, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	defer func() { _ = sseSrv.Shutdown(context.Background()) }()
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	tr, err := proxy.NewSSEClientTransport(proxy.SSEClientConfig{
		Name: "short_startup_ctx",
		URL:  ts.URL + "/sse",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()

	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 3*time.Second)
	if err := tr.Start(startupCtx); err != nil {
		cancelStartup()
		t.Fatalf("Start(/sse) failed: %v", err)
	}
	// Cancel startupCtx immediately after handshake — SSE stream must remain alive!
	cancelStartup()
	time.Sleep(30 * time.Millisecond)

	pingReq, _ := mcp.NewRequest(mcp.NewIntID(99), mcp.MethodPing, nil)
	if err := tr.Send(pingReq); err != nil {
		t.Fatalf("Send after startupCtx cancellation failed: %v", err)
	}
	pingResp, err := tr.Receive()
	if err != nil || pingResp == nil || pingResp.ID == nil || pingResp.ID.String() != "99" {
		t.Fatalf("expected live SSE stream response id=99 after startupCtx cancel, got resp=%+v err=%v", pingResp, err)
	}
}

func TestProxy_NilUpstreamDoesNotPanicOnAnyMessageType(t *testing.T) {
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), nil, nil)
	prx.RegisterClient(nil) // nil session guard
	prx.Start()
	defer prx.Shutdown()

	sess := proxy.NewSSESession(context.Background(), "nil-up-sess", 8)
	defer func() { _ = sess.Close() }()
	prx.RegisterClient(sess)

	notif := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, Method: mcp.MethodNotificationsInitialized}
	if err := prx.HandleClientMessage(sess, notif); err == nil {
		t.Errorf("expected error forwarding notification with nil upstream")
	}

	id := mcp.NewIntID(7)
	clientResp := &mcp.Message{JSONRPC: mcp.JSONRPCVersion, ID: &id, Result: json.RawMessage(`{}`)}
	if err := prx.HandleClientMessage(sess, clientResp); err == nil {
		t.Errorf("expected error forwarding client response with nil upstream")
	}

	clientReq, _ := mcp.NewRequest(mcp.NewIntID(8), mcp.MethodPing, nil)
	if err := prx.HandleClientMessage(sess, clientReq); err == nil {
		t.Errorf("expected error forwarding client request with nil upstream")
	}
}
