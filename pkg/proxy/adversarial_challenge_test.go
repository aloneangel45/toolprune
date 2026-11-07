package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// ============================================================================
// Multi-call OS Subprocess Helper for Adversarial Testing
// ============================================================================

func TestAdversarialHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ADVERSARIAL_HELPER") != "1" {
		return
	}

	mode := os.Getenv("ADVERSARIAL_MODE")
	switch mode {
	case "stderr_flood_5mb":
		// Write exactly 5MB (5 * 1024 * 1024 = 5242880 bytes) of stderr output
		const totalBytes = 5 * 1024 * 1024
		chunk := strings.Repeat("D", 1023) + "\n" // 1024 bytes per line
		written := 0
		for written < totalBytes {
			toWrite := len(chunk)
			if written+toWrite > totalBytes {
				toWrite = totalBytes - written
			}
			_, _ = os.Stderr.WriteString(chunk[:toWrite])
			written += toWrite
		}

		// Emit single valid JSON-RPC message to stdout
		_, _ = os.Stdout.WriteString(`{"jsonrpc":"2.0","id":100,"result":{"status":"drained_5mb"}}` + "\n")
		os.Exit(0)

	case "echo_and_hang":
		// Read one line, respond, then hang forever waiting to be forcefully killed
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			line := scanner.Text()
			var msg mcp.Message
			if err := json.Unmarshal([]byte(line), &msg); err == nil {
				resp, _ := mcp.NewResponse(*msg.ID, map[string]string{"ack": "received"})
				data, _ := json.Marshal(resp)
				_, _ = os.Stdout.Write(append(data, '\n'))
			}
		}
		// Hang indefinitely
		select {}

	case "hang_forever":
		// Do not respond at all; wait to be killed
		select {}

	default:
		os.Exit(0)
	}
}

// FastConcurrentUpstream simulates high-performance upstream MCP server
// with unbounded concurrency to prevent artificial test harness backpressure.
type FastConcurrentUpstream struct {
	mu     sync.Mutex
	closed bool
	inCh   chan *mcp.Message
}

func NewFastConcurrentUpstream() *FastConcurrentUpstream {
	return &FastConcurrentUpstream{
		inCh: make(chan *mcp.Message, 10000),
	}
}

func (u *FastConcurrentUpstream) Send(msg *mcp.Message) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return io.ErrClosedPipe
	}
	// Echo back immediately on inCh
	resp, _ := mcp.NewResponse(*msg.ID, map[string]any{
		"method": msg.Method,
		"echo":   "fast_ok",
	})
	u.inCh <- resp
	return nil
}

func (u *FastConcurrentUpstream) Receive() (*mcp.Message, error) {
	msg, ok := <-u.inCh
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

func (u *FastConcurrentUpstream) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.closed {
		u.closed = true
		close(u.inCh)
	}
	return nil
}

func (u *FastConcurrentUpstream) Push(msg *mcp.Message) {
	u.inCh <- msg
}

// ============================================================================
// Objective 1: SSE Concurrency & Session Thrashing (50 clients x 100 messages)
// ============================================================================

func TestAdversarial_Objective1_SSE_Concurrency(t *testing.T) {
	// Baseline goroutine count
	runtime.GC()
	initialGoroutines := runtime.NumGoroutine()

	upstream := NewFastConcurrentUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), upstream, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// High-throughput HTTP transport for test client pool
	tr := &http.Transport{
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
	}
	clientPool := &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
	}

	const numClients = 50
	const messagesPerClient = 100
	totalExpected := numClients * messagesPerClient

	var totalReceived atomic.Int64
	var crosstalkErrors atomic.Int64
	var postErrors atomic.Int64

	start := time.Now()
	var wg sync.WaitGroup

	for clientIdx := 0; clientIdx < numClients; clientIdx++ {
		wg.Add(1)
		go func(cID int) {
			defer wg.Done()

			// 1. Establish GET /sse stream
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
			if err != nil {
				t.Errorf("Client %d failed to create GET /sse request: %v", cID, err)
				return
			}
			req.Header.Set("Accept", "text/event-stream")

			resp, err := clientPool.Do(req)
			if err != nil {
				t.Errorf("Client %d failed to connect to /sse: %v", cID, err)
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("Client %d got status %d", cID, resp.StatusCode)
				return
			}

			// Read initial endpoint event
			reader := bufio.NewReader(resp.Body)
			_, err = reader.ReadString('\n') // event: endpoint
			if err != nil {
				t.Errorf("Client %d failed to read event line: %v", cID, err)
				return
			}
			dataLine, err := reader.ReadString('\n') // data: /messages?sessionId=...
			if err != nil {
				t.Errorf("Client %d failed to read endpoint data: %v", cID, err)
				return
			}
			_, _ = reader.ReadString('\n') // blank line

			endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")
			postURL := ts.URL + endpoint

			// 2. Start SSE listener goroutine for this client
			receivedMap := make(map[string]bool)
			doneReading := make(chan struct{})

			go func() {
				defer close(doneReading)
				for {
					line, rErr := reader.ReadString('\n')
					if rErr != nil {
						return
					}
					line = strings.TrimSpace(line)
					if line == "event: message" {
						dataL, dErr := reader.ReadString('\n')
						if dErr != nil {
							return
						}
						dataL = strings.TrimSpace(dataL)
						rawPayload := strings.TrimPrefix(dataL, "data: ")

						var msg mcp.Message
						if uErr := json.Unmarshal([]byte(rawPayload), &msg); uErr == nil && msg.ID != nil {
							idStr := msg.ID.String()
							// Check crosstalk: ID must have prefix "c{cID}-"
							expectedPrefix := fmt.Sprintf("c%d-", cID)
							if !strings.HasPrefix(idStr, expectedPrefix) {
								crosstalkErrors.Add(1)
								t.Errorf("CROSSTALK DETECTED! Client %d received message belonging to another client: ID=%s", cID, idStr)
							} else {
								receivedMap[idStr] = true
								totalReceived.Add(1)
							}
						}

						if len(receivedMap) >= messagesPerClient {
							return
						}
					}
				}
			}()

			// 3. Send bursts of 100 POST /messages
			for mIdx := 0; mIdx < messagesPerClient; mIdx++ {
				msgID := fmt.Sprintf("c%d-m%d", cID, mIdx)
				jsonPayload := fmt.Sprintf(`{"jsonrpc":"2.0","id":%q,"method":"ping","params":{"seq":%d}}`, msgID, mIdx)

				pResp, pErr := clientPool.Post(postURL, "application/json", strings.NewReader(jsonPayload))
				if pErr != nil {
					postErrors.Add(1)
					continue
				}
				_ = pResp.Body.Close()
				if pResp.StatusCode != http.StatusAccepted {
					postErrors.Add(1)
				}
			}

			// Await all responses for this client (up to 10s)
			select {
			case <-doneReading:
			case <-time.After(10 * time.Second):
				t.Errorf("Client %d timed out waiting for all messages (got %d/%d)", cID, len(receivedMap), messagesPerClient)
			}
		}(clientIdx)
	}

	wg.Wait()
	duration := time.Since(start)

	t.Logf("=== Concurrency Test Summary ===")
	t.Logf("Total Clients: %d", numClients)
	t.Logf("Messages Per Client: %d", messagesPerClient)
	t.Logf("Expected Messages: %d", totalExpected)
	t.Logf("Total Received: %d", totalReceived.Load())
	t.Logf("POST Errors: %d", postErrors.Load())
	t.Logf("Crosstalk Errors: %d", crosstalkErrors.Load())
	t.Logf("Duration: %v (Throughput: %.1f msg/sec)", duration, float64(totalReceived.Load())/duration.Seconds())

	if crosstalkErrors.Load() > 0 {
		t.Fatalf("FAILED: %d crosstalk errors detected!", crosstalkErrors.Load())
	}
	if totalReceived.Load() != int64(totalExpected) {
		t.Fatalf("FAILED: received %d out of %d messages", totalReceived.Load(), totalExpected)
	}

	// Goroutine leak check
	ts.Close()
	prx.Shutdown()
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	finalGoroutines := runtime.NumGoroutine()
	t.Logf("Goroutines: initial=%d, final=%d, delta=%d", initialGoroutines, finalGoroutines, finalGoroutines-initialGoroutines)
	if finalGoroutines-initialGoroutines > 35 {
		t.Errorf("Potential goroutine leak: %d leaked goroutines", finalGoroutines-initialGoroutines)
	}
}

// ============================================================================
// Objective 2: Abrupt Disconnection Stress & Panic On Closed Channel
// ============================================================================

// TestAdversarial_Objective2_DirectChannelPanic verifies zero panics occur during concurrent Send / Close.
func TestAdversarial_Objective2_DirectChannelPanic(t *testing.T) {
	panicked := false
	var panicMsg any
	var panicIter int

	for i := 0; i < 500; i++ {
		sess := proxy.NewSSESession(context.Background(), fmt.Sprintf("stress-sess-%d", i), 1)

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					panicked = true
					panicMsg = r
					panicIter = i
				}
			}()

			msg, _ := mcp.NewRequest(mcp.NewIntID(1), "ping", nil)
			for j := 0; j < 50; j++ {
				_ = sess.Send(msg)
			}
		}()

		go func() {
			defer wg.Done()
			time.Sleep(5 * time.Microsecond)
			_ = sess.Close()
		}()

		wg.Wait()
		if panicked {
			t.Fatalf("Regression: SSESession.Send panicked on concurrent close at iteration %d with: %v", panicIter, panicMsg)
		}
	}
}

// TestAdversarial_Objective2_HTTP_AbruptClientDisconnect stresses real HTTP SSE client disconnects
// during high message volume to evaluate session cleanup and server stability.
func TestAdversarial_Objective2_HTTP_AbruptClientDisconnect(t *testing.T) {
	upstream := NewFastConcurrentUpstream()
	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), upstream, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	const numClients = 30
	var wg sync.WaitGroup

	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 50,
	}
	httpClient := &http.Client{Transport: tr}

	for i := 0; i < numClients; i++ {
		wg.Add(1)
		go func(cID int) {
			defer wg.Done()

			// Connect to SSE
			req, err := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
			if err != nil {
				return
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return
			}

			// Read endpoint
			reader := bufio.NewReader(resp.Body)
			_, _ = reader.ReadString('\n')
			dataLine, _ := reader.ReadString('\n')
			_, _ = reader.ReadString('\n')
			endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")
			postURL := ts.URL + endpoint

			// Rapidly send 20 requests
			for j := 0; j < 20; j++ {
				msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":"disc-%d-%d","method":"ping"}`, cID, j)
				pResp, pErr := httpClient.Post(postURL, "application/json", strings.NewReader(msg))
				if pErr == nil {
					_ = pResp.Body.Close()
				}
			}

			// Abruptly sever the connection mid-stream
			_ = resp.Body.Close()
		}(i)
	}

	wg.Wait()

	// Allow session sweeper/cleanup to run with polling deadline
	var lastSessions int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		healthResp, err := http.Get(ts.URL + "/health")
		if err != nil {
			continue
		}
		var healthBody struct {
			Status   string `json:"status"`
			Sessions int    `json:"sessions"`
		}
		_ = json.NewDecoder(healthResp.Body).Decode(&healthBody)
		_ = healthResp.Body.Close()
		lastSessions = healthBody.Sessions
		if lastSessions == 0 {
			break
		}
	}

	t.Logf("Active sessions after 30 abrupt client disconnects: %d", lastSessions)
	if lastSessions != 0 {
		t.Errorf("LEAK: Expected 0 active sessions after abrupt disconnects, got: %d", lastSessions)
	}
}

// ============================================================================
// Objective 3: Subprocess Stderr Flood (5MB) & Crash Recovery
// ============================================================================

func TestAdversarial_Objective3_SubprocessStderrFlood5MB(t *testing.T) {
	// Launch subprocess helper that writes 5MB to stderr
	cfg := proxy.SupervisorConfig{
		Name:    "stderr_flood_5mb_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestAdversarialHelperProcess"},
		Env: map[string]string{
			"GO_WANT_ADVERSARIAL_HELPER": "1",
			"ADVERSARIAL_MODE":           "stderr_flood_5mb",
		},
		GracePeriod:  3 * time.Second,
		RingCapacity: 200,
	}

	sup := proxy.NewSupervisor(cfg)
	start := time.Now()
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	// Read message from stdout. If stderr drainer saturated pipe buffer (~64KB),
	// this call will deadlock and freeze.
	msgCh := make(chan *mcp.Message, 1)
	errCh := make(chan error, 1)

	go func() {
		msg, err := sup.ReadMessage()
		if err != nil {
			errCh <- err
		} else {
			msgCh <- msg
		}
	}()

	select {
	case msg := <-msgCh:
		elapsed := time.Since(start)
		t.Logf("SUCCESS: ReadMessage succeeded in %v without deadlocking on 5MB stderr!", elapsed)
		if msg.JSONRPC != "2.0" {
			t.Errorf("unexpected jsonrpc: %s", msg.JSONRPC)
		}
	case err := <-errCh:
		t.Fatalf("ReadMessage failed: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatalf("DEADLOCK DETECTED! Supervisor froze while upstream spewed 5MB to stderr (pipe buffer saturated).")
	}

	_ = sup.Stop()

	// Verify ring buffer captured the trailing lines
	trailing := sup.TrailingStderr()
	t.Logf("Ring buffer captured %d trailing stderr lines", len(trailing))
	if len(trailing) == 0 {
		t.Errorf("expected captured stderr lines in ring buffer, got 0")
	}
}

func TestAdversarial_Objective3_SubprocessCrashRecovery(t *testing.T) {
	// 1. Launch a real subprocess that responds to 1 message and hangs
	cfg := proxy.SupervisorConfig{
		Name:    "crash_kill_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestAdversarialHelperProcess"},
		Env: map[string]string{
			"GO_WANT_ADVERSARIAL_HELPER": "1",
			"ADVERSARIAL_MODE":           "echo_and_hang",
		},
		GracePeriod: 1 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	prx := proxy.NewProxy(proxy.DefaultProxyConfig(), sup, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// 2. Connect client via SSE
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to connect SSE: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	_, _ = reader.ReadString('\n')
	dataLine, _ := reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")
	postURL := ts.URL + endpoint

	// 3. Send handshake message to prove connection works
	initMsg := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{}}`
	pResp, err := http.Post(postURL, "application/json", strings.NewReader(initMsg))
	if err != nil || pResp.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST /messages failed: %v, status: %d", err, pResp.StatusCode)
	}
	_ = pResp.Body.Close()

	// Read response 1 from SSE stream
	ev1, _ := reader.ReadString('\n')
	d1, _ := reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	t.Logf("First response received: %s, data: %s", strings.TrimSpace(ev1), strings.TrimSpace(d1))

	// 4. Send second in-flight request
	inFlightMsg := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"slow_tool"}}`
	pResp2, err := http.Post(postURL, "application/json", strings.NewReader(inFlightMsg))
	if err != nil || pResp2.StatusCode != http.StatusAccepted {
		t.Fatalf("second POST /messages failed: %v", err)
	}
	_ = pResp2.Body.Close()

	// Short pause to ensure request reached proxy tracker
	time.Sleep(100 * time.Millisecond)

	// 5. Abruptly kill the upstream subprocess via taskkill / PID kill
	pid := sup.Pid()
	t.Logf("Killing upstream subprocess PID %d abruptly...", pid)

	if p, findErr := os.FindProcess(pid); findErr == nil {
		_ = p.Kill()
	}

	// 6. Read next SSE event. It MUST be an internal error (-32603) or EOF, not hang indefinitely!
	type sseResult struct {
		event string
		data  string
		err   error
	}
	resultCh := make(chan sseResult, 1)

	go func() {
		ev, rErr := reader.ReadString('\n')
		if rErr != nil {
			resultCh <- sseResult{err: rErr}
			return
		}
		dt, rErr := reader.ReadString('\n')
		if rErr != nil {
			resultCh <- sseResult{err: rErr}
			return
		}
		_, _ = reader.ReadString('\n')
		resultCh <- sseResult{event: ev, data: dt}
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Logf("SSE stream closed with EOF/error: %v (valid crash recovery behavior)", res.err)
		} else {
			t.Logf("Received crash event: %s, data: %s", strings.TrimSpace(res.event), strings.TrimSpace(res.data))
			cleanData := strings.TrimPrefix(strings.TrimSpace(res.data), "data: ")
			var errMsg mcp.Message
			if err := json.Unmarshal([]byte(cleanData), &errMsg); err != nil {
				t.Fatalf("failed to parse crash response JSON: %v", err)
			}
			if errMsg.Error == nil {
				t.Fatalf("expected error object in crash response, got: %s", cleanData)
			}
			t.Logf("Error Code received: %d, Message: %s", errMsg.Error.Code, errMsg.Error.Message)
			if errMsg.Error.Code != mcp.CodeInternalError {
				t.Errorf("expected error code %d (-32603), got %d", mcp.CodeInternalError, errMsg.Error.Code)
			}
			if !strings.Contains(errMsg.Error.Message, "terminated unexpectedly") {
				t.Errorf("expected error message to mention termination, got: %s", errMsg.Error.Message)
			}
		}

	case <-time.After(5 * time.Second):
		t.Fatalf("CRASH RECOVERY HANG DETECTED! Proxy failed to notify client of upstream crash within 5 seconds.")
	}
}

// TestAdversarial_Objective3_SubsequentRequestAfterCrash verifies proxy behavior
// when a client sends a new request AFTER the upstream process is already dead.
func TestAdversarial_Objective3_SubsequentRequestAfterCrash(t *testing.T) {
	cfg := proxy.SupervisorConfig{
		Name:    "dead_upstream_test",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestAdversarialHelperProcess"},
		Env: map[string]string{
			"GO_WANT_ADVERSARIAL_HELPER": "1",
			"ADVERSARIAL_MODE":           "echo_and_hang",
		},
		GracePeriod: 1 * time.Second,
	}

	sup := proxy.NewSupervisor(cfg)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Supervisor.Start failed: %v", err)
	}

	prx := proxy.NewProxy(proxy.ProxyConfig{
		RequestTimeout: 2 * time.Second, // Set short timeout for testing
	}, sup, nil)
	prx.Start()
	defer prx.Shutdown()

	sseSrv := proxy.NewSSEServer(proxy.DefaultSSEServerConfig(), prx)
	ts := httptest.NewServer(sseSrv.Handler())
	defer ts.Close()

	// Connect client
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /sse failed: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	_, _ = reader.ReadString('\n')
	dataLine, _ := reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	endpoint := strings.TrimPrefix(strings.TrimSpace(dataLine), "data: ")
	postURL := ts.URL + endpoint

	// Kill upstream process immediately
	pid := sup.Pid()
	if p, findErr := os.FindProcess(pid); findErr == nil {
		_ = p.Kill()
	}
	time.Sleep(200 * time.Millisecond)

	// Now send a request to the proxy when upstream is ALREADY DEAD
	deadMsg := `{"jsonrpc":"2.0","id":999,"method":"ping","params":{}}`
	pResp, pErr := http.Post(postURL, "application/json", strings.NewReader(deadMsg))
	if pErr != nil {
		t.Fatalf("POST failed: %v", pErr)
	}
	defer pResp.Body.Close()

	// What does the client receive?
	start := time.Now()
	ev, _ := reader.ReadString('\n')
	dt, _ := reader.ReadString('\n')
	_, _ = reader.ReadString('\n')
	elapsed := time.Since(start)

	t.Logf("Response received after upstream death: event=%s data=%s (elapsed: %v)",
		strings.TrimSpace(ev), strings.TrimSpace(dt), elapsed)

	if elapsed > 5*time.Second {
		t.Fatalf("Subsequent request hung for %v after upstream death; expected prompt error response (<5s)", elapsed)
	}
	if !strings.Contains(dt, `"-32603"`) && !strings.Contains(dt, `-32603`) {
		t.Fatalf("Expected -32603 JSON-RPC error response, got: %s", dt)
	}
}

