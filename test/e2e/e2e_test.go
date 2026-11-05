package e2e_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

var (
	toolpruneBin string
	testserverBin string
)

func TestMain(m *testing.M) {
	tempDir, err := os.MkdirTemp("", "toolprune-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create temp dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tempDir)

	goBin := "go"
	if customGo := os.Getenv("GO_BIN"); customGo != "" {
		goBin = customGo
	} else if _, err := os.Stat(`C:\Program Files\Go\bin\go.exe`); err == nil {
		goBin = `C:\Program Files\Go\bin\go.exe`
	}

	toolpruneBin = filepath.Join(tempDir, "toolprune.exe")
	testserverBin = filepath.Join(tempDir, "testserver.exe")

	// Build toolprune binary
	buildToolprune := exec.Command(goBin, "build", "-o", toolpruneBin, "./cmd/toolprune")
	buildToolprune.Dir = findRepoRoot()
	if out, err := buildToolprune.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build toolprune: %v\n%s\n", err, string(out))
		os.Exit(1)
	}

	// Build testserver binary
	buildTestserver := exec.Command(goBin, "build", "-o", testserverBin, "./test/testserver")
	buildTestserver.Dir = findRepoRoot()
	if out, err := buildTestserver.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to build testserver: %v\n%s\n", err, string(out))
		os.Exit(1)
	}

	code := m.Run()
	os.Exit(code)
}

func findRepoRoot() string {
	// Starting from current dir, find go.mod
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

// -----------------------------------------------------------------------------
// Subprocess Helper
// -----------------------------------------------------------------------------

type proxyProcess struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	reader  *bufio.Reader
	stderr  *bytes.Buffer
	mu      sync.Mutex
	idCount atomic.Int64
}

func spawnProxy(t *testing.T, args ...string) (*proxyProcess, func()) {
	t.Helper()

	cmd := exec.Command(toolpruneBin, args...)
	cmd.Dir = findRepoRoot()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("cmd.StdinPipe failed: %v", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("cmd.StdoutPipe failed: %v", err)
	}

	stderrBuf := new(bytes.Buffer)
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}

	proc := &proxyProcess{
		cmd:    cmd,
		stdin:  stdin,
		reader: bufio.NewReaderSize(stdout, 10*1024*1024),
		stderr: stderrBuf,
	}

	cleanup := func() {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()

		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}

	return proc, cleanup
}

func (p *proxyProcess) Call(t *testing.T, method string, params any) *mcp.Message {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	reqID := mcp.NewIntID(p.idCount.Add(1))
	req, err := mcp.NewRequest(reqID, method, params)
	if err != nil {
		t.Fatalf("NewRequest failed: %v", err)
	}

	if err := mcp.WriteMessage(p.stdin, req); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	resp, err := mcp.ReadMessage(p.reader)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v (stderr: %s)", err, p.stderr.String())
	}
	if resp == nil {
		t.Fatalf("expected response, got nil")
	}
	return resp
}

func (p *proxyProcess) Notify(t *testing.T, method string, params any) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	notif, err := mcp.NewNotification(method, params)
	if err != nil {
		t.Fatalf("NewNotification failed: %v", err)
	}
	if err := mcp.WriteMessage(p.stdin, notif); err != nil {
		t.Fatalf("WriteMessage notification failed: %v", err)
	}
}

func performStandardHandshake(t *testing.T, p *proxyProcess) *mcp.InitializeResult {
	t.Helper()

	initParams := mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "E2ETestClient", Version: "1.0.0"},
		Capabilities:    mcp.ClientCapabilities{},
	}

	resp := p.Call(t, mcp.MethodInitialize, initParams)
	if resp.Error != nil {
		t.Fatalf("initialize returned error: %v", resp.Error)
	}

	var initRes mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &initRes); err != nil {
		t.Fatalf("failed to unmarshal initialize result: %v", err)
	}

	p.Notify(t, mcp.MethodNotificationsInitialized, nil)
	return &initRes
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

func TestE2E_SchemaTokenBaselineAndTiers(t *testing.T) {
	// Directly spawn testserver to query its baseline tools
	cmd := exec.Command(testserverBin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe failed: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe failed: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("cmd.Start failed: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	reader := bufio.NewReaderSize(stdout, 10*1024*1024)

	// Handshake
	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "TokenCounter", Version: "1.0.0"},
	})
	_ = mcp.WriteMessage(stdin, initReq)
	_, _ = mcp.ReadMessage(reader)

	// List tools
	listReq, _ := mcp.NewRequest(mcp.NewIntID(2), mcp.MethodToolsList, nil)
	_ = mcp.WriteMessage(stdin, listReq)
	listResp, err := mcp.ReadMessage(reader)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	tools := listResult.Tools
	if len(tools) < 5 {
		t.Fatalf("expected at least 5 tools, got %d", len(tools))
	}

	rawTokens, err := benchmark.CountToolsTokens(tools)
	if err != nil {
		t.Fatalf("CountToolsTokens failed: %v", err)
	}

	t.Logf("Baseline TestServer Raw Unpruned Tokens: %d tokens across %d tools", rawTokens, len(tools))
	if rawTokens < 3500 {
		t.Fatalf("FAILURE: Expected > 3,500 unpruned tokens, but got %d", rawTokens)
	}

	// Verify pruning across tiers
	tiers := []struct {
		tier       pruner.PruneTier
		minSavings float64
	}{
		{pruner.TierLossless, 0.03},
		{pruner.TierCompact, 0.30},
		{pruner.TierAggressive, 0.40},
	}

	prevPruned := rawTokens
	for _, tc := range tiers {
		opts := pruner.DefaultPruneOptions(tc.tier)
		pruned, err := pruner.PruneToolsList(tools, opts)
		if err != nil {
			t.Fatalf("[%s] PruneToolsList failed: %v", tc.tier, err)
		}

		prunedTokens, err := benchmark.CountToolsTokens(pruned)
		if err != nil {
			t.Fatalf("[%s] CountToolsTokens failed: %v", tc.tier, err)
		}

		savings := 1.0 - float64(prunedTokens)/float64(rawTokens)
		t.Logf("[%s] Pruned tokens: %d (savings: %.1f%%)", tc.tier, prunedTokens, savings*100)

		if savings < tc.minSavings {
			t.Errorf("[%s] expected savings >= %.1f%%, got %.1f%%", tc.tier, tc.minSavings*100, savings*100)
		}
		if prunedTokens >= prevPruned && tc.tier != pruner.TierLossless {
			t.Errorf("[%s] expected monotonic reduction, got %d vs %d", tc.tier, prunedTokens, prevPruned)
		}
		prevPruned = prunedTokens

		// Verify every pruned schema remains valid Draft-07
		valResult := pruner.ValidateTools(pruned)
		if !valResult.Valid {
			t.Errorf("[%s] pruned tools produced invalid Draft-07 schema: %v", tc.tier, valResult.Errors)
		}
	}
}

func TestE2E_ZeroConfigWrapper_HandshakeAndLifecycle(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	initRes := performStandardHandshake(t, p)
	if initRes.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Errorf("expected protocol version %q, got %q", mcp.LatestProtocolVersion, initRes.ProtocolVersion)
	}
	if !strings.Contains(initRes.ServerInfo.Name, "ToolPrune") {
		t.Errorf("expected server name to indicate ToolPrune, got %q", initRes.ServerInfo.Name)
	}

	// Test ping
	pingResp := p.Call(t, mcp.MethodPing, map[string]any{})
	if pingResp.Error != nil {
		t.Fatalf("ping returned error: %v", pingResp.Error)
	}

	// Test clean shutdown: close stdin and assert exit code 0
	_ = p.stdin.Close()
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected clean exit code 0, got error: %v", err)
		}
	case <-time.After(4 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("proxy did not exit cleanly within 4s after stdin EOF")
	}
}

func TestE2E_ZeroConfigWrapper_ToolsListPruning(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--mode=aggressive", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	resp := p.Call(t, mcp.MethodToolsList, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list error: %v", resp.Error)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		t.Fatalf("failed to unmarshal tools/list: %v", err)
	}

	if len(listResult.Tools) == 0 {
		t.Fatalf("expected non-empty tools list")
	}

	prunedTokens, err := benchmark.CountToolsTokens(listResult.Tools)
	if err != nil {
		t.Fatalf("token count error: %v", err)
	}

	t.Logf("Aggressive Pruned Tools/List Tokens through Proxy: %d tokens", prunedTokens)

	// In aggressive mode, tokens should be reduced significantly relative to baseline >3,500
	if prunedTokens > 2300 {
		t.Errorf("expected aggressive pruned tokens <= 2300, got %d", prunedTokens)
	}

	// Check each tool
	for _, tool := range listResult.Tools {
		if tool.Name == "" {
			t.Errorf("found tool with empty name")
		}
		if tool.InputSchema.Type != "object" {
			t.Errorf("tool %q inputSchema.type must be 'object', got %q", tool.Name, tool.InputSchema.Type)
		}
	}
}

func TestE2E_ZeroConfigWrapper_ToolsCall_RealExecution(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	// 1. Math Eval: Add
	callAdd := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "math_eval",
		Arguments: json.RawMessage(`{
			"operation": "add",
			"a": 123.5,
			"b": 876.5
		}`),
	})
	if callAdd.Error != nil {
		t.Fatalf("math_eval add failed: %v", callAdd.Error)
	}
	var resAdd mcp.CallToolResult
	_ = json.Unmarshal(callAdd.Result, &resAdd)
	if len(resAdd.Content) == 0 || resAdd.Content[0].Text != "1000" {
		t.Errorf("expected '1000', got: %+v", resAdd)
	}

	// 2. Math Eval: Power
	callPow := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "math_eval",
		Arguments: json.RawMessage(`{
			"operation": "power",
			"a": 2,
			"b": 10
		}`),
	})
	var resPow mcp.CallToolResult
	_ = json.Unmarshal(callPow.Result, &resPow)
	if len(resPow.Content) == 0 || resPow.Content[0].Text != "1024" {
		t.Errorf("expected '1024', got: %+v", resPow)
	}

	// 3. Math Eval: Factorial
	callFact := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "math_eval",
		Arguments: json.RawMessage(`{
			"operation": "factorial",
			"a": 6
		}`),
	})
	var resFact mcp.CallToolResult
	_ = json.Unmarshal(callFact.Result, &resFact)
	if len(resFact.Content) == 0 || resFact.Content[0].Text != "720" {
		t.Errorf("expected '720', got: %+v", resFact)
	}

	// 4. Math Eval: Statistics (Mean & Median)
	callStats := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "math_eval",
		Arguments: json.RawMessage(`{
			"operation": "mean",
			"numbers": [10, 20, 30, 40, 50]
		}`),
	})
	var resStats mcp.CallToolResult
	_ = json.Unmarshal(callStats.Result, &resStats)
	if len(resStats.Content) == 0 || resStats.Content[0].Text != "30" {
		t.Errorf("expected '30', got: %+v", resStats)
	}

	// 5. String Transform: Uppercase
	callUpper := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "string_transform",
		Arguments: json.RawMessage(`{
			"action": "uppercase",
			"input": "toolprune token squeezer"
		}`),
	})
	var resUpper mcp.CallToolResult
	_ = json.Unmarshal(callUpper.Result, &resUpper)
	if len(resUpper.Content) == 0 || resUpper.Content[0].Text != "TOOLPRUNE TOKEN SQUEEZER" {
		t.Errorf("expected uppercase output, got: %+v", resUpper)
	}

	// 6. String Transform: SHA-256 Digest
	callSHA := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "string_transform",
		Arguments: json.RawMessage(`{
			"action": "sha256",
			"input": "hello world"
		}`),
	})
	var resSHA mcp.CallToolResult
	_ = json.Unmarshal(callSHA.Result, &resSHA)
	expectedSHA := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if len(resSHA.Content) == 0 || resSHA.Content[0].Text != expectedSHA {
		t.Errorf("expected SHA256 %s, got: %+v", expectedSHA, resSHA)
	}

	// 7. System Info
	callSys := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "system_info",
		Arguments: json.RawMessage(`{}`),
	})
	var resSys mcp.CallToolResult
	_ = json.Unmarshal(callSys.Result, &resSys)
	if len(resSys.Content) == 0 || !strings.Contains(resSys.Content[0].Text, "go_version") {
		t.Errorf("expected real system_info payload, got: %+v", resSys)
	}

	// 8. Data Query
	callQuery := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "data_query",
		Arguments: json.RawMessage(`{
			"dataset": [
				{"name": "Alice", "score": 95},
				{"name": "Bob", "score": 72},
				{"name": "Charlie", "score": 88}
			],
			"filter_field": "score",
			"filter_operator": "gte",
			"filter_value": 85
		}`),
	})
	var resQuery mcp.CallToolResult
	_ = json.Unmarshal(callQuery.Result, &resQuery)
	if len(resQuery.Content) == 0 || !strings.Contains(resQuery.Content[0].Text, "Alice") || strings.Contains(resQuery.Content[0].Text, "Bob") {
		t.Errorf("expected filtered dataset (Alice, Charlie), got: %+v", resQuery)
	}
}

func TestE2E_ZeroConfigWrapper_ToolsCall_ErrorHandling(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	// 1. Division by zero
	callDivZero := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "math_eval",
		Arguments: json.RawMessage(`{
			"operation": "divide",
			"a": 10,
			"b": 0
		}`),
	})
	var resDiv mcp.CallToolResult
	_ = json.Unmarshal(callDivZero.Result, &resDiv)
	if !resDiv.IsError || !strings.Contains(resDiv.Content[0].Text, "division by zero") {
		t.Errorf("expected division by zero error, got: %+v", resDiv)
	}

	// 2. Unknown tool name
	callUnknown := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "non_existent_tool_xyz",
		Arguments: json.RawMessage(`{}`),
	})
	var resUnk mcp.CallToolResult
	_ = json.Unmarshal(callUnknown.Result, &resUnk)
	if !resUnk.IsError || !strings.Contains(resUnk.Content[0].Text, "unknown tool") {
		t.Errorf("expected unknown tool error, got: %+v", resUnk)
	}
}

func TestE2E_MultiUpstream_ProxyMode(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "toolprune.yaml")

	// Create real configuration running two instances of testserver
	yamlContent := fmt.Sprintf(`version: "1"
server:
  transport: "stdio"
pruning:
  tier: "compact"
routing:
  namespace_mode: "always"
  default_profile: "full"
upstreams:
  - id: "alpha"
    prefix: "alpha"
    transport: "stdio"
    command: %q
  - id: "beta"
    prefix: "beta"
    transport: "stdio"
    command: %q
`, testserverBin, testserverBin)

	if err := os.WriteFile(configPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	p, cleanup := spawnProxy(t, "proxy", "--config", configPath)
	defer cleanup()

	performStandardHandshake(t, p)

	// Request tools/list
	listResp := p.Call(t, mcp.MethodToolsList, nil)
	if listResp.Error != nil {
		t.Fatalf("multi-upstream tools/list failed: %v", listResp.Error)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		t.Fatalf("unmarshal tools/list failed: %v", err)
	}

	toolNames := make(map[string]bool)
	for _, tool := range listResult.Tools {
		toolNames[tool.Name] = true
	}

	// Verify namespacing
	if !toolNames["alpha__math_eval"] {
		t.Errorf("expected namespaced tool 'alpha__math_eval' in multi-upstream, got tools: %v", toolNames)
	}
	if !toolNames["beta__math_eval"] {
		t.Errorf("expected namespaced tool 'beta__math_eval' in multi-upstream, got tools: %v", toolNames)
	}
	if !toolNames["alpha__string_transform"] {
		t.Errorf("expected namespaced tool 'alpha__string_transform' in multi-upstream")
	}

	// Call alpha upstream tool
	callAlpha := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "alpha__math_eval",
		Arguments: json.RawMessage(`{
			"operation": "add",
			"a": 55,
			"b": 45
		}`),
	})
	if callAlpha.Error != nil {
		t.Fatalf("call to alpha__math_eval failed: %v", callAlpha.Error)
	}
	var resAlpha mcp.CallToolResult
	_ = json.Unmarshal(callAlpha.Result, &resAlpha)
	if len(resAlpha.Content) == 0 || resAlpha.Content[0].Text != "100" {
		t.Errorf("expected '100' from alpha__math_eval, got: %+v", resAlpha)
	}

	// Call beta upstream tool
	callBeta := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name: "beta__string_transform",
		Arguments: json.RawMessage(`{
			"action": "uppercase",
			"input": "multiplexed test"
		}`),
	})
	if callBeta.Error != nil {
		t.Fatalf("call to beta__string_transform failed: %v", callBeta.Error)
	}
	var resBeta mcp.CallToolResult
	_ = json.Unmarshal(callBeta.Result, &resBeta)
	if len(resBeta.Content) == 0 || resBeta.Content[0].Text != "MULTIPLEXED TEST" {
		t.Errorf("expected 'MULTIPLEXED TEST' from beta, got: %+v", resBeta)
	}
}

func TestE2E_HTTP_SSE_Transport(t *testing.T) {
	// Find a free TCP port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	p, cleanup := spawnProxy(t, "proxy", "--transport=sse", fmt.Sprintf("--port=%d", port), "--", testserverBin)
	defer cleanup()

	// Wait for SSE server to bind and respond
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	sseURL := baseURL + "/sse"

	client := &http.Client{Timeout: 10 * time.Second}
	var sseResp *http.Response
	for i := 0; i < 20; i++ {
		req, _ := http.NewRequest("GET", sseURL, nil)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			sseResp = resp
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if sseResp == nil {
		t.Fatalf("failed to connect to SSE endpoint at %s (stderr: %s)", sseURL, p.stderr.String())
	}
	defer sseResp.Body.Close()

	// Read initial endpoint event from SSE stream
	scanner := bufio.NewScanner(sseResp.Body)
	var endpointLine string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			endpointLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}

	if endpointLine == "" {
		t.Fatalf("did not receive endpoint event on SSE stream")
	}

	messagesURL := baseURL + endpointLine
	if !strings.HasPrefix(endpointLine, "/") {
		messagesURL = baseURL + "/" + endpointLine
	}

	t.Logf("SSE endpoint established: %s", messagesURL)

	// Send POST /messages with initialize request
	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "SSETestClient", Version: "1.0.0"},
	})
	initBytes, _ := json.Marshal(initReq)

	postResp, err := client.Post(messagesURL, "application/json", bytes.NewReader(initBytes))
	if err != nil {
		t.Fatalf("POST /messages failed: %v", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode != http.StatusAccepted && postResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 202 Accepted, got %d", postResp.StatusCode)
	}

	// Read initialize response from SSE stream
	var initResponseReceived bool
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			data := strings.TrimPrefix(line, "data: ")
			var msg mcp.Message
			if err := json.Unmarshal([]byte(data), &msg); err == nil && msg.IsResponse() {
				initResponseReceived = true
				break
			}
		}
	}

	if !initResponseReceived {
		t.Errorf("did not receive initialize response over SSE stream")
	}
}

func TestE2E_UpstreamProcessTerminationPropagation(t *testing.T) {
	// Tests polish refinement 3: child exit terminates runStdioTransport
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	// Query testserver PID via system_info
	sysCall := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "system_info",
		Arguments: json.RawMessage(`{}`),
	})
	if sysCall.Error != nil {
		t.Fatalf("system_info call failed: %v", sysCall.Error)
	}

	var sysRes mcp.CallToolResult
	_ = json.Unmarshal(sysCall.Result, &sysRes)
	if len(sysRes.Content) == 0 {
		t.Fatalf("empty system_info content")
	}

	var info map[string]any
	_ = json.Unmarshal([]byte(sysRes.Content[0].Text), &info)
	childPIDFloat, ok := info["pid"].(float64)
	if !ok {
		t.Fatalf("could not extract child pid from system_info: %s", sysRes.Content[0].Text)
	}
	childPID := int(childPIDFloat)

	t.Logf("Supervised child testserver PID: %d, Proxy PID: %d", childPID, p.cmd.Process.Pid)

	// Kill the child testserver directly
	childProc, err := os.FindProcess(childPID)
	if err == nil {
		_ = childProc.Kill()
	}

	// Verify that toolprune proxy exits promptly (within 3 seconds) instead of hanging on stdin!
	done := make(chan error, 1)
	go func() {
		done <- p.cmd.Wait()
	}()

	select {
	case <-done:
		t.Logf("SUCCESS: ToolPrune proxy terminated immediately when upstream child died!")
	case <-time.After(3 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("FAILURE: ToolPrune proxy remained stuck after upstream child died (did not propagate exit event)")
	}
}

func TestE2E_CommandNotFoundExit127(t *testing.T) {
	// Tests polish refinements 1 & 2: exec.ErrNotFound exits with code 127 and prints error to stderr
	cmd := exec.Command(toolpruneBin, "proxy", "--", "definitely_non_existent_command_xyz_404")
	cmd.Dir = findRepoRoot()

	stderrBuf := new(bytes.Buffer)
	cmd.Stderr = stderrBuf

	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected command not found to return error, got nil")
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode := exitErr.ExitCode()
		if exitCode != 127 {
			t.Errorf("expected exit code 127 on command not found, got %d", exitCode)
		}
	} else {
		t.Fatalf("expected ExitError, got %T: %v", err, err)
	}

	stderrOutput := stderrBuf.String()
	if !strings.Contains(stderrOutput, "Error:") {
		t.Errorf("expected 'Error:' in stderr, got: %q", stderrOutput)
	}
	t.Logf("Command not found test passed: exit code 127 and printed 'Error:' to stderr.")
}

func TestE2E_RoutesInspectionCLI(t *testing.T) {
	// Tests polish refinement 4: rFlags.profile and rFlags.filter applied
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "routes_test.yaml")

	yamlContent := fmt.Sprintf(`version: "1"
routing:
  default_profile: "full"
upstreams:
  - id: "alpha_service"
    prefix: "alpha"
    command: %q
  - id: "beta_service"
    prefix: "beta"
    command: %q
`, testserverBin, testserverBin)

	if err := os.WriteFile(configPath, []byte(yamlContent), 0o600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	// 1. Test --profile dev
	cmdProfile := exec.Command(toolpruneBin, "routes", "--config", configPath, "--profile", "dev")
	outProf, err := cmdProfile.CombinedOutput()
	if err != nil {
		t.Fatalf("routes --profile dev failed: %v\n%s", err, string(outProf))
	}
	if !strings.Contains(string(outProf), "Active Profile: dev") {
		t.Errorf("expected 'Active Profile: dev' in output, got:\n%s", string(outProf))
	}

	// 2. Test --filter beta
	cmdFilter := exec.Command(toolpruneBin, "routes", "--config", configPath, "--filter", "beta")
	outFilt, err := cmdFilter.CombinedOutput()
	if err != nil {
		t.Fatalf("routes --filter beta failed: %v\n%s", err, string(outFilt))
	}
	filtStr := string(outFilt)
	if !strings.Contains(filtStr, "beta_service") {
		t.Errorf("expected 'beta_service' in filtered output, got:\n%s", filtStr)
	}
	if strings.Contains(filtStr, "alpha_service") {
		t.Errorf("expected 'alpha_service' to be filtered out, got:\n%s", filtStr)
	}
	if !strings.Contains(filtStr, "Active Filter:  beta") {
		t.Errorf("expected 'Active Filter:  beta' in output, got:\n%s", filtStr)
	}

	// 3. Test --json with filter
	cmdJSON := exec.Command(toolpruneBin, "routes", "--config", configPath, "--filter", "beta", "--json")
	outJSON, err := cmdJSON.CombinedOutput()
	if err != nil {
		t.Fatalf("routes --json failed: %v\n%s", err, string(outJSON))
	}
	var parsedJSON map[string]any
	if err := json.Unmarshal(outJSON, &parsedJSON); err != nil {
		t.Fatalf("failed to parse JSON routes: %v\n%s", err, string(outJSON))
	}
	upstreams, ok := parsedJSON["upstreams"].([]any)
	if !ok || len(upstreams) != 1 {
		t.Errorf("expected 1 filtered upstream in JSON, got: %+v", parsedJSON["upstreams"])
	}
}
