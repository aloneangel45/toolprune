package proxy

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

var (
	// ErrSessionClosed is returned when an operation is attempted on a closed session.
	ErrSessionClosed = errors.New("session is closed")

	// ErrSessionNotFound is returned when a requested session is not registered.
	ErrSessionNotFound = errors.New("session not found")

	// ErrSendTimeout is returned when sending to an outbound channel exceeds timeout.
	ErrSendTimeout = errors.New("send to client timed out (channel full)")
)

// ClientSession defines the common contract for any client connected to ToolPrune (stdio or SSE).
type ClientSession interface {
	ID() string
	Send(msg *mcp.Message) error
	Context() context.Context
}

// SSESession represents a single active HTTP/SSE connection.
type SSESession struct {
	id        string
	createdAt time.Time
	lastSeen  atomic.Int64 // Unix timestamp in seconds
	outbound  chan []byte  // Buffer of serialized JSON-RPC messages
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	mu        sync.RWMutex
	closeOnce sync.Once
}

// NewSSESession creates an initialized SSE session with a buffered channel.
func NewSSESession(parentCtx context.Context, id string, bufferSize int) *SSESession {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	ctx, cancel := context.WithCancel(parentCtx)
	s := &SSESession{
		id:        id,
		createdAt: time.Now(),
		outbound:  make(chan []byte, bufferSize),
		ctx:       ctx,
		cancel:    cancel,
	}
	s.lastSeen.Store(time.Now().Unix())
	return s
}

// ID returns the unique session identifier.
func (s *SSESession) ID() string {
	return s.id
}

// Context returns the session cancellation context.
func (s *SSESession) Context() context.Context {
	return s.ctx
}

// Outbound returns the channel of serialized message payloads.
func (s *SSESession) Outbound() <-chan []byte {
	return s.outbound
}

// Send enqueues a JSON-RPC message into the session outbound channel.
// Uses a non-blocking timeout to prevent proxy deadlocks if a client stalls.
func (s *SSESession) Send(msg *mcp.Message) (err error) {
	// Fast-path: avoid JSON marshaling if session is already closed
	if s.closed.Load() {
		return ErrSessionClosed
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message for session %s: %w", s.id, err)
	}

	// Read-lock guarantees close(s.outbound) in Close() cannot execute concurrently
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Re-verify closed state under lock
	if s.closed.Load() {
		return ErrSessionClosed
	}

	// Defense-in-depth: convert any runtime channel panic into clean error
	defer func() {
		if r := recover(); r != nil {
			err = ErrSessionClosed
		}
	}()

	s.lastSeen.Store(time.Now().Unix())

	select {
	case s.outbound <- data:
		return nil
	case <-s.ctx.Done():
		return ErrSessionClosed
	case <-time.After(2 * time.Second):
		return ErrSendTimeout
	}
}

// Close closes the session, cancels context, and drains the outbound channel.
func (s *SSESession) Close() error {
	s.closeOnce.Do(func() {
		// 1. Cancel context FIRST so any in-flight Send() waiting on full channel
		// immediately aborts via <-s.ctx.Done() and releases its RLock.
		s.cancel()

		// 2. Acquire exclusive lock to guarantee zero active Send() calls remain.
		s.mu.Lock()
		defer s.mu.Unlock()

		s.closed.Store(true)

		// 3. Close channel safely under lock.
		close(s.outbound)
	})
	return nil
}

// StdioSession implements ClientSession over standard I/O via StdioTransport.
type StdioSession struct {
	id        string
	transport *StdioTransport
	ctx       context.Context
	cancel    context.CancelFunc
}

// NewStdioSession creates a new StdioSession wrapping a StdioTransport.
func NewStdioSession(transport *StdioTransport) *StdioSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &StdioSession{
		id:        "stdio-client",
		transport: transport,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// ID returns the constant identifier for stdio.
func (s *StdioSession) ID() string {
	return s.id
}

// Send writes the message out via StdioTransport.
func (s *StdioSession) Send(msg *mcp.Message) error {
	return s.transport.WriteMessage(msg)
}

// Context returns the stdio session context.
func (s *StdioSession) Context() context.Context {
	return s.ctx
}

// Close tears down the stdio session.
func (s *StdioSession) Close() error {
	s.cancel()
	return s.transport.Close()
}

// SessionManager manages active client sessions in memory.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*SSESession
	closed   bool
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*SSESession),
	}
}

// GenerateSessionID generates a standard cryptographically strong UUIDv4.
func GenerateSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// CreateSession creates and registers a new SSESession.
func (m *SessionManager) CreateSession(parentCtx context.Context) *SSESession {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}

	id := GenerateSessionID()
	session := NewSSESession(parentCtx, id, 256)
	m.sessions[id] = session
	return session
}

// GetSession retrieves an active session by ID.
func (m *SessionManager) GetSession(id string) (*SSESession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, ok := m.sessions[id]
	return session, ok && !session.closed.Load()
}

// RemoveSession deletes and closes a session.
func (m *SessionManager) RemoveSession(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if session, ok := m.sessions[id]; ok {
		_ = session.Close()
		delete(m.sessions, id)
	}
}

// Count returns the number of active sessions.
func (m *SessionManager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// CloseAll closes and removes all active sessions.
func (m *SessionManager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	for id, s := range m.sessions {
		_ = s.Close()
		delete(m.sessions, id)
	}
}
