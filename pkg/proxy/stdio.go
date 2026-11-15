package proxy

import (
	"bufio"
	"bytes"
	"context"
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
// over standard input/output streams, supporting both single messages and JSON-RPC 2.0 batch arrays.
type StdioTransport struct {
	reader         *bufio.Reader
	writer         io.Writer
	writeMu        sync.Mutex
	batchWg        sync.WaitGroup
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

// ReadPayload reads the next newline-delimited JSON-RPC 2.0 payload (either a single object
// or a non-empty JSON-RPC 2.0 batch array [...]) from the input stream.
func (t *StdioTransport) ReadPayload() (*ParsedPayload, error) {
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

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}

		parsed, parseErr := ParseJSONRPCPayload(trimmed)
		if parseErr != nil {
			t.logger.Warn("malformed JSON-RPC message received",
				"error", parseErr,
				"raw_snippet", truncateSnippet(trimmed, 128),
			)
			return nil, parseErr
		}

		return parsed, nil
	}
}

// ReadMessage reads a single newline-delimited JSON-RPC 2.0 message from the input stream.
func (t *StdioTransport) ReadMessage() (*mcp.Message, error) {
	parsed, err := t.ReadPayload()
	if err != nil {
		return nil, err
	}
	if parsed.IsBatch {
		return nil, fmt.Errorf("%w: unexpected batch array in single-message ReadMessage; use ReadPayload or HandleNext", mcp.ErrInvalidRequest)
	}
	return parsed.Single, nil
}

// HandleNext reads one JSON-RPC 2.0 line (single object or batch array [...]), emits protocol-compliant
// error responses on recoverable parse/invalid-request errors, and dispatches valid messages or batches.
// Returns io.EOF or ErrTransportClosed cleanly when the stream terminates.
func (t *StdioTransport) HandleNext(ctx context.Context, session ClientSession, dispatcher MessageDispatcher) error {
	payload, err := t.ReadPayload()
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, ErrTransportClosed) {
			t.batchWg.Wait()
			return err
		}
		if rpcErrResp := ErrorToJSONRPCResponse(err); rpcErrResp != nil {
			_ = t.WriteMessage(rpcErrResp)
			return nil
		}
		t.batchWg.Wait()
		return err
	}

	if payload.IsBatch {
		elems := payload.Batch
		t.batchWg.Add(1)
		go func() {
			defer t.batchWg.Done()
			resps := DispatchBatch(ctx, session, dispatcher, elems)
			if len(resps) > 0 {
				_ = t.WriteBatch(resps)
			}
		}()
		return nil
	}

	if hErr := dispatcher.HandleClientMessage(session, payload.Single); hErr != nil {
		t.logger.Warn("error handling stdio client message", "err", hErr)
	}
	return nil
}

// WaitPendingBatches blocks until all in-flight batch responses have been written.
func (t *StdioTransport) WaitPendingBatches() {
	t.batchWg.Wait()
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

// WriteBatch serializes a non-empty slice of JSON-RPC 2.0 responses as a single JSON array line.
func (t *StdioTransport) WriteBatch(msgs []*mcp.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if t.closed.Load() {
		return ErrTransportClosed
	}

	data, err := json.Marshal(msgs)
	if err != nil {
		return fmt.Errorf("marshal JSON-RPC batch response: %w", err)
	}

	return t.WriteRaw(data)
}

// WriteRaw writes arbitrary raw JSON bytes followed by a newline atomically under the write lock.
func (t *StdioTransport) WriteRaw(data []byte) error {
	if t.closed.Load() {
		return ErrTransportClosed
	}

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
