package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// DefaultMaxMessageSize is the maximum size (10MB) for a single JSON-RPC message.
	// This prevents bufio.Scanner 64KB token overflow when processing large tool schemas.
	DefaultMaxMessageSize = 10 * 1024 * 1024

	// InitialScanBufferSize is the initial allocation for the line scanner buffer (64KB).
	InitialScanBufferSize = 64 * 1024
)

var (
	// ErrMessageTooLarge is returned when a line exceeds the configured maximum message size.
	ErrMessageTooLarge = errors.New("mcp: message exceeds maximum allowed buffer size (10MB)")

	// ErrEmptyMessage is returned when an empty message line is encountered unexpectedly.
	ErrEmptyMessage = errors.New("mcp: empty message line")
)

// Reader reads newline-delimited JSON-RPC messages from an underlying io.Reader.
// It is specifically designed to safely handle payloads up to 10MB without buffer overflow panics.
type Reader struct {
	scanner    *bufio.Scanner
	maxMsgSize int
}

// NewReader creates a new Reader configured with the default 10MB maximum buffer size.
func NewReader(r io.Reader) *Reader {
	return NewReaderWithSize(r, DefaultMaxMessageSize)
}

// NewReaderWithSize creates a new Reader with a custom maximum buffer size.
func NewReaderWithSize(r io.Reader, maxMsgSize int) *Reader {
	scanner := bufio.NewScanner(r)
	buf := make([]byte, InitialScanBufferSize)
	scanner.Buffer(buf, maxMsgSize)
	return &Reader{
		scanner:    scanner,
		maxMsgSize: maxMsgSize,
	}
}

// ReadMessage reads the next non-empty JSON-RPC message from the stream.
// It strips leading and trailing whitespace and automatically skips blank lines.
// Returns io.EOF cleanly when the stream terminates.
func (r *Reader) ReadMessage() (*Message, error) {
	for r.scanner.Scan() {
		line := bytes.TrimSpace(r.scanner.Bytes())
		if len(line) == 0 {
			// Skip blank/empty lines (e.g., extra newlines between messages)
			continue
		}

		var msg Message
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("mcp: failed to parse JSON-RPC message: %w", err)
		}
		return &msg, nil
	}

	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, ErrMessageTooLarge
		}
		return nil, fmt.Errorf("mcp: stream read error: %w", err)
	}

	return nil, io.EOF
}

// ReadRaw reads the next non-empty raw JSON byte slice from the stream without unmarshaling.
// Useful for fast pass-through proxying when inspecting headers only.
func (r *Reader) ReadRaw() ([]byte, error) {
	for r.scanner.Scan() {
		line := bytes.TrimSpace(r.scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// Return an independent copy of the slice as the scanner buffer is reused
		buf := make([]byte, len(line))
		copy(buf, line)
		return buf, nil
	}

	if err := r.scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, ErrMessageTooLarge
		}
		return nil, fmt.Errorf("mcp: stream read error: %w", err)
	}

	return nil, io.EOF
}

// ReadMessage reads from a *bufio.Reader directly (Interface Contract for PROJECT.md line 62).
// Reads bytes up to newline '\n' while enforcing the 10MB safety boundary.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return nil, err
		}

		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			if err != nil {
				return nil, err
			}
			continue
		}

		if len(trimmed) > DefaultMaxMessageSize {
			return nil, ErrMessageTooLarge
		}

		var msg Message
		if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
			return nil, fmt.Errorf("mcp: failed to parse JSON-RPC message: %w", unmarshalErr)
		}
		return &msg, nil
	}
}

// WriteMessage serializes a Message and writes it to w followed by a newline '\n'.
// (Interface Contract for PROJECT.md line 63).
func WriteMessage(w io.Writer, msg *Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("mcp: failed to marshal message: %w", err)
	}

	// Append newline delimiter
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("mcp: failed to write message: %w", err)
	}
	return nil
}

// Writer is a thread-safe message writer that serializes writes to an underlying stream.
type Writer struct {
	w  io.Writer
	mu sync.Mutex
}

// NewWriter creates a new thread-safe Writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// WriteMessage serializes and writes a Message atomically under lock.
func (w *Writer) WriteMessage(msg *Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WriteMessage(w.w, msg)
}

// WriteRaw writes raw JSON bytes followed by a newline atomically under lock.
func (w *Writer) WriteRaw(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	buf := append(trimmed, '\n')
	_, err := w.w.Write(buf)
	return err
}
