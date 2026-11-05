package proxy_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// ============================================================================
// Real Subprocess Helper for Zero-Mock Testing (Per R5 Mandate)
// ============================================================================

// TestHelperProcess serves as the multi-call real OS child process.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	mode := os.Getenv("HELPER_MODE")
	switch mode {
	case "stderr_flood":
		// Flood 2MB of stderr output to stress pipe draining
		chunk := strings.Repeat("E", 1024) + "\n"
		for i := 0; i < 2048; i++ {
			_, _ = os.Stderr.WriteString(chunk)
		}
		// Write a valid JSON-RPC message to stdout so supervisor can read it
		_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}` + "\n")
		os.Exit(0)

	case "echo":
		// Read lines from stdin, echo back a response, exit cleanly on EOF
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := scanner.Text()
			var msg mcp.Message
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				resp, _ := mcp.NewResponse(*msg.ID, map[string]string{"echo": msg.Method})
				data, _ := json.Marshal(resp)
				_, _ = os.Stdout.Write(append(data, '\n'))
			}
		}
		os.Exit(0)

	case "stubborn":
		// Ignore stdin and sleep until forcefully killed
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case "crash":
		// Emit fatal error to stderr and exit with code 1
		_, _ = os.Stderr.WriteString("FATAL: database connection refused on port 5432\n")
		os.Exit(1)

	default:
		os.Exit(0)
	}
}

// ============================================================================
// 1. Stdio Transport Tests
// ============================================================================

func TestStdio_10MB_LineBuffer(t *testing.T) {
	// Construct an oversized 4MB JSON-RPC line (exceeds default 64KB bufio.Scanner limit)
	largeData := strings.Repeat("A", 4*1024*1024)
	rawJSON := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"data":"%s"}}`+"\n", largeData)

	reader := bytes.NewReader([]byte(rawJSON))
	writer := &bytes.Buffer{}

	transport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:  reader,
		Out: writer,
	})

	msg, err := transport.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed on 4MB payload: %v", err)
	}

	if msg.Method != "tools/call" {
		t.Errorf("expected method 'tools/call', got %q", msg.Method)
	}
}

func TestStdio_ConcurrentWriteSerialization(t *testing.T) {
	reader := &bytes.Buffer{}
	writer := &bytes.Buffer{}

	transport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:  reader,
		Out: writer,
	})

	const numGoroutines = 30
	const messagesPerGoroutine = 20
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for j := 0; j < messagesPerGoroutine; j++ {
				id := mcp.NewStringID(fmt.Sprintf("g%d-m%d", gID, j))
				msg, _ := mcp.NewRequest(id, "notifications/progress", map[string]int{"progress": j})
				if err := transport.WriteMessage(msg); err != nil {
					t.Errorf("WriteMessage failed: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()

	// Verify all messages were written as discrete, valid JSON lines without byte interleaving
	lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
	expectedCount := numGoroutines * messagesPerGoroutine
	if len(lines) != expectedCount {
		t.Fatalf("expected %d lines, got %d", expectedCount, len(lines))
	}

	for idx, line := range lines {
		var parsed mcp.Message
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d is corrupted: %v (content: %s)", idx, err, line)
		}
	}
}

func TestStdio_ReadWriteMessage_Roundtrip(t *testing.T) {
	inBuf := &bytes.Buffer{}
	outBuf := &bytes.Buffer{}

	transport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:  inBuf,
		Out: outBuf,
	})

	// Write newline delimited message with CR/LF and extra blank lines
	rawStream := "\r\n  \n" + `{"jsonrpc":"2.0","id":10,"method":"ping"}` + "\r\n\r\n"
	inBuf.WriteString(rawStream)

	msg, err := transport.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}
	if msg.Method != "ping" || msg.ID.String() != "10" {
		t.Fatalf("unexpected message: %v", msg)
	}

	// Write response
	resp, _ := mcp.NewResponse(*msg.ID, map[string]string{"pong": "true"})
	if err := transport.WriteMessage(resp); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Verify newline termination
	outBytes := outBuf.Bytes()
	if len(outBytes) == 0 || outBytes[len(outBytes)-1] != '\n' {
		t.Fatalf("expected newline termination")
	}

	_ = transport.Close()
	_, err = transport.ReadMessage()
	if !errors.Is(err, proxy.ErrTransportClosed) {
		t.Fatalf("expected ErrTransportClosed, got: %v", err)
	}
}

// ============================================================================
// 2. Subprocess Supervisor Tests (Real Subprocesses)
// ============================================================================

func TestSupervisor_StderrDeadlockPrevention(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "flood_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "stderr_flood",
		},
		GracePeriod: 2 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	// Read output message from stdout. If stderr was not continuously drained,
	// the child would deadlock on writing 2MB to stderr and this call would freeze.
	msg, err := sup.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage failed (potential deadlock): %v", err)
	}

	if msg.JSONRPC != "2.0" {
		t.Errorf("expected jsonrpc '2.0', got %q", msg.JSONRPC)
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("Supervisor.Stop failed: %v", err)
	}

	// Verify ring buffer captured trailing lines
	trailing := sup.TrailingStderr()
	if len(trailing) == 0 {
		t.Errorf("expected captured stderr lines in ring buffer, got 0")
	}
}

func TestSupervisor_GracefulStdinEOF(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "echo_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "echo",
		},
		GracePeriod: 2 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	// Send request to child
	req, _ := mcp.NewRequest(mcp.NewIntID(1), "hello", nil)
	if err := sup.WriteMessage(req); err != nil {
		t.Fatalf("WriteMessage to supervisor failed: %v", err)
	}

	resp, err := sup.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage from supervisor failed: %v", err)
	}
	if resp.ID.String() != "1" {
		t.Fatalf("expected ID '1', got %s", resp.ID.String())
	}

	start := time.Now()
	// Stop should close stdin and child should exit cleanly on EOF well before 2s grace period
	if err := sup.Stop(); err != nil {
		t.Fatalf("Supervisor.Stop failed: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 1500*time.Millisecond {
		t.Errorf("expected quick exit on stdin EOF, took %v", elapsed)
	}
}

func TestSupervisor_StubbornProcess_TimeoutKill(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "stubborn_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "stubborn",
		},
		GracePeriod: 500 * time.Millisecond,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	start := time.Now()
	// Subprocess sleeps 30s; Stop should timeout at ~500ms and forcefully terminate the tree
	_ = sup.Stop()
	elapsed := time.Since(start)

	if elapsed > 2500*time.Millisecond {
		t.Errorf("forced kill took too long: %v (expected ~500-1500ms)", elapsed)
	}
}

func TestSupervisor_CrashDiagnostics(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "crash_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "crash",
		},
		GracePeriod: 1 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	// Wait for process to exit
	_ = sup.Stop()

	trailing := sup.TrailingStderr()
	found := false
	for _, l := range trailing {
		if strings.Contains(l, "FATAL: database connection refused") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected fatal error line in trailing stderr, got: %v", trailing)
	}
}

// ============================================================================
// 3. HTTP / SSE Transport Tests
// ============================================================================

type MockUpstream struct {
	mu       sync.Mutex
	inbound  chan *mcp.Message
	outbound chan *mcp.Message
	closed   bool
}

func NewMockUpstream() *MockUpstream {
	return &MockUpstream{
		inbound:  make(chan *mcp.Message, 100),
		outbound: make(chan *mcp.Message, 100),
	}
}

func (m *MockUpstream) Send(msg *mcp.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return io.ErrClosedPipe
	}
	m.outbound <- msg
	return nil
}

func (m *MockUpstream) Receive() (*mcp.Message, error) {
	msg, ok := <-m.inbound
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (m *MockUpstream) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.inbound)
	}
	return nil
}

func (m *MockUpstream) PushToProxy(msg *mcp.Message) {
	m.inbound <- msg
}

func (m *MockUpstream) WaitOutbound(t *testing.T, timeout time.Duration) *mcp.Message {
	select {
	case msg := <-m.outbound:
		return msg
	case <-time.After(timeout):
		t.Fatal("timed out waiting for outbound message from proxy")
		return nil
	}
}

func TestSSE_EndpointDiscovery(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	cfg := proxy.DefaultSSEServerConfig()
	cfg.KeepAlivePeriod = 1 * time.Second
	sseSrv := proxy.NewSSEServer(cfg, prx)

	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// Connect to GET /sse
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to connect to /sse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200 OK, got: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("expected Content-Type text/event-stream, got: %s", contentType)
	}

	// Read initial SSE endpoint event
	reader := bufio.NewReader(resp.Body)
	line1, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read event line: %v", err)
	}
	if strings.TrimSpace(line1) != "event: endpoint" {
		t.Fatalf("expected 'event: endpoint', got: %s", line1)
	}

	line2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read data line: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(line2), "data: /messages?sessionId=") {
		t.Fatalf("expected data prefix '/messages?sessionId=', got: %s", line2)
	}
}

func TestSSE_PostMessages_Success(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// 1. Establish SSE stream
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to open SSE: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	_, _ = reader.ReadString('\n') // event: endpoint
	dataLine, _ := reader.ReadString('\n')
	_, _ = reader.ReadString('\n') // blank line

	// Extract endpoint
	endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")

	// 2. Post JSON-RPC request to /messages?sessionId=...
	postURL := ts.URL + endpoint
	jsonRPCReq := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`
	postResp, err := http.Post(postURL, "application/json", strings.NewReader(jsonRPCReq))
	if err != nil {
		t.Fatalf("POST /messages failed: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected status 202 Accepted, got: %d", postResp.StatusCode)
	}

	// 3. Verify upstream received the message
	upstreamMsg := mockUpstream.WaitOutbound(t, 2*time.Second)
	if upstreamMsg.Method != "ping" {
		t.Fatalf("expected upstream method 'ping', got: %s", upstreamMsg.Method)
	}

	// 4. Send response from upstream -> proxy -> SSE client
	mockUpstream.PushToProxy(&mcp.Message{
		JSONRPC: "2.0",
		ID:      upstreamMsg.ID,
		Result:  json.RawMessage(`{}`),
	})

	// 5. Read SSE message event from client stream
	msgEvent, _ := reader.ReadString('\n')
	if strings.TrimSpace(msgEvent) != "event: message" {
		t.Fatalf("expected 'event: message', got: %s", msgEvent)
	}
	msgData, _ := reader.ReadString('\n')
	if !strings.Contains(msgData, `"jsonrpc":"2.0"`) || !strings.Contains(msgData, `"id":1`) {
		t.Fatalf("unexpected message payload: %s", msgData)
	}
}

func TestSSE_PostMessages_Validation(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// Missing sessionId -> 400 Bad Request
	resp, _ := http.Post(ts.URL+"/messages", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing sessionId, got: %d", resp.StatusCode)
	}

	// Unknown sessionId -> 404 Not Found
	resp2, _ := http.Post(ts.URL+"/messages?sessionId=non-existent-uuid", "application/json", strings.NewReader(`{}`))
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for invalid sessionId, got: %d", resp2.StatusCode)
	}

	// Malformed JSON -> 400 Bad Request
	// First create active session
	getResp, err := http.Get(ts.URL + "/sse")
	if err != nil {
		t.Fatalf("GET /sse failed: %v", err)
	}
	defer getResp.Body.Close()

	reader := bufio.NewReader(getResp.Body)
	_, _ = reader.ReadString('\n')
	dataLine, _ := reader.ReadString('\n')
	endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")

	resp3, _ := http.Post(ts.URL+endpoint, "application/json", strings.NewReader(`{invalid-json`))
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got: %d", resp3.StatusCode)
	}
}

func TestSSE_CORS_PreflightAndHeaders(t *testing.T) {
	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), nil)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodOptions, ts.URL+"/sse", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS request failed: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 No Content for CORS preflight, got: %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("expected Access-Control-Allow-Origin: *, got: %s", resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

// ============================================================================
// 4. Core Proxy Engine Tests
// ============================================================================

type MockPruner struct {
	called bool
}

func (m *MockPruner) PruneTools(tools []mcp.Tool) ([]mcp.Tool, proxy.PruneMetrics, error) {
	m.called = true
	pruned := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		t.Description = "pruned"
		pruned[i] = t
	}
	return pruned, proxy.PruneMetrics{RawTokens: 1000, PrunedTokens: 200, SavingsRatio: 0.8}, nil
}

type MockClientSession struct {
	id       string
	received chan *mcp.Message
}

func NewMockClientSession(id string) *MockClientSession {
	return &MockClientSession{id: id, received: make(chan *mcp.Message, 10)}
}

func (m *MockClientSession) ID() string                  { return m.id }
func (m *MockClientSession) Send(msg *mcp.Message) error { m.received <- msg; return nil }
func (m *MockClientSession) Context() context.Context    { return context.Background() }

func TestProxy_ToolsListInterception(t *testing.T) {
	mockUpstream := NewMockUpstream()
	mockPruner := &MockPruner{}
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, mockPruner)
	prx.Start()
	defer prx.Shutdown()

	client := NewMockClientSession("c1")
	prx.RegisterClient(client)

	// Send tools/list from client
	id := mcp.NewIntID(100)
	_ = prx.HandleClientMessage(client, &mcp.Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  mcp.MethodToolsList,
		Params:  json.RawMessage(`{}`),
	})

	// Catch upstream outbound
	upstreamMsg := mockUpstream.WaitOutbound(t, 2*time.Second)

	// Simulate upstream tools/list response
	rawResult := `{"tools":[{"name":"test_tool","description":"very verbose original description","inputSchema":{"type":"object"}}]}`
	mockUpstream.PushToProxy(&mcp.Message{
		JSONRPC: "2.0",
		ID:      upstreamMsg.ID,
		Result:  json.RawMessage(rawResult),
	})

	// Wait for client to receive intercepted response
	select {
	case clientResp := <-client.received:
		if clientResp.ID.String() != "100" {
			t.Fatalf("expected client ID 100, got: %v", clientResp.ID)
		}
		if !mockPruner.called {
			t.Fatal("expected pruner to be called for tools/list response")
		}
		if !strings.Contains(string(clientResp.Result), `"pruned"`) {
			t.Fatalf("expected pruned payload, got: %s", string(clientResp.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client response")
	}
}

func TestProxy_ToolsCallPassthrough(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	client := NewMockClientSession("c1")
	prx.RegisterClient(client)

	id := mcp.NewStringID("call-1")
	_ = prx.HandleClientMessage(client, &mcp.Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  mcp.MethodToolsCall,
		Params:  json.RawMessage(`{"name":"calc","arguments":{"expr":"2+2"}}`),
	})

	upstreamMsg := mockUpstream.WaitOutbound(t, 2*time.Second)
	if upstreamMsg.Method != mcp.MethodToolsCall {
		t.Fatalf("expected method %s, got %s", mcp.MethodToolsCall, upstreamMsg.Method)
	}

	// Upstream responds with CallToolResult
	callResult := `{"content":[{"type":"text","text":"4"}],"isError":false}`
	mockUpstream.PushToProxy(&mcp.Message{
		JSONRPC: "2.0",
		ID:      upstreamMsg.ID,
		Result:  json.RawMessage(callResult),
	})

	select {
	case clientResp := <-client.received:
		if clientResp.ID.String() != "call-1" {
			t.Fatalf("expected client ID 'call-1', got: %v", clientResp.ID)
		}
		if !strings.Contains(string(clientResp.Result), `"4"`) {
			t.Fatalf("expected text '4', got: %s", string(clientResp.Result))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client response")
	}
}

func TestProxy_JSONRPCErrorPreservation(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	client := NewMockClientSession("c1")
	prx.RegisterClient(client)

	id := mcp.NewIntID(999)
	_ = prx.HandleClientMessage(client, &mcp.Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "unknownMethod",
	})

	upstreamMsg := mockUpstream.WaitOutbound(t, 2*time.Second)

	// Return standard JSON-RPC MethodNotFound error
	mockUpstream.PushToProxy(&mcp.Message{
		JSONRPC: "2.0",
		ID:      upstreamMsg.ID,
		Error:   mcp.NewMethodNotFound("unknownMethod"),
	})

	select {
	case clientResp := <-client.received:
		if clientResp.ID.String() != "999" {
			t.Fatalf("expected client ID 999, got: %v", clientResp.ID)
		}
		if clientResp.Error == nil {
			t.Fatal("expected error object in response")
		}
		if clientResp.Error.Code != mcp.CodeMethodNotFound {
			t.Fatalf("expected error code %d, got: %d", mcp.CodeMethodNotFound, clientResp.Error.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client error response")
	}
}

func TestProxy_UpstreamCrashHandling(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	client := NewMockClientSession("c1")
	prx.RegisterClient(client)

	id := mcp.NewIntID(500)
	_ = prx.HandleClientMessage(client, &mcp.Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  mcp.MethodToolsCall,
		Params:  json.RawMessage(`{"name":"calc"}`),
	})

	_ = mockUpstream.WaitOutbound(t, 2*time.Second)

	// Close upstream connection unexpectedly (crash simulation)
	_ = mockUpstream.Close()

	// Client should receive JSON-RPC InternalError rather than hanging
	select {
	case clientResp := <-client.received:
		if clientResp.Error == nil {
			t.Fatal("expected error response on upstream crash")
		}
		if clientResp.Error.Code != mcp.CodeInternalError {
			t.Fatalf("expected InternalError (%d), got: %d", mcp.CodeInternalError, clientResp.Error.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client error response")
	}
}

func TestSSE_KeepAliveComments(t *testing.T) {
	mockUpstream := NewMockUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	cfg := proxy.DefaultSSEServerConfig()
	cfg.KeepAlivePeriod = 100 * time.Millisecond // Fast ping for testing
	sseSrv := proxy.NewSSEServer(cfg, prx)

	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse failed: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	// Drain endpoint event
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	_, _ = reader.ReadString('\n')

	// Next event should be keepalive comment ": ping\n\n"
	line1, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("failed to read ping line: %v", err)
	}
	if strings.TrimSpace(line1) != ": ping" {
		t.Fatalf("expected ': ping', got: %q", line1)
	}
}

func TestProxy_RequestTimeout(t *testing.T) {
	mockUpstream := NewMockUpstream()
	cfg := proxy.DefaultProxyConfig()
	cfg.RequestTimeout = 100 * time.Millisecond // Fast timeout for testing
	prx := proxy.NewProxy(cfg, mockUpstream, nil)
	prx.Start()
	defer prx.Shutdown()

	client := NewMockClientSession("c1")
	prx.RegisterClient(client)

	id := mcp.NewIntID(777)
	_ = prx.HandleClientMessage(client, &mcp.Message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "slowMethod",
	})

	_ = mockUpstream.WaitOutbound(t, 2*time.Second)
	// Do not reply from upstream, let timeout sweeper fire

	select {
	case clientResp := <-client.received:
		if clientResp.Error == nil {
			t.Fatal("expected timeout error response")
		}
		if clientResp.Error.Code != mcp.CodeInternalError {
			t.Fatalf("expected InternalError (%d), got: %d", mcp.CodeInternalError, clientResp.Error.Code)
		}
		if !strings.Contains(clientResp.Error.Message, "timed out") {
			t.Fatalf("expected message to mention 'timed out', got: %s", clientResp.Error.Message)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for proxy sweeper to abort request")
	}
}

func TestSupervisor_StateTransitions(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "state_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "echo",
		},
		GracePeriod: 1 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if sup.State() != proxy.StateCreated {
		t.Fatalf("expected StateCreated, got %v", sup.State())
	}

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if sup.State() != proxy.StateRunning {
		t.Fatalf("expected StateRunning, got %v", sup.State())
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if sup.State() != proxy.StateTerminated {
		t.Fatalf("expected StateTerminated, got %v", sup.State())
	}
}

func TestStdioSession_Lifecycle(t *testing.T) {
	inBuf := &bytes.Buffer{}
	outBuf := &bytes.Buffer{}
	transport := proxy.NewStdioTransport(proxy.StdioOptions{In: inBuf, Out: outBuf})
	session := proxy.NewStdioSession(transport)

	if session.ID() != "stdio-client" {
		t.Fatalf("expected 'stdio-client', got %s", session.ID())
	}

	msg, _ := mcp.NewRequest(mcp.NewIntID(1), "ping", nil)
	if err := session.Send(msg); err != nil {
		t.Fatalf("session.Send failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), `"ping"`) {
		t.Fatalf("expected output to contain 'ping', got %s", outBuf.String())
	}

	if err := session.Close(); err != nil {
		t.Fatalf("session.Close failed: %v", err)
	}
}

