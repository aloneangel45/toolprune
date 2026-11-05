package mcp

import (
	"fmt"
)

// Standard JSON-RPC 2.0 error codes
const (
	CodeParseError     = -32700 // Invalid JSON was received by the server.
	CodeInvalidRequest = -32600 // The JSON sent is not a valid Request object.
	CodeMethodNotFound = -32601 // The method does not exist / is not available.
	CodeInvalidParams  = -32602 // Invalid method parameter(s).
	CodeInternalError  = -32603 // Internal JSON-RPC error.

	// JSON-RPC 2.0 reserved server error range (-32000 to -32099)
	CodeServerErrorStart = -32000
	CodeServerErrorEnd   = -32099

	// Model Context Protocol custom error codes
	CodeUrlElicitationRequired = -32042 // Out-of-band user authorization / URL visit needed
)

// JSONRPCError represents a protocol-level JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the standard Go error interface.
func (e *JSONRPCError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("jsonrpc error: code=%d message=%s data=%v", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("jsonrpc error: code=%d message=%s", e.Code, e.Message)
}

// NewParseError creates a standard -32700 ParseError.
func NewParseError(data any) *JSONRPCError {
	return &JSONRPCError{
		Code:    CodeParseError,
		Message: "Parse error",
		Data:    data,
	}
}

// NewInvalidRequest creates a standard -32600 InvalidRequest error.
func NewInvalidRequest(message string) *JSONRPCError {
	if message == "" {
		message = "Invalid Request"
	}
	return &JSONRPCError{
		Code:    CodeInvalidRequest,
		Message: message,
	}
}

// NewMethodNotFound creates a standard -32601 MethodNotFound error.
func NewMethodNotFound(method string) *JSONRPCError {
	return &JSONRPCError{
		Code:    CodeMethodNotFound,
		Message: fmt.Sprintf("Method not found: %s", method),
	}
}

// NewInvalidParams creates a standard -32602 InvalidParams error.
func NewInvalidParams(message string) *JSONRPCError {
	if message == "" {
		message = "Invalid params"
	}
	return &JSONRPCError{
		Code:    CodeInvalidParams,
		Message: message,
	}
}

// NewInternalError creates a standard -32603 InternalError.
func NewInternalError(message string) *JSONRPCError {
	if message == "" {
		message = "Internal error"
	}
	return &JSONRPCError{
		Code:    CodeInternalError,
		Message: message,
	}
}

// NewUrlElicitationRequired creates a standard MCP -32042 error.
func NewUrlElicitationRequired(url string) *JSONRPCError {
	return &JSONRPCError{
		Code:    CodeUrlElicitationRequired,
		Message: "URL elicitation required",
		Data: map[string]string{
			"url": url,
		},
	}
}

// NewErrorResponse creates a full JSON-RPC error response Message from a typed RequestID.
func NewErrorResponse(id RequestID, err *JSONRPCError) *Message {
	return &Message{
		JSONRPC: JSONRPCVersion,
		ID:      &id,
		Error:   err,
	}
}

// NewErrorMessage creates a full JSON-RPC error response Message from an arbitrary ID.
func NewErrorMessage(id any, code int, message string) *Message {
	reqID := NewRequestIDFromAny(id)
	return &Message{
		JSONRPC: JSONRPCVersion,
		ID:      &reqID,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
		},
	}
}

// NewErrorMessageWithData creates a full JSON-RPC error response Message with extra data.
func NewErrorMessageWithData(id any, code int, message string, data any) *Message {
	reqID := NewRequestIDFromAny(id)
	return &Message{
		JSONRPC: JSONRPCVersion,
		ID:      &reqID,
		Error: &JSONRPCError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}
}

// ============================================================================
// Tool Execution Results & Error Boundary Helpers
// ============================================================================
// CRITICAL SPECIFICATION NOTE:
// Tool execution failures (such as database query errors, file not found, network
// timeouts) MUST NOT be returned as JSON-RPC error responses (CodeInternalError).
// Instead, they MUST be returned as successful JSON-RPC responses containing a
// CallToolResult with IsError = true and diagnostic details in Content.
// This allows the calling LLM to read the error and self-correct its query.
// ============================================================================

// NewToolSuccessResult creates a successful CallToolResult with text or content items.
func NewToolSuccessResult(items ...ContentItem) *CallToolResult {
	return &CallToolResult{
		Content: items,
		IsError: false,
	}
}

// NewToolTextResult creates a successful single-text CallToolResult.
func NewToolTextResult(text string) *CallToolResult {
	return &CallToolResult{
		Content: []ContentItem{
			{
				Type: "text",
				Text: text,
			},
		},
		IsError: false,
	}
}

// NewToolExecutionErrorResult creates a tool failure CallToolResult (IsError = true).
func NewToolExecutionErrorResult(errorText string) *CallToolResult {
	return &CallToolResult{
		Content: []ContentItem{
			{
				Type: "text",
				Text: errorText,
			},
		},
		IsError: true,
	}
}
