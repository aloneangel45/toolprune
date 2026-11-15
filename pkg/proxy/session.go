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

const (
	// DefaultMaxSessions caps concurrent tracked HTTP/SSE sessions to bound memory usage.
	DefaultMaxSessions = 1024
	// DefaultSessionIdleTTL evicts idle stateless HTTP sessions that have no active SSE stream.
	DefaultSessionIdleTTL = 30 * time.Minute
)

var (
	// ErrSessionClosed is returned when an operation is attempted on a closed session.
	ErrSessionClosed = errors.New("session is closed")

	// ErrSessionNotFound is returned when a requested session is not registered.
	ErrSessionNotFound = errors.New("session not found")

	// ErrSendTimeout is returned when an outbound session buffer is full.
	ErrSendTimeout = errors.New("send to client dropped (channel full)")
)

// ClientSession defines the common contract for any client connected to ToolPrune (stdio or SSE).
type ClientSession interface {
	ID() string
	Send(msg *mcp.Message) error
	Context() context.Context
}

// SSESession represents a single active HTTP/SSE or Streamable HTTP session.
type SSESession struct {
	id            string
	createdAt     time.Time
	lastSeen      atomic.Int64 // Unix timestamp in nanoseconds
	activeStreams atomic.Int32 // Number of open SSE GET streams bound to this session
	outbound      chan []byte  // Buffer of serialized JSON-RPC messages
	ctx           context.Context
	cancel        context.CancelFunc
	closed        atomic.Bool
	mu            sync.RWMutex
	closeOnce     sync.Once
}

// NewSSESession creates an initialized SSE session with a buffered channel.
func NewSSESession(parentCtx context.Context, id string, bufferSize int) *SSESession {
	if bufferSize <= 0 {
		bufferSize = 256
	}
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(parentCtx)
	now := time.Now()
	s := &SSESession{
		id:        id,
		createdAt: now,
		outbound:  make(chan []byte, bufferSize),
		ctx:       ctx,
		cancel:    cancel,
	}
	s.lastSeen.Store(now.UnixNano())
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

// Touch updates the session's last-activity timestamp.
func (s *SSESession) Touch() {
	s.lastSeen.Store(time.Now().UnixNano())
}

// LastSeen returns the last-activity timestamp for idle eviction checks.
func (s *SSESession) LastSeen() time.Time {
	return time.Unix(0, s.lastSeen.Load())
}

// AcquireStream marks an active SSE stream attached to this session.
func (s *SSESession) AcquireStream() {
	s.activeStreams.Add(1)
	s.Touch()
}

// ReleaseStream decrements the active SSE stream count attached to this session.
func (s *SSESession) ReleaseStream() {
	s.activeStreams.Add(-1)
	s.Touch()
}

// ActiveStreams returns the number of currently open SSE GET streams for this session.
func (s *SSESession) ActiveStreams() int32 {
	return s.activeStreams.Load()
}

// Send enqueues a JSON-RPC message into the session outbound channel without blocking if full.
func (s *SSESession) Send(msg *mcp.Message) error {
	if s.closed.Load() {
		return ErrSessionClosed
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message for session %s: %w", s.id, err)
	}

	return s.SendRaw(data)
}

// SendRaw enqueues pre-serialized JSON bytes (e.g. a single message or batch response array)
// into the session outbound channel without blocking when the buffer is full.
func (s *SSESession) SendRaw(data []byte) (err error) {
	if s.closed.Load() {
		return ErrSessionClosed
	}

	// Read-lock guarantees close(s.outbound) in Close() cannot execute concurrently
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed.Load() {
		return ErrSessionClosed
	}

	// Defense-in-depth: convert any runtime channel panic into clean error
	defer func() {
		if r := recover(); r != nil {
			err = ErrSessionClosed
		}
	}()

	select {
	case <-s.ctx.Done():
		return ErrSessionClosed
	default:
	}

	s.Touch()

	select {
	case s.outbound <- data:
		return nil
	case <-s.ctx.Done():
		return ErrSessionClosed
	default:
		return ErrSendTimeout
	}
}

// Close closes the session, cancels context, and closes the outbound channel.
func (s *SSESession) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()

		s.mu.Lock()
		defer s.mu.Unlock()

		s.closed.Store(true)
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

// SessionManagerConfig configures session capacity limits and idle eviction TTL.
type SessionManagerConfig struct {
	MaxSessions int
	IdleTTL     time.Duration
	OnEvict     func(sessionID string)
}

// SessionManager manages active client sessions in memory with bounded capacity and idle eviction.
type SessionManager struct {
	mu          sync.RWMutex
	sessions    map[string]*SSESession
	maxSessions int
	idleTTL     time.Duration
	onEvict     func(sessionID string)
	closed      bool
}

// NewSessionManager creates a new SessionManager with production defaults.
func NewSessionManager() *SessionManager {
	return NewSessionManagerWithConfig(SessionManagerConfig{})
}

// NewSessionManagerWithConfig creates a new SessionManager with explicit capacity and idle TTL settings.
func NewSessionManagerWithConfig(cfg SessionManagerConfig) *SessionManager {
	maxSessions := cfg.MaxSessions
	if maxSessions <= 0 {
		maxSessions = DefaultMaxSessions
	}
	idleTTL := cfg.IdleTTL
	if idleTTL <= 0 {
		idleTTL = DefaultSessionIdleTTL
	}
	return &SessionManager{
		sessions:    make(map[string]*SSESession),
		maxSessions: maxSessions,
		idleTTL:     idleTTL,
		onEvict:     cfg.OnEvict,
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

func (m *SessionManager) evictIdleLocked(now time.Time) []string {
	if m.idleTTL <= 0 {
		return nil
	}
	var evicted []string
	for id, s := range m.sessions {
		if s.closed.Load() || (s.ActiveStreams() <= 0 && now.Sub(s.LastSeen()) > m.idleTTL) {
			_ = s.Close()
			delete(m.sessions, id)
			evicted = append(evicted, id)
		}
	}
	return evicted
}

// EvictIdle sweeps expired idle sessions that have no open SSE stream and returns the count evicted.
func (m *SessionManager) EvictIdle(now time.Time) int {
	m.mu.Lock()
	evicted := m.evictIdleLocked(now)
	onEvict := m.onEvict
	m.mu.Unlock()

	if onEvict != nil {
		for _, id := range evicted {
			onEvict(id)
		}
	}
	return len(evicted)
}

// CreateSession creates and registers a new SSESession, evicting expired or oldest idle sessions when at capacity.
func (m *SessionManager) CreateSession(parentCtx context.Context) *SSESession {
	now := time.Now()

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}

	evicted := m.evictIdleLocked(now)

	for m.maxSessions > 0 && len(m.sessions) >= m.maxSessions {
		var oldestID string
		var oldestNano int64
		for id, s := range m.sessions {
			if s.ActiveStreams() > 0 {
				continue
			}
			ts := s.lastSeen.Load()
			if oldestID == "" || ts < oldestNano || (ts == oldestNano && id < oldestID) {
				oldestID = id
				oldestNano = ts
			}
		}
		if oldestID == "" {
			// All sessions have active live SSE streams; reject over-capacity allocation.
			onEvict := m.onEvict
			m.mu.Unlock()
			if onEvict != nil {
				for _, id := range evicted {
					onEvict(id)
				}
			}
			return nil
		}
		if victim, ok := m.sessions[oldestID]; ok {
			_ = victim.Close()
			delete(m.sessions, oldestID)
			evicted = append(evicted, oldestID)
		}
	}

	id := GenerateSessionID()
	session := NewSSESession(parentCtx, id, 256)
	m.sessions[id] = session
	onEvict := m.onEvict
	m.mu.Unlock()

	if onEvict != nil {
		for _, evID := range evicted {
			onEvict(evID)
		}
	}

	return session
}

// GetSession retrieves an active session by ID, evicting it if its idle TTL has expired.
func (m *SessionManager) GetSession(id string) (*SSESession, bool) {
	m.mu.RLock()
	session, ok := m.sessions[id]
	idleTTL := m.idleTTL
	m.mu.RUnlock()

	if !ok || session.closed.Load() {
		return nil, false
	}

	if idleTTL > 0 && session.ActiveStreams() <= 0 && time.Since(session.LastSeen()) > idleTTL {
		m.RemoveSession(id)
		return nil, false
	}

	session.Touch()
	return session, true
}

// RemoveSession deletes and closes a session.
func (m *SessionManager) RemoveSession(id string) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	if ok {
		_ = session.Close()
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
	m.closed = true
	toClose := make([]*SSESession, 0, len(m.sessions))
	for id, s := range m.sessions {
		toClose = append(toClose, s)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	for _, s := range toClose {
		_ = s.Close()
	}
}
