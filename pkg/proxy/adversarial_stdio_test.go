package proxy_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// ============================================================================
// Objective 1: Oversized Message Boundary on StdioTransport
// ============================================================================

func TestAdversarial_Stdio_OversizedBoundaries(t *testing.T) {
	sizes := []struct {
		name      string
		targetLen int
	}{
		{"100KB", 100 * 1024},
		{"500KB", 500 * 1024},
		{"1MB", 1 * 1024 * 1024},
		{"5MB", 5 * 1024 * 1024},
		{"9.5MB", int(9.5 * 1024 * 1024)},
	}

	for _, tc := range sizes {
		t.Run(tc.name, func(t *testing.T) {
			prefix := `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"data":"`
			suffix := `"}}` + "\n"
			padLen := tc.targetLen - len(prefix) - len(suffix)

			var payloadBuf bytes.Buffer
			payloadBuf.Grow(tc.targetLen + 64)
			payloadBuf.WriteString(prefix)
			payloadBuf.WriteString(strings.Repeat("S", padLen))
			payloadBuf.WriteString(suffix)

			rawBytes := payloadBuf.Bytes()

			discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
			transport := proxy.NewStdioTransport(proxy.StdioOptions{
				In:     bytes.NewReader(rawBytes),
				Out:    io.Discard,
				Logger: discardLog,
			})

			start := time.Now()
			msg, err := transport.ReadMessage()
			elapsed := time.Since(start)

			if err != nil {
				t.Fatalf("[%s] StdioTransport.ReadMessage failed on %d bytes: %v", tc.name, len(rawBytes), err)
			}
			if msg == nil {
				t.Fatalf("[%s] expected non-nil message", tc.name)
			}
			if msg.Method != "tools/call" {
				t.Fatalf("[%s] expected method 'tools/call', got %q", tc.name, msg.Method)
			}

			var params struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil {
				t.Fatalf("[%s] failed to unmarshal params: %v", tc.name, err)
			}
			if len(params.Data) != padLen {
				t.Fatalf("[%s] payload truncated! expected %d bytes, got %d bytes", tc.name, padLen, len(params.Data))
			}

			t.Logf("[%s] StdioTransport SUCCESS: size=%d bytes, duration=%v", tc.name, len(rawBytes), elapsed)
		})
	}
}

func TestAdversarial_Stdio_Exceeding10MB(t *testing.T) {
	overflowSizes := []struct {
		name      string
		targetLen int
	}{
		{"10MB+100B", proxy.DefaultMaxMessageSize + 100},
		{"10.5MB", int(10.5 * 1024 * 1024)},
		{"11MB", 11 * 1024 * 1024},
	}

	for _, tc := range overflowSizes {
		t.Run(tc.name, func(t *testing.T) {
			prefix := `{"jsonrpc":"2.0","id":99,"method":"oversized","params":{"data":"`
			suffix := `"}}` + "\n"
			padLen := tc.targetLen - len(prefix) - len(suffix)

			var payloadBuf bytes.Buffer
			payloadBuf.Grow(tc.targetLen + 64)
			payloadBuf.WriteString(prefix)
			payloadBuf.WriteString(strings.Repeat("X", padLen))
			payloadBuf.WriteString(suffix)

			rawBytes := payloadBuf.Bytes()

			discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
			transport := proxy.NewStdioTransport(proxy.StdioOptions{
				In:     bytes.NewReader(rawBytes),
				Out:    io.Discard,
				Logger: discardLog,
			})

			msg, err := transport.ReadMessage()
			if msg != nil {
				t.Fatalf("[%s] expected nil message for oversized payload, got %v", tc.name, msg)
			}
			if err == nil {
				t.Fatalf("[%s] expected ErrMessageTooLarge, got nil error", tc.name)
			}
			if !errors.Is(err, proxy.ErrMessageTooLarge) {
				t.Fatalf("[%s] expected ErrMessageTooLarge, got: %v", tc.name, err)
			}

			t.Logf("[%s] StdioTransport correctly rejected oversized payload (%d bytes): %v",
				tc.name, len(rawBytes), err)
		})
	}
}

// ============================================================================
// Objective 2: Malformed JSON Robustness on StdioTransport
// ============================================================================

func TestAdversarial_Stdio_MalformedJSON(t *testing.T) {
	testCases := []struct {
		name        string
		input       string
		description string
		expectError bool
	}{
		{
			name:        "InvalidSyntax",
			input:       `{"jsonrpc":"2.0", incomplete_key}` + "\n",
			description: "Malformed syntax without values",
			expectError: true,
		},
		{
			name:        "UnclosedBrace",
			input:       `{"jsonrpc":"2.0","id":1` + "\n",
			description: "Unclosed opening brace",
			expectError: true,
		},
		{
			name:        "NullByteInjection",
			input:       "{\"jsonrpc\":\"2.0\",\x00\"id\":2}\n",
			description: "Embedded null byte",
			expectError: true,
		},
		{
			name:        "RawNonUTF8Bytes",
			input:       "{\"jsonrpc\":\"2.0\",\xff\xfe\"id\":3}\n",
			description: "Raw non-UTF-8 bytes in JSON structure outside quotes",
			expectError: true,
		},
		{
			name:        "NonUTF8InStringLiteral",
			input:       "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"\x80\x81\x82\"}\n",
			description: "Non-UTF-8 bytes inside string literal (Go json.Unmarshal replaces with \\uFFFD)",
			expectError: false,
		},
		{
			name:        "TrailingDataOnLine",
			input:       `{"jsonrpc":"2.0","id":4,"method":"ping"} trailing_garbage` + "\n",
			description: "Trailing data after valid JSON",
			expectError: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
			transport := proxy.NewStdioTransport(proxy.StdioOptions{
				In:     strings.NewReader(tc.input),
				Out:    io.Discard,
				Logger: discardLog,
			})

			msg, err := transport.ReadMessage()
			if tc.expectError {
				if err == nil {
					t.Fatalf("[%s] expected error on malformed input, but got nil (msg: %v)", tc.name, msg)
				}
				t.Logf("[%s] StdioTransport safely rejected malformed input without panic: %v", tc.name, err)
			} else {
				if err != nil {
					t.Logf("[%s] StdioTransport returned error: %v", tc.name, err)
				} else {
					t.Logf("[%s] StdioTransport parsed payload with sanitized UTF-8 without panic: %v", tc.name, msg)
				}
			}
		})
	}
}

// ============================================================================
// Objective 3: Concurrent Stdio Write Serialization (1,000 Messages across 10 Goroutines)
// ============================================================================

func TestAdversarial_Stdio_ConcurrentWriteSerialization_1000Messages(t *testing.T) {
	outBuf := &bytes.Buffer{}
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	transport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:     strings.NewReader(""),
		Out:    outBuf,
		Logger: discardLog,
	})

	const numGoroutines = 10
	const messagesPerGoroutine = 100
	const expectedTotal = numGoroutines * messagesPerGoroutine // Exactly 1,000 messages

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	startBarrier := make(chan struct{})

	for g := 0; g < numGoroutines; g++ {
		go func(gID int) {
			defer wg.Done()
			<-startBarrier // Contention barrier to trigger maximum write race conditions

			for m := 0; m < messagesPerGoroutine; m++ {
				reqID := mcp.NewStringID(fmt.Sprintf("worker-%02d-msg-%04d", gID, m))
				msg, err := mcp.NewRequest(reqID, "tools/call", map[string]any{
					"workerID":  gID,
					"seq":       m,
					"timestamp": time.Now().UnixNano(),
					"payload":   fmt.Sprintf("stress-payload-content-from-worker-%d-iteration-%d", gID, m),
				})
				if err != nil {
					t.Errorf("NewRequest error: %v", err)
					return
				}

				if err := transport.WriteMessage(msg); err != nil {
					t.Errorf("WriteMessage error: %v", err)
					return
				}
			}
		}(g)
	}

	// Release all 10 goroutines simultaneously
	close(startBarrier)
	wg.Wait()

	// Analyze output stream for line atomicity and serialization
	rawOutput := outBuf.String()
	lines := strings.Split(strings.TrimSpace(rawOutput), "\n")

	if len(lines) != expectedTotal {
		t.Fatalf("FAILED: Expected exactly %d output lines, got %d", expectedTotal, len(lines))
	}

	seenIDs := make(map[string]bool, expectedTotal)
	for idx, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("Line %d is empty", idx)
		}

		var parsed mcp.Message
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("CORRUPTION DETECTED at line %d: %v\nRaw Line: %s", idx, err, line)
		}

		if parsed.JSONRPC != "2.0" {
			t.Fatalf("Line %d invalid jsonrpc version: %s", idx, parsed.JSONRPC)
		}
		if parsed.ID == nil {
			t.Fatalf("Line %d missing ID: %s", idx, line)
		}

		idStr := parsed.ID.String()
		if seenIDs[idStr] {
			t.Fatalf("DUPLICATE MESSAGE DETECTED: ID %s appears multiple times", idStr)
		}
		seenIDs[idStr] = true
	}

	if len(seenIDs) != expectedTotal {
		t.Fatalf("FAILED: Expected %d distinct message IDs, got %d", expectedTotal, len(seenIDs))
	}

	t.Logf("SUCCESS: Concurrently serialized %d messages across %d goroutines with zero corruption, zero interleaving, and 100%% integrity.",
		expectedTotal, numGoroutines)
}

// High-load stress test with variable payload sizes
func TestAdversarial_Stdio_VariableSizeConcurrentStress(t *testing.T) {
	outBuf := &bytes.Buffer{}
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	transport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:     strings.NewReader(""),
		Out:    outBuf,
		Logger: discardLog,
	})

	const numGoroutines = 10
	const messagesPerGoroutine = 500
	const expectedTotal = numGoroutines * messagesPerGoroutine // 5,000 messages with variable payload

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	startBarrier := make(chan struct{})

	for g := 0; g < numGoroutines; g++ {
		go func(gID int) {
			defer wg.Done()
			<-startBarrier

			for m := 0; m < messagesPerGoroutine; m++ {
				// Vary payload size between 16B, 256B, 4KB, 32KB
				var padLen int
				switch m % 4 {
				case 0:
					padLen = 16
				case 1:
					padLen = 256
				case 2:
					padLen = 4096
				case 3:
					padLen = 32768
				}

				reqID := mcp.NewStringID(fmt.Sprintf("vworker-%02d-msg-%04d", gID, m))
				msg, _ := mcp.NewRequest(reqID, "tools/call", map[string]any{
					"worker": gID,
					"seq":    m,
					"data":   strings.Repeat("V", padLen),
				})

				if err := transport.WriteMessage(msg); err != nil {
					t.Errorf("WriteMessage failed: %v", err)
					return
				}
			}
		}(g)
	}

	close(startBarrier)
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(outBuf.String()), "\n")
	if len(lines) != expectedTotal {
		t.Fatalf("expected %d lines, got %d", expectedTotal, len(lines))
	}

	seenIDs := make(map[string]bool, expectedTotal)
	for idx, line := range lines {
		var parsed mcp.Message
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("corruption in variable size stress at line %d: %v", idx, err)
		}
		idStr := parsed.ID.String()
		if seenIDs[idStr] {
			t.Fatalf("duplicate ID: %s", idStr)
		}
		seenIDs[idStr] = true
	}

	t.Logf("SUCCESS: Variable-size stress test passed: %d messages across %d goroutines with zero corruption.",
		expectedTotal, numGoroutines)
}
