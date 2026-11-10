package proxy_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
