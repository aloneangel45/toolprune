package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const (
	// JSONRPCVersion is the required version string for JSON-RPC 2.0.
	JSONRPCVersion = "2.0"

	// LatestProtocolVersion is the MCP specification version implemented by ToolPrune.
	LatestProtocolVersion = "2024-11-05"

	// Standard MCP method names
	MethodInitialize                    = "initialize"
	MethodNotificationsInitialized       = "notifications/initialized"
	MethodPing                          = "ping"
	MethodToolsList                     = "tools/list"
	MethodToolsCall                     = "tools/call"
	MethodNotificationsToolsListChanged = "notifications/tools/list_changed"
	MethodPromptsList                   = "prompts/list"
	MethodPromptsGet                    = "prompts/get"
	MethodResourcesList                 = "resources/list"
	MethodResourcesRead                 = "resources/read"
	MethodLoggingSetLevel               = "logging/setLevel"
)

// IDKind indicates the underlying type of a RequestID.
type IDKind int

const (
	IDKindInt IDKind = iota
	IDKindString
	IDKindNull
	IDKindFloat
)

// ErrInvalidRequestID indicates a JSON-RPC id field with a disallowed type (boolean, array, object).
var ErrInvalidRequestID = errors.New("mcp: invalid JSON-RPC request id type")

// RequestID represents a polymorphic JSON-RPC 2.0 request identifier.
// In JSON-RPC 2.0, IDs can be string or number, or null in error responses.
type RequestID struct {
	kind     IDKind
	intVal   int64
	floatVal float64
	strVal   string
}

// NewIntID creates a RequestID from an int64.
func NewIntID(val int64) RequestID {
	return RequestID{kind: IDKindInt, intVal: val}
}

// NewFloatID creates a RequestID from a fractional float64.
func NewFloatID(val float64) RequestID {
	if !math.IsNaN(val) && !math.IsInf(val, 0) && math.Trunc(val) == val && val >= -9007199254740992 && val <= 9007199254740992 {
		return NewIntID(int64(val))
	}
	return RequestID{
		kind:     IDKindFloat,
		floatVal: val,
		strVal:   strconv.FormatFloat(val, 'f', -1, 64),
	}
}

// NewStringID creates a RequestID from a string.
func NewStringID(val string) RequestID {
	return RequestID{kind: IDKindString, strVal: val}
}

// NewNullID creates a RequestID representing null (used in error responses).
func NewNullID() RequestID {
	return RequestID{kind: IDKindNull}
}

// NewRequestIDFromAny converts an arbitrary value into a typed RequestID.
func NewRequestIDFromAny(v any) RequestID {
	if v == nil {
		return NewNullID()
	}
	switch val := v.(type) {
	case RequestID:
		return val
	case *RequestID:
		if val == nil {
			return NewNullID()
		}
		return *val
	case bool:
		return NewNullID()
	case int:
		return NewIntID(int64(val))
	case int8:
		return NewIntID(int64(val))
	case int16:
		return NewIntID(int64(val))
	case int32:
		return NewIntID(int64(val))
	case int64:
		return NewIntID(val)
	case uint:
		return NewIntID(int64(val))
	case uint8:
		return NewIntID(int64(val))
	case uint16:
		return NewIntID(int64(val))
	case uint32:
		return NewIntID(int64(val))
	case uint64:
		return NewIntID(int64(val))
	case float32:
		return NewFloatID(float64(val))
	case float64:
		return NewFloatID(val)
	case json.Number:
		if n, err := val.Int64(); err == nil {
			return NewIntID(n)
		}
		if f, err := val.Float64(); err == nil {
			return RequestID{kind: IDKindFloat, floatVal: f, strVal: val.String()}
		}
		return NewNullID()
	case string:
		return NewStringID(val)
	default:
		return NewNullID()
	}
}

// Kind returns the underlying IDKind.
func (id RequestID) Kind() IDKind {
	return id.kind
}

// IsInt returns true if the ID is an integer.
func (id RequestID) IsInt() bool {
	return id.kind == IDKindInt
}

// IsString returns true if the ID is a string.
func (id RequestID) IsString() bool {
	return id.kind == IDKindString
}

// IsNull returns true if the ID represents JSON null.
func (id RequestID) IsNull() bool {
	return id.kind == IDKindNull
}

// IsFloat returns true if the ID is a fractional number.
func (id RequestID) IsFloat() bool {
	return id.kind == IDKindFloat
}

// Float64 returns the numeric value and true if the ID is numeric (int or float).
func (id RequestID) Float64() (float64, bool) {
	switch id.kind {
	case IDKindInt:
		return float64(id.intVal), true
	case IDKindFloat:
		return id.floatVal, true
	default:
		return 0, false
	}
}

// Int64 returns the integer value and true if the ID is an integer.
func (id RequestID) Int64() (int64, bool) {
	if id.kind == IDKindInt {
		return id.intVal, true
	}
	return 0, false
}

// String returns a string representation of the ID.
func (id RequestID) String() string {
	switch id.kind {
	case IDKindInt:
		return strconv.FormatInt(id.intVal, 10)
	case IDKindFloat:
		if id.strVal != "" {
			return id.strVal
		}
		return strconv.FormatFloat(id.floatVal, 'f', -1, 64)
	case IDKindString:
		return id.strVal
	default:
		return "null"
	}
}

// Raw returns the underlying primitive value (int64, float64, string, or nil).
func (id RequestID) Raw() any {
	switch id.kind {
	case IDKindInt:
		return id.intVal
	case IDKindFloat:
		return id.floatVal
	case IDKindString:
		return id.strVal
	default:
		return nil
	}
}

// Equal returns true if two RequestIDs represent identical values and types.
func (id RequestID) Equal(other RequestID) bool {
	if id.kind != other.kind {
		return false
	}
	switch id.kind {
	case IDKindInt:
		return id.intVal == other.intVal
	case IDKindFloat:
		return id.floatVal == other.floatVal
	case IDKindString:
		return id.strVal == other.strVal
	case IDKindNull:
		return true
	default:
		return false
	}
}

// MarshalJSON encodes the RequestID to valid JSON.
func (id RequestID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case IDKindInt:
		return strconv.AppendInt(nil, id.intVal, 10), nil
	case IDKindFloat:
		if id.strVal != "" && json.Valid([]byte(id.strVal)) {
			return []byte(id.strVal), nil
		}
		return strconv.AppendFloat(nil, id.floatVal, 'f', -1, 64), nil
	case IDKindString:
		return json.Marshal(id.strVal)
	case IDKindNull:
		return []byte("null"), nil
	default:
		return []byte("null"), nil
	}
}

// UnmarshalJSON parses a JSON byte slice into a RequestID.
// Booleans, arrays, and objects are strictly rejected per JSON-RPC 2.0.
func (id *RequestID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return ErrInvalidRequestID
	}
	if bytes.Equal(trimmed, []byte("null")) {
		id.kind = IDKindNull
		id.intVal = 0
		id.floatVal = 0
		id.strVal = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		id.kind = IDKindString
		id.strVal = s
		id.intVal = 0
		id.floatVal = 0
		return nil
	}
	if trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9') {
		var n int64
		if err := json.Unmarshal(trimmed, &n); err == nil {
			id.kind = IDKindInt
			id.intVal = n
			id.floatVal = 0
			id.strVal = ""
			return nil
		}
		var f float64
		if err := json.Unmarshal(trimmed, &f); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			if math.Trunc(f) == f && f >= -9007199254740992 && f <= 9007199254740992 {
				id.kind = IDKindInt
				id.intVal = int64(f)
				id.floatVal = 0
				id.strVal = ""
				return nil
			}
			id.kind = IDKindFloat
			id.floatVal = f
			id.intVal = 0
			id.strVal = string(trimmed)
			return nil
		}
	}

	return fmt.Errorf("%w: %s", ErrInvalidRequestID, string(trimmed))
}

// Message is the primary JSON-RPC 2.0 message envelope for all MCP interactions.
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *RequestID      `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// UnmarshalJSON decodes a JSON-RPC 2.0 envelope while preserving the distinction
// between an omitted "id" field (notification) and an explicit `"id": null`.
func (m *Message) UnmarshalJSON(data []byte) error {
	type rawEnvelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *JSONRPCError   `json:"error"`
	}
	var raw rawEnvelope
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.JSONRPC = raw.JSONRPC
	m.Method = raw.Method
	m.Params = raw.Params
	m.Result = raw.Result
	m.Error = raw.Error

	trimmedID := bytes.TrimSpace(raw.ID)
	if len(trimmedID) == 0 {
		m.ID = nil
		return nil
	}
	var parsedID RequestID
	if err := parsedID.UnmarshalJSON(trimmedID); err != nil {
		return err
	}
	m.ID = &parsedID
	return nil
}

// IsRequest returns true if the message is a JSON-RPC request expecting a response.
func (m *Message) IsRequest() bool {
	return m.ID != nil && !m.ID.IsNull() && m.Method != ""
}

// IsNotification returns true if the message is a fire-and-forget notification (omits "id").
func (m *Message) IsNotification() bool {
	return m.ID == nil && m.Method != ""
}

// HasExplicitNullID returns true when an explicit `"id": null` field was present in JSON.
func (m *Message) HasExplicitNullID() bool {
	return m.ID != nil && m.ID.IsNull()
}

// ValidateEnvelope checks JSON-RPC 2.0 and MCP message envelope invariants.
func (m *Message) ValidateEnvelope() error {
	if m == nil {
		return ErrInvalidRequest
	}
	if m.JSONRPC != JSONRPCVersion {
		return fmt.Errorf("%w: unsupported jsonrpc version %q", ErrInvalidRequest, m.JSONRPC)
	}
	if m.Method != "" && m.HasExplicitNullID() {
		return fmt.Errorf("%w: MCP request id must not be null", ErrInvalidRequest)
	}
	if !m.IsRequest() && !m.IsNotification() && !m.IsResponse() {
		return fmt.Errorf("%w: message is neither request, notification, nor response", ErrInvalidRequest)
	}
	return nil
}

// IsResponse returns true if the message is a response to a prior request.
func (m *Message) IsResponse() bool {
	return m.ID != nil && m.Method == "" && (m.Result != nil || m.Error != nil)
}

// IsSuccess returns true if the message is a successful response.
func (m *Message) IsSuccess() bool {
	return m.IsResponse() && m.Error == nil
}

// IsError returns true if the message contains a protocol-level error.
func (m *Message) IsError() bool {
	return m.Error != nil
}

// NewRequest creates a typed JSON-RPC request Message.
func NewRequest(id RequestID, method string, params any) (*Message, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		rawParams = b
	}
	return &Message{
		JSONRPC: JSONRPCVersion,
		ID:      &id,
		Method:  method,
		Params:  rawParams,
	}, nil
}

// NewNotification creates a typed JSON-RPC notification Message.
func NewNotification(method string, params any) (*Message, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal notification params: %w", err)
		}
		rawParams = b
	}
	return &Message{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  rawParams,
	}, nil
}

// NewResponse creates a typed JSON-RPC success response Message.
func NewResponse(id RequestID, result any) (*Message, error) {
	var rawResult json.RawMessage
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal result: %w", err)
		}
		rawResult = b
	} else {
		rawResult = json.RawMessage("{}")
	}
	return &Message{
		JSONRPC: JSONRPCVersion,
		ID:      &id,
		Result:  rawResult,
	}, nil
}

// Implementation describes an MCP client or server application.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities declares features supported by an MCP client.
type ClientCapabilities struct {
	Experimental map[string]any      `json:"experimental,omitempty"`
	Sampling     *SamplingCapability `json:"sampling,omitempty"`
	Roots        *RootsCapability    `json:"roots,omitempty"`
}

// SamplingCapability indicates support for model sampling from the server.
type SamplingCapability struct{}

// RootsCapability indicates support for listing filesystem/workspace roots.
type RootsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// InitializeRequestParams represents the parameters for the 'initialize' method.
type InitializeRequestParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// ServerCapabilities declares features supported by an MCP server.
type ServerCapabilities struct {
	Experimental map[string]any       `json:"experimental,omitempty"`
	Logging      *LoggingCapability   `json:"logging,omitempty"`
	Prompts      *PromptsCapability   `json:"prompts,omitempty"`
	Resources    *ResourcesCapability `json:"resources,omitempty"`
	Tools        *ToolsCapability     `json:"tools,omitempty"`
}

// ToolsCapability indicates support for tools and tool change notifications.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ResourcesCapability indicates support for resources.
type ResourcesCapability struct {
	Subscribe   bool `json:"subscribe,omitempty"`
	ListChanged bool `json:"listChanged,omitempty"`
}

// PromptsCapability indicates support for prompts.
type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// LoggingCapability indicates support for logging.
type LoggingCapability struct{}

// InitializeResult represents the result returned by the server on 'initialize'.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// Tool describes an invokable tool exposed via MCP.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  InputSchema      `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Icons        []ToolIcon       `json:"icons,omitempty"`
}

// ToolAnnotations holds MCP behavioral hints for a tool.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ToolIcon describes an icon asset associated with an MCP tool.
type ToolIcon struct {
	Src      string   `json:"src"`
	MIMEType string   `json:"mimeType,omitempty"`
	Sizes    []string `json:"sizes,omitempty"`
	Theme    string   `json:"theme,omitempty"`
}

// InputSchema defines the JSON Schema for tool parameters, preserving full Draft-07 and 2020-12 keywords.
type InputSchema struct {
	Type                 string              `json:"type"` // MUST be "object"
	Properties           map[string]any      `json:"properties,omitempty"`
	Required             []string            `json:"required,omitempty"`
	AdditionalProperties any                 `json:"additionalProperties,omitempty"`
	Schema               string              `json:"$schema,omitempty"`
	ID                   string              `json:"$id,omitempty"`
	Title                string              `json:"title,omitempty"`
	Description          string              `json:"description,omitempty"`
	Comment              string              `json:"$comment,omitempty"`
	Examples             []any               `json:"examples,omitempty"`
	Defs                 map[string]any      `json:"$defs,omitempty"`
	Definitions          map[string]any      `json:"definitions,omitempty"`
	AnyOf                []any               `json:"anyOf,omitempty"`
	OneOf                []any               `json:"oneOf,omitempty"`
	AllOf                []any               `json:"allOf,omitempty"`
	Not                  map[string]any      `json:"not,omitempty"`
	PatternProperties    map[string]any      `json:"patternProperties,omitempty"`
	DependentSchemas     map[string]any      `json:"dependentSchemas,omitempty"`
	DependentRequired    map[string][]string `json:"dependentRequired,omitempty"`
	MinProperties        *int                `json:"minProperties,omitempty"`
	MaxProperties        *int                `json:"maxProperties,omitempty"`
}

// ListToolsRequestParams represents pagination arguments for 'tools/list'.
type ListToolsRequestParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult represents the result payload for 'tools/list'.
// Note: Tools slice does not have omitempty because empty tools list MUST serialize as [].
type ListToolsResult struct {
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolRequestParams represents parameters for 'tools/call'.
type CallToolRequestParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult represents the execution result of 'tools/call'.
type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ContentItem represents a multi-modal content fragment in tool results.
type ContentItem struct {
	Type     string            `json:"type"` // "text", "image", or "resource"
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`     // Base64 encoded data
	MIMEType string            `json:"mimeType,omitempty"` // For image or resource
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource holds an embedded resource object inside a ContentItem.
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}
