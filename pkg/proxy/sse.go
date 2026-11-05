package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// SSEServerConfig specifies runtime options for the SSE transport.
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

// SSEServer implements the Model Context Protocol HTTP/SSE transport.
type SSEServer struct {
	config   SSEServerConfig
	sessions *SessionManager
	proxy    *Proxy
	httpSrv  *http.Server
	listener net.Listener
	running  atomic.Bool
	ctx      context.Context
	cancel   context.CancelFunc
}

// NewSSEServer creates a new SSEServer instance.
func NewSSEServer(cfg SSEServerConfig, proxy *Proxy) *SSEServer {
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
		config:   cfg,
		sessions: NewSessionManager(),
		proxy:    proxy,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// Handler initializes HTTP routing handlers with CORS middleware.
func (s *SSEServer) Handler() http.Handler {
	mux := http.NewServeMux()
	ssePath := s.config.BasePath + "/sse"
	msgPath := s.config.BasePath + "/messages"

	mux.HandleFunc(ssePath, s.handleSSE)
	mux.HandleFunc(msgPath, s.handleMessages)
	mux.HandleFunc("/health", s.handleHealth)

	return s.corsMiddleware(mux)
}

// handleSSE handles GET /sse client requests per the MCP SSE specification.
func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	// 1. CORS Preflight
	if r.Method == http.MethodOptions {
		s.writeCORSPreflight(w)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"Method Not Allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 2. Validate Flusher support
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, `{"error":"Streaming unsupported by server"}`, http.StatusInternalServerError)
		return
	}

	// 3. Set SSE response headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable Nginx proxy buffering
	s.applyCORS(w)
	w.WriteHeader(http.StatusOK)

	// 4. Create and register session
	session := s.sessions.CreateSession(r.Context())
	if session == nil {
		http.Error(w, `{"error":"Server shutting down"}`, http.StatusServiceUnavailable)
		return
	}
	defer s.sessions.RemoveSession(session.ID())
	if s.proxy != nil {
		defer s.proxy.UnregisterClient(session.ID())
		s.proxy.RegisterClient(session)
	}

	// 5. Send initial "endpoint" event per MCP spec
	endpointURI := fmt.Sprintf("%s/messages?sessionId=%s", s.config.BasePath, session.ID())
	if _, err := fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpointURI); err != nil {
		slog.Error("failed to write initial endpoint event", "sessionId", session.ID(), "err", err)
		return
	}
	flusher.Flush()

	slog.Info("SSE client connected", "sessionId", session.ID(), "remote", r.RemoteAddr)

	// 6. Streaming Event Loop
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
			// Write keepalive comment to prevent router/proxy timeout
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleMessages handles POST /messages?sessionId=... requests.
func (s *SSEServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	// 1. CORS Preflight
	if r.Method == http.MethodOptions {
		s.writeCORSPreflight(w)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"Method Not Allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// 2. Extract and validate sessionId
	sessionID := r.URL.Query().Get("sessionId")
	if sessionID == "" {
		sessionID = r.Header.Get("X-Session-ID")
	}
	if sessionID == "" {
		s.applyCORS(w)
		http.Error(w, `{"error":"missing required sessionId query parameter"}`, http.StatusBadRequest)
		return
	}

	session, exists := s.sessions.GetSession(sessionID)
	if !exists {
		s.applyCORS(w)
		http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
		return
	}

	// 3. Bound request body size (up to 10MB)
	r.Body = http.MaxBytesReader(w, r.Body, s.config.MaxBodySize)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.applyCORS(w)
		http.Error(w, fmt.Sprintf(`{"error":"failed to read request body: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// 4. Parse JSON-RPC 2.0 message
	var msg mcp.Message
	if err := json.Unmarshal(body, &msg); err != nil {
		s.applyCORS(w)
		http.Error(w, fmt.Sprintf(`{"error":"invalid JSON-RPC payload: %s"}`, err.Error()), http.StatusBadRequest)
		return
	}

	// 5. Dispatch message to core proxy
	if s.proxy != nil {
		go func() {
			if err := s.proxy.HandleClientMessage(session, &msg); err != nil {
				slog.Error("proxy error handling message", "sessionId", sessionID, "method", msg.Method, "err", err)
			}
		}()
	}

	// 6. Return HTTP 202 Accepted immediately per MCP specification
	s.applyCORS(w)
	w.WriteHeader(http.StatusAccepted)
}

func (s *SSEServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.applyCORS(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":   "ok",
		"sessions": s.sessions.Count(),
	})
}

// CORS helpers
func (s *SSEServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.applyCORS(w)
		if r.Method == http.MethodOptions {
			s.writeCORSPreflight(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *SSEServer) applyCORS(w http.ResponseWriter) {
	origin := strings.Join(s.config.CORSOrigins, ", ")
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-ID, X-Requested-With")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Type")
}

func (s *SSEServer) writeCORSPreflight(w http.ResponseWriter) {
	s.applyCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// Start begins listening and serving HTTP requests.
func (s *SSEServer) Start() error {
	addr := fmt.Sprintf("%s:%d", s.config.Host, s.config.Port)
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
	if s.running.CompareAndSwap(true, false) {
		s.cancel()
		s.sessions.CloseAll()
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
