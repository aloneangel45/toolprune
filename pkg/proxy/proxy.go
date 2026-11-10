package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// UpstreamTransport abstracts communication with the upstream MCP server (stdio child process or remote server).
type UpstreamTransport interface {
	Send(msg *mcp.Message) error
	Receive() (*mcp.Message, error)
	Close() error
}

// PruneEngine represents the pluggable schema pruner interface (implemented by pkg/pruner in M2).
type PruneEngine interface {
	PruneTools(tools []mcp.Tool) ([]mcp.Tool, PruneMetrics, error)
}

// PruneMetrics captures token optimization metrics for observability.
type PruneMetrics struct {
	RawTokens    int
	PrunedTokens int
	SavingsRatio float64
}

// LocalCatalogInterceptor allows dynamic catalog meta-tools (Tier 4) to be served in wrapper mode.
type LocalCatalogInterceptor interface {
	SyncUpstreamTools(tools []mcp.Tool) ([]mcp.Tool, error)
	InterceptCall(toolName string, rawArgs json.RawMessage) (handled bool, forwardedName string, forwardedArgs json.RawMessage, localResult *mcp.CallToolResult)
}

// SessionStatsSnapshot reports cumulative token savings and RPC interception counts for a session.
type SessionStatsSnapshot struct {
	ListsIntercepted  uint64
	CallsIntercepted  uint64
	PreBindRejections uint64
	CacheHits         uint64
	RawTokensTotal    uint64
	PrunedTokensTotal uint64
}

// TokensSaved returns cumulative tokens saved across all tools/list responses.
func (s SessionStatsSnapshot) TokensSaved() uint64 {
	if s.RawTokensTotal > s.PrunedTokensTotal {
		return s.RawTokensTotal - s.PrunedTokensTotal
	}
	return 0
}

// SavingsPercent returns cumulative percentage reduction across all tools/list responses.
func (s SessionStatsSnapshot) SavingsPercent() float64 {
	if s.RawTokensTotal == 0 {
		return 0
	}
	return float64(s.TokensSaved()) * 100.0 / float64(s.RawTokensTotal)
}

// ProxyConfig holds operational settings for the proxy engine.
type ProxyConfig struct {
	PruneMode              string                  // "none", "lossless", "compact", "aggressive", "dynamic"
	MaxSchemaTokens        int                     // RFC #1 progressive token budget cap (0 = disabled)
	PinnedTools            []string                // Tools immune to Pass 5 stub projection
	ToolFilterFunc         func(name string) bool  // Optional profile allowlist/blocklist predicate
	DynamicCatalog         LocalCatalogInterceptor // Optional Tier 4 dynamic catalog handler for wrapper mode
	RequestTimeout         time.Duration           // Default: 60s
	TimeoutSweeperInterval time.Duration           // Interval for sweeper ticker (default: min(RequestTimeout/2, 5s), min 50ms)
	InjectProxyInfo        bool                    // Inject ToolPrune identification into initialize response
}

// DefaultProxyConfig returns standard production defaults.
func DefaultProxyConfig() ProxyConfig {
	return ProxyConfig{
		PruneMode:              "aggressive",
		RequestTimeout:         60 * time.Second,
		TimeoutSweeperInterval: 0, // 0 triggers automatic dynamic calibration in NewProxy
		InjectProxyInfo:        true,
	}
}

// InFlightRequest tracks a pending request originating from a client.
type InFlightRequest struct {
	ClientID      *mcp.RequestID // Original client request ID
	ClientSession ClientSession  // Target client session
	Method        string         // RPC Method name ("tools/list", "tools/call", etc.)
	StartTime     time.Time
}

// InFlightTracker provides thread-safe correlation between client IDs and upstream IDs.
type InFlightTracker struct {
	mu       sync.Mutex
	requests map[string]*InFlightRequest
	counter  atomic.Uint64
}

// NewInFlightTracker creates a new InFlightTracker.
func NewInFlightTracker() *InFlightTracker {
	return &InFlightTracker{
		requests: make(map[string]*InFlightRequest),
	}
}

// Track stores the in-flight request and generates a unique upstream request message.
func (t *InFlightTracker) Track(session ClientSession, msg *mcp.Message) *mcp.Message {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Generate monotonic synthetic upstream ID to prevent collisions across multiple clients
	upID := t.counter.Add(1)
	upIDKey := fmt.Sprintf("%d", upID)
	syntheticID := mcp.NewIntID(int64(upID))

	t.requests[upIDKey] = &InFlightRequest{
		ClientID:      msg.ID,
		ClientSession: session,
		Method:        msg.Method,
		StartTime:     time.Now(),
	}

	// Create cloned message with upstream synthetic ID
	upstreamMsg := *msg
	upstreamMsg.ID = &syntheticID
	return &upstreamMsg
}

// Pop removes and returns the correlated in-flight request for a given upstream ID.
func (t *InFlightTracker) Pop(upstreamID *mcp.RequestID) (*InFlightRequest, bool) {
	if upstreamID == nil {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	key := upstreamID.String()
	req, ok := t.requests[key]
	if ok {
		delete(t.requests, key)
	}
	return req, ok
}

// DrainAll removes and returns all pending requests (used on upstream crash).
func (t *InFlightTracker) DrainAll() []*InFlightRequest {
	t.mu.Lock()
	defer t.mu.Unlock()

	result := make([]*InFlightRequest, 0, len(t.requests))
	for k, req := range t.requests {
		result = append(result, req)
		delete(t.requests, k)
	}
	return result
}

// CleanupExpired sweeps requests that have exceeded their timeout.
func (t *InFlightTracker) CleanupExpired(timeout time.Duration) []*InFlightRequest {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	var expired []*InFlightRequest
	for k, req := range t.requests {
		if now.Sub(req.StartTime) > timeout {
			expired = append(expired, req)
			delete(t.requests, k)
		}
	}
	return expired
}

// Proxy coordinates bidirectional message exchange and schema pruning.
type Proxy struct {
	config       ProxyConfig
	upstream     UpstreamTransport
	tracker      *InFlightTracker
	prunerMu     sync.RWMutex
	pruner       PruneEngine // May be nil for Milestone 1 passthrough
	streamFilter *StreamFilter
	clientsMu    sync.RWMutex
	clients      map[string]ClientSession
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup

	listsIntercepted  atomic.Uint64
	callsIntercepted  atomic.Uint64
	preBindRejections atomic.Uint64
	cacheHits         atomic.Uint64
	rawTokensTotal    atomic.Uint64
	prunedTokensTotal atomic.Uint64
}

// NewProxy instantiates a new Proxy engine.
func NewProxy(cfg ProxyConfig, upstream UpstreamTransport, pruner PruneEngine) *Proxy {
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	if cfg.TimeoutSweeperInterval <= 0 {
		// Dynamically calibrate sweeper ticker:
		// Half of RequestTimeout, capped at 5s, floor at 50ms.
		interval := cfg.RequestTimeout / 2
		if interval > 5*time.Second {
			interval = 5 * time.Second
		}
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
		cfg.TimeoutSweeperInterval = interval
	}
	ctx, cancel := context.WithCancel(context.Background())
	sf := NewStreamFilter(StreamFilterConfig{
		PruneMode:       cfg.PruneMode,
		MaxSchemaTokens: cfg.MaxSchemaTokens,
		PinnedTools:     cfg.PinnedTools,
	})
	return &Proxy{
		config:       cfg,
		upstream:     upstream,
		tracker:      NewInFlightTracker(),
		pruner:       pruner,
		streamFilter: sf,
		clients:      make(map[string]ClientSession),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// SetPruner allows attaching or changing a pruner engine post-initialization safely across goroutines.
func (p *Proxy) SetPruner(pruner PruneEngine) {
	p.prunerMu.Lock()
	p.pruner = pruner
	p.prunerMu.Unlock()
	if p.streamFilter != nil {
		p.streamFilter.InvalidateCache()
	}
}

func (p *Proxy) getPruner() PruneEngine {
	p.prunerMu.RLock()
	defer p.prunerMu.RUnlock()
	return p.pruner
}

// StreamFilter returns the underlying adaptive stream filter.
func (p *Proxy) StreamFilter() *StreamFilter {
	return p.streamFilter
}

// Stats returns a thread-safe snapshot of session pruning and call statistics.
func (p *Proxy) Stats() SessionStatsSnapshot {
	return SessionStatsSnapshot{
		ListsIntercepted:  p.listsIntercepted.Load(),
		CallsIntercepted:  p.callsIntercepted.Load(),
		PreBindRejections: p.preBindRejections.Load(),
		CacheHits:         p.cacheHits.Load(),
		RawTokensTotal:    p.rawTokensTotal.Load(),
		PrunedTokensTotal: p.prunedTokensTotal.Load(),
	}
}

// RegisterClient adds an active client session.
func (p *Proxy) RegisterClient(session ClientSession) {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	p.clients[session.ID()] = session
}

// UnregisterClient removes a client session.
func (p *Proxy) UnregisterClient(sessionID string) {
	p.clientsMu.Lock()
	defer p.clientsMu.Unlock()
	delete(p.clients, sessionID)
}

// Start launches the background upstream reader and timeout supervisor.
func (p *Proxy) Start() {
	p.wg.Add(2)
	go p.runUpstreamLoop()
	go p.runTimeoutSweeper()
}

// Shutdown cleanly stops the proxy and notifies upstream and clients.
func (p *Proxy) Shutdown() {
	p.cancel()
	if p.upstream != nil {
		_ = p.upstream.Close()
	}
	p.wg.Wait()
}

// HandleClientMessage processes any message arriving from a client (stdio or SSE POST).
func (p *Proxy) HandleClientMessage(session ClientSession, msg *mcp.Message) error {
	if msg == nil {
		return errors.New("nil message received")
	}

	// 1. Client Notification (ID is nil, Method is present)
	if msg.IsNotification() {
		return p.handleClientNotification(session, msg)
	}

	// 2. Client Request (ID is non-nil, Method is present)
	if msg.IsRequest() {
		return p.handleClientRequest(session, msg)
	}

	// 3. Client Response (ID is non-nil, Method is empty)
	if msg.IsResponse() {
		return p.handleClientResponse(session, msg)
	}

	return fmt.Errorf("invalid JSON-RPC 2.0 message: missing method or id")
}

// handleClientRequest assigns a tracking entry and forwards to upstream.
func (p *Proxy) handleClientRequest(session ClientSession, msg *mcp.Message) error {
	if msg.Method == mcp.MethodToolsCall {
		p.callsIntercepted.Add(1)
		var callParams mcp.CallToolRequestParams
		if len(msg.Params) > 0 && json.Unmarshal(msg.Params, &callParams) == nil && callParams.Name != "" {
			// Check profile filter before forwarding tools/call if configured
			if p.config.ToolFilterFunc != nil && !strings.HasPrefix(callParams.Name, "toolprune_") {
				if !p.config.ToolFilterFunc(callParams.Name) {
					res := mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q is blocked by the active ToolPrune profile", callParams.Name))
					resp, _ := mcp.NewResponse(*msg.ID, res)
					return session.Send(resp)
				}
			}

			// Optional Tier 4 dynamic catalog interception in wrapper mode
			if p.config.DynamicCatalog != nil {
				handled, fwdName, fwdArgs, localRes := p.config.DynamicCatalog.InterceptCall(callParams.Name, callParams.Arguments)
				if handled && localRes != nil {
					if localRes.IsError {
						p.preBindRejections.Add(1)
					}
					resp, err := mcp.NewResponse(*msg.ID, *localRes)
					if err != nil {
						return err
					}
					return session.Send(resp)
				}
				if fwdName != "" {
					callParams.Name = fwdName
					callParams.Arguments = fwdArgs
					if rewritten, err := json.Marshal(callParams); err == nil {
						cloned := *msg
						cloned.Params = rewritten
						msg = &cloned
					}
				}
			}

			// Adaptive stream filter: track call context + pre-validate compacted/stubbed calls
			if p.streamFilter != nil {
				if errRes := p.streamFilter.InterceptToolCall(callParams.Name, callParams.Arguments); errRes != nil {
					p.preBindRejections.Add(1)
					resp, err := mcp.NewResponse(*msg.ID, *errRes)
					if err != nil {
						return err
					}
					return session.Send(resp)
				}
			}
		}
	}

	upstreamMsg := p.tracker.Track(session, msg)

	switch msg.Method {
	case mcp.MethodInitialize:
		slog.Debug("proxy routing initialize request to upstream", "id", upstreamMsg.ID)
	case mcp.MethodToolsList:
		slog.Debug("proxy routing tools/list request to upstream", "id", upstreamMsg.ID)
	case mcp.MethodToolsCall:
		slog.Debug("proxy pass-through tools/call to upstream", "id", upstreamMsg.ID)
	case mcp.MethodPing:
		slog.Debug("proxy pass-through ping to upstream", "id", upstreamMsg.ID)
	default:
		// Transparent pass-through for prompts/*, resources/*, logging/*, custom methods
		slog.Debug("proxy pass-through generic method", "method", msg.Method, "id", upstreamMsg.ID)
	}

	if p.upstream == nil {
		p.tracker.Pop(upstreamMsg.ID)
		errMsg := mcp.NewErrorMessage(
			msg.ID,
			mcp.CodeInternalError,
			"upstream transport not configured or unavailable",
		)
		_ = session.Send(errMsg)
		return errors.New("upstream transport is nil")
	}

	if err := p.upstream.Send(upstreamMsg); err != nil {
		// Clean up tracker immediately so request does not leak
		p.tracker.Pop(upstreamMsg.ID)

		// Immediately return JSON-RPC -32603 InternalError to client
		errMsg := mcp.NewErrorMessage(
			msg.ID,
			mcp.CodeInternalError,
			fmt.Sprintf("failed to send request to upstream MCP server: %v", err),
		)
		_ = session.Send(errMsg)
		return err
	}

	return nil
}

// handleClientNotification forwards notifications to upstream.
func (p *Proxy) handleClientNotification(session ClientSession, msg *mcp.Message) error {
	switch msg.Method {
	case "notifications/cancelled":
		slog.Info("client requested request cancellation", "params", string(msg.Params))
		return p.upstream.Send(msg)
	case mcp.MethodNotificationsInitialized:
		slog.Info("client sent notifications/initialized, handshake complete")
		return p.upstream.Send(msg)
	default:
		return p.upstream.Send(msg)
	}
}

// handleClientResponse forwards client responses (e.g. sampling responses) to upstream.
func (p *Proxy) handleClientResponse(session ClientSession, msg *mcp.Message) error {
	return p.upstream.Send(msg)
}

// runUpstreamLoop continuously reads responses and notifications from upstream.
func (p *Proxy) runUpstreamLoop() {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		msg, err := p.upstream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				slog.Info("upstream MCP process disconnected")
			} else {
				slog.Error("error reading from upstream MCP server", "err", err)
			}
			p.handleUpstreamCrash(err)
			return
		}

		if msg == nil {
			continue
		}

		if msg.IsResponse() {
			p.handleUpstreamResponse(msg)
		} else if msg.IsNotification() {
			p.handleUpstreamNotification(msg)
		} else if msg.IsRequest() {
			p.handleUpstreamRequest(msg)
		}
	}
}

// handleUpstreamResponse correlates the response, applies pruning, and forwards to client.
func (p *Proxy) handleUpstreamResponse(msg *mcp.Message) {
	req, ok := p.tracker.Pop(msg.ID)
	if !ok {
		slog.Warn("received response for unknown or timed out request", "id", msg.ID)
		return
	}

	// Restore original client ID
	msg.ID = req.ClientID

	// If upstream returned an error, preserve it and deliver immediately to client
	if msg.Error != nil {
		slog.Warn("upstream returned protocol error", "code", msg.Error.Code, "msg", msg.Error.Message)
		_ = req.ClientSession.Send(msg)
		return
	}

	// Interception Gate
	switch req.Method {
	case mcp.MethodToolsList:
		p.interceptToolsListResponse(req, msg)
	case mcp.MethodInitialize:
		p.interceptInitializeResponse(req, msg)
	default:
		// Pass-through for tools/call, ping, prompts/*, resources/*
		_ = req.ClientSession.Send(msg)
	}
}

// interceptToolsListResponse applies profile filtering, schema pruning, and RFC #1 token budgeting to tools/list results.
func (p *Proxy) interceptToolsListResponse(req *InFlightRequest, msg *mcp.Message) {
	p.listsIntercepted.Add(1)
	prunerEngine := p.getPruner()

	needsInterception := (prunerEngine != nil && (p.config.PruneMode != "none" || p.config.MaxSchemaTokens > 0)) ||
		p.config.ToolFilterFunc != nil ||
		p.config.DynamicCatalog != nil

	if needsInterception {
		var listResult mcp.ListToolsResult
		if err := json.Unmarshal(msg.Result, &listResult); err != nil {
			slog.Error("failed to unmarshal tools/list payload for pruning, falling back to unpruned", "err", err)
			_ = req.ClientSession.Send(msg)
			return
		}

		tools := listResult.Tools
		if p.config.ToolFilterFunc != nil {
			filtered := make([]mcp.Tool, 0, len(tools))
			for _, t := range tools {
				if p.config.ToolFilterFunc(t.Name) {
					filtered = append(filtered, t)
				}
			}
			tools = filtered
		}

		// Tier 4 dynamic catalog projection in wrapper mode if enabled
		if p.config.DynamicCatalog != nil {
			projected, err := p.config.DynamicCatalog.SyncUpstreamTools(tools)
			if err != nil {
				slog.Error("dynamic catalog sync failed, falling back to standard pruning", "err", err)
			} else {
				// Register raw tools in streamFilter for pre-binding validation and context tracking
				if p.streamFilter != nil {
					p.streamFilter.RegisterRawTools(tools, true, nil)
					if p.config.MaxSchemaTokens > 0 && prunerEngine != nil {
						if budgeted, bMetrics, bErr := p.streamFilter.FilterToolsList(projected, prunerEngine); bErr == nil {
							projected = budgeted
							p.rawTokensTotal.Add(uint64(bMetrics.RawTokens))
							p.prunedTokensTotal.Add(uint64(bMetrics.PrunedTokens))
						}
					}
				}
				listResult.Tools = projected
				if newResult, mErr := json.Marshal(listResult); mErr == nil {
					msg.Result = newResult
				}
				_ = req.ClientSession.Send(msg)
				return
			}
		}

		if prunerEngine != nil && (p.config.PruneMode != "none" || p.config.MaxSchemaTokens > 0) {
			var prunedTools []mcp.Tool
			var bMetrics BudgetPruneMetrics
			var err error

			if p.streamFilter != nil {
				prunedTools, bMetrics, err = p.streamFilter.FilterToolsList(tools, prunerEngine)
			} else {
				var pm PruneMetrics
				prunedTools, pm, err = prunerEngine.PruneTools(tools)
				bMetrics = BudgetPruneMetrics{PruneMetrics: pm}
			}
			if err != nil {
				slog.Error("pruning failed, falling back to raw schemas", "err", err)
				_ = req.ClientSession.Send(msg)
				return
			}

			if bMetrics.CacheHit {
				p.cacheHits.Add(1)
			}
			if bMetrics.RawTokens > 0 {
				p.rawTokensTotal.Add(uint64(bMetrics.RawTokens))
			}
			if bMetrics.PrunedTokens > 0 {
				p.prunedTokensTotal.Add(uint64(bMetrics.PrunedTokens))
			}

			listResult.Tools = prunedTools
		} else {
			listResult.Tools = tools
		}

		if listResult.Tools == nil {
			listResult.Tools = []mcp.Tool{}
		}

		newResult, err := json.Marshal(listResult)
		if err != nil {
			slog.Error("failed to re-marshal pruned tools result", "err", err)
			_ = req.ClientSession.Send(msg)
			return
		}

		msg.Result = newResult
		slog.Info("tools/list pruned successfully",
			"toolCount", len(listResult.Tools),
		)
	}

	_ = req.ClientSession.Send(msg)
}

// interceptInitializeResponse optionally decorates serverInfo and instructions.
func (p *Proxy) interceptInitializeResponse(req *InFlightRequest, msg *mcp.Message) {
	if p.config.InjectProxyInfo {
		var initResult mcp.InitializeResult
		if err := json.Unmarshal(msg.Result, &initResult); err == nil {
			initResult.ServerInfo.Name = fmt.Sprintf("%s (pruned by ToolPrune)", initResult.ServerInfo.Name)
			if initResult.Instructions == "" {
				initResult.Instructions = "ToolPrune optimized proxy: tool schemas have been pruned to save context tokens."
			} else {
				initResult.Instructions += " [ToolPrune: tool schemas pruned for token efficiency]"
			}
			if newResult, err := json.Marshal(initResult); err == nil {
				msg.Result = newResult
			}
		}
	}
	_ = req.ClientSession.Send(msg)
}

// handleUpstreamNotification broadcasts notifications to connected clients.
func (p *Proxy) handleUpstreamNotification(msg *mcp.Message) {
	if msg.Method == mcp.MethodNotificationsToolsListChanged && p.streamFilter != nil {
		p.streamFilter.InvalidateCache()
	}
	p.clientsMu.RLock()
	defer p.clientsMu.RUnlock()

	for _, client := range p.clients {
		_ = client.Send(msg)
	}
}

// handleUpstreamRequest routes reverse requests (e.g. sampling/createMessage) to clients.
func (p *Proxy) handleUpstreamRequest(msg *mcp.Message) {
	// For single-client routing, forward to first active client
	p.clientsMu.RLock()
	defer p.clientsMu.RUnlock()

	for _, client := range p.clients {
		_ = client.Send(msg)
		return
	}
}

// handleUpstreamCrash terminates all in-flight requests with a JSON-RPC InternalError.
func (p *Proxy) handleUpstreamCrash(err error) {
	inFlights := p.tracker.DrainAll()
	slog.Warn("draining in-flight requests after upstream crash", "count", len(inFlights))

	for _, req := range inFlights {
		errMsg := mcp.NewErrorMessage(
			req.ClientID,
			mcp.CodeInternalError,
			fmt.Sprintf("upstream MCP server terminated unexpectedly: %v", err),
		)
		_ = req.ClientSession.Send(errMsg)
	}
}

// runTimeoutSweeper periodically aborts requests exceeding RequestTimeout.
func (p *Proxy) runTimeoutSweeper() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.config.TimeoutSweeperInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			expired := p.tracker.CleanupExpired(p.config.RequestTimeout)
			for _, req := range expired {
				slog.Warn("in-flight request timed out waiting for upstream", "method", req.Method, "id", req.ClientID)
				errMsg := mcp.NewErrorMessage(
					req.ClientID,
					mcp.CodeInternalError,
					"upstream request timed out",
				)
				_ = req.ClientSession.Send(errMsg)
			}
		}
	}
}
