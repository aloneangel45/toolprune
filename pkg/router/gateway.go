package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// GatewayConfig configures the multi-upstream RouterGateway.
type GatewayConfig struct {
	RequestTimeout   time.Duration
	InjectProxyInfo  bool
	ServerVersion    string
	DebounceInterval time.Duration
	Logger           *slog.Logger
}

type gatewayInFlight struct {
	ClientID      *mcp.RequestID
	ClientSession proxy.ClientSession
	UpstreamID    string
	Method        string
	StartTime     time.Time
}

// RouterGateway bridges client transports (Stdio, SSE, Streamable HTTP) to a multi-upstream Router
// with full asynchronous multiplexing, per-upstream crash isolation, timeout sweeping, and debounced
// tools/list_changed notifications.
type RouterGateway struct {
	cfg      GatewayConfig
	router   *Router
	notifier *DebouncedNotifier
	logger   *slog.Logger

	clientsMu sync.RWMutex
	clients   map[string]proxy.ClientSession

	trackMu  sync.Mutex
	inFlight map[string]*gatewayInFlight
	seq      atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewRouterGateway constructs a new asynchronous multi-upstream gateway around Router.
func NewRouterGateway(r *Router, cfg GatewayConfig) *RouterGateway {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	if cfg.ServerVersion == "" {
		cfg.ServerVersion = "1.2.0"
	}
	if cfg.DebounceInterval <= 0 {
		cfg.DebounceInterval = 50 * time.Millisecond
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	gw := &RouterGateway{
		cfg:      cfg,
		router:   r,
		logger:   logger,
		clients:  make(map[string]proxy.ClientSession),
		inFlight: make(map[string]*gatewayInFlight),
		ctx:      ctx,
		cancel:   cancel,
	}
	gw.seq.Store(1000)

	gw.notifier = NewDebouncedNotifier(gw.broadcastMessage, cfg.DebounceInterval)
	r.SetNotifier(gw.notifier.Trigger)
	return gw
}

// Notifier returns the gateway's debounced list_changed notifier (so ToolWindow can share it).
func (g *RouterGateway) Notifier() *DebouncedNotifier {
	return g.notifier
}

// Start activates multiplexer reader loops and request timeout sweeping.
func (g *RouterGateway) Start() error {
	if err := g.router.Start(g); err != nil {
		return err
	}
	g.wg.Add(1)
	go g.runTimeoutSweeper()
	return nil
}

// Shutdown cleanly stops the notifier, timeout sweeper, and all upstream transports.
func (g *RouterGateway) Shutdown() {
	g.cancel()
	if g.notifier != nil {
		g.notifier.Stop()
	}
	_ = g.router.Close()
	g.wg.Wait()
}

// RegisterClient satisfies proxy.MessageDispatcher.
func (g *RouterGateway) RegisterClient(session proxy.ClientSession) {
	g.clientsMu.Lock()
	defer g.clientsMu.Unlock()
	g.clients[session.ID()] = session
}

// UnregisterClient satisfies proxy.MessageDispatcher.
func (g *RouterGateway) UnregisterClient(sessionID string) {
	g.clientsMu.Lock()
	defer g.clientsMu.Unlock()
	delete(g.clients, sessionID)
}

func (g *RouterGateway) broadcastMessage(msg *mcp.Message) error {
	g.clientsMu.RLock()
	defer g.clientsMu.RUnlock()
	for _, client := range g.clients {
		_ = client.Send(msg)
	}
	return nil
}

// HandleClientMessage satisfies proxy.MessageDispatcher for stdio, SSE, and Streamable HTTP sessions.
func (g *RouterGateway) HandleClientMessage(session proxy.ClientSession, msg *mcp.Message) error {
	if msg == nil {
		return errors.New("nil message received")
	}

	if msg.IsNotification() {
		// Client notifications (e.g. notifications/initialized) require no response.
		return nil
	}

	if !msg.IsRequest() {
		return nil
	}

	switch msg.Method {
	case mcp.MethodInitialize:
		serverName := "ToolPrune Unified Proxy"
		instructions := ""
		if g.cfg.InjectProxyInfo {
			instructions = "ToolPrune multiplexed MCP proxy: tools pruned and dynamically routed."
		}
		initRes := mcp.InitializeResult{
			ProtocolVersion: mcp.LatestProtocolVersion,
			Capabilities: mcp.ServerCapabilities{
				Tools: &mcp.ToolsCapability{ListChanged: true},
			},
			ServerInfo:   mcp.Implementation{Name: serverName, Version: g.cfg.ServerVersion},
			Instructions: instructions,
		}
		resp, err := mcp.NewResponse(*msg.ID, initRes)
		if err != nil {
			return err
		}
		return session.Send(resp)

	case mcp.MethodPing:
		resp, err := mcp.NewResponse(*msg.ID, map[string]any{})
		if err != nil {
			return err
		}
		return session.Send(resp)

	case mcp.MethodToolsList:
		resp, err := g.router.HandleToolsList(session.Context(), msg)
		if err != nil {
			resp = mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, err.Error())
		}
		return session.Send(resp)

	case mcp.MethodToolsCall:
		if internalResp, handled := g.router.HandleInternalToolCall(msg); handled {
			return session.Send(internalResp)
		}

		if preValResp := g.router.PreValidateCall(msg); preValResp != nil {
			return session.Send(preValResp)
		}

		targetTransport, rewrittenMsg, err := g.router.RouteToolCall(msg)
		if err != nil {
			code := mcp.CodeMethodNotFound
			errText := err.Error()
			var rpcErr *mcp.JSONRPCError
			if errors.As(err, &rpcErr) {
				code = rpcErr.Code
				errText = rpcErr.Message
			}
			return session.Send(mcp.NewErrorMessage(msg.ID, code, errText))
		}

		if targetTransport == nil {
			return session.Send(mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, "upstream transport unavailable"))
		}

		upstreamID := g.resolveUpstreamIDForTransport(targetTransport)
		trackedMsg, key := g.trackRequest(session, msg, rewrittenMsg, upstreamID)

		if sendErr := targetTransport.Send(trackedMsg); sendErr != nil {
			g.popRequestByKey(key)
			return session.Send(mcp.NewErrorMessage(
				msg.ID,
				mcp.CodeInternalError,
				fmt.Sprintf("failed to send request to upstream %q: %v", upstreamID, sendErr),
			))
		}
		return nil

	default:
		return session.Send(mcp.NewErrorMessage(msg.ID, mcp.CodeMethodNotFound, fmt.Sprintf("Method not found: %s", msg.Method)))
	}
}

func (g *RouterGateway) resolveUpstreamIDForTransport(tr UpstreamTransport) string {
	mux := g.router.Multiplexer()
	mux.mu.RLock()
	defer mux.mu.RUnlock()
	for id, target := range mux.upstreams {
		if target.transport == tr {
			return id
		}
	}
	return ""
}

func (g *RouterGateway) trackRequest(
	session proxy.ClientSession,
	origMsg *mcp.Message,
	rewrittenMsg *mcp.Message,
	upstreamID string,
) (*mcp.Message, string) {
	upNum := g.seq.Add(1)
	synID := mcp.NewIntID(int64(upNum))
	key := fmt.Sprintf("%s:%s", upstreamID, synID.String())

	g.trackMu.Lock()
	g.inFlight[key] = &gatewayInFlight{
		ClientID:      origMsg.ID,
		ClientSession: session,
		UpstreamID:    upstreamID,
		Method:        origMsg.Method,
		StartTime:     time.Now(),
	}
	g.trackMu.Unlock()

	cloned := *rewrittenMsg
	cloned.ID = &synID
	return &cloned, key
}

func (g *RouterGateway) popRequest(upstreamID string, id *mcp.RequestID) (*gatewayInFlight, bool) {
	if id == nil {
		return nil, false
	}
	key := fmt.Sprintf("%s:%s", upstreamID, id.String())
	return g.popRequestByKey(key)
}

func (g *RouterGateway) popRequestByKey(key string) (*gatewayInFlight, bool) {
	g.trackMu.Lock()
	defer g.trackMu.Unlock()
	req, ok := g.inFlight[key]
	if ok {
		delete(g.inFlight, key)
	}
	return req, ok
}

// HandleUpstreamResponse satisfies UpstreamMessageHandler.
func (g *RouterGateway) HandleUpstreamResponse(upstreamID string, msg *mcp.Message) {
	if msg == nil || msg.ID == nil {
		return
	}
	req, ok := g.popRequest(upstreamID, msg.ID)
	if !ok {
		g.logger.Debug("ignoring uncorrelated upstream response", "upstream", upstreamID, "id", msg.ID.String())
		return
	}

	out := *msg
	out.ID = req.ClientID
	_ = req.ClientSession.Send(&out)
}

// HandleUpstreamNotification satisfies UpstreamMessageHandler.
func (g *RouterGateway) HandleUpstreamNotification(upstreamID string, msg *mcp.Message) {
	if msg == nil {
		return
	}
	if msg.Method == mcp.MethodNotificationsToolsListChanged {
		if g.notifier != nil {
			g.notifier.Trigger()
		}
		return
	}
	_ = g.broadcastMessage(msg)
}

// HandleUpstreamCrash satisfies UpstreamMessageHandler, failing only requests routed to the crashed upstream.
func (g *RouterGateway) HandleUpstreamCrash(upstreamID string, err error) {
	g.trackMu.Lock()
	var affected []*gatewayInFlight
	for k, req := range g.inFlight {
		if req.UpstreamID == upstreamID {
			affected = append(affected, req)
			delete(g.inFlight, k)
		}
	}
	g.trackMu.Unlock()

	for _, req := range affected {
		errMsg := mcp.NewErrorMessage(
			req.ClientID,
			mcp.CodeInternalError,
			fmt.Sprintf("upstream %q terminated unexpectedly: %v", upstreamID, err),
		)
		_ = req.ClientSession.Send(errMsg)
	}
}

func (g *RouterGateway) runTimeoutSweeper() {
	defer g.wg.Done()
	interval := g.cfg.RequestTimeout / 2
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-g.ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			var expired []*gatewayInFlight
			g.trackMu.Lock()
			for k, req := range g.inFlight {
				if now.Sub(req.StartTime) > g.cfg.RequestTimeout {
					expired = append(expired, req)
					delete(g.inFlight, k)
				}
			}
			g.trackMu.Unlock()

			for _, req := range expired {
				errMsg := mcp.NewErrorMessage(req.ClientID, mcp.CodeInternalError, "upstream request timed out")
				_ = req.ClientSession.Send(errMsg)
			}
		}
	}
}
