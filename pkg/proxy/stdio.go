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

	// DefaultReadBufferSize sets the initial read buffer size (256KB).
	DefaultReadBufferSize = 256 * 1024
)

var (
	// ErrTransportClosed is returned when operations are attempted on a closed transport.
	ErrTransportClosed = errors.New("stdio transport is closed")

	// ErrMessageTooLarge is returned when an incoming line exceeds DefaultMaxMessageSize.
	ErrMessageTooLarge = errors.New("JSON-RPC message exceeds maximum buffer size (10MB)")
)

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
// It handles CR/LF normalization and validates payload size.
func (t *StdioTransport) ReadMessage() (*mcp.Message, error) {
	if t.closed.Load() {
		return nil, ErrTransportClosed
	}

	for {
		line, err := t.readLine()
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

		var msg mcp.Message
		if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
			t.logger.Warn("malformed JSON-RPC message received",
				"error", unmarshalErr,
				"raw_snippet", truncateSnippet(trimmed, 128),
			)
			return nil, fmt.Errorf("malformed JSON-RPC: %w", unmarshalErr)
		}

		return &msg, nil
	}
}

// readLine reads bytes until '\n', enforcing maxMessageSize.
func (t *StdioTransport) readLine() ([]byte, error) {
	var fullLine []byte

	for {
		chunk, isPrefix, err := t.reader.ReadLine()
		if err != nil {
			return fullLine, err
		}

		if len(fullLine)+len(chunk) > t.maxMessageSize {
			return nil, ErrMessageTooLarge
		}

		fullLine = append(fullLine, chunk...)

		if !isPrefix {
			break
		}
	}

	return fullLine, nil
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
