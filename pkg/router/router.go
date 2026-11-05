package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// RouterConfig holds settings for the Unified Router Engine.
type RouterConfig struct {
	NamespaceMode  NamespaceMode `json:"namespaceMode" yaml:"namespace_mode"`
	PruneMode      string        `json:"pruneMode" yaml:"prune_mode"`            // "none", "lossless", "compact", "aggressive", "dynamic"
	ActiveProfile  string        `json:"activeProfile" yaml:"active_profile"`    // e.g. "read_only", "dev", "full"
	MaxActiveTools int           `json:"maxActiveTools" yaml:"max_active_tools"` // For active windowing (default: 15)
	EnableCatalog  bool          `json:"enableCatalog" yaml:"enable_catalog"`    // Tier 4 dynamic catalog
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

// Router orchestrates multiplexing, pruning, filtering, windowing, and cataloging.
type Router struct {
	mu          sync.RWMutex
	cfg         RouterConfig
	multiplexer *Multiplexer
	pruner      *pruner.Engine
	filter      FilterEngine
	window      WindowEngine
	logger      *slog.Logger
	notifier    func() // Broadcast notifications/tools/list_changed to clients
}

// NewRouter constructs a new Router instance.
func NewRouter(cfg RouterConfig, prunerEngine *pruner.Engine, logger *slog.Logger) *Router {
	if logger == nil {
		logger = slog.Default()
	}
	mux := NewMultiplexer(cfg.NamespaceMode, logger)
	return &Router{
		cfg:         cfg,
		multiplexer: mux,
		pruner:      prunerEngine,
		logger:      logger,
	}
}

// AttachFilter attaches a filter engine to the router.
func (r *Router) AttachFilter(filter FilterEngine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filter = filter
}

// AttachWindow attaches an active window engine to the router.
func (r *Router) AttachWindow(window WindowEngine) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.window = window
}

// SetNotifier registers the callback to broadcast notifications/tools/list_changed.
func (r *Router) SetNotifier(notifier func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifier = notifier
	r.multiplexer.SetOnToolsChanged(notifier)
}

// Multiplexer returns the internal multiplexer instance.
func (r *Router) Multiplexer() *Multiplexer {
	return r.multiplexer
}

// Start satisfies proxy.RouterEngine.
func (r *Router) Start(handler UpstreamMessageHandler) error {
	r.multiplexer.SetMessageHandler(handler)
	return nil
}

// Close satisfies proxy.RouterEngine.
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
	if notifier != nil {
		notifier()
	}
	return nil
}

// HandleToolsList processes an incoming tools/list request, returning a pruned, filtered catalog.
func (r *Router) HandleToolsList(ctx context.Context, req *mcp.Message) (*mcp.Message, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. Gather all exposed tools across upstreams
	tools := r.multiplexer.ListAllTools()

	// 2. Apply Active Windowing (if window engine attached)
	if r.window != nil {
		tools = r.window.ActiveTools()
	}

	// 3. Apply Profile and Regex Filtering
	if r.filter != nil {
		tools = r.filter.Apply(tools)
	}

	// 4. Apply Schema Pruning (Tiers 0-3)
	if r.pruner != nil && r.cfg.PruneMode != "none" {
		pruned, _, err := r.pruner.PruneToolsWithTier(tools, pruner.PruneTier(r.cfg.PruneMode))
		if err != nil {
			r.logger.Warn("pruner failed, falling back to unpruned tools", "err", err)
		} else {
			tools = pruned
		}
	}

	// 5. Inject Router Meta-Tools
	tools = append(tools, r.getInternalMetaTools()...)

	if tools == nil {
		tools = make([]mcp.Tool, 0)
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

	resp := &mcp.Message{
		JSONRPC: mcp.JSONRPCVersion,
		ID:      reqID,
		Result:  data,
	}
	return resp, nil
}

// RouteToolCall resolves and rewrites a tools/call request for an upstream server.
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

	// 3. Record Usage in Window (Auto-Promotion)
	if window != nil {
		window.RecordUsage(params.Name)
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

// HandleInternalToolCall intercepts and handles internal discovery meta-tools.
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
	case "toolprune_search_tools":
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		resultText := r.searchTools(args.Query)
		resp, _ := mcp.NewResponse(reqID, mcp.NewToolTextResult(resultText))
		return resp, true

	case "toolprune_activate_tools":
		var args struct {
			Tools []string `json:"tools"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		count := 0
		if r.window != nil {
			for _, t := range args.Tools {
				if promoted, _, _ := r.window.Promote(t); promoted {
					count++
				}
			}
		}
		msgText := fmt.Sprintf("Activated %d tools. Catalog list_changed emitted.", count)
		resp, _ := mcp.NewResponse(reqID, mcp.NewToolTextResult(msgText))
		return resp, true

	default:
		return nil, false
	}
}

func (r *Router) getInternalMetaTools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name:        "toolprune_search_tools",
			Description: "Search dormant tools in the catalog by keyword.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"query": map[string]any{"type": "string", "description": "Search keyword"},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "toolprune_activate_tools",
			Description: "Activate tools from the dormant catalog into the active window.",
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"tools": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				Required: []string{"tools"},
			},
		},
	}
}

func (r *Router) searchTools(query string) string {
	tools := r.multiplexer.ListAllTools()
	var matched []string
	q := strings.ToLower(query)
	for _, t := range tools {
		if strings.Contains(strings.ToLower(t.Name), q) || strings.Contains(strings.ToLower(t.Description), q) {
			matched = append(matched, fmt.Sprintf("- %s: %s", t.Name, t.Description))
		}
	}
	if len(matched) == 0 {
		return "No matching tools found in catalog."
	}
	return "Matching tools:\n" + strings.Join(matched, "\n")
}
