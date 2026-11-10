package mcp_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// 1. RequestID Polymorphism Tests
func TestRequestID_Int(t *testing.T) {
	id := mcp.NewIntID(42)
	if !id.IsInt() || id.IsString() || id.IsNull() {
		t.Fatalf("expected int ID, got kind=%v", id.Kind())
	}
	val, ok := id.Int64()
	if !ok || val != 42 {
		t.Fatalf("expected 42, got %d (ok=%v)", val, ok)
	}
	if id.String() != "42" {
		t.Fatalf("expected '42', got %s", id.String())
	}

	// Marshaling
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(b) != "42" {
		t.Fatalf("expected '42', got '%s'", string(b))
	}

	// Unmarshaling
	var unmarshaled mcp.RequestID
	if err := json.Unmarshal([]byte("42"), &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !id.Equal(unmarshaled) {
		t.Fatalf("expected equal IDs")
	}
}

func TestRequestID_String(t *testing.T) {
	id := mcp.NewStringID("req-99")
	if !id.IsString() || id.IsInt() || id.IsNull() {
		t.Fatalf("expected string ID, got kind=%v", id.Kind())
	}
	if id.String() != "req-99" {
		t.Fatalf("expected 'req-99', got %s", id.String())
	}

	// Marshaling
	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(b) != `"req-99"` {
		t.Fatalf("expected '\"req-99\"', got '%s'", string(b))
	}

	// Unmarshaling
	var unmarshaled mcp.RequestID
	if err := json.Unmarshal([]byte(`"req-99"`), &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !id.Equal(unmarshaled) {
		t.Fatalf("expected equal IDs")
	}
}

func TestRequestID_Null(t *testing.T) {
	id := mcp.NewNullID()
	if !id.IsNull() || id.IsInt() || id.IsString() {
		t.Fatalf("expected null ID, got kind=%v", id.Kind())
	}
	if id.String() != "null" {
		t.Fatalf("expected 'null', got %s", id.String())
	}

	b, err := json.Marshal(id)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if string(b) != "null" {
		t.Fatalf("expected 'null', got '%s'", string(b))
	}

	var unmarshaled mcp.RequestID
	if err := json.Unmarshal([]byte("null"), &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if !id.Equal(unmarshaled) {
		t.Fatalf("expected equal null IDs")
	}
}

func TestRequestID_FromAny(t *testing.T) {
	tests := []struct {
		input    any
		expected string
		isInt    bool
	}{
		{int(1), "1", true},
		{int64(100), "100", true},
		{float64(55), "55", true},
		{"req-custom", "req-custom", false},
		{nil, "null", false},
	}

	for _, tt := range tests {
		reqID := mcp.NewRequestIDFromAny(tt.input)
		if reqID.String() != tt.expected {
			t.Errorf("input %v: expected string %q, got %q", tt.input, tt.expected, reqID.String())
		}
		if tt.isInt && !reqID.IsInt() {
			t.Errorf("input %v: expected IsInt to be true", tt.input)
		}
	}
}

// 2. Message Envelope & Classification Tests
func TestMessage_Classification(t *testing.T) {
	intID := mcp.NewIntID(1)

	// Request
	req, _ := mcp.NewRequest(intID, "ping", map[string]string{"foo": "bar"})
	if !req.IsRequest() || req.IsNotification() || req.IsResponse() || req.IsError() {
		t.Fatalf("expected request classification")
	}

	// Notification
	notif, _ := mcp.NewNotification("notifications/initialized", nil)
	if !notif.IsNotification() || notif.IsRequest() || notif.IsResponse() {
		t.Fatalf("expected notification classification")
	}

	// Success Response
	resp, _ := mcp.NewResponse(intID, map[string]string{"pong": "true"})
	if !resp.IsResponse() || !resp.IsSuccess() || resp.IsRequest() || resp.IsError() {
		t.Fatalf("expected response success classification")
	}

	// Error Response
	errResp := mcp.NewErrorResponse(intID, mcp.NewMethodNotFound("foo"))
	if !errResp.IsResponse() || !errResp.IsError() || errResp.IsSuccess() {
		t.Fatalf("expected error response classification")
	}
}

func TestMessage_RoundtripJSON(t *testing.T) {
	rawJSON := `{"jsonrpc":"2.0","id":123,"method":"tools/call","params":{"name":"calculator","arguments":{"a":1,"b":2}}}`
	var msg mcp.Message
	if err := json.Unmarshal([]byte(rawJSON), &msg); err != nil {
		t.Fatalf("unmarshal message failed: %v", err)
	}

	if !msg.IsRequest() {
		t.Fatalf("expected request")
	}
	if msg.ID.String() != "123" {
		t.Fatalf("expected ID '123', got %s", msg.ID.String())
	}
	if msg.Method != "tools/call" {
		t.Fatalf("expected method 'tools/call', got %s", msg.Method)
	}

	remarshaled, err := json.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var msg2 mcp.Message
	if err := json.Unmarshal(remarshaled, &msg2); err != nil {
		t.Fatalf("re-unmarshal failed: %v", err)
	}
	if !msg.ID.Equal(*msg2.ID) || msg.Method != msg2.Method {
		t.Fatalf("message mismatch after roundtrip")
	}
}

// 3. MCP Lifecycle Handshake Tests
func TestLifecycle_Handshake(t *testing.T) {
	initParams := mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		Capabilities: mcp.ClientCapabilities{
			Roots: &mcp.RootsCapability{ListChanged: true},
		},
		ClientInfo: mcp.Implementation{
			Name:    "TestClient",
			Version: "1.0.0",
		},
	}

	initReq, err := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, initParams)
	if err != nil {
		t.Fatalf("failed to create initialize request: %v", err)
	}

	var parsedParams mcp.InitializeRequestParams
	if err := json.Unmarshal(initReq.Params, &parsedParams); err != nil {
		t.Fatalf("failed to parse init params: %v", err)
	}
	if parsedParams.ProtocolVersion != mcp.LatestProtocolVersion {
		t.Fatalf("expected protocol version %s, got %s", mcp.LatestProtocolVersion, parsedParams.ProtocolVersion)
	}

	initResult := mcp.InitializeResult{
		ProtocolVersion: mcp.LatestProtocolVersion,
		Capabilities: mcp.ServerCapabilities{
			Tools: &mcp.ToolsCapability{ListChanged: true},
		},
		ServerInfo: mcp.Implementation{
			Name:    "ToolPrune",
			Version: "0.1.0",
		},
		Instructions: "Pruned MCP Router",
	}

	resp, err := mcp.NewResponse(mcp.NewIntID(1), initResult)
	if err != nil {
		t.Fatalf("failed to create initialize response: %v", err)
	}

	var parsedResult mcp.InitializeResult
	if err := json.Unmarshal(resp.Result, &parsedResult); err != nil {
		t.Fatalf("failed to parse init result: %v", err)
	}
	if parsedResult.ServerInfo.Name != "ToolPrune" {
		t.Fatalf("expected server name 'ToolPrune', got %s", parsedResult.ServerInfo.Name)
	}
}

// 4. Tool Protocol Structs Tests
func TestToolProtocol_Types(t *testing.T) {
	tool := mcp.Tool{
		Name:        "search",
		Description: "Search internet",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "search query",
				},
			},
			Required: []string{"query"},
		},
	}

	listResult := mcp.ListToolsResult{
		Tools: []mcp.Tool{tool},
	}

	b, err := json.Marshal(listResult)
	if err != nil {
		t.Fatalf("marshal list tools result failed: %v", err)
	}

	var decoded mcp.ListToolsResult
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal list tools result failed: %v", err)
	}

	if len(decoded.Tools) != 1 || decoded.Tools[0].Name != "search" {
		t.Fatalf("expected 1 tool 'search', got %v", decoded.Tools)
	}

	// Verify empty tools serialize as [] and not null
	emptyResult := mcp.ListToolsResult{Tools: []mcp.Tool{}}
	emptyJSON, _ := json.Marshal(emptyResult)
	if !strings.Contains(string(emptyJSON), `"tools":[]`) {
		t.Fatalf("expected '\"tools\":[]', got %s", string(emptyJSON))
	}
}

// 5. Tool Error Boundary: Protocol Error vs Tool Execution Error
func TestToolExecutionError_Vs_ProtocolError(t *testing.T) {
	// Protocol Error: JSON-RPC error field
	protoErr := mcp.NewErrorResponse(mcp.NewIntID(1), mcp.NewMethodNotFound("unknown_tool"))
	if protoErr.Error == nil || protoErr.Error.Code != mcp.CodeMethodNotFound {
		t.Fatalf("expected protocol error code %d", mcp.CodeMethodNotFound)
	}

	// Tool Execution Error: IsError = true inside CallToolResult
	toolErrResult := mcp.NewToolExecutionErrorResult("database connection failed")
	if !toolErrResult.IsError {
		t.Fatalf("expected IsError = true")
	}
	if len(toolErrResult.Content) != 1 || toolErrResult.Content[0].Text != "database connection failed" {
		t.Fatalf("unexpected content: %v", toolErrResult.Content)
	}

	toolSuccessResult := mcp.NewToolTextResult("query result 42")
	if toolSuccessResult.IsError {
		t.Fatalf("expected IsError = false")
	}
	if toolSuccessResult.Content[0].Text != "query result 42" {
		t.Fatalf("expected text 'query result 42'")
	}
}

// 6. Framing: 10MB Buffer, Oversized Payloads, and Scanner Overflow Hazard
func TestFraming_10MB_OversizedMessage(t *testing.T) {
	// Generate an oversized 2MB JSON-RPC message (much larger than 64KB bufio.MaxScanTokenSize)
	largeData := strings.Repeat("X", 2*1024*1024)
	rawMsg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"data":"%s"}}`+"\n", largeData)

	reader := mcp.NewReader(strings.NewReader(rawMsg))
	msg, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("Reader.ReadMessage failed on 2MB message: %v", err)
	}

	if msg.Method != "tools/call" {
		t.Fatalf("expected method 'tools/call', got %s", msg.Method)
	}

	// Direct ReadMessage(*bufio.Reader) contract
	bufReader := bufio.NewReaderSize(strings.NewReader(rawMsg), 256*1024)
	msgDirect, err := mcp.ReadMessage(bufReader)
	if err != nil {
		t.Fatalf("ReadMessage direct failed: %v", err)
	}
	if msgDirect.Method != "tools/call" {
		t.Fatalf("expected method 'tools/call', got %s", msgDirect.Method)
	}
}

func TestFraming_DefaultScanner_FailsOnOversized(t *testing.T) {
	// Prove that standard bufio.Scanner without 10MB buffer crashes on >64KB payloads
	largeData := strings.Repeat("X", 100*1024) // 100KB
	rawMsg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"data":"%s"}}`+"\n", largeData)

	defaultScanner := bufio.NewScanner(strings.NewReader(rawMsg))
	scanResult := defaultScanner.Scan()
	if scanResult {
		t.Fatalf("expected default scanner to fail on 100KB line")
	}
	if !errors.Is(defaultScanner.Err(), bufio.ErrTooLong) {
		t.Fatalf("expected bufio.ErrTooLong, got: %v", defaultScanner.Err())
	}
}

func TestFraming_WhitespaceAndBlankLines(t *testing.T) {
	stream := "\r\n  \n\n" + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n\n  \r\n" + `{"jsonrpc":"2.0","id":2,"method":"pong"}` + "\n"
	reader := mcp.NewReader(strings.NewReader(stream))

	msg1, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("read msg1 failed: %v", err)
	}
	if msg1.Method != "ping" {
		t.Fatalf("expected ping, got %s", msg1.Method)
	}

	msg2, err := reader.ReadMessage()
	if err != nil {
		t.Fatalf("read msg2 failed: %v", err)
	}
	if msg2.Method != "pong" {
		t.Fatalf("expected pong, got %s", msg2.Method)
	}

	// Clean EOF
	msg3, err := reader.ReadMessage()
	if !errors.Is(err, io.EOF) || msg3 != nil {
		t.Fatalf("expected io.EOF, got err=%v, msg=%v", err, msg3)
	}
}

func TestFraming_Writer_Concurrent(t *testing.T) {
	buf := &bytes.Buffer{}
	writer := mcp.NewWriter(buf)

	const numGoroutines = 20
	const msgsPerGoroutine = 50
	var wg sync.WaitGroup

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for j := 0; j < msgsPerGoroutine; j++ {
				msg, _ := mcp.NewRequest(mcp.NewIntID(int64(gID*1000+j)), "ping", nil)
				if err := writer.WriteMessage(msg); err != nil {
					t.Errorf("WriteMessage failed: %v", err)
				}
			}
		}(i)
	}

	wg.Wait()

	// Validate output integrity
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	expectedCount := numGoroutines * msgsPerGoroutine
	if len(lines) != expectedCount {
		t.Fatalf("expected %d lines, got %d", expectedCount, len(lines))
	}

	for idx, line := range lines {
		var parsed mcp.Message
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("line %d is corrupted: %v (line: %s)", idx, err, line)
		}
	}
}

func TestRequestID_FractionalAndInvalidTypes(t *testing.T) {
	// Fractional JSON-RPC ID must preserve numeric representation
	var fracID mcp.RequestID
	if err := json.Unmarshal([]byte("12.5"), &fracID); err != nil {
		t.Fatalf("unexpected error on fractional ID: %v", err)
	}
	if !fracID.IsFloat() || fracID.String() != "12.5" {
		t.Fatalf("expected float ID 12.5, got kind=%v str=%q", fracID.Kind(), fracID.String())
	}
	marshaled, err := json.Marshal(fracID)
	if err != nil || string(marshaled) != "12.5" {
		t.Fatalf("expected marshaled 12.5, got %s (err=%v)", string(marshaled), err)
	}

	// Booleans, arrays, and objects must be rejected
	for _, invalid := range []string{"true", "false", "[1]", `{"a":1}`} {
		var badID mcp.RequestID
		if err := json.Unmarshal([]byte(invalid), &badID); err == nil {
			t.Fatalf("expected error unmarshaling invalid request ID %s, got %+v", invalid, badID)
		}
	}
}

func TestMessage_ExplicitNullIDNotNotification(t *testing.T) {
	var msg mcp.Message
	if err := json.Unmarshal([]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`), &msg); err != nil {
		t.Fatalf("unexpected unmarshal error: %v", err)
	}
	if msg.IsNotification() {
		t.Fatal("message with explicit id:null must NOT be classified as notification")
	}
	if !msg.HasExplicitNullID() {
		t.Fatal("expected HasExplicitNullID to be true")
	}
	if err := msg.ValidateEnvelope(); !errors.Is(err, mcp.ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest on request with id:null, got %v", err)
	}
}

func TestFraming_RecoverAfterOversizedLine(t *testing.T) {
	oversized := `{"jsonrpc":"2.0","id":1,"method":"big","params":{"x":"` + strings.Repeat("A", 1024) + `"}}` + "\n"
	validNext := `{"jsonrpc":"2.0","id":2,"method":"ping"}` + "\n"

	r := mcp.NewReaderWithSize(strings.NewReader(oversized+validNext), 256)
	_, err := r.ReadMessage()
	if !errors.Is(err, mcp.ErrMessageTooLarge) {
		t.Fatalf("expected ErrMessageTooLarge on first line, got %v", err)
	}

	nextMsg, err := r.ReadMessage()
	if err != nil {
		t.Fatalf("expected stream recovery on second line, got error: %v", err)
	}
	if nextMsg.Method != "ping" || nextMsg.ID.String() != "2" {
		t.Fatalf("unexpected recovered message: %+v", nextMsg)
	}
}

func TestModernMCPToolAndSchemaFieldsPreserved(t *testing.T) {
	rawToolJSON := `{
		"name": "pydantic_query",
		"title": "Pydantic Database Query",
		"description": "Execute typed SQL query",
		"annotations": {
			"title": "Read-Only SQL",
			"readOnlyHint": true,
			"destructiveHint": false,
			"idempotentHint": true,
			"openWorldHint": false
		},
		"icons": [{"src": "https://example.com/db.png", "mimeType": "image/png", "sizes": ["32x32"]}],
		"outputSchema": {
			"type": "object",
			"properties": {"rows": {"type": "array", "items": {"type": "object"}}}
		},
		"inputSchema": {
			"type": "object",
			"$defs": {
				"Filter": {
					"type": "object",
					"properties": {"col": {"type": "string"}, "val": {"type": "string"}},
					"required": ["col"]
				}
			},
			"properties": {
				"filter": {"$ref": "#/$defs/Filter"}
			},
			"anyOf": [{"required": ["filter"]}],
			"minProperties": 1,
			"maxProperties": 5
		}
	}`

	var tool mcp.Tool
	if err := json.Unmarshal([]byte(rawToolJSON), &tool); err != nil {
		t.Fatalf("unmarshal modern tool failed: %v", err)
	}
	if tool.Title != "Pydantic Database Query" {
		t.Fatalf("title lost: %q", tool.Title)
	}
	if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint != false {
		t.Fatalf("annotations destructiveHint=false lost: %+v", tool.Annotations)
	}
	if len(tool.Icons) != 1 || tool.Icons[0].Src != "https://example.com/db.png" {
		t.Fatalf("icons lost: %+v", tool.Icons)
	}
	if len(tool.OutputSchema) == 0 {
		t.Fatal("outputSchema lost")
	}
	if len(tool.InputSchema.Defs) == 0 {
		t.Fatal("root $defs lost")
	}
	if len(tool.InputSchema.AnyOf) != 1 {
		t.Fatal("root anyOf lost")
	}
	if tool.InputSchema.MinProperties == nil || *tool.InputSchema.MinProperties != 1 {
		t.Fatal("minProperties lost")
	}
}
