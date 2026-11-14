package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// RouterConfig holds settings for the Unified Router Engine.
type RouterConfig struct {
	NamespaceMode   NamespaceMode `json:"namespaceMode" yaml:"namespace_mode"`
	PruneMode       string        `json:"pruneMode" yaml:"prune_mode"`                // "none", "lossless", "compact", "aggressive", "dynamic"
	MaxSchemaTokens int           `json:"maxSchemaTokens" yaml:"max_schema_tokens"`   // RFC #1 progressive token budget cap (0 = unlimited)
	ActiveProfile   string        `json:"activeProfile" yaml:"active_profile"`        // e.g. "read_only", "dev", "full", "minimal"
	MaxActiveTools  int           `json:"maxActiveTools" yaml:"max_active_tools"`     // For active windowing (default: 15)
	PinnedTools     []string      `json:"pinnedTools" yaml:"pinned_tools"`            // Tools immune to Pass 5 projection / window eviction
	EnableCatalog   bool          `json:"enableCatalog" yaml:"enable_catalog"`        // Tier 4 dynamic catalog
	Catalog         CatalogConfig `json:"catalog,omitempty" yaml:"catalog,omitempty"` // Optional Tier 4 catalog overrides
}

// DefaultRouterConfig returns standard production defaults.
func DefaultRouterConfig() RouterConfig {
	return RouterConfig{
		NamespaceMode:  NamespaceModeAuto,
		PruneMode:      "aggressive",
		ActiveProfile:  "full",
		MaxActiveTools: 15,
		EnableCatalog:  false,
	}
}

// FilterEngine abstracts profile and regex filtering (implemented by pkg/router.ToolFilter).
type FilterEngine interface {
	Apply(tools []mcp.Tool) []mcp.Tool
	IsAllowed(toolName string) bool
	SetProfile(profile string) error
}

// WindowEngine abstracts sliding LRU window management (implemented by pkg/router.ToolWindow).
type WindowEngine interface {
	ActiveTools() []mcp.Tool
	IsActive(toolName string) bool
	RecordUsage(toolName string)
	Promote(toolName string) (promoted bool, evicted string, err error)
}

// Router orchestrates multiplexing, pruning, filtering, windowing, cataloging, and adaptive stream filtering.
type Router struct {
	mu           sync.RWMutex
	cfg          RouterConfig
	multiplexer  *Multiplexer
	pruner       *pruner.Engine
	filter       FilterEngine
	window       WindowEngine
	schemaStore  *SchemaStore
	schemaDirty  atomic.Bool
	streamFilter *proxy.StreamFilter
	logger       *slog.Logger
	notifier     func() // Broadcast notifications/tools/list_changed to clients

	listsIntercepted  atomic.Uint64
	callsIntercepted  atomic.Uint64
	preBindRejections atomic.Uint64
	cacheHits         atomic.Uint64
	rawTokensTotal    atomic.Uint64
	prunedTokensTotal atomic.Uint64
}

// NewRouter constructs a new Router instance.
func NewRouter(cfg RouterConfig, prunerEngine *pruner.Engine, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.PruneMode == "dynamic" {
		cfg.EnableCatalog = true
	}
	pinned := append([]string(nil), cfg.PinnedTools...)
	if len(pinned) == 0 {
		pinned = []string{MetaToolSearch, MetaToolGetSchema, MetaToolActivate, MetaToolInvoke}
	} else {
		for _, mt := range []string{MetaToolSearch, MetaToolGetSchema, MetaToolActivate, MetaToolInvoke} {
			if !containsString(pinned, mt) {
				pinned = append(pinned, mt)
			}
		}
	}

	mux := NewMultiplexer(cfg.NamespaceMode, logger)
	sf := proxy.NewStreamFilter(proxy.StreamFilterConfig{
		PruneMode:       cfg.PruneMode,
		MaxSchemaTokens: cfg.MaxSchemaTokens,
		PinnedTools:     pinned,
	})

	r := &Router{
		cfg:          cfg,
		multiplexer:  mux,
		pruner:       prunerEngine,
		schemaStore:  NewSchemaStore(),
		streamFilter: sf,
		logger:       logger,
	}
	r.schemaDirty.Store(true)
	mux.SetOnToolsChanged(r.handleMultiplexerChanged)
	return r
}

func (r *Router) handleMultiplexerChanged() {
	r.schemaDirty.Store(true)
	if r.streamFilter != nil {
		r.streamFilter.InvalidateCache()
	}
	r.mu.RLock()
	notifier := r.notifier
	r.mu.RUnlock()
	if notifier != nil {
		notifier()
	}
}

func (r *Router) effectiveCatalogConfig() CatalogConfig {
	cat := r.cfg.Catalog
	if cat.MaxSummaryLength <= 0 && cat.WindowSize <= 0 && cat.Mode == "" {
		cat = DefaultCatalogConfig()
	}
	if cat.WindowSize <= 0 {
		cat.WindowSize = r.cfg.MaxActiveTools
		if cat.WindowSize <= 0 {
			cat.WindowSize = 15
		}
	}
	if cat.MaxSummaryLength <= 0 {
		cat.MaxSummaryLength = 80
	}
	cat.EnableMetaTools = true
	cat.EnableSearchMeta = true
	cat.EnableSchemaMeta = true
	cat.EnableActivateMeta = true
	cat.EnableInvokeMeta = true
	return cat
}

func (r *Router) ensureSchemaStoreSynced() {
	if !r.schemaDirty.CompareAndSwap(true, false) {
		return
	}

	allTools := r.multiplexer.ListAllTools()
	catCfg := r.effectiveCatalogConfig()

	var prunedSlice []mcp.Tool
	if r.pruner != nil {
		tier := pruner.PruneTier(r.cfg.PruneMode)
		if tier == pruner.TierDynamic || tier == pruner.TierNone || tier == "" {
			tier = pruner.TierAggressive
		}
		if p, _, err := r.pruner.PruneToolsWithTier(allTools, tier); err == nil {
			prunedSlice = p
		}
	}
	if len(prunedSlice) != len(allTools) {
		prunedSlice = pruner.DeepCopyTools(allTools)
	}

	newStore := NewSchemaStore()
	for i, rawTool := range allTools {
		serverID := ""
		if route, ok := r.multiplexer.ResolveRoute(rawTool.Name); ok {
			serverID = route.UpstreamID
		}
		compactTool := ProjectCompactTool(rawTool, catCfg)
		newStore.Register(serverID, rawTool, prunedSlice[i], compactTool)
	}

	if r.streamFilter != nil {
		stubbed := r.cfg.EnableCatalog || r.cfg.PruneMode == "dynamic"
		r.streamFilter.RegisterRawTools(allTools, stubbed, nil)
	}

	r.mu.Lock()
	r.schemaStore = newStore
	r.mu.Unlock()
}

// SchemaStore returns the router's cached tool schema store.
func (r *Router) SchemaStore() *SchemaStore {
	r.ensureSchemaStoreSynced()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.schemaStore
}

// StreamFilter returns the router's adaptive stream filter.
func (r *Router) StreamFilter() *proxy.StreamFilter {
	return r.streamFilter
}

// Stats returns a thread-safe snapshot of session token and call metrics.
func (r *Router) Stats() proxy.SessionStatsSnapshot {
	return proxy.SessionStatsSnapshot{
		ListsIntercepted:  r.listsIntercepted.Load(),
		CallsIntercepted:  r.callsIntercepted.Load(),
		PreBindRejections: r.preBindRejections.Load(),
		CacheHits:         r.cacheHits.Load(),
		RawTokensTotal:    r.rawTokensTotal.Load(),
		PrunedTokensTotal: r.prunedTokensTotal.Load(),
	}
}

// AttachFilter attaches a filter engine to the router.
func (r *Router) AttachFilter(filter FilterEngine) {
	r.mu.Lock()
	r.filter = filter
	r.mu.Unlock()
	if r.streamFilter != nil {
		r.streamFilter.InvalidateCache()
	}
}

// AttachWindow attaches an active window engine to the router.
func (r *Router) AttachWindow(window WindowEngine) {
	r.mu.Lock()
	r.window = window
	r.mu.Unlock()
	if r.streamFilter != nil {
		r.streamFilter.InvalidateCache()
	}
}

// SetNotifier registers the callback to broadcast notifications/tools/list_changed.
func (r *Router) SetNotifier(notifier func()) {
	r.mu.Lock()
	r.notifier = notifier
	r.mu.Unlock()
}

// Multiplexer returns the internal multiplexer instance.
func (r *Router) Multiplexer() *Multiplexer {
	return r.multiplexer
}

// Start activates upstream message loops on the multiplexer.
func (r *Router) Start(handler UpstreamMessageHandler) error {
	r.multiplexer.SetMessageHandler(handler)
	return nil
}

// Close shuts down the multiplexer and all upstream transports.
func (r *Router) Close() error {
	return r.multiplexer.Close()
}

// SetProfile dynamically switches the active filtering profile.
func (r *Router) SetProfile(profile string) error {
	r.mu.Lock()
	r.cfg.ActiveProfile = profile
	filter := r.filter
	notifier := r.notifier
	r.mu.Unlock()

	if filter != nil {
		if err := filter.SetProfile(profile); err != nil {
			return err
		}
	}
	if r.streamFilter != nil {
		r.streamFilter.InvalidateCache()
	}
	if notifier != nil {
		notifier()
	}
	return nil
}

// HandleToolsList processes an incoming tools/list request, returning a pruned, filtered, and budgeted catalog.
func (r *Router) HandleToolsList(ctx context.Context, req *mcp.Message) (*mcp.Message, error) {
	r.listsIntercepted.Add(1)
	r.ensureSchemaStoreSynced()

	r.mu.RLock()
	cfg := r.cfg
	filter := r.filter
	window := r.window
	prunerEngine := r.pruner
	catCfg := r.effectiveCatalogConfig()
	r.mu.RUnlock()

	rawAll := r.multiplexer.ListAllTools()
	if filter != nil {
		rawAll = filter.Apply(rawAll)
	}
	rawTok := pruner.EstimateToolsBudgetTokens(rawAll)

	var tools []mcp.Tool
	isDynamicCatalog := cfg.EnableCatalog || cfg.PruneMode == "dynamic"

	if isDynamicCatalog {
		// Tier 4 Dynamic Catalog: active window tools keep Tier 3 schemas; dormant tools become compact catalog stubs.
		var prunedAll []mcp.Tool
		if prunerEngine != nil {
			if p, _, err := prunerEngine.PruneToolsWithTier(rawAll, pruner.TierAggressive); err == nil {
				prunedAll = p
			}
		}
		if len(prunedAll) != len(rawAll) {
			prunedAll = pruner.DeepCopyTools(rawAll)
		}

		tools = make([]mcp.Tool, 0, len(rawAll)+4)
		for i, t := range rawAll {
			if window != nil && window.IsActive(t.Name) {
				tools = append(tools, prunedAll[i])
			} else if catCfg.Mode != CatalogModeMetaOnly {
				tools = append(tools, ProjectCompactTool(t, catCfg))
			}
		}
		tools = append(tools, GetMetaToolDefinitions(catCfg)...)
	} else {
		// Standard Tiers 0-3 (with optional active sliding window)
		tools = rawAll
		if window != nil {
			tools = window.ActiveTools()
			if filter != nil {
				tools = filter.Apply(tools)
			}
		}

		if prunerEngine != nil && cfg.PruneMode != "none" {
			pruned, _, err := prunerEngine.PruneToolsWithTier(tools, pruner.PruneTier(cfg.PruneMode))
			if err != nil {
				r.logger.Warn("pruner failed, falling back to unpruned tools", "err", err)
			} else {
				tools = pruned
			}
		}

		tools = append(tools, GetMetaToolDefinitions(catCfg)...)
	}

	// Enforce RFC #1 token budget if configured
	if cfg.MaxSchemaTokens > 0 && prunerEngine != nil && r.streamFilter != nil {
		r.streamFilter.RegisterRawTools(rawAll, isDynamicCatalog, nil)
		if budgeted, bMetrics, err := r.streamFilter.FilterProjectedToolsList(tools, prunerEngine); err == nil {
			tools = budgeted
			if bMetrics.CacheHit {
				r.cacheHits.Add(1)
			}
		}
	}

	if tools == nil {
		tools = make([]mcp.Tool, 0)
	}

	prunedTok := pruner.EstimateToolsBudgetTokens(tools)
	if rawTok > 0 {
		r.rawTokensTotal.Add(uint64(rawTok))
		r.prunedTokensTotal.Add(uint64(prunedTok))
	}

	listResult := mcp.ListToolsResult{
		Tools: tools,
	}

	data, err := json.Marshal(listResult)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal list tools result: %w", err)
	}

	var reqID *mcp.RequestID
	if req != nil {
		reqID = req.ID
	}

	return &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      reqID,
		Result:  data,
	}, nil
}

// PreValidateCall records call context for RFC #1 dynamic budget boosting and performs
// fast pre-binding validation against cached RawTool schemas when schemas are stubbed or budgeted.
// Returns a non-nil self-correcting CallToolResult JSON-RPC response if validation fails.
func (r *Router) PreValidateCall(msg *mcp.Message) *mcp.Message {
	if msg == nil || len(msg.Params) == 0 || msg.ID == nil {
		return nil
	}
	var params mcp.CallToolRequestParams
	if err := json.Unmarshal(msg.Params, &params); err != nil || params.Name == "" || IsMetaTool(params.Name) {
		return nil
	}

	r.mu.RLock()
	cfg := r.cfg
	filter := r.filter
	r.mu.RUnlock()

	if filter != nil && !filter.IsAllowed(params.Name) {
		return nil // Handled by RouteToolCall's profile error contract
	}

	route, ok := r.multiplexer.ResolveRoute(params.Name)
	if !ok {
		return nil
	}

	if r.streamFilter != nil {
		r.streamFilter.RecordToolCall(route.ExposedName)
	}

	if cfg.EnableCatalog || cfg.PruneMode == "dynamic" || cfg.MaxSchemaTokens > 0 {
		if vErr := proxy.ValidateToolArguments(route.ExposedName, route.Tool.InputSchema, params.Arguments); vErr != nil {
			r.preBindRejections.Add(1)
			resp, err := mcp.NewResponse(*msg.ID, vErr.ToCallToolResult())
			if err == nil {
				return resp
			}
		}
	}
	return nil
}

// RouteToolCall resolves and rewrites a tools/call request (including unwrapped toolprune_invoke) for an upstream server.
func (r *Router) RouteToolCall(msg *mcp.Message) (UpstreamTransport, *mcp.Message, error) {
	if msg == nil || len(msg.Params) == 0 {
		return nil, nil, mcp.NewInvalidParams("missing params in tools/call request")
	}

	var params mcp.CallToolRequestParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, nil, mcp.NewInvalidParams(fmt.Sprintf("malformed tools/call params: %v", err))
	}

	if params.Name == "" {
		return nil, nil, mcp.NewInvalidParams("tool name is required")
	}

	r.callsIntercepted.Add(1)

	// Unwrap toolprune_invoke execution meta-tool transparently
	if params.Name == MetaToolInvoke {
		var invokeArgs struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params.Arguments, &invokeArgs); err != nil || strings.TrimSpace(invokeArgs.ToolName) == "" {
			return nil, nil, mcp.NewInvalidParams("toolprune_invoke requires 'tool_name' and 'arguments'")
		}
		params.Name = strings.TrimSpace(invokeArgs.ToolName)
		if len(invokeArgs.Arguments) == 0 {
			params.Arguments = json.RawMessage(`{}`)
		} else {
			params.Arguments = invokeArgs.Arguments
		}
	}

	r.mu.RLock()
	filter := r.filter
	window := r.window
	r.mu.RUnlock()

	// 1. Check Profile Security Filter
	if filter != nil && !filter.IsAllowed(params.Name) {
		return nil, nil, mcp.NewMethodNotFound(fmt.Sprintf("tool %q is blocked by active profile", params.Name))
	}

	// 2. Resolve Route via Multiplexer
	route, ok := r.multiplexer.ResolveRoute(params.Name)
	if !ok {
		return nil, nil, mcp.NewMethodNotFound(fmt.Sprintf("tool %q not found in router catalog", params.Name))
	}

	// Re-check resolved exposed name against filter to defeat unprefixed fallback bypass!
	if filter != nil && !filter.IsAllowed(route.ExposedName) {
		return nil, nil, mcp.NewMethodNotFound(fmt.Sprintf("tool %q is blocked by active profile", route.ExposedName))
	}

	// 3. Record Usage in Window (Auto-Promotion) and StreamFilter
	if window != nil {
		window.RecordUsage(route.ExposedName)
	}
	if r.streamFilter != nil {
		r.streamFilter.RecordToolCall(route.ExposedName)
	}

	// 4. Rewrite Tool Name to OriginalName for Upstream
	rewrittenParams := params
	rewrittenParams.Name = route.OriginalName

	newParamsJSON, err := json.Marshal(rewrittenParams)
	if err != nil {
		return nil, nil, mcp.NewInternalError(fmt.Sprintf("failed to marshal rewritten params: %v", err))
	}

	rewrittenMsg := *msg
	rewrittenMsg.Params = newParamsJSON

	return route.Target.Transport(), &rewrittenMsg, nil
}

// HandleInternalToolCall intercepts and handles internal discovery/hydration meta-tools.
// For valid toolprune_invoke calls, it validates arguments and returns (nil, false) so RouteToolCall
// forwards the unwrapped call to the target upstream server.
func (r *Router) HandleInternalToolCall(msg *mcp.Message) (*mcp.Message, bool) {
	if msg == nil || len(msg.Params) == 0 {
		return nil, false
	}

	var params mcp.CallToolRequestParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		return nil, false
	}

	var reqID mcp.RequestID
	if msg.ID != nil {
		reqID = *msg.ID
	}

	switch params.Name {
	case MetaToolSearch:
		r.callsIntercepted.Add(1)
		var args struct {
			Query  string `json:"query"`
			Server string `json:"server"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		resultText := r.searchTools(args.Query, args.Server, args.Limit)
		resp, _ := mcp.NewResponse(reqID, mcp.NewToolTextResult(resultText))
		return resp, true

	case MetaToolGetSchema:
		r.callsIntercepted.Add(1)
		var args struct {
			ToolNames []string `json:"tool_names"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil || len(args.ToolNames) == 0 {
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult("toolprune_get_tool_schema requires a non-empty 'tool_names' array"))
			return resp, true
		}
		r.ensureSchemaStoreSynced()
		r.mu.RLock()
		store := r.schemaStore
		filter := r.filter
		r.mu.RUnlock()

		text := formatToolSchemasResponse(store, filter, args.ToolNames)
		resp, _ := mcp.NewResponse(reqID, mcp.NewToolTextResult(text))
		return resp, true

	case MetaToolActivate:
		r.callsIntercepted.Add(1)
		var args struct {
			Tools []string `json:"tools"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult("invalid arguments for toolprune_activate_tools"))
			return resp, true
		}
		r.mu.RLock()
		filter := r.filter
		window := r.window
		notifier := r.notifier
		r.mu.RUnlock()

		count := 0
		var blocked []string
		for _, t := range args.Tools {
			if filter != nil && !filter.IsAllowed(t) {
				blocked = append(blocked, t)
				continue
			}
			// Also verify resolved exposed name if caller omitted prefix
			targetName := t
			if route, ok := r.multiplexer.ResolveRoute(t); ok {
				if filter != nil && !filter.IsAllowed(route.ExposedName) {
					blocked = append(blocked, route.ExposedName)
					continue
				}
				targetName = route.ExposedName
			}
			if window != nil {
				if promoted, _, err := window.Promote(targetName); err == nil && promoted {
					count++
				}
			}
		}
		if count > 0 && notifier != nil {
			notifier()
		}
		msgText := fmt.Sprintf("Activated %d tools. Catalog list_changed emitted.", count)
		if len(blocked) > 0 {
			msgText = fmt.Sprintf("Activated %d tools. Blocked by active profile: %s.", count, strings.Join(blocked, ", "))
		}
		resp, _ := mcp.NewResponse(reqID, mcp.NewToolTextResult(msgText))
		return resp, true

	case MetaToolInvoke:
		var args struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params.Arguments, &args); err != nil || strings.TrimSpace(args.ToolName) == "" {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult("toolprune_invoke requires 'tool_name' and 'arguments'"))
			return resp, true
		}
		targetName := strings.TrimSpace(args.ToolName)
		if IsMetaTool(targetName) {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult("toolprune_invoke cannot recursively invoke meta-tools"))
			return resp, true
		}

		r.mu.RLock()
		filter := r.filter
		r.mu.RUnlock()

		if filter != nil && !filter.IsAllowed(targetName) {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q is blocked by active profile", targetName)))
			return resp, true
		}
		route, ok := r.multiplexer.ResolveRoute(targetName)
		if !ok {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q not found in router catalog", targetName)))
			return resp, true
		}
		if filter != nil && !filter.IsAllowed(route.ExposedName) {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q is blocked by active profile", route.ExposedName)))
			return resp, true
		}
		if vErr := proxy.ValidateToolArguments(route.ExposedName, route.Tool.InputSchema, args.Arguments); vErr != nil {
			r.preBindRejections.Add(1)
			resp, _ := mcp.NewResponse(reqID, vErr.ToCallToolResult())
			return resp, true
		}
		// Validated! Return (nil, false) so RouteToolCall forwards the unwrapped invocation upstream.
		return nil, false

	default:
		return nil, false
	}
}

func (r *Router) searchTools(query string, server string, limit int) string {
	r.ensureSchemaStoreSynced()
	r.mu.RLock()
	store := r.schemaStore
	filter := r.filter
	r.mu.RUnlock()

	if limit <= 0 {
		limit = 25
	}
	candidates := store.Search(query, server, 50)
	var matched []string
	for _, c := range candidates {
		if filter != nil && !filter.IsAllowed(c.Name) {
			continue
		}
		matched = append(matched, fmt.Sprintf("- %s: %s", c.Name, c.RawTool.Description))
		if len(matched) >= limit {
			break
		}
	}
	if len(matched) == 0 {
		return "No matching tools found in catalog."
	}
	return "Matching tools:\n" + strings.Join(matched, "\n")
}
