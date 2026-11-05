//go:build windows

package e2e_test

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"golang.org/x/sys/windows"
)

// helper to check if a process is still alive on Windows
func isProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	const stillActive = 259
	return exitCode == stillActive
}

// -----------------------------------------------------------------------------
// 1. Burst Testing: TestServer & Proxy
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

	// Handshake
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

	// Fire burst of 100 requests in rapid succession
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

	// Perform initialize
	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "StdinClosureClient", Version: "1.0.0"},
	})
	_ = mcp.WriteMessage(stdin, initReq)
	_, err = mcp.ReadMessage(reader)
	if err != nil {
		t.Fatalf("ReadMessage failed: %v", err)
	}

	// Abruptly close stdin
	closeStart := time.Now()
	if err := stdin.Close(); err != nil {
		t.Fatalf("stdin.Close failed: %v", err)
	}

	// Wait for process to exit
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
// 4. Large Payload Framing & Safe Scanner Buffering (500KB - 2MB)
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

			// Generate repeatable ASCII payload
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
				// Verify reversed sample characters
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
// 5. Windows Job Object Orphan Prevention & Clean Propagation
// -----------------------------------------------------------------------------

func TestAdversarial_WindowsJobObject_OrphanPrevention(t *testing.T) {
	// Spawns ToolPrune proxy with testserver child
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	// Retrieve child PID via system_info
	resp := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "system_info",
		Arguments: json.RawMessage(`{}`),
	})
	if resp.Error != nil {
		t.Fatalf("system_info failed: %v", resp.Error)
	}

	var callRes mcp.CallToolResult
	_ = json.Unmarshal(resp.Result, &callRes)
	if len(callRes.Content) == 0 {
		t.Fatalf("empty system_info content")
	}

	var info map[string]any
	_ = json.Unmarshal([]byte(callRes.Content[0].Text), &info)
	childPIDFloat, ok := info["pid"].(float64)
	if !ok {
		t.Fatalf("missing child PID in system_info: %s", callRes.Content[0].Text)
	}
	childPID := int(childPIDFloat)
	proxyPID := p.cmd.Process.Pid

	t.Logf("Proxy PID: %d, Upstream Child PID: %d", proxyPID, childPID)

	// Verify child PID is actively running
	if !isProcessAlive(childPID) {
		t.Fatalf("child process %d is not running prior to kill test", childPID)
	}

	// Force kill PROXY directly using taskkill /F (simulating SIGKILL / crash of parent)
	killCmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(proxyPID))
	if out, err := killCmd.CombinedOutput(); err != nil {
		t.Fatalf("taskkill proxy failed: %v (output: %s)", err, string(out))
	}

	// Windows Job Object limit JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE must automatically
	// terminate the child process when the proxy process terminates and its job handle closes!
	var childDied bool
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(childPID) {
			childDied = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !childDied {
		// Clean up leaked process before failing test
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(childPID)).Run()
		t.Fatalf("FAILURE: Windows Job Object failed to terminate child process %d after proxy %d was killed! Orphaned process detected.",
			childPID, proxyPID)
	}

	t.Logf("SUCCESS: Windows Job Object automatically terminated child process %d when proxy died. Zero orphan leak confirmed!", childPID)
}
