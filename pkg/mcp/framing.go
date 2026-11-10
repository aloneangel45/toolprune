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

	// ErrParse indicates malformed JSON syntax (-32700 Parse error).
	ErrParse = errors.New("mcp: JSON-RPC parse error")

	// ErrInvalidRequest indicates valid JSON that is not a valid JSON-RPC 2.0 object (-32600 Invalid Request).
	ErrInvalidRequest = errors.New("mcp: JSON-RPC invalid request")
)

// Reader reads newline-delimited JSON-RPC messages from an underlying io.Reader.
// It uses a 64KB initial buffer and enforces a bounded 10MB hard cap before allocation.
type Reader struct {
	bufReader  *bufio.Reader
	maxMsgSize int
}

// NewReader creates a new Reader configured with the default 10MB maximum message size.
func NewReader(r io.Reader) *Reader {
	return NewReaderWithSize(r, DefaultMaxMessageSize)
}

// NewReaderWithSize creates a new Reader with a custom maximum message size.
func NewReaderWithSize(r io.Reader, maxMsgSize int) *Reader {
	if maxMsgSize <= 0 {
		maxMsgSize = DefaultMaxMessageSize
	}
	initBuf := InitialScanBufferSize
	if maxMsgSize < initBuf {
		initBuf = maxMsgSize
	}
	if initBuf < 4096 {
		initBuf = 4096
	}
	return &Reader{
		bufReader:  bufio.NewReaderSize(r, initBuf),
		maxMsgSize: maxMsgSize,
	}
}

// ReadBoundedLine reads a single newline-delimited frame from r with a strict pre-allocation byte cap.
// Returns trimmed non-empty line bytes, skipping blank/whitespace-only lines.
// Returns io.EOF when the stream ends cleanly without pending data.
func ReadBoundedLine(r *bufio.Reader, maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageSize
	}
	// Allow a small margin for trailing \r\n whitespace prior to TrimSpace.
	rawCap := maxBytes + 64

	for {
		frag, err := r.ReadSlice('\n')
		if err == nil {
			trimmed := bytes.TrimSpace(frag)
			if len(trimmed) == 0 {
				continue
			}
			if len(trimmed) > maxBytes {
				return nil, ErrMessageTooLarge
			}
			out := make([]byte, len(trimmed))
			copy(out, trimmed)
			return out, nil
		}

		if !errors.Is(err, bufio.ErrBufferFull) {
			trimmed := bytes.TrimSpace(frag)
			if len(trimmed) == 0 {
				return nil, err
			}
			if len(trimmed) > maxBytes {
				return nil, ErrMessageTooLarge
			}
			out := make([]byte, len(trimmed))
			copy(out, trimmed)
			return out, nil
		}

		// Line exceeded bufio internal buffer: accumulate chunks with strict pre-allocation bound.
		if len(frag) > rawCap {
			drainLineRemainder(r)
			return nil, ErrMessageTooLarge
		}

		acc := make([]byte, 0, minInt(len(frag)*2, rawCap))
		acc = append(acc, frag...)

		for {
			part, partErr := r.ReadSlice('\n')
			if len(acc)+len(part) > rawCap {
				if errors.Is(partErr, bufio.ErrBufferFull) {
					drainLineRemainder(r)
				}
				return nil, ErrMessageTooLarge
			}
			acc = append(acc, part...)

			if partErr == nil {
				break
			}
			if errors.Is(partErr, bufio.ErrBufferFull) {
				continue
			}
			if errors.Is(partErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("mcp: stream read error: %w", partErr)
		}

		trimmed := bytes.TrimSpace(acc)
		if len(trimmed) == 0 {
			continue
		}
		if len(trimmed) > maxBytes {
			return nil, ErrMessageTooLarge
		}
		return trimmed, nil
	}
}

// drainLineRemainder discards remaining bytes on an oversized line up to '\n' or EOF without allocating.
func drainLineRemainder(r *bufio.Reader) {
	for {
		_, err := r.ReadSlice('\n')
		if !errors.Is(err, bufio.ErrBufferFull) {
			return
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseJSONRPCLine unmarshals a trimmed JSON-RPC line and classifies syntax vs invalid-request errors.
func parseJSONRPCLine(trimmed []byte) (*Message, error) {
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("%w: invalid JSON syntax", ErrParse)
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("%w: JSON-RPC message must be a JSON object", ErrInvalidRequest)
	}
	var msg Message
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		if errors.Is(err, ErrInvalidRequestID) {
			return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return nil, fmt.Errorf("%w: %v", ErrParse, err)
		}
		return nil, fmt.Errorf("%w: failed to unmarshal JSON-RPC object: %v", ErrInvalidRequest, err)
	}
	return &msg, nil
}

// ReadMessage reads the next non-empty JSON-RPC message from the stream.
// It strips leading and trailing whitespace and automatically skips blank lines.
// Returns io.EOF cleanly when the stream terminates.
func (r *Reader) ReadMessage() (*Message, error) {
	line, err := ReadBoundedLine(r.bufReader, r.maxMsgSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, ErrMessageTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("mcp: stream read error: %w", err)
	}
	return parseJSONRPCLine(line)
}

// ReadRaw reads the next non-empty raw JSON byte slice from the stream without unmarshaling.
// Useful for fast pass-through proxying when inspecting headers only.
func (r *Reader) ReadRaw() ([]byte, error) {
	line, err := ReadBoundedLine(r.bufReader, r.maxMsgSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, ErrMessageTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("mcp: stream read error: %w", err)
	}
	return line, nil
}

// ReadMessage reads from a *bufio.Reader directly (Interface Contract for PROJECT.md line 62).
// Reads bytes up to newline '\n' while enforcing the 10MB safety boundary BEFORE allocation.
func ReadMessage(r *bufio.Reader) (*Message, error) {
	line, err := ReadBoundedLine(r, DefaultMaxMessageSize)
	if err != nil {
		return nil, err
	}
	return parseJSONRPCLine(line)
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
