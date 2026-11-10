package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// UpstreamTransport abstracts communication with an upstream MCP process.
type UpstreamTransport interface {
	Send(msg *mcp.Message) error
	Receive() (*mcp.Message, error)
	Close() error
}

// UpstreamMessageHandler receives messages dispatched from upstream reader loops.
type UpstreamMessageHandler interface {
	HandleUpstreamResponse(upstreamID string, msg *mcp.Message)
	HandleUpstreamNotification(upstreamID string, msg *mcp.Message)
	HandleUpstreamCrash(upstreamID string, err error)
}

var (
	// ErrUpstreamAlreadyExists is returned when attempting to register an upstream with duplicate ID.
	ErrUpstreamAlreadyExists = errors.New("upstream server already registered")
	// ErrUpstreamNotFound is returned when operating on an unknown upstream server ID.
	ErrUpstreamNotFound = errors.New("upstream server not found")
	// ErrToolCollision is returned when multiple upstreams expose identical tools under NamespaceModeNone.
	ErrToolCollision = errors.New("tool name collision across upstreams")
	// ErrToolNotFound is returned when a requested tool does not exist in the routing table.
	ErrToolNotFound = errors.New("tool not found in routing table")
	// ErrInvalidNamespaceMode is returned for unsupported namespace mode strings.
	ErrInvalidNamespaceMode = errors.New("invalid namespace mode")
)

// NamespaceMode defines how tools from different upstream servers are namespaced.
type NamespaceMode string

const (
	// NamespaceModeAuto uses un-prefixed names if 1 upstream exists, or always-prefix if >= 2 upstreams.
	NamespaceModeAuto NamespaceMode = "auto"

	// NamespaceModePrefixAlways prefixes all tools: "<prefix>__<toolName>".
	NamespaceModePrefixAlways NamespaceMode = "always"

	// NamespaceModePrefixCollision prefixes tools only when name collisions occur.
	NamespaceModePrefixCollision NamespaceMode = "on_collision"

	// NamespaceModeNone leaves all tool names untouched; returns ErrToolCollision if duplicate names exist.
	NamespaceModeNone NamespaceMode = "none"
)

// UpstreamConfig defines registration settings for an upstream MCP server.
type UpstreamConfig struct {
	ID          string            `json:"id" yaml:"id"`                   // Unique identifier (e.g. "github")
	Prefix      string            `json:"prefix" yaml:"prefix"`           // Custom prefix (defaults to ID)
	Transport   UpstreamTransport `json:"-" yaml:"-"`                     // Bidirectional transport
	Description string            `json:"description" yaml:"description"` // Optional description
	Disabled    bool              `json:"disabled" yaml:"disabled"`
}

// ToolRoute stores the mapping between the exposed tool name and upstream target.
type ToolRoute struct {
	ExposedName  string          // Name exposed to LLM (e.g. "github__create_issue")
	OriginalName string          // Original name on upstream (e.g. "create_issue")
	UpstreamID   string          // Upstream server ID (e.g. "github")
	Target       *UpstreamTarget // Target upstream server instance
	Tool         mcp.Tool        // Cached full tool definition with ExposedName
}

// UpstreamTarget represents an active upstream MCP server managed by the multiplexer.
type UpstreamTarget struct {
	mu          sync.RWMutex
	id          string
	prefix      string
	transport   UpstreamTransport
	info        *mcp.InitializeResult
	tools       map[string]mcp.Tool // originalName -> Tool
	closed      atomic.Bool
	loopStarted atomic.Bool
	lastSync    time.Time
}

// ID returns the upstream identifier.
func (u *UpstreamTarget) ID() string {
	return u.id
}

// Prefix returns the namespace prefix (or ID if prefix is empty).
func (u *UpstreamTarget) Prefix() string {
	if u.prefix != "" {
		return u.prefix
	}
	return u.id
}

// Transport returns the underlying transport.
func (u *UpstreamTarget) Transport() UpstreamTransport {
	return u.transport
}

// Multiplexer coordinates multi-server upstream registration, routing, and message reading.
type Multiplexer struct {
	mu             sync.RWMutex
	mode           NamespaceMode
	upstreams      map[string]*UpstreamTarget
	routes         map[string]ToolRoute // exposedName -> ToolRoute
	handler        UpstreamMessageHandler
	onToolsChanged func()
	logger         *slog.Logger
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
}

// NewMultiplexer creates an initialized Multiplexer instance.
func NewMultiplexer(mode NamespaceMode, logger *slog.Logger) *Multiplexer {
	if mode == "" {
		mode = NamespaceModeAuto
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Multiplexer{
		mode:      mode,
		upstreams: make(map[string]*UpstreamTarget),
		routes:    make(map[string]ToolRoute),
		logger:    logger,
		ctx:       ctx,
		cancel:    cancel,
	}
}

// SetMessageHandler configures the upstream message callback handler.
func (m *Multiplexer) SetMessageHandler(handler UpstreamMessageHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handler = handler
	if handler == nil {
		return
	}
	for _, target := range m.upstreams {
		if target.transport != nil && !target.closed.Load() && target.loopStarted.CompareAndSwap(false, true) {
			m.wg.Add(1)
			go m.runUpstreamLoop(target)
		}
	}
}

// SetOnToolsChanged sets a callback triggered when available tools change.
func (m *Multiplexer) SetOnToolsChanged(cb func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onToolsChanged = cb
}

// RegisterUpstream registers and connects an upstream MCP server.
func (m *Multiplexer) RegisterUpstream(ctx context.Context, cfg UpstreamConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cfg.ID == "" {
		return errors.New("upstream ID cannot be empty")
	}
	if strings.Contains(cfg.ID, "__") {
		return errors.New("upstream ID cannot contain '__'")
	}
	if _, exists := m.upstreams[cfg.ID]; exists {
		return fmt.Errorf("%w: %q", ErrUpstreamAlreadyExists, cfg.ID)
	}

	target := &UpstreamTarget{
		id:        cfg.ID,
		prefix:    cfg.Prefix,
		transport: cfg.Transport,
		tools:     make(map[string]mcp.Tool),
	}
	m.upstreams[cfg.ID] = target

	// If transport is active and handler is registered, launch dedicated background reader loop
	if target.transport != nil && m.handler != nil && target.loopStarted.CompareAndSwap(false, true) {
		m.wg.Add(1)
		go m.runUpstreamLoop(target)
	}

	return nil
}

// UnregisterUpstream closes and removes an upstream server.
func (m *Multiplexer) UnregisterUpstream(ctx context.Context, id string) error {
	m.mu.Lock()
	target, exists := m.upstreams[id]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUpstreamNotFound, id)
	}

	delete(m.upstreams, id)
	m.rebuildRoutesUnderLock()
	cb := m.onToolsChanged
	m.mu.Unlock()

	if target.transport != nil {
		target.closed.Store(true)
		_ = target.transport.Close()
	}

	if cb != nil {
		cb()
	}
	return nil
}

// SyncUpstreamTools updates an upstream's toolset and regenerates the routing table.
func (m *Multiplexer) SyncUpstreamTools(ctx context.Context, id string, tools []mcp.Tool) {
	m.mu.Lock()
	target, exists := m.upstreams[id]
	if !exists {
		m.mu.Unlock()
		return
	}

	target.mu.Lock()
	target.tools = make(map[string]mcp.Tool, len(tools))
	for _, tool := range tools {
		target.tools[tool.Name] = tool
	}
	target.lastSync = time.Now()
	target.mu.Unlock()

	m.rebuildRoutesUnderLock()
	cb := m.onToolsChanged
	m.mu.Unlock()

	if cb != nil {
		cb()
	}
}

// rebuildRoutesUnderLock regenerates the routes table according to active NamespaceMode.
func (m *Multiplexer) rebuildRoutesUnderLock() {
	newRoutes := make(map[string]ToolRoute)
	effectiveMode := m.mode
	if effectiveMode == NamespaceModeAuto {
		if len(m.upstreams) > 1 {
			effectiveMode = NamespaceModePrefixAlways
		} else {
			effectiveMode = NamespaceModeNone
		}
	}

	// Detect collisions if in on_collision or none mode
	toolOccurrences := make(map[string]int)
	if effectiveMode == NamespaceModePrefixCollision || effectiveMode == NamespaceModeNone {
		for _, target := range m.upstreams {
			target.mu.RLock()
			for toolName := range target.tools {
				toolOccurrences[toolName]++
			}
			target.mu.RUnlock()
		}
	}

	for _, target := range m.upstreams {
		target.mu.RLock()
		prefix := target.Prefix()
		for originalName, tool := range target.tools {
			var exposedName string
			switch effectiveMode {
			case NamespaceModePrefixAlways:
				exposedName = fmt.Sprintf("%s__%s", prefix, originalName)
			case NamespaceModePrefixCollision:
				if toolOccurrences[originalName] > 1 {
					exposedName = fmt.Sprintf("%s__%s", prefix, originalName)
				} else {
					exposedName = originalName
				}
			case NamespaceModeNone:
				fallthrough
			default:
				exposedName = originalName
			}

			// Clone tool and update Name to exposedName
			exposedTool := tool
			exposedTool.Name = exposedName

			newRoutes[exposedName] = ToolRoute{
				ExposedName:  exposedName,
				OriginalName: originalName,
				UpstreamID:   target.id,
				Target:       target,
				Tool:         exposedTool,
			}
		}
		target.mu.RUnlock()
	}

	m.routes = newRoutes
}

// ResolveRoute resolves an exposed tool name to its target upstream and original tool.
func (m *Multiplexer) ResolveRoute(exposedName string) (ToolRoute, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 1. Direct match
	if route, ok := m.routes[exposedName]; ok {
		return route, true
	}

	// 2. Resilience Fallback: If caller omitted prefix, find unique match by OriginalName
	var matchedRoute ToolRoute
	matches := 0
	for _, route := range m.routes {
		if route.OriginalName == exposedName {
			matchedRoute = route
			matches++
		}
	}
	if matches == 1 {
		return matchedRoute, true
	}

	return ToolRoute{}, false
}

// ListAllTools returns all active tools with their exposed names in deterministic sorted order.
func (m *Multiplexer) ListAllTools() []mcp.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tools := make([]mcp.Tool, 0, len(m.routes))
	for _, route := range m.routes {
		tools = append(tools, route.Tool)
	}
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})
	return tools
}

// GetTool satisfies the CatalogProvider interface.
func (m *Multiplexer) GetTool(name string) (mcp.Tool, bool) {
	route, ok := m.ResolveRoute(name)
	if !ok {
		return mcp.Tool{}, false
	}
	return route.Tool, true
}

// GetAllTools satisfies the CatalogProvider interface.
func (m *Multiplexer) GetAllTools() []mcp.Tool {
	return m.ListAllTools()
}

// runUpstreamLoop reads messages continuously from a single upstream transport.
func (m *Multiplexer) runUpstreamLoop(target *UpstreamTarget) {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		if target.transport == nil || target.closed.Load() {
			return
		}

		msg, err := target.transport.Receive()
		if err != nil {
			if !target.closed.Load() {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					m.logger.Info("upstream transport closed", "upstream", target.id)
				} else {
					m.logger.Error("upstream transport error", "upstream", target.id, "err", err)
				}
				m.mu.RLock()
				handler := m.handler
				m.mu.RUnlock()
				if handler != nil {
					handler.HandleUpstreamCrash(target.id, err)
				}
			}
			return
		}

		if msg == nil {
			continue
		}

		m.mu.RLock()
		handler := m.handler
		m.mu.RUnlock()

		if handler != nil {
			if msg.IsResponse() {
				handler.HandleUpstreamResponse(target.id, msg)
			} else if msg.IsNotification() {
				if msg.Method == mcp.MethodNotificationsToolsListChanged {
					m.logger.Info("upstream emitted tools/list_changed", "upstream", target.id)
				}
				handler.HandleUpstreamNotification(target.id, msg)
			}
		}
	}
}

// Close gracefully shuts down all upstreams and reader loops.
func (m *Multiplexer) Close() error {
	m.cancel()
	m.mu.Lock()
	for _, target := range m.upstreams {
		target.closed.Store(true)
		if target.transport != nil {
			_ = target.transport.Close()
		}
	}
	m.mu.Unlock()
	m.wg.Wait()
	return nil
}
