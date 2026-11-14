package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// MessageDispatcher abstracts client message handling so SSEServer can front
// either a single-upstream Proxy or a multi-upstream RouterGateway.
type MessageDispatcher interface {
	RegisterClient(session ClientSession)
	UnregisterClient(sessionID string)
	HandleClientMessage(session ClientSession, msg *mcp.Message) error
}

// SSEServerConfig specifies runtime options for the SSE and Streamable HTTP transports.
type SSEServerConfig struct {
	Host            string        // Bind address (default: "127.0.0.1")
	Port            int           // Port (default: 8080)
	BasePath        string        // Optional URL prefix (e.g. "/mcp")
	CORSOrigins     []string      // Allowed CORS origins (default: ["*"])
	KeepAlivePeriod time.Duration // Interval for ": ping" comments (default: 15s)
	MaxBodySize     int64         // Maximum inbound POST body (default: 10MB)
}

// DefaultSSEServerConfig returns production-ready default configuration.
func DefaultSSEServerConfig() SSEServerConfig {
	return SSEServerConfig{
		Host:            "127.0.0.1",
		Port:            8080,
		BasePath:        "",
		CORSOrigins:     []string{"*"},
		KeepAlivePeriod: 15 * time.Second,
		MaxBodySize:     10 * 1024 * 1024, // 10MB
	}
}

// SSEServer implements both the legacy MCP HTTP+SSE transport (/sse + /messages)
// and the modern MCP Streamable HTTP transport (POST/GET/DELETE /mcp).
type SSEServer struct {
	config     SSEServerConfig
	sessions   *SessionManager
	proxy      *Proxy
	dispatcher MessageDispatcher
	httpSrv    *http.Server
	listener   net.Listener
	running    atomic.Bool
	ctx        context.Context
	cancel     context.CancelFunc
}

// NewSSEServer creates a new SSEServer instance backed by a single-upstream Proxy.
func NewSSEServer(cfg SSEServerConfig, proxy *Proxy) *SSEServer {
	var disp MessageDispatcher
	if proxy != nil {
		disp = proxy
	}
	srv := NewSSEServerWithDispatcher(cfg, disp)
	srv.proxy = proxy
	return srv
}

// NewSSEServerWithDispatcher creates a new SSEServer instance backed by any MessageDispatcher.
func NewSSEServerWithDispatcher(cfg SSEServerConfig, dispatcher MessageDispatcher) *SSEServer {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.KeepAlivePeriod <= 0 {
		cfg.KeepAlivePeriod = 15 * time.Second
	}
	if cfg.MaxBodySize <= 0 {
		cfg.MaxBodySize = 10 * 1024 * 1024
	}
	if len(cfg.CORSOrigins) == 0 {
		cfg.CORSOrigins = []string{"*"}
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &SSEServer{
		config:     cfg,
		sessions:   NewSessionManager(),
		dispatcher: dispatcher,
		ctx:        ctx,
		cancel:     cancel,
	}
}

// Handler initializes HTTP routing handlers wrapped with localhost DNS-rebinding and CORS security middleware.
func (s *SSEServer) Handler() http.Handler {
	mux := http.NewServeMux()
	basePath := strings.TrimRight(s.config.BasePath, "/")
	ssePath := basePath + "/sse"
	msgPath := basePath + "/messages"

	mux.HandleFunc(ssePath, s.handleSSE)
	mux.HandleFunc(msgPath, s.handleMessages)

	if basePath == "" || basePath == "/mcp" {
		mux.HandleFunc("/mcp", s.handleStreamableHTTP)
	} else {
		mux.HandleFunc(basePath, s.handleStreamableHTTP)
		mux.HandleFunc(basePath+"/mcp", s.handleStreamableHTTP)
	}

	mux.HandleFunc("/health", s.handleHealth)

	return s.securityAndCORSMiddleware(mux)
}

// handleSSE handles GET /sse client requests per the MCP HTTP+SSE specification.
func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "Streaming unsupported by server")
		return
	}

	session := s.sessions.CreateSession(r.Context())
	if session == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "Server shutting down")
		return
	}
	defer s.sessions.RemoveSession(session.ID())
	if s.dispatcher != nil {
		defer s.dispatcher.UnregisterClient(session.ID())
		s.dispatcher.RegisterClient(session)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	basePath := strings.TrimRight(s.config.BasePath, "/")
	endpointURI := fmt.Sprintf("%s/messages?sessionId=%s", basePath, session.ID())
	if _, err := fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURI); err != nil {
		slog.Error("failed to write initial endpoint event", "sessionId", session.ID(), "err", err)
		return
	}
	flusher.Flush()

	slog.Info("SSE client connected", "sessionId", session.ID(), "remote", r.RemoteAddr)

	s.streamSessionEvents(w, r, flusher, session)
}

func (s *SSEServer) streamSessionEvents(w http.ResponseWriter, r *http.Request, flusher http.Flusher, session *SSESession) {
	ticker := time.NewTicker(s.config.KeepAlivePeriod)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			slog.Info("SSE client disconnected", "sessionId", session.ID())
			return

		case <-s.ctx.Done():
			slog.Info("SSE server shutting down, closing connection", "sessionId", session.ID())
			return

		case msgData, ok := <-session.outbound:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: message\ndata: %s\n\n", msgData); err != nil {
				slog.Error("failed to flush message to SSE client", "sessionId", session.ID(), "err", err)
				return
			}
			flusher.Flush()

		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleMessages handles POST /messages?sessionId=... requests.
func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}

	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		sessionID = r.Header.Get("Mcp-Session-Id")
	}
	if sessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing required sessionId query parameter")
		return
	}

	session, exists := s.sessions.GetSession(sessionID)
	if !exists {
		writeJSONError(w, http.StatusNotFound, "session not found or expired")
		return
	}

	msg, status, rpcErrMsg := s.readAndValidateBody(w, r)
	if rpcErrMsg != nil {
		writeJSONRPCError(w, status, rpcErrMsg)
		return
	}

	if s.dispatcher != nil {
		go func() {
			if err := s.dispatcher.HandleClientMessage(session, msg); err != nil {
				slog.Error("dispatcher error handling SSE message", "sessionId", sessionID, "method", msg.Method, "err", err)
			}
		}()
	}

	w.WriteHeader(http.StatusAccepted)
}

// streamableCallSession intercepts the single JSON-RPC response correlated to a POST /mcp request
// while forwarding server notifications to the underlying persistent session stream.
type streamableCallSession struct {
	ctx        context.Context
	underlying *SSESession
	expectedID mcp.RequestID
	respOnce   sync.Once
	respCh     chan *mcp.Message
}

func newStreamableCallSession(ctx context.Context, underlying *SSESession, expectedID mcp.RequestID) *streamableCallSession {
	return &streamableCallSession{
		ctx:        ctx,
		underlying: underlying,
		expectedID: expectedID,
		respCh:     make(chan *mcp.Message, 1),
	}
}

func (c *streamableCallSession) ID() string {
	return c.underlying.ID()
}

func (c *streamableCallSession) Context() context.Context {
	return c.ctx
}

func (c *streamableCallSession) Send(msg *mcp.Message) error {
	if msg != nil && msg.IsResponse() && msg.ID != nil && msg.ID.Equal(c.expectedID) {
		c.respOnce.Do(func() {
			c.respCh <- msg
		})
		return nil
	}
	return c.underlying.Send(msg)
}

// handleStreamableHTTP implements the MCP Streamable HTTP specification (POST / GET / DELETE /mcp).
func (s *SSEServer) handleStreamableHTTP(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.Header.Get("X-Session-ID"))
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.URL.Query().Get("sessionId"))
	}

	switch r.Method {
	case http.MethodDelete:
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "missing Mcp-Session-Id header")
			return
		}
		if _, exists := s.sessions.GetSession(sessionID); !exists {
			writeJSONError(w, http.StatusNotFound, "session not found or expired")
			return
		}
		if s.dispatcher != nil {
			s.dispatcher.UnregisterClient(sessionID)
		}
		s.sessions.RemoveSession(sessionID)
		w.WriteHeader(http.StatusNoContent)
		return

	case http.MethodGet:
		flusher, ok := w.(http.Flusher)
		if !ok {
			writeJSONError(w, http.StatusInternalServerError, "Streaming unsupported by server")
			return
		}

		var session *SSESession
		if sessionID != "" {
			existing, exists := s.sessions.GetSession(sessionID)
			if !exists {
				writeJSONError(w, http.StatusNotFound, "session not found or expired")
				return
			}
			session = existing
		} else {
			session = s.sessions.CreateSession(s.ctx)
			if session == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "Server shutting down")
				return
			}
			if s.dispatcher != nil {
				s.dispatcher.RegisterClient(session)
			}
		}

		w.Header().Set("Mcp-Session-Id", session.ID())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		s.streamSessionEvents(w, r, flusher, session)
		return

	case http.MethodPost:
		msg, status, rpcErrMsg := s.readAndValidateBody(w, r)
		if rpcErrMsg != nil {
			writeJSONRPCError(w, status, rpcErrMsg)
			return
		}

		if s.dispatcher == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "dispatcher not configured")
			return
		}

		var session *SSESession
		if sessionID == "" {
			session = s.sessions.CreateSession(s.ctx)
			if session == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "Server shutting down")
				return
			}
			s.dispatcher.RegisterClient(session)
		} else {
			existing, exists := s.sessions.GetSession(sessionID)
			if !exists {
				writeJSONError(w, http.StatusNotFound, "session not found or expired")
				return
			}
			session = existing
		}

		w.Header().Set("Mcp-Session-Id", session.ID())

		// Notifications and client responses return 202 Accepted immediately.
		if msg.IsNotification() || msg.IsResponse() {
			go func() {
				if err := s.dispatcher.HandleClientMessage(session, msg); err != nil {
					slog.Error("dispatcher error handling Streamable HTTP message", "sessionId", session.ID(), "method", msg.Method, "err", err)
				}
			}()
			w.WriteHeader(http.StatusAccepted)
			return
		}

		// Requests wait for the correlated response and return it directly over HTTP.
		capture := newStreamableCallSession(r.Context(), session, *msg.ID)
		go func() {
			if err := s.dispatcher.HandleClientMessage(capture, msg); err != nil {
				slog.Error("dispatcher error handling Streamable HTTP request", "sessionId", session.ID(), "method", msg.Method, "err", err)
			}
		}()

		select {
		case respMsg := <-capture.respCh:
			accept := r.Header.Get("Accept")
			if strings.Contains(accept, "text/event-stream") && !strings.Contains(accept, "application/json") {
				data, err := json.Marshal(respMsg)
				if err != nil {
					writeJSONError(w, http.StatusInternalServerError, "failed to marshal response")
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache, no-transform")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(respMsg)
			return

		case <-r.Context().Done():
			return

		case <-s.ctx.Done():
			writeJSONError(w, http.StatusServiceUnavailable, "Server shutting down")
			return
		}

	default:
		writeJSONError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

func (s *SSEServer) readAndValidateBody(w http.ResponseWriter, r *http.Request) (*mcp.Message, int, *mcp.Message) {
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, http.StatusRequestEntityTooLarge, mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "request body exceeds maximum allowed size")
		}
		return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeParseError, fmt.Sprintf("failed to read request body: %v", err))
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeParseError, "empty JSON-RPC payload")
	}

	if trimmed[0] != '{' {
		var probe any
		if synErr := json.Unmarshal(trimmed, &probe); synErr != nil {
			return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeParseError, "Parse error")
		}
		return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Invalid Request: batch arrays and non-object payloads are not supported")
	}

	var msg mcp.Message
	if err := json.Unmarshal(trimmed, &msg); err != nil {
		if errors.Is(err, mcp.ErrInvalidRequestID) {
			return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, err.Error())
		}
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeParseError, "Parse error")
		}
		return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, "Invalid Request")
	}

	if envErr := msg.ValidateEnvelope(); envErr != nil {
		return nil, http.StatusBadRequest, mcp.NewErrorMessage(nil, mcp.CodeInvalidRequest, envErr.Error())
	}

	return &msg, http.StatusOK, nil
}

func (s *SSEServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"sessions": s.sessions.Count(),
	})
}

// securityAndCORSMiddleware validates Host and Origin headers to defeat localhost DNS-rebinding
// and cross-origin browser attacks, and emits spec-compliant single-origin CORS headers.
func (s *SSEServer) securityAndCORSMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isLoopbackBindHost(s.config.Host) {
			if !isLoopbackHostHeader(r.Host) {
				writeJSONError(w, http.StatusForbidden, "forbidden Host header on loopback listener")
				return
			}
		}

		origin := strings.TrimSpace(r.Header.Get("Origin"))
		matchedOrigin, allowed := s.matchAllowedOrigin(origin)
		if !allowed {
			writeJSONError(w, http.StatusForbidden, "Origin not allowed")
			return
		}

		s.applyCORSHeaders(w, matchedOrigin)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (s *SSEServer) hasWildcardCORS() bool {
	for _, o := range s.config.CORSOrigins {
		if strings.TrimSpace(o) == "*" {
			return true
		}
	}
	return false
}

func (s *SSEServer) matchAllowedOrigin(origin string) (string, bool) {
	loopbackBind := isLoopbackBindHost(s.config.Host)
	wildcard := s.hasWildcardCORS()

	if origin == "" {
		if !loopbackBind && wildcard {
			return "*", true
		}
		return "", true
	}

	for _, allowed := range s.config.CORSOrigins {
		trimmed := strings.TrimSpace(allowed)
		if trimmed == "" || trimmed == "*" {
			continue
		}
		if strings.EqualFold(trimmed, origin) {
			return origin, true
		}
	}

	if wildcard {
		if loopbackBind {
			if isLoopbackOrigin(origin) {
				return origin, true
			}
			return "", false
		}
		if strings.EqualFold(origin, "null") {
			return "", false
		}
		return origin, true
	}

	return "", false
}

func (s *SSEServer) applyCORSHeaders(w http.ResponseWriter, matchedOrigin string) {
	w.Header().Add("Vary", "Origin")
	if matchedOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", matchedOrigin)
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID, Mcp-Session-Id, Accept, Last-Event-ID, X-Requested-With")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type, Mcp-Session-Id")
}

func isLoopbackBindHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "" {
		return true
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func isLoopbackHostHeader(hostHeader string) bool {
	h := strings.TrimSpace(hostHeader)
	if h == "" {
		return false
	}
	if splitHost, _, err := net.SplitHostPort(h); err == nil {
		h = splitHost
	} else {
		h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	}
	h = strings.ToLower(strings.TrimSpace(h))
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	payload, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{
		Error: message,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

func writeJSONRPCError(w http.ResponseWriter, status int, msg *mcp.Message) {
	payload, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// Start begins listening and serving HTTP requests.
func (s *SSEServer) Start() error {
	addr := net.JoinHostPort(s.config.Host, fmt.Sprintf("%d", s.config.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind SSE server listener on %s: %w", addr, err)
	}
	s.listener = ln
	s.running.Store(true)

	// Note: WriteTimeout MUST be 0 for long-lived SSE connections in net/http
	s.httpSrv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	slog.Info("SSE server listening", "addr", ln.Addr().String(), "basePath", s.config.BasePath)

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("SSE HTTP server error", "err", err)
		}
	}()

	return nil
}

// Shutdown gracefully terminates all active SSE streams and stops the server.
func (s *SSEServer) Shutdown(ctx context.Context) error {
	s.cancel()
	s.sessions.CloseAll()
	if s.running.CompareAndSwap(true, false) {
		if s.httpSrv != nil {
			return s.httpSrv.Shutdown(ctx)
		}
	}
	return nil
}

// Addr returns the bound address of the server (useful when bound to port 0 for tests).
func (s *SSEServer) Addr() net.Addr {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return nil
}
