package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

const (
	// DefaultMaxMessageSize sets the maximum allowed size of a single JSON-RPC line (10MB).
	DefaultMaxMessageSize = 10 * 1024 * 1024

	// DefaultReadBufferSize sets the initial read buffer size (64KB).
	DefaultReadBufferSize = 64 * 1024
)

var (
	// ErrTransportClosed is returned when operations are attempted on a closed transport.
	ErrTransportClosed = errors.New("stdio transport is closed")

	// ErrMessageTooLarge is returned when an incoming line exceeds DefaultMaxMessageSize.
	ErrMessageTooLarge = errors.New("JSON-RPC message exceeds maximum buffer size (10MB)")
)

// ErrorToJSONRPCResponse maps recoverable JSON-RPC framing/parsing/envelope errors to a
// protocol-compliant JSON-RPC 2.0 error response (with explicit "id": null).
// Returns nil when err is an unrecoverable stream termination error (io.EOF, ErrTransportClosed, I/O failure).
func ErrorToJSONRPCResponse(err error) *mcp.Message {
	if err == nil {
		return nil
	}
	if errors.Is(err, mcp.ErrParse) {
		return mcp.NewErrorMessage(nil, mcp.CodeParseError, "Parse error")
	}
	if errors.Is(err, mcp.ErrInvalidRequest) {
		return mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Invalid Request")
	}
	if errors.Is(err, ErrMessageTooLarge) {
		return mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Request exceeds maximum message size")
	}
	return nil
}

// StdioTransport manages thread-safe, buffered newline-delimited JSON-RPC 2.0 communication
// over standard input/output streams.
type StdioTransport struct {
	reader         *bufio.Reader
	writer         io.Writer
	writeMu        sync.Mutex
	logger         *slog.Logger
	maxMessageSize int
	closed         atomic.Bool
	closeCh        chan struct{}
}

// StdioOptions configures StdioTransport.
type StdioOptions struct {
	In             io.Reader
	Out            io.Writer
	Logger         *slog.Logger
	MaxMessageSize int
}

// NewStdioTransport creates a new StdioTransport. If In or Out are nil, os.Stdin and os.Stdout
// are used by default.
func NewStdioTransport(opts StdioOptions) *StdioTransport {
	in := opts.In
	if in == nil {
		in = os.Stdin
	}
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	maxSize := opts.MaxMessageSize
	if maxSize <= 0 {
		maxSize = DefaultMaxMessageSize
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	return &StdioTransport{
		reader:         bufio.NewReaderSize(in, DefaultReadBufferSize),
		writer:         out,
		logger:         logger,
		maxMessageSize: maxSize,
		closeCh:        make(chan struct{}),
	}
}

// ReadMessage reads a single newline-delimited JSON-RPC 2.0 message from the input stream.
// It enforces maxMessageSize prior to heap allocation and classifies parse vs invalid-request errors
// so stdio loops can emit protocol-compliant JSON-RPC error responses without crashing.
func (t *StdioTransport) ReadMessage() (*mcp.Message, error) {
	if t.closed.Load() {
		return nil, ErrTransportClosed
	}

	for {
		line, err := mcp.ReadBoundedLine(t.reader, t.maxMessageSize)
		if errors.Is(err, mcp.ErrMessageTooLarge) {
			return nil, ErrMessageTooLarge
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				return nil, io.EOF
			}
			if len(line) == 0 {
				return nil, err
			}
		}

		// Normalize line endings and strip whitespace
		trimmed := bytes.TrimSpace(line)
		// Ignore empty lines / blank heartbeats
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}

		// Batch arrays [...] or bare JSON primitives are valid JSON syntax (-32600 Invalid Request)
		// rather than syntax parse errors (-32700 Parse error).
		if trimmed[0] != '{' {
			var probe any
			if synErr := json.Unmarshal(trimmed, &probe); synErr != nil {
				t.logger.Warn("malformed JSON-RPC message received",
					"error", synErr,
					"raw_snippet", truncateSnippet(trimmed, 128),
				)
				return nil, fmt.Errorf("%w: %v", mcp.ErrParse, synErr)
			}
			return nil, fmt.Errorf("%w: JSON-RPC batch arrays and non-object payloads are not supported", mcp.ErrInvalidRequest)
		}

		var msg mcp.Message
		if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
			t.logger.Warn("malformed JSON-RPC message received",
				"error", unmarshalErr,
				"raw_snippet", truncateSnippet(trimmed, 128),
			)
			if errors.Is(unmarshalErr, mcp.ErrInvalidRequestID) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrInvalidRequest, unmarshalErr)
			}
			var syntaxErr *json.SyntaxError
			if errors.As(unmarshalErr, &syntaxErr) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrParse, unmarshalErr)
			}
			var typeErr *json.UnmarshalTypeError
			if errors.As(unmarshalErr, &typeErr) {
				return nil, fmt.Errorf("%w: %v", mcp.ErrInvalidRequest, unmarshalErr)
			}
			return nil, fmt.Errorf("%w: %v", mcp.ErrParse, unmarshalErr)
		}

		if envErr := msg.ValidateEnvelope(); envErr != nil {
			return nil, envErr
		}

		return &msg, nil
	}
}

// WriteMessage serializes a JSON-RPC 2.0 message to JSON and writes it to stdout with a trailing newline.
// It is fully thread-safe and guarantees atomic line output.
func (t *StdioTransport) WriteMessage(msg *mcp.Message) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC message: %w", err)
	}

	return t.WriteRaw(data)
}

// WriteRaw writes arbitrary raw JSON bytes followed by a newline atomically under the write lock.
func (t *StdioTransport) WriteRaw(data []byte) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}

	// Prepare buffer with trailing newline to execute a single atomic Write call
	payload := make([]byte, len(data)+1)
	copy(payload, data)
	payload[len(data)] = '\n'

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	_, err := t.writer.Write(payload)
	return err
}

// Close closes the transport and unblocks any waiting operations.
func (t *StdioTransport) Close() error {
	if t.closed.CompareAndSwap(false, true) {
		close(t.closeCh)
	}
	return nil
}

// Done returns a channel that is closed when the transport is shut down.
func (t *StdioTransport) Done() <-chan struct{} {
	return t.closeCh
}

func truncateSnippet(b []byte, maxLen int) string {
	if len(b) <= maxLen {
		return string(b)
	}
	return string(b[:maxLen]) + "...[truncated]"
}
