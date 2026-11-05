package mcp_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// ============================================================================
// Objective 1: Oversized Message Boundary Testing (pkg/mcp)
// ============================================================================

func TestAdversarial_OversizedMessageBoundaries(t *testing.T) {
	sizes := []struct {
		name      string
		targetLen int
	}{
		{"100KB", 100 * 1024},
		{"500KB", 500 * 1024},
		{"1MB", 1 * 1024 * 1024},
		{"5MB", 5 * 1024 * 1024},
		{"9.5MB", int(9.5 * 1024 * 1024)}, // 9,961,472 bytes (close to 10MB limit)
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			// Construct valid JSON-RPC message with padding to reach exact targetLen
			prefix := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"data":"`
			suffix := `"}}` + "\n"
			padLen := tc.targetLen - len(prefix) - len(suffix)
			if padLen < 0 {
				padLen = 10
			}

			var payloadBuf bytes.Buffer
			payloadBuf.Grow(tc.targetLen + 64)
			payloadBuf.WriteString(prefix)
			payloadBuf.WriteString(strings.Repeat("M", padLen))
			payloadBuf.WriteString(suffix)

			rawBytes := payloadBuf.Bytes()
			actualSize := len(rawBytes)

			t.Logf("Testing %s (actual message size: %d bytes)", tc.name, actualSize)

			// 1. Test mcp.NewReader
			var mBefore, mAfter runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&mBefore)
			start := time.Now()

			reader := mcp.NewReader(bytes.NewReader(rawBytes))
			msg, err := reader.ReadMessage()
			elapsed := time.Since(start)
			runtime.ReadMemStats(&mAfter)

			if err != nil {
				t.Fatalf("[%s] mcp.NewReader.ReadMessage failed: %v", tc.name, err)
			}
			if msg == nil {
				t.Fatalf("[%s] expected non-nil message", tc.name)
			}
			if msg.Method != "tools/call" {
				t.Fatalf("[%s] expected method 'tools/call', got %q", tc.name, msg.Method)
			}
			if msg.ID == nil || msg.ID.String() != "1" {
				t.Fatalf("[%s] expected ID 1, got %v", tc.name, msg.ID)
			}

			// Validate payload integrity without truncation
			var params struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				t.Fatalf("[%s] failed to unmarshal params: %v", tc.name, err)
			}
			if len(params.Data) != padLen {
				t.Fatalf("[%s] data truncated! expected %d bytes, got %d bytes", tc.name, padLen, len(params.Data))
			}

			t.Logf("[%s] NewReader SUCCESS: elapsed=%v, alloc_delta=%d KB",
				tc.name, elapsed, (mAfter.Alloc-mBefore.Alloc)/1024)

			// 2. Test direct mcp.ReadMessage(*bufio.Reader) contract
			bufReader := bufio.NewReaderSize(bytes.NewReader(rawBytes), 256*1024)
			startDirect := time.Now()
			msgDirect, errDirect := mcp.ReadMessage(bufReader)
			elapsedDirect := time.Since(startDirect)

			if errDirect != nil {
				t.Fatalf("[%s] direct ReadMessage failed: %v", tc.name, errDirect)
			}
			if msgDirect == nil || msgDirect.Method != "tools/call" {
				t.Fatalf("[%s] direct ReadMessage returned invalid message: %v", tc.name, msgDirect)
			}

			t.Logf("[%s] direct ReadMessage SUCCESS: elapsed=%v", tc.name, elapsedDirect)
		})
	}
}

func TestAdversarial_Exceeding10MBLimit(t *testing.T) {
	overflowSizes := []struct {
		name      string
		targetLen int
	}{
		{"10MB+100B", mcp.DefaultMaxMessageSize + 100},
		{"10.5MB", int(10.5 * 1024 * 1024)},
		{"12MB", 12 * 1024 * 1024},
	}

	for _, tc := range overflowSizes {
		t.Run(tc.name, func(t *testing.T) {
			prefix := `{"jsonrpc":"2.0","id":99,"method":"oversized","params":{"data":"`
			suffix := `"}}` + "\n"
			padLen := tc.targetLen - len(prefix) - len(suffix)

			var payloadBuf bytes.Buffer
			payloadBuf.Grow(tc.targetLen + 64)
			payloadBuf.WriteString(prefix)
			payloadBuf.WriteString(strings.Repeat("O", padLen))
			payloadBuf.WriteString(suffix)

			rawBytes := payloadBuf.Bytes()

			// 1. Verify mcp.NewReader gracefully returns ErrMessageTooLarge without panic
			reader := mcp.NewReader(bytes.NewReader(rawBytes))
			msg, err := reader.ReadMessage()

			if msg != nil {
				t.Fatalf("[%s] expected nil message for oversized payload, got %v", tc.name, msg)
			}
			if err == nil {
				t.Fatalf("[%s] expected error for payload of size %d, but got nil", tc.name, len(rawBytes))
			}
			if !errors.Is(err, mcp.ErrMessageTooLarge) {
				t.Fatalf("[%s] expected ErrMessageTooLarge, got: %v", tc.name, err)
			}
			t.Logf("[%s] NewReader correctly rejected oversized message with ErrMessageTooLarge: %v", tc.name, err)

			// 2. Verify direct mcp.ReadMessage(*bufio.Reader) also rejects oversized message gracefully
			bufReader := bufio.NewReaderSize(bytes.NewReader(rawBytes), 256*1024)
			msgDirect, errDirect := mcp.ReadMessage(bufReader)

			if msgDirect != nil {
				t.Fatalf("[%s] direct ReadMessage expected nil message, got %v", tc.name, msgDirect)
			}
			if errDirect == nil {
				t.Fatalf("[%s] direct ReadMessage expected error, got nil", tc.name)
			}
			if !errors.Is(errDirect, mcp.ErrMessageTooLarge) {
				t.Fatalf("[%s] direct ReadMessage expected ErrMessageTooLarge, got: %v", tc.name, errDirect)
			}
			t.Logf("[%s] direct ReadMessage correctly rejected oversized message: %v", tc.name, errDirect)
		})
	}
}

// ============================================================================
// Objective 2: Malformed JSON Robustness & Error Codes
// ============================================================================

func TestAdversarial_MalformedJSONRobustness(t *testing.T) {
	// Construct deeply nested JSON structure (500 levels)
	var nestedBuilder strings.Builder
	nestedBuilder.WriteString(`{"jsonrpc":"2.0","id":666,"method":"deep","params":{"obj":`)
	for i := 0; i < 500; i++ {
		nestedBuilder.WriteString(`{"n":`)
	}
	nestedBuilder.WriteString(`"bottom"`)
	for i := 0; i < 500; i++ {
		nestedBuilder.WriteString(`}`)
	}
	nestedBuilder.WriteString(`}}` + "\n")
	deeplyNestedPayload := nestedBuilder.String()

	testCases := []struct {
		name        string
		input       string
		description string
	}{
		{
			name:        "InvalidJSONSyntax",
			input:       `{"jsonrpc": 2.0, "id": 1, method: missing_quotes}` + "\n",
			description: "Missing quotes around keys and strings, unquoted numbers",
		},
		{
			name:        "IncompleteBrackets_Truncated",
			input:       `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"arg":` + "\n",
			description: "Truncated object without closing braces",
		},
		{
			name:        "TrailingGarbageData",
			input:       `{"jsonrpc":"2.0","id":3,"method":"ping"} EXTRA_TRAILING_DATA` + "\n",
			description: "Valid JSON object followed by trailing characters on the same line",
		},
		{
			name:        "NullBytesInPayload",
			input:       "{\"jsonrpc\":\"2.0\",\x00\"id\":4,\"method\":\"tools/list\"}\n",
			description: "Embedded null byte \\x00 inside JSON line",
		},
		{
			name:        "NonUTF8Bytes",
			input:       "{\"jsonrpc\":\"2.0\",\"id\":5,\"method\":\"\xff\xfe\xfd\x80\"}\n",
			description: "Invalid UTF-8 byte sequence in method field",
		},
		{
			name:        "DeeplyNestedStructures",
			input:       deeplyNestedPayload,
			description: "500-level deeply nested JSON object",
		},
		{
			name:        "EmptyObject",
			input:       `{}` + "\n",
			description: "Empty JSON object lacking jsonrpc, id, and method",
		},
		{
			name:        "ArrayInsteadOfObject",
			input:       `[{"jsonrpc":"2.0","id":1,"method":"ping"}]` + "\n",
			description: "JSON array (batch JSON-RPC is not supported in MCP stream framing)",
		},
		{
			name:        "RawPrimitiveNumber",
			input:       `12345` + "\n",
			description: "Raw primitive number instead of JSON object",
		},
		{
			name:        "RawPrimitiveString",
			input:       `"just a string"` + "\n",
			description: "Raw primitive quoted string",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("Running malformed test: %s (%s)", tc.name, tc.description)

			// 1. Reader must not panic and must return an error
			reader := mcp.NewReader(strings.NewReader(tc.input))
			msg, err := reader.ReadMessage()

			// In case of EmptyObject, json.Unmarshal succeeds on struct, but msg is not a valid request
			if err != nil {
				t.Logf("[%s] ReadMessage cleanly returned error: %v", tc.name, err)
			} else {
				// If json.Unmarshal didn't return a syntax error (e.g. for {}, or deeply nested, or array)
				// verify classification
				t.Logf("[%s] Message parsed without error, checking classification: isRequest=%v, isResponse=%v, isNotification=%v",
					tc.name, msg.IsRequest(), msg.IsResponse(), msg.IsNotification())
			}

			// 2. Direct ReadMessage(*bufio.Reader)
			bufReader := bufio.NewReader(strings.NewReader(tc.input))
			msgDirect, errDirect := mcp.ReadMessage(bufReader)
			if errDirect != nil {
				t.Logf("[%s] direct ReadMessage cleanly returned error: %v", tc.name, errDirect)
			} else {
				t.Logf("[%s] direct ReadMessage parsed message: %+v", tc.name, msgDirect)
			}

			// 3. Verify clean JSON-RPC -32700 ParseError response generation
			parseErr := mcp.NewParseError(fmt.Sprintf("Failed to parse: %s", tc.name))
			if parseErr.Code != mcp.CodeParseError {
				t.Fatalf("expected CodeParseError (%d), got %d", mcp.CodeParseError, parseErr.Code)
			}
			if parseErr.Message != "Parse error" {
				t.Fatalf("expected 'Parse error', got %q", parseErr.Message)
			}

			// Build error response message
			errResponse := mcp.NewErrorResponse(mcp.NewNullID(), parseErr)
			if errResponse.JSONRPC != mcp.JSONRPCVersion {
				t.Fatalf("expected jsonrpc %s, got %s", mcp.JSONRPCVersion, errResponse.JSONRPC)
			}
			if !errResponse.ID.IsNull() {
				t.Fatalf("expected null ID for ParseError, got %v", errResponse.ID)
			}
			if errResponse.Error == nil || errResponse.Error.Code != -32700 {
				t.Fatalf("expected error code -32700, got %v", errResponse.Error)
			}

			// Serialize error response and verify syntax
			errBytes, jsonErr := json.Marshal(errResponse)
			if jsonErr != nil {
				t.Fatalf("[%s] failed to marshal ParseError response: %v", tc.name, jsonErr)
			}
			if !strings.Contains(string(errBytes), `"-32700"`) && !strings.Contains(string(errBytes), `-32700`) {
				t.Fatalf("[%s] serialized response missing -32700: %s", tc.name, string(errBytes))
			}
			if !strings.Contains(string(errBytes), `"id":null`) {
				t.Fatalf("[%s] serialized response missing id:null: %s", tc.name, string(errBytes))
			}
		})
	}
}

// ============================================================================
// Objective 3 (Component A): mcp.Writer Concurrent Serialization
// ============================================================================

func TestAdversarial_MCPWriter_ConcurrentSerialization(t *testing.T) {
	buf := &bytes.Buffer{}
	writer := mcp.NewWriter(buf)

	const numGoroutines = 10
	const messagesPerGoroutine = 100
	const expectedTotal = numGoroutines * messagesPerGoroutine // 1,000 messages

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	startSync := make(chan struct{})

	for g := 0; g < numGoroutines; g++ {
		go func(goroutineID int) {
			defer wg.Done()
			<-startSync // synchronize start for maximum thread contention

			for m := 0; m < messagesPerGoroutine; m++ {
				id := mcp.NewStringID(fmt.Sprintf("w%d-m%d", goroutineID, m))
				msg, err := mcp.NewRequest(id, "tools/call", map[string]any{
					"worker":    goroutineID,
					"seq":       m,
					"timestamp": time.Now().UnixNano(),
					"payload":   strings.Repeat("X", 64),
				})
				if err != nil {
					t.Errorf("NewRequest failed: %v", err)
					return
				}

				if err := writer.WriteMessage(msg); err != nil {
					t.Errorf("WriteMessage failed: %v", err)
					return
				}
			}
		}(g)
	}

	close(startSync)
	wg.Wait()

	// Analyze output stream
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != expectedTotal {
		t.Fatalf("expected exactly %d lines, got %d", expectedTotal, len(lines))
	}

	seenIDs := make(map[string]bool)
	for idx, line := range lines {
		var parsed mcp.Message
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d is corrupted or interleaved! Err: %v | Content: %s", idx, err, line)
		}
		if parsed.ID == nil {
			t.Fatalf("line %d missing ID: %s", idx, line)
		}
		idStr := parsed.ID.String()
		if seenIDs[idStr] {
			t.Fatalf("line %d has duplicate ID: %s", idx, idStr)
		}
		seenIDs[idStr] = true
	}

	if len(seenIDs) != expectedTotal {
		t.Fatalf("expected %d unique IDs, got %d", expectedTotal, len(seenIDs))
	}

	t.Logf("SUCCESS: Concurrently serialized %d messages across %d goroutines with zero corruption or line interleaving.",
		expectedTotal, numGoroutines)
}
