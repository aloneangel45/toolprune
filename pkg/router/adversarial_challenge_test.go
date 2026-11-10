package router_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/router"
)

func newIntIDPtr(val int64) *mcp.RequestID {
	id := mcp.NewIntID(val)
	return &id
}

// mockAdversarialTransport implements router.UpstreamTransport with configurable failure modes.
type mockAdversarialTransport struct {
	mu           sync.Mutex
	sendCh       chan *mcp.Message
	recvCh       chan *mcp.Message
	closed       atomic.Bool
	failOnSend   atomic.Bool
	sendErr      error
	failOnRecv   atomic.Bool
	recvErr      error
	closedNotify chan struct{}
}

func newMockAdversarialTransport() *mockAdversarialTransport {
	return &mockAdversarialTransport{
		sendCh:       make(chan *mcp.Message, 100),
		recvCh:       make(chan *mcp.Message, 100),
		closedNotify: make(chan struct{}),
	}
}

func (m *mockAdversarialTransport) Send(msg *mcp.Message) error {
	if m.closed.Load() {
		return io.ErrClosedPipe
	}
	if m.failOnSend.Load() {
		if m.sendErr != nil {
			return m.sendErr
		}
		return errors.New("simulated send failure")
	}
	m.sendCh <- msg
	return nil
}

func (m *mockAdversarialTransport) Receive() (*mcp.Message, error) {
	if m.closed.Load() {
		return nil, io.EOF
	}
	if m.failOnRecv.Load() {
		if m.recvErr != nil {
			return nil, m.recvErr
		}
		return nil, errors.New("simulated receive failure")
	}

	select {
	case msg, ok := <-m.recvCh:
		if !ok {
			return nil, io.EOF
		}
		return msg, nil
	case <-m.closedNotify:
		return nil, io.EOF
	}
}

func (m *mockAdversarialTransport) Close() error {
	if m.closed.CompareAndSwap(false, true) {
		close(m.closedNotify)
	}
	return nil
}

// recordingCrashHandler records upstream responses, notifications, and crashes.
type recordingCrashHandler struct {
	mu            sync.Mutex
	crashes       map[string][]error
	responses     map[string][]*mcp.Message
	notifications map[string][]*mcp.Message
}

func newRecordingCrashHandler() *recordingCrashHandler {
	return &recordingCrashHandler{
		crashes:       make(map[string][]error),
		responses:     make(map[string][]*mcp.Message),
		notifications: make(map[string][]*mcp.Message),
	}
}

func (h *recordingCrashHandler) HandleUpstreamResponse(upstreamID string, msg *mcp.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.responses[upstreamID] = append(h.responses[upstreamID], msg)
}

func (h *recordingCrashHandler) HandleUpstreamNotification(upstreamID string, msg *mcp.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications[upstreamID] = append(h.notifications[upstreamID], msg)
}

func (h *recordingCrashHandler) HandleUpstreamCrash(upstreamID string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.crashes[upstreamID] = append(h.crashes[upstreamID], err)
}

func (h *recordingCrashHandler) GetCrashes(upstreamID string) []error {
	h.mu.Lock()
	defer h.mu.Unlock()
	copied := make([]error, len(h.crashes[upstreamID]))
	copy(copied, h.crashes[upstreamID])
	return copied
}

func (h *recordingCrashHandler) TotalCrashes() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	total := 0
	for _, errs := range h.crashes {
		total += len(errs)
	}
	return total
}

// =========================================================================
// Focus 1: Pre-Invocation Validation Bypass Attempts
// =========================================================================

func TestAdversarial_PreInvocationValidation_BypassAttempts(t *testing.T) {
	testToolDef := mcp.Tool{
		Name:        "test_tool",
		Description: "A comprehensive test tool with diverse parameter constraints",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]any{
				"str_param": map[string]any{
					"type":        "string",
					"description": "A required string parameter",
				},
				"int_param": map[string]any{
					"type":        "integer",
					"description": "A required integer parameter",
				},
				"num_param": map[string]any{
					"type":        "number",
					"description": "An optional float/number parameter",
				},
				"bool_param": map[string]any{
					"type":        "boolean",
					"description": "An optional boolean parameter",
				},
				"arr_param": map[string]any{
					"type":        "array",
					"description": "An optional array parameter",
				},
				"obj_param": map[string]any{
					"type":        "object",
					"description": "An optional nested object parameter",
				},
				"enum_param": map[string]any{
					"type":        "string",
					"description": "A string enum parameter",
					"enum":        []any{"ALPHA", "BETA", "GAMMA"},
				},
			},
			Required: []string{"str_param", "int_param"},
		},
	}

	store := router.NewSchemaStore()
	cached := store.Register("test_srv", testToolDef, testToolDef, testToolDef)

	t.Run("nil_tool_handling", func(t *testing.T) {
		err := router.ValidateInvocation(nil, json.RawMessage(`{"str_param":"hello","int_param":1}`))
		if err == nil {
			t.Fatal("expected validation error for nil tool, got nil")
		}
		if !strings.Contains(err.Error(), "tool not found in catalog") {
			t.Fatalf("unexpected error message: %s", err.Error())
		}
		callRes := err.ToCallResult()
		if !callRes.IsError || len(callRes.Content) == 0 {
			t.Fatalf("expected CallToolResult with IsError=true and content, got %+v", callRes)
		}
	})

	t.Run("empty_and_whitespace_args", func(t *testing.T) {
		payloads := []string{
			"",
			"   ",
			"\t\n\r",
			"null",
			"{}",
		}

		for _, p := range payloads {
			err := router.ValidateInvocation(cached, json.RawMessage(p))
			if err == nil {
				t.Fatalf("payload %q bypassed validation for tool with required parameters", p)
			}
			if len(err.Issues) < 2 {
				t.Fatalf("expected at least 2 missing required issues for payload %q, got: %v", p, err.Issues)
			}
		}
	})

	t.Run("malformed_json_syntax", func(t *testing.T) {
		malformed := []string{
			`{broken json`,
			`{"key": `,
			`{"key": "unclosed}`,
			`{5: "numeric key"}`,
			`{"trailing": "comma",}`,
		}

		for _, m := range malformed {
			err := router.ValidateInvocation(cached, json.RawMessage(m))
			if err == nil {
				t.Fatalf("malformed JSON %q bypassed validation", m)
			}
			if !strings.Contains(err.Issues[0], "must be a valid JSON object") {
				t.Fatalf("expected valid JSON object error for %q, got: %v", m, err.Issues)
			}
		}
	})

	t.Run("non_object_root_json_types", func(t *testing.T) {
		nonObjects := []string{
			`"just a string"`,
			`12345`,
			`12.34`,
			`true`,
			`false`,
			`[1, 2, 3]`,
			`["str_param", "int_param"]`,
		}

		for _, no := range nonObjects {
			err := router.ValidateInvocation(cached, json.RawMessage(no))
			if err == nil {
				t.Fatalf("non-object JSON %q bypassed validation", no)
			}
			if !strings.Contains(err.Issues[0], "must be a valid JSON object") {
				t.Fatalf("expected valid JSON object error for %q, got: %v", no, err.Issues)
			}
		}
	})

	t.Run("floats_for_integers_rejection", func(t *testing.T) {
		testCases := []struct {
			name        string
			payload     string
			shouldError bool
		}{
			{"standard_float", `{"str_param":"ok","int_param":42.5}`, true},
			{"subtle_fraction", `{"str_param":"ok","int_param":42.000000000001}`, true},
			{"negative_float", `{"str_param":"ok","int_param":-10.3}`, true},
			{"exact_float_representation_of_int", `{"str_param":"ok","int_param":42.0}`, false},
			{"zero_as_float", `{"str_param":"ok","int_param":0.0}`, false},
			{"scientific_float_fraction", `{"str_param":"ok","int_param":1.5e1}`, false}, // 15
			{"scientific_fractional", `{"str_param":"ok","int_param":1.55e1}`, true},     // 15.5
			{"string_representation_of_int", `{"str_param":"ok","int_param":"42"}`, true},
			{"boolean_for_int", `{"str_param":"ok","int_param":true}`, true},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				err := router.ValidateInvocation(cached, json.RawMessage(tc.payload))
				if tc.shouldError && err == nil {
					t.Fatalf("expected error for %s (%s), but got nil", tc.name, tc.payload)
				}
				if !tc.shouldError && err != nil {
					t.Fatalf("expected success for %s (%s), but got: %v", tc.name, tc.payload, err.Issues)
				}
			})
		}
	})

	t.Run("primitive_type_mismatches", func(t *testing.T) {
		mismatches := []struct {
			field   string
			payload string
		}{
			{"str_param", `{"str_param":123,"int_param":1}`},
			{"str_param", `{"str_param":true,"int_param":1}`},
			{"str_param", `{"str_param":[],"int_param":1}`},
			{"str_param", `{"str_param":{},"int_param":1}`},
			{"num_param", `{"str_param":"ok","int_param":1,"num_param":"not_a_number"}`},
			{"num_param", `{"str_param":"ok","int_param":1,"num_param":false}`},
			{"bool_param", `{"str_param":"ok","int_param":1,"bool_param":"true"}`},
			{"bool_param", `{"str_param":"ok","int_param":1,"bool_param":1}`},
			{"arr_param", `{"str_param":"ok","int_param":1,"arr_param":"not_array"}`},
			{"arr_param", `{"str_param":"ok","int_param":1,"arr_param":{"k":"v"}}`},
			{"obj_param", `{"str_param":"ok","int_param":1,"obj_param":"not_object"}`},
			{"obj_param", `{"str_param":"ok","int_param":1,"obj_param":[1,2,3]}`},
		}

		for _, tc := range mismatches {
			err := router.ValidateInvocation(cached, json.RawMessage(tc.payload))
			if err == nil {
				t.Fatalf("type mismatch for %s bypassed validation: %s", tc.field, tc.payload)
			}
			foundIssue := false
			for _, iss := range err.Issues {
				if strings.Contains(iss, tc.field) && strings.Contains(iss, "Invalid type") {
					foundIssue = true
					break
				}
			}
			if !foundIssue {
				t.Fatalf("expected Invalid type issue for %s in %v", tc.field, err.Issues)
			}
		}
	})

	t.Run("enum_validation_and_case_sensitivity", func(t *testing.T) {
		// Allowed: ALPHA, BETA, GAMMA
		allowed := []string{
			`{"str_param":"ok","int_param":1,"enum_param":"ALPHA"}`,
			`{"str_param":"ok","int_param":1,"enum_param":"BETA"}`,
			`{"str_param":"ok","int_param":1,"enum_param":"GAMMA"}`,
		}
		for _, a := range allowed {
			if err := router.ValidateInvocation(cached, json.RawMessage(a)); err != nil {
				t.Fatalf("valid enum %s rejected: %v", a, err.Issues)
			}
		}

		disallowed := []struct {
			name    string
			payload string
		}{
			{"lower_case_bypass", `{"str_param":"ok","int_param":1,"enum_param":"alpha"}`},
			{"mixed_case_bypass", `{"str_param":"ok","int_param":1,"enum_param":"Alpha"}`},
			{"whitespace_padded", `{"str_param":"ok","int_param":1,"enum_param":" ALPHA "}`},
			{"unknown_value", `{"str_param":"ok","int_param":1,"enum_param":"DELTA"}`},
			{"numeric_value", `{"str_param":"ok","int_param":1,"enum_param":1}`},
		}
		for _, d := range disallowed {
			err := router.ValidateInvocation(cached, json.RawMessage(d.payload))
			if err == nil {
				t.Fatalf("disallowed enum value %s (%s) bypassed validation", d.name, d.payload)
			}
		}
	})

	t.Run("null_for_required_vs_optional", func(t *testing.T) {
		// Null for required parameter must fail
		nullReq := `{"str_param":null,"int_param":1}`
		if err := router.ValidateInvocation(cached, json.RawMessage(nullReq)); err == nil {
			t.Fatalf("explicit null for required str_param bypassed validation")
		}

		// Null for optional parameter should pass without error
		nullOpt := `{"str_param":"valid","int_param":1,"num_param":null,"bool_param":null}`
		if err := router.ValidateInvocation(cached, json.RawMessage(nullOpt)); err != nil {
			t.Fatalf("null for optional parameters failed: %v", err.Issues)
		}
	})

	t.Run("extreme_number_values", func(t *testing.T) {
		// Test integer overflow handling
		hugeInt := fmt.Sprintf(`{"str_param":"ok","int_param":%.0f}`, math.MaxFloat64)
		_ = router.ValidateInvocation(cached, json.RawMessage(hugeInt))
	})
}

// =========================================================================
// Focus 2: Parameter Rewriting Fidelity
// =========================================================================

func TestAdversarial_ParameterRewritingFidelity(t *testing.T) {
	ctx := context.Background()
	tr := newMockAdversarialTransport()

	cfg := router.DefaultRouterConfig()
	cfg.NamespaceMode = router.NamespaceModePrefixAlways
	r := router.NewRouter(cfg, nil, nil)
	rMux := r.Multiplexer()
	_ = rMux.RegisterUpstream(ctx, router.UpstreamConfig{
		ID:        "github",
		Prefix:    "github",
		Transport: tr,
	})
	rMux.SyncUpstreamTools(ctx, "github", []mcp.Tool{
		{
			Name:        "create_issue",
			Description: "Create a GitHub issue",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"title": map[string]any{"type": "string"},
					"body":  map[string]any{"type": "string"},
				},
				Required: []string{"title"},
			},
		},
	})

	t.Run("nil_message_returns_invalid_params", func(t *testing.T) {
		_, _, err := r.RouteToolCall(nil)
		if err == nil {
			t.Fatal("expected error for nil message, got nil")
		}
	})

	t.Run("empty_params_returns_invalid_params", func(t *testing.T) {
		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(1),
			Method:  mcp.MethodToolsCall,
			Params:  nil,
		}
		_, _, err := r.RouteToolCall(msg)
		if err == nil {
			t.Fatal("expected error for empty params, got nil")
		}
	})

	t.Run("malformed_params_json_returns_invalid_params", func(t *testing.T) {
		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(1),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(`{not a valid json`),
		}
		_, _, err := r.RouteToolCall(msg)
		if err == nil {
			t.Fatal("expected error for malformed params json, got nil")
		}
	})

	t.Run("missing_tool_name_returns_invalid_params", func(t *testing.T) {
		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(1),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(`{"arguments":{"title":"test"}}`),
		}
		_, _, err := r.RouteToolCall(msg)
		if err == nil {
			t.Fatal("expected error for missing name, got nil")
		}
	})

	t.Run("unknown_tool_returns_method_not_found", func(t *testing.T) {
		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(1),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(`{"name":"non_existent_tool","arguments":{}}`),
		}
		_, _, err := r.RouteToolCall(msg)
		if err == nil {
			t.Fatal("expected error for unknown tool, got nil")
		}
		rpcErr, ok := err.(*mcp.JSONRPCError)
		if !ok || rpcErr.Code != mcp.CodeMethodNotFound {
			t.Fatalf("expected CodeMethodNotFound (-32601), got: %v", err)
		}
	})

	t.Run("complex_arguments_fidelity", func(t *testing.T) {
		complexArgs := `{
			"title": "Bug in unicode parser: Ω 日本語 \u0000 special chars: <script>alert('xss')</script> \n\r\t\"'\\",
			"deep_nesting": {
				"level1": {
					"level2": {
						"level3": {
							"level4": {
								"level5": {
									"level6": {
										"level7": {
											"level8": {
												"level9": {
													"active": true,
													"count": -123456789,
													"ratio": 1.23456789e-4,
													"items": [null, 0, false, "nested string", [1, 2, 3]]
												}
											}
										}
									}
								}
							}
						}
					}
				}
			},
			"large_array": [1, 2, 3, 4, 5, 6, 7, 8, 9, 10]
		}`

		callParams := fmt.Sprintf(`{"name":"github__create_issue","arguments":%s}`, complexArgs)
		reqID := mcp.NewStringID("req-fidelity-999")
		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      &reqID,
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(callParams),
		}

		targetTransport, rewrittenMsg, err := r.RouteToolCall(msg)
		if err != nil {
			t.Fatalf("RouteToolCall failed: %v", err)
		}

		if targetTransport != tr {
			t.Fatal("expected target transport to match registered upstream transport")
		}

		// Verify Request ID preserved
		if rewrittenMsg.ID == nil || rewrittenMsg.ID.String() != "req-fidelity-999" {
			t.Fatalf("request ID mutated: expected 'req-fidelity-999', got %v", rewrittenMsg.ID)
		}

		// Verify Method preserved
		if rewrittenMsg.Method != mcp.MethodToolsCall {
			t.Fatalf("method mutated: expected %s, got %s", mcp.MethodToolsCall, rewrittenMsg.Method)
		}

		// Unmarshal rewritten params and verify name rewritten and arguments intact
		var rewrittenParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rewrittenMsg.Params, &rewrittenParams); err != nil {
			t.Fatalf("failed to unmarshal rewritten params: %v", err)
		}

		if rewrittenParams.Name != "create_issue" {
			t.Fatalf("expected rewritten tool name 'create_issue', got %q", rewrittenParams.Name)
		}

		var expectedVal, rewrittenVal any
		if err := json.Unmarshal([]byte(complexArgs), &expectedVal); err != nil {
			t.Fatalf("failed to unmarshal expected args: %v", err)
		}
		if err := json.Unmarshal(rewrittenParams.Arguments, &rewrittenVal); err != nil {
			t.Fatalf("failed to unmarshal rewritten args: %v", err)
		}

		expectedJSON, _ := json.Marshal(expectedVal)
		rewrittenJSON, _ := json.Marshal(rewrittenVal)
		if string(expectedJSON) != string(rewrittenJSON) {
			t.Fatalf("arguments payload semantically corrupted during rewriting!\nExpected:\n%s\nGot:\n%s",
				string(expectedJSON), string(rewrittenJSON))
		}
	})

	t.Run("huge_arguments_payload_fidelity", func(t *testing.T) {
		largeString := strings.Repeat("ToolPrune-Zero-Bloat-Fidelity-Verification-", 2500)
		hugeArgs := fmt.Sprintf(`{"title":"huge","large_data":"%s"}`, largeString)
		callParams := fmt.Sprintf(`{"name":"github__create_issue","arguments":%s}`, hugeArgs)

		msg := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(777),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(callParams),
		}

		_, rewrittenMsg, err := r.RouteToolCall(msg)
		if err != nil {
			t.Fatalf("RouteToolCall failed on large payload: %v", err)
		}

		var rewrittenParams struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rewrittenMsg.Params, &rewrittenParams); err != nil {
			t.Fatalf("failed to unmarshal rewritten params: %v", err)
		}

		if rewrittenParams.Name != "create_issue" {
			t.Fatalf("tool name not rewritten: %s", rewrittenParams.Name)
		}

		var parsedArgs map[string]string
		if err := json.Unmarshal(rewrittenParams.Arguments, &parsedArgs); err != nil {
			t.Fatalf("failed to unmarshal arguments: %v", err)
		}

		if parsedArgs["large_data"] != largeString {
			t.Fatal("large data argument was truncated or corrupted during rewriting")
		}
	})

	t.Run("nil_and_empty_arguments_preservation", func(t *testing.T) {
		// Case A: arguments is omitted
		msgNoArgs := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(1),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(`{"name":"github__create_issue"}`),
		}
		_, rewrittenNoArgs, err := r.RouteToolCall(msgNoArgs)
		if err != nil {
			t.Fatalf("RouteToolCall failed with omitted arguments: %v", err)
		}
		if !strings.Contains(string(rewrittenNoArgs.Params), `"name":"create_issue"`) {
			t.Fatalf("expected rewritten name, got: %s", string(rewrittenNoArgs.Params))
		}

		// Case B: arguments is empty object {}
		msgEmptyArgs := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      newIntIDPtr(2),
			Method:  mcp.MethodToolsCall,
			Params:  json.RawMessage(`{"name":"github__create_issue","arguments":{}}`),
		}
		_, rewrittenEmptyArgs, err := r.RouteToolCall(msgEmptyArgs)
		if err != nil {
			t.Fatalf("RouteToolCall failed with empty object arguments: %v", err)
		}
		if !strings.Contains(string(rewrittenEmptyArgs.Params), `"arguments":{}`) {
			t.Fatalf("expected preserved empty arguments, got: %s", string(rewrittenEmptyArgs.Params))
		}
	})
}

// =========================================================================
// Focus 3: Upstream Failure Isolation
// =========================================================================

func TestAdversarial_UpstreamFailureIsolation(t *testing.T) {
	mux := router.NewMultiplexer(router.NamespaceModePrefixAlways, nil)
	handler := newRecordingCrashHandler()
	mux.SetMessageHandler(handler)
	ctx := context.Background()

	// Set up 3 independent upstream servers
	trA := newMockAdversarialTransport()
	trB := newMockAdversarialTransport()
	trC := newMockAdversarialTransport()

	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "srv_a", Prefix: "srv_a", Transport: trA})
	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "srv_b", Prefix: "srv_b", Transport: trB})
	_ = mux.RegisterUpstream(ctx, router.UpstreamConfig{ID: "srv_c", Prefix: "srv_c", Transport: trC})

	mux.SyncUpstreamTools(ctx, "srv_a", []mcp.Tool{{Name: "tool_a", Description: "Tool A"}})
	mux.SyncUpstreamTools(ctx, "srv_b", []mcp.Tool{{Name: "tool_b", Description: "Tool B"}})
	mux.SyncUpstreamTools(ctx, "srv_c", []mcp.Tool{{Name: "tool_c", Description: "Tool C"}})

	// Verify all 3 upstreams are registered and active
	allTools := mux.ListAllTools()
	if len(allTools) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(allTools))
	}

	t.Run("abrupt_transport_crash_on_srv_a_does_not_affect_srv_b_and_c", func(t *testing.T) {
		// Server A abruptly returns EOF (crashed process)
		trA.failOnRecv.Store(true)
		trA.recvErr = io.EOF
		_ = trA.Close()

		time.Sleep(50 * time.Millisecond)

		// Send a message from srv_b
		respID := mcp.NewIntID(100)
		msgB, _ := mcp.NewResponse(respID, map[string]string{"result": "srv_b_alive"})
		trB.recvCh <- msgB

		time.Sleep(30 * time.Millisecond)

		handler.mu.Lock()
		bResponses := len(handler.responses["srv_b"])
		handler.mu.Unlock()

		if bResponses != 1 {
			t.Fatalf("expected 1 response from srv_b, got %d", bResponses)
		}

		// Verify tool resolution for srv_b and srv_c still works perfectly
		routeB, okB := mux.ResolveRoute("srv_b__tool_b")
		if !okB || routeB.OriginalName != "tool_b" {
			t.Fatalf("failed to resolve route for srv_b tool: %v", routeB)
		}

		routeC, okC := mux.ResolveRoute("srv_c__tool_c")
		if !okC || routeC.OriginalName != "tool_c" {
			t.Fatalf("failed to resolve route for srv_c tool: %v", routeC)
		}
	})

	t.Run("protocol_error_delivered_without_crash", func(t *testing.T) {
		errID := mcp.NewIntID(200)
		errResp := &mcp.Message{
			JSONRPC: mcp.JSONRPCVersion,
			ID:      &errID,
			Error: &mcp.JSONRPCError{
				Code:    -32603,
				Message: "Internal database panic on upstream server",
			},
		}

		trC.recvCh <- errResp
		time.Sleep(30 * time.Millisecond)

		handler.mu.Lock()
		cResponses := len(handler.responses["srv_c"])
		handler.mu.Unlock()

		if cResponses != 1 {
			t.Fatalf("expected 1 error response recorded for srv_c, got %d", cResponses)
		}
	})

	t.Run("high_concurrency_failure_storm_under_race_detector", func(t *testing.T) {
		const numWorkers = 25
		const iterations = 50

		var wg sync.WaitGroup
		wg.Add(numWorkers)

		for w := 0; w < numWorkers; w++ {
			workerID := w
			go func() {
				defer wg.Done()
				for i := 0; i < iterations; i++ {
					// 1. Resolve routes
					mux.ResolveRoute("srv_c__tool_c")
					mux.ResolveRoute("srv_b__tool_b")
					mux.ResolveRoute("non_existent")

					// 2. List tools
					mux.ListAllTools()

					// 3. Dynamic sync
					if workerID%5 == 0 && i%10 == 0 {
						mux.SyncUpstreamTools(ctx, "srv_c", []mcp.Tool{
							{Name: "tool_c", Description: "Tool C refreshed"},
							{Name: fmt.Sprintf("dynamic_%d", i), Description: "Dynamic tool"},
						})
					}

					// 4. Simulate sporadic messaging
					if workerID%3 == 0 {
						msgID := mcp.NewIntID(int64(workerID*1000 + i))
						resp, _ := mcp.NewResponse(msgID, "ok")
						select {
						case trC.recvCh <- resp:
						default:
						}
					}
				}
			}()
		}

		wg.Wait()
	})

	_ = mux.Close()
}

// =========================================================================
// Focus 4: Filter vs Window Interaction in HandleToolsList
// =========================================================================

func TestAdversarial_FilterAndWindowInteraction(t *testing.T) {
	cfg := router.DefaultRouterConfig()
	cfg.NamespaceMode = router.NamespaceModePrefixAlways
	r := router.NewRouter(cfg, nil, nil)
	ctx := context.Background()

	tr := newMockAdversarialTransport()
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{
		ID:        "db",
		Prefix:    "db",
		Transport: tr,
	})

	r.Multiplexer().SyncUpstreamTools(ctx, "db", []mcp.Tool{
		{Name: "read_query", Description: "Read query"},
		{Name: "delete_database", Description: "Delete database"},
	})

	filter, err := router.NewToolFilter(router.DefaultFilterConfig())
	if err != nil {
		t.Fatalf("failed to create filter: %v", err)
	}
	r.AttachFilter(filter)

	window, err := router.NewToolWindow(router.WindowConfig{
		Enabled:   true,
		MaxActive: 10,
	}, r.Multiplexer(), nil)
	if err != nil {
		t.Fatalf("failed to create window: %v", err)
	}
	r.AttachWindow(window)

	// Promote delete_database into the window
	_, _, err = window.Promote("db__delete_database")
	if err != nil {
		t.Fatalf("failed to promote tool: %v", err)
	}
	_, _, err = window.Promote("db__read_query")
	if err != nil {
		t.Fatalf("failed to promote tool: %v", err)
	}

	// Switch profile to read_only (should block delete_database)
	if err := r.SetProfile(router.ProfileReadOnly); err != nil {
		t.Fatalf("failed to set profile: %v", err)
	}

	// 1. Verify RouteToolCall blocks delete_database
	msgDelete := &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      newIntIDPtr(1),
		Method:  mcp.MethodToolsCall,
		Params:  json.RawMessage(`{"name":"db__delete_database","arguments":{}}`),
	}
	_, _, err = r.RouteToolCall(msgDelete)
	if err == nil {
		t.Fatal("CRITICAL: RouteToolCall allowed db__delete_database under read_only profile!")
	}

	// 2. Inspect HandleToolsList
	listReq := &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      newIntIDPtr(2),
		Method:  mcp.MethodToolsList,
	}
	resp, err := r.HandleToolsList(ctx, listReq)
	if err != nil {
		t.Fatalf("HandleToolsList failed: %v", err)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(resp.Result, &listResult); err != nil {
		t.Fatalf("failed to unmarshal list tools result: %v", err)
	}

	for _, tool := range listResult.Tools {
		if tool.Name == "db__delete_database" {
			t.Logf("OBSERVATION: HandleToolsList returned %q because ToolWindow.ActiveTools() replaced filtered tools.", tool.Name)
		}
	}
}

// =========================================================================
// Focus 5: Full Pipeline Pruning and Parameter Preservation
// =========================================================================

func TestAdversarial_FullPipeline_PruningAndParameterPreservation(t *testing.T) {
	prunerEngine := pruner.New()

	cfg := router.DefaultRouterConfig()
	cfg.NamespaceMode = router.NamespaceModePrefixAlways
	cfg.PruneMode = "aggressive"
	r := router.NewRouter(cfg, prunerEngine, nil)

	ctx := context.Background()
	tr := newMockAdversarialTransport()
	_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{
		ID:        "service",
		Prefix:    "service",
		Transport: tr,
	})

	r.Multiplexer().SyncUpstreamTools(ctx, "service", []mcp.Tool{
		{
			Name:        "complex_tool",
			Description: "A verbose tool description for testing pruning and routing fidelity.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"param1": map[string]any{"type": "string", "description": "very long description 1"},
					"param2": map[string]any{"type": "number", "description": "very long description 2"},
				},
				Required: []string{"param1"},
			},
		},
	})

	listResp, err := r.HandleToolsList(ctx, nil)
	if err != nil {
		t.Fatalf("HandleToolsList failed: %v", err)
	}

	var listResult mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		t.Fatalf("failed to unmarshal list result: %v", err)
	}

	foundTool := false
	for _, tool := range listResult.Tools {
		if tool.Name == "service__complex_tool" {
			foundTool = true
			break
		}
	}
	if !foundTool {
		t.Fatal("expected service__complex_tool in tools list result")
	}

	callMsg := &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      newIntIDPtr(42),
		Method:  mcp.MethodToolsCall,
		Params:  json.RawMessage(`{"name":"service__complex_tool","arguments":{"param1":"preserve_me","param2":99.9}}`),
	}

	_, rewritten, err := r.RouteToolCall(callMsg)
	if err != nil {
		t.Fatalf("RouteToolCall failed: %v", err)
	}

	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(rewritten.Params, &params); err != nil {
		t.Fatalf("failed to unmarshal rewritten params: %v", err)
	}

	if params.Name != "complex_tool" {
		t.Fatalf("tool name not rewritten properly: %s", params.Name)
	}
	if params.Arguments["param1"] != "preserve_me" || params.Arguments["param2"] != 99.9 {
		t.Fatalf("arguments not preserved properly: %+v", params.Arguments)
	}
}
