package e2e_test

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// -----------------------------------------------------------------------------
// 1. Burst Testing: TestServer & Proxy (Cross-Platform)
// -----------------------------------------------------------------------------

func TestAdversarial_TestServer_RequestBurst(t *testing.T) {
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

	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "BurstClient", Version: "1.0.0"},
	})
	if err := mcp.WriteMessage(stdin, initReq); err != nil {
		t.Fatalf("init write failed: %v", err)
	}
	if _, err := mcp.ReadMessage(reader); err != nil {
		t.Fatalf("init read failed: %v", err)
	}

	const burstCount = 100
	t.Logf("Firing burst of %d requests directly to testserver...", burstCount)

	start := time.Now()
	for i := 1; i <= burstCount; i++ {
		reqID := mcp.NewIntID(int64(100 + i))
		var req *mcp.Message
		switch i % 3 {
		case 0:
			req, _ = mcp.NewRequest(reqID, mcp.MethodPing, map[string]any{})
		case 1:
			req, _ = mcp.NewRequest(reqID, mcp.MethodToolsCall, mcp.CallToolRequestParams{
				Name: "math_eval",
				Arguments: json.RawMessage(fmt.Sprintf(`{
					"operation": "add",
					"a": %d,
					"b": 10
				}`, i)),
			})
		case 2:
			req, _ = mcp.NewRequest(reqID, mcp.MethodToolsCall, mcp.CallToolRequestParams{
				Name: "string_transform",
				Arguments: json.RawMessage(fmt.Sprintf(`{
					"action": "reverse",
					"input": "burst-test-%d"
				}`, i)),
			})
		}

		if err := mcp.WriteMessage(stdin, req); err != nil {
			t.Fatalf("request %d write failed: %v", i, err)
		}

		resp, err := mcp.ReadMessage(reader)
		if err != nil {
			t.Fatalf("response %d read failed: %v", i, err)
		}
		if resp == nil || resp.ID == nil {
			t.Fatalf("response %d nil or missing ID: got %+v", i, resp)
		}
		idVal, ok := resp.ID.Int64()
		if !ok || idVal != int64(100+i) {
			t.Fatalf("response %d ID mismatch: expected %d, got %v", i, 100+i, resp.ID)
		}
	}

	duration := time.Since(start)
	t.Logf("SUCCESS: TestServer handled %d burst requests in %v (%.2f req/s)",
		burstCount, duration, float64(burstCount)/duration.Seconds())
}

func TestAdversarial_Proxy_RequestBurst(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	const burstCount = 100
	t.Logf("Firing burst of %d requests through ToolPrune proxy...", burstCount)

	start := time.Now()
	for i := 1; i <= burstCount; i++ {
		var resp *mcp.Message
		switch i % 3 {
		case 0:
			resp = p.Call(t, mcp.MethodPing, map[string]any{})
			if resp.Error != nil {
				t.Fatalf("ping %d failed: %v", i, resp.Error)
			}
		case 1:
			resp = p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
				Name: "math_eval",
				Arguments: json.RawMessage(fmt.Sprintf(`{
					"operation": "multiply",
					"a": %d,
					"b": 2
				}`, i)),
			})
			if resp.Error != nil {
				t.Fatalf("math_eval %d failed: %v", i, resp.Error)
			}
			var res mcp.CallToolResult
			_ = json.Unmarshal(resp.Result, &res)
			expected := fmt.Sprintf("%d", i*2)
			if len(res.Content) == 0 || res.Content[0].Text != expected {
				t.Fatalf("math_eval %d expected %s, got %+v", i, expected, res)
			}
		case 2:
			resp = p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
				Name: "string_transform",
				Arguments: json.RawMessage(fmt.Sprintf(`{
					"action": "uppercase",
					"input": "alpha_%d"
				}`, i)),
			})
			if resp.Error != nil {
				t.Fatalf("string_transform %d failed: %v", i, resp.Error)
			}
			var res mcp.CallToolResult
			_ = json.Unmarshal(resp.Result, &res)
			expected := fmt.Sprintf("ALPHA_%d", i)
			if len(res.Content) == 0 || res.Content[0].Text != expected {
				t.Fatalf("string_transform %d expected %s, got %+v", i, expected, res)
			}
		}
	}

	duration := time.Since(start)
	t.Logf("SUCCESS: ToolPrune proxy pipelined %d burst requests in %v (%.2f req/s)",
		burstCount, duration, float64(burstCount)/duration.Seconds())
}

// -----------------------------------------------------------------------------
// 2. Error Handling & Adversarial Arguments Verification
// -----------------------------------------------------------------------------

func TestAdversarial_ErrorHandlingAndInvalidArguments(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	tests := []struct {
		name        string
		method      string
		params      any
		wantRpcErr  bool
		wantToolErr bool
		errSnippet  string
	}{
		{
			name:   "Missing required parameter 'operation' for math_eval",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "math_eval",
				Arguments: json.RawMessage(`{"a": 10, "b": 20}`),
			},
			wantToolErr: true,
			errSnippet:  "missing required parameter 'operation'",
		},
		{
			name:   "Unsupported operation for math_eval",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "math_eval",
				Arguments: json.RawMessage(`{"operation": "quantum_eigenvalues", "a": 10}`),
			},
			wantToolErr: true,
			errSnippet:  "unsupported operation",
		},
		{
			name:   "Missing required parameter 'action' for string_transform",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "string_transform",
				Arguments: json.RawMessage(`{"input": "hello"}`),
			},
			wantToolErr: true,
			errSnippet:  "missing required parameter 'action'",
		},
		{
			name:   "Invalid base64 payload for string_transform base64_decode",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name: "string_transform",
				Arguments: json.RawMessage(`{
					"action": "base64_decode",
					"input": "!!!NotValidBase64!!!"
				}`),
			},
			wantToolErr: true,
			errSnippet:  "invalid base64 payload",
		},
		{
			name:   "Missing required provider for cloud_config_validator",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "cloud_config_validator",
				Arguments: json.RawMessage(`{"region": "us-west-2"}`),
			},
			wantToolErr: true,
			errSnippet:  "missing required parameter 'provider'",
		},
		{
			name:   "Missing required engine for database_schema_analyzer",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "database_schema_analyzer",
				Arguments: json.RawMessage(`{"schema_sql": "CREATE TABLE users(id INT);"}`),
			},
			wantToolErr: true,
			errSnippet:  "missing required parameter 'engine'",
		},
		{
			name:   "Missing dataset for data_query",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "data_query",
				Arguments: json.RawMessage(`{"filter_field": "age", "filter_value": 30}`),
			},
			wantToolErr: true,
			errSnippet:  "missing or empty 'dataset' array",
		},
		{
			name:   "Unknown tool dispatch",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "totally_non_existent_tool_xyz",
				Arguments: json.RawMessage(`{}`),
			},
			wantToolErr: true,
			errSnippet:  "unknown tool",
		},
		{
			name:   "Malformed arguments type (array instead of object map)",
			method: mcp.MethodToolsCall,
			params: mcp.CallToolRequestParams{
				Name:      "math_eval",
				Arguments: json.RawMessage(`[1, 2, 3]`),
			},
			wantToolErr: true,
			errSnippet:  "JSON unmarshal error for arguments",
		},
		{
			name:       "Unknown JSON-RPC method",
			method:     "custom/non_existent_method",
			params:     map[string]any{},
			wantRpcErr: true,
			errSnippet: "method \"custom/non_existent_method\" not found",
		},
		{
			name:       "Invalid JSON-RPC params structure",
			method:     mcp.MethodToolsCall,
			params:     "this is a raw string rather than an object",
			wantRpcErr: true,
			errSnippet: "invalid call params",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := p.Call(t, tc.method, tc.params)
			if tc.wantRpcErr {
				if resp.Error == nil {
					t.Fatalf("expected JSON-RPC error, got success response: %+v", resp)
				}
				if !strings.Contains(resp.Error.Message, tc.errSnippet) {
					t.Errorf("expected error message to contain %q, got %q", tc.errSnippet, resp.Error.Message)
				}
				return
			}

			if resp.Error != nil {
				t.Fatalf("unexpected JSON-RPC protocol error: %v", resp.Error)
			}

			if tc.wantToolErr {
				var callRes mcp.CallToolResult
				if err := json.Unmarshal(resp.Result, &callRes); err != nil {
					t.Fatalf("failed to unmarshal CallToolResult: %v", err)
				}
				if !callRes.IsError {
					t.Errorf("expected CallToolResult.IsError = true, got false")
				}
				if len(callRes.Content) == 0 {
					t.Fatalf("expected non-empty error content")
				}
				if !strings.Contains(callRes.Content[0].Text, tc.errSnippet) {
					t.Errorf("expected error text to contain %q, got %q", tc.errSnippet, callRes.Content[0].Text)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 3. Stdin Abrupt Closure & Clean Process Termination
// -----------------------------------------------------------------------------

func TestAdversarial_TestServer_AbruptStdinClosure(t *testing.T) {
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

	reader := bufio.NewReaderSize(stdout, 1024*1024)

	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "StdinClosureClient", Version: "1.0.0"},
	})
	_ = mcp.WriteMessage(stdin, initReq)
	_, err = mcp.ReadMessage(reader)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	closeStart := time.Now()
	if err := stdin.Close(); err != nil {
		t.Fatalf("stdin.Close failed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected clean exit code 0 on stdin close, got: %v", err)
		}
		duration := time.Since(closeStart)
		t.Logf("SUCCESS: testserver exited cleanly in %v after stdin closure", duration)
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("FAILURE: testserver hung and did not terminate within 2s of stdin closure")
	}
}

// -----------------------------------------------------------------------------
// 4. Large Payload Framing & Safe Bounded Buffering (500KB - 2MB)
// -----------------------------------------------------------------------------

func TestAdversarial_Proxy_LargePayloadFraming_MultiMegabyte(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	payloadSizes := []struct {
		name      string
		byteCount int
		action    string
	}{
		{
			name:      "500KB Payload SHA256",
			byteCount: 500 * 1024,
			action:    "sha256",
		},
		{
			name:      "1MB Payload Reverse (Bidirectional Large Frame)",
			byteCount: 1024 * 1024,
			action:    "reverse",
		},
		{
			name:      "2MB Payload SHA256",
			byteCount: 2 * 1024 * 1024,
			action:    "sha256",
		},
	}

	for _, tc := range payloadSizes {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("Generating %d bytes test payload...", tc.byteCount)

			buf := make([]byte, tc.byteCount)
			for i := 0; i < tc.byteCount; i++ {
				buf[i] = byte('A' + (i % 26))
			}
			inputStr := string(buf)

			start := time.Now()
			resp := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
				Name: "string_transform",
				Arguments: json.RawMessage(fmt.Sprintf(`{
					"action": %q,
					"input": %q
				}`, tc.action, inputStr)),
			})

			duration := time.Since(start)

			if resp.Error != nil {
				t.Fatalf("[%s] JSON-RPC error: %v", tc.name, resp.Error)
			}

			var callRes mcp.CallToolResult
			if err := json.Unmarshal(resp.Result, &callRes); err != nil {
				t.Fatalf("[%s] unmarshal CallToolResult failed: %v", tc.name, err)
			}
			if callRes.IsError {
				t.Fatalf("[%s] tool execution returned error: %+v", tc.name, callRes)
			}
			if len(callRes.Content) == 0 {
				t.Fatalf("[%s] returned empty content", tc.name)
			}

			output := callRes.Content[0].Text

			switch tc.action {
			case "sha256":
				expectedHash := sha256.Sum256(buf)
				expectedHex := hex.EncodeToString(expectedHash[:])
				if output != expectedHex {
					t.Fatalf("[%s] SHA256 hash mismatch: expected %s, got %s", tc.name, expectedHex, output)
				}
				t.Logf("SUCCESS [%s]: Validated %d byte payload processed and hashed in %v",
					tc.name, tc.byteCount, duration)

			case "reverse":
				if len(output) != len(inputStr) {
					t.Fatalf("[%s] reversed output length mismatch: expected %d, got %d",
						tc.name, len(inputStr), len(output))
				}
				if output[0] != inputStr[len(inputStr)-1] || output[len(output)-1] != inputStr[0] {
					t.Fatalf("[%s] reversed content mismatch", tc.name)
				}
				t.Logf("SUCCESS [%s]: Validated %d byte bidirectional roundtrip in %v",
					tc.name, tc.byteCount, duration)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 5. v1.2.0 E2E: RFC #1 Token Budget & Pre-Binding Self-Correction
// -----------------------------------------------------------------------------

func TestE2E_V12_MaxSchemaTokensAndPreBindSelfCorrection(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--max-schema-tokens=650", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	listResp := p.Call(t, mcp.MethodToolsList, map[string]any{})
	if listResp.Error != nil {
		t.Fatalf("tools/list failed: %v", listResp.Error)
	}
	var listRes mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listRes); err != nil {
		t.Fatalf("unmarshal ListToolsResult: %v", err)
	}
	toks, err := benchmark.CountToolsTokens(listRes.Tools)
	if err != nil {
		t.Fatalf("CountToolsTokens: %v", err)
	}
	t.Logf("v1.2.0 --max-schema-tokens=650 enforced exact cl100k_base tokens=%d across %d tools", toks, len(listRes.Tools))
	if toks > 650 {
		t.Fatalf("expected cl100k_base tokens <= 650, got %d", toks)
	}

	// Missing required parameter 'operation' must be intercepted by StreamFilter before upstream
	badResp := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "math_eval",
		Arguments: json.RawMessage(`{"a": 21, "b": 21}`),
	})
	if badResp.Error != nil {
		t.Fatalf("unexpected JSON-RPC error: %v", badResp.Error)
	}
	var badCallRes mcp.CallToolResult
	if err := json.Unmarshal(badResp.Result, &badCallRes); err != nil {
		t.Fatalf("unmarshal CallToolResult: %v", err)
	}
	if !badCallRes.IsError || len(badCallRes.Content) == 0 {
		t.Fatalf("expected self-correcting CallToolResult{IsError: true}, got %+v", badCallRes)
	}
	if !strings.Contains(badCallRes.Content[0].Text, "Pre-Binding Schema Validation Failed") {
		t.Fatalf("expected pre-binding validation diagnostic, got: %s", badCallRes.Content[0].Text)
	}

	// Subsequent valid call must succeed cleanly
	okResp := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "math_eval",
		Arguments: json.RawMessage(`{"operation": "add", "a": 21, "b": 21}`),
	})
	if okResp.Error != nil {
		t.Fatalf("valid math_eval failed: %v", okResp.Error)
	}
	var okCallRes mcp.CallToolResult
	_ = json.Unmarshal(okResp.Result, &okCallRes)
	if okCallRes.IsError || len(okCallRes.Content) == 0 || okCallRes.Content[0].Text != "42" {
		t.Fatalf("expected math_eval result '42', got %+v", okCallRes)
	}
}

// -----------------------------------------------------------------------------
// 6. v1.2.0 E2E: Malformed JSON & Batch Array Recovery over Stdio
// -----------------------------------------------------------------------------

func TestE2E_V12_StdioMalformedJSONAndBatchRecovery(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--preset=balanced", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	// 1. Malformed JSON line -> JSON-RPC -32700 Parse error (must not kill proxy)
	p.mu.Lock()
	_, err := io.WriteString(p.stdin, "{malformed_json_line\n")
	if err != nil {
		p.mu.Unlock()
		t.Fatalf("write malformed JSON failed: %v", err)
	}
	parseErrMsg, err := mcp.ReadMessage(p.reader)
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("read -32700 response failed: %v", err)
	}
	if parseErrMsg.Error == nil || parseErrMsg.Error.Code != mcp.CodeParseError {
		t.Fatalf("expected -32700 Parse error, got %+v", parseErrMsg)
	}

	// 2. Unsupported JSON-RPC batch array -> JSON-RPC -32600 Invalid Request
	p.mu.Lock()
	_, err = io.WriteString(p.stdin, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`+"\n")
	if err != nil {
		p.mu.Unlock()
		t.Fatalf("write batch array failed: %v", err)
	}
	batchErrMsg, err := mcp.ReadMessage(p.reader)
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("read -32600 response failed: %v", err)
	}
	if batchErrMsg.Error == nil || batchErrMsg.Error.Code != mcp.CodeInvalidRequest {
		t.Fatalf("expected -32600 Invalid Request, got %+v", batchErrMsg)
	}

	// 3. Subsequent normal ping must succeed cleanly
	pingResp := p.Call(t, mcp.MethodPing, map[string]any{})
	if pingResp.Error != nil {
		t.Fatalf("ping after recovery failed: %v", pingResp.Error)
	}
}

// -----------------------------------------------------------------------------
// 7. v1.2.0 E2E: Streamable HTTP (/mcp) & Live Subprocess Benchmarking
// -----------------------------------------------------------------------------

func TestE2E_V12_StreamableHTTP_And_LiveBench(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	cmd := exec.Command(toolpruneBin, "proxy", "--transport=sse", "--port="+strconv.Itoa(port), "--preset=strict", "--", testserverBin)
	cmd.Dir = findRepoRoot()
	stderrBuf := new(bytes.Buffer)
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start SSE/HTTP proxy: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		resp, hErr := http.Get(baseURL + "/health")
		if hErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("SSE/HTTP proxy did not become healthy on %s; stderr: %s", baseURL, stderrBuf.String())
	}

	// Send initialize via Streamable HTTP POST /mcp
	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "StreamableHTTPClient", Version: "1.2.0"},
	})
	initBytes, _ := json.Marshal(initReq)
	httpReq, _ := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(initBytes))
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("POST /mcp initialize failed: %v", err)
	}
	sessionID := httpResp.Header.Get("Mcp-Session-Id")
	var initMsg mcp.Message
	if err := json.NewDecoder(httpResp.Body).Decode(&initMsg); err != nil {
		_ = httpResp.Body.Close()
		t.Fatalf("decode POST /mcp initialize response: %v", err)
	}
	_ = httpResp.Body.Close()
	if sessionID == "" {
		t.Fatalf("expected Mcp-Session-Id response header from POST /mcp")
	}
	if initMsg.Error != nil {
		t.Fatalf("POST /mcp initialize error: %v", initMsg.Error)
	}

	// Send tools/call over POST /mcp using Mcp-Session-Id
	callReq, _ := mcp.NewRequest(mcp.NewIntID(2), mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "math_eval",
		Arguments: json.RawMessage(`{"operation": "multiply", "a": 6, "b": 7}`),
	})
	callBytes, _ := json.Marshal(callReq)
	callHTTPReq, _ := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(callBytes))
	callHTTPReq.Header.Set("Content-Type", "application/json")
	callHTTPReq.Header.Set("Mcp-Session-Id", sessionID)

	callHTTPResp, err := http.DefaultClient.Do(callHTTPReq)
	if err != nil {
		t.Fatalf("POST /mcp tools/call failed: %v", err)
	}
	var callMsg mcp.Message
	if err := json.NewDecoder(callHTTPResp.Body).Decode(&callMsg); err != nil {
		_ = callHTTPResp.Body.Close()
		t.Fatalf("decode POST /mcp tools/call response: %v", err)
	}
	_ = callHTTPResp.Body.Close()
	var callRes mcp.CallToolResult
	_ = json.Unmarshal(callMsg.Result, &callRes)
	if callRes.IsError || len(callRes.Content) == 0 || callRes.Content[0].Text != "42" {
		t.Fatalf("expected '42' over Streamable HTTP /mcp, got %+v", callRes)
	}

	// Verify live subprocess benchmarking (`toolprune bench --format csv -- <testserverBin>`)
	benchCmd := exec.Command(toolpruneBin, "bench", "--format", "csv", "--max-schema-tokens", "500", "--", testserverBin)
	benchCmd.Dir = findRepoRoot()
	benchOut, err := benchCmd.Output()
	if err != nil {
		t.Fatalf("live toolprune bench failed: %v\n%s", err, string(benchOut))
	}
	csvStr := string(benchOut)
	if !strings.Contains(csvStr, "record_type,name,tier,tokens,bytes,tokens_saved,savings_percent") {
		t.Fatalf("expected CSV header in live bench output, got:\n%s", csvStr)
	}
	if !strings.Contains(csvStr, "tier,Budget (<=500 tok),budget,") {
		t.Fatalf("expected RFC #1 budget row in live bench CSV output, got:\n%s", csvStr)
	}
}
