package router

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// WrapperCatalogAdapter implements proxy.LocalCatalogInterceptor so single-upstream
// wrapper mode supports full Tier 4 dynamic catalog indexing, sliding window promotion,
// profile enforcement, and all 4 ToolPrune meta-tools.
type WrapperCatalogAdapter struct {
	mu         sync.RWMutex
	store      *SchemaStore
	window     *ToolWindow
	filter     FilterEngine
	pruner     *pruner.Engine
	catalogCfg CatalogConfig
	rawCatalog *wrapperCatalogProvider
	onChanged  func()
}

type wrapperCatalogProvider struct {
	mu    sync.RWMutex
	tools map[string]mcp.Tool
	order []string
}

func newWrapperCatalogProvider() *wrapperCatalogProvider {
	return &wrapperCatalogProvider{
		tools: make(map[string]mcp.Tool),
	}
}

func (p *wrapperCatalogProvider) sync(tools []mcp.Tool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tools = make(map[string]mcp.Tool, len(tools))
	p.order = make([]string, 0, len(tools))
	for _, t := range tools {
		p.tools[t.Name] = pruner.CloneTool(t)
		p.order = append(p.order, t.Name)
	}
}

func (p *wrapperCatalogProvider) GetTool(name string) (mcp.Tool, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.tools[name]
	return t, ok
}

func (p *wrapperCatalogProvider) GetAllTools() []mcp.Tool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]mcp.Tool, 0, len(p.order))
	for _, name := range p.order {
		out = append(out, p.tools[name])
	}
	return out
}

// NewWrapperCatalogAdapter creates a Tier 4 dynamic catalog interceptor for wrapper mode.
func NewWrapperCatalogAdapter(
	windowSize int,
	pinnedTools []string,
	filter FilterEngine,
	prunerEngine *pruner.Engine,
	notifier *DebouncedNotifier,
	onChanged func(),
) (*WrapperCatalogAdapter, error) {
	if windowSize <= 0 {
		windowSize = 15
	}
	if len(pinnedTools) == 0 {
		pinnedTools = []string{MetaToolSearch, MetaToolGetSchema, MetaToolActivate, MetaToolInvoke}
	}
	provider := newWrapperCatalogProvider()
	win, err := NewToolWindow(WindowConfig{
		Enabled:           true,
		MaxActive:         windowSize,
		AutoPromoteOnCall: true,
		PinnedTools:       pinnedTools,
	}, provider, notifier)
	if err != nil {
		return nil, err
	}

	catCfg := DefaultCatalogConfig()
	catCfg.Mode = CatalogModeWindowed
	catCfg.WindowSize = windowSize
	catCfg.EnableInvokeMeta = true

	if prunerEngine == nil {
		prunerEngine = pruner.New(pruner.DefaultPruneOptions(pruner.TierAggressive))
	}

	return &WrapperCatalogAdapter{
		store:      NewSchemaStore(),
		window:     win,
		filter:     filter,
		pruner:     prunerEngine,
		catalogCfg: catCfg,
		rawCatalog: provider,
		onChanged:  onChanged,
	}, nil
}

// SyncUpstreamTools caches raw/pruned/compact variants and projects windowed Tier 4 output.
func (a *WrapperCatalogAdapter) SyncUpstreamTools(tools []mcp.Tool) ([]mcp.Tool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	allowed := tools
	if a.filter != nil {
		allowed = a.filter.Apply(tools)
	}
	a.rawCatalog.sync(allowed)

	prunedSlice, _, err := a.pruner.PruneToolsWithTier(allowed, pruner.TierAggressive)
	if err != nil {
		prunedSlice = pruner.DeepCopyTools(allowed)
	}

	a.store = NewSchemaStore()
	projected := make([]mcp.Tool, 0, len(allowed)+4)
	for i, rawTool := range allowed {
		prunedTool := rawTool
		if i < len(prunedSlice) {
			prunedTool = prunedSlice[i]
		}
		compactTool := ProjectCompactTool(rawTool, a.catalogCfg)
		a.store.Register("upstream", rawTool, prunedTool, compactTool)

		if a.window.IsActive(rawTool.Name) {
			projected = append(projected, prunedTool)
		} else if a.catalogCfg.Mode != CatalogModeMetaOnly {
			projected = append(projected, compactTool)
		}
	}

	projected = append(projected, GetMetaToolDefinitions(a.catalogCfg)...)
	return projected, nil
}

// InterceptCall handles Tier 4 meta-tools locally or unwraps toolprune_invoke for upstream dispatch.
func (a *WrapperCatalogAdapter) InterceptCall(
	toolName string,
	rawArgs json.RawMessage,
) (handled bool, forwardedName string, forwardedArgs json.RawMessage, localResult *mcp.CallToolResult) {
	switch toolName {
	case MetaToolSearch:
		var args struct {
			Query  string `json:"query"`
			Server string `json:"server"`
			Limit  int    `json:"limit"`
		}
		_ = json.Unmarshal(rawArgs, &args)
		a.mu.RLock()
		matches := a.store.Search(args.Query, args.Server, args.Limit)
		filter := a.filter
		a.mu.RUnlock()

		var lines []string
		for _, m := range matches {
			if filter != nil && !filter.IsAllowed(m.Name) {
				continue
			}
			lines = append(lines, formatCatalogMatchLine(m.RawTool))
		}
		if len(lines) == 0 {
			return true, "", nil, mcp.NewToolTextResult("No matching tools found in catalog.")
		}
		return true, "", nil, mcp.NewToolTextResult("Matching tools:\n" + strings.Join(lines, "\n"))

	case MetaToolGetSchema:
		var args struct {
			ToolNames []string `json:"tool_names"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil || len(args.ToolNames) == 0 {
			return true, "", nil, mcp.NewToolExecutionErrorResult("toolprune_get_tool_schema requires a non-empty 'tool_names' string array")
		}
		a.mu.RLock()
		text := formatToolSchemasResponse(a.store, a.filter, args.ToolNames)
		a.mu.RUnlock()
		return true, "", nil, mcp.NewToolTextResult(text)

	case MetaToolActivate:
		var args struct {
			Tools []string `json:"tools"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil || len(args.Tools) == 0 {
			return true, "", nil, mcp.NewToolExecutionErrorResult("toolprune_activate_tools requires a non-empty 'tools' string array")
		}
		a.mu.RLock()
		filter := a.filter
		a.mu.RUnlock()

		promotedCount := 0
		var blocked []string
		for _, name := range args.Tools {
			if filter != nil && !filter.IsAllowed(name) {
				blocked = append(blocked, name)
				continue
			}
			if promoted, _, err := a.window.Promote(name); err == nil && promoted {
				promotedCount++
			}
		}
		if promotedCount > 0 && a.onChanged != nil {
			a.onChanged()
		}
		msg := fmt.Sprintf("Activated %d tools.", promotedCount)
		if len(blocked) > 0 {
			msg += fmt.Sprintf(" Blocked by active profile: %s.", strings.Join(blocked, ", "))
		}
		return true, "", nil, mcp.NewToolTextResult(msg)

	case MetaToolInvoke:
		var args struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil || strings.TrimSpace(args.ToolName) == "" {
			return true, "", nil, mcp.NewToolExecutionErrorResult("toolprune_invoke requires 'tool_name' and 'arguments'")
		}
		if IsMetaTool(args.ToolName) {
			return true, "", nil, mcp.NewToolExecutionErrorResult("toolprune_invoke cannot recursively invoke meta-tools")
		}
		a.mu.RLock()
		filter := a.filter
		cached, exists := a.store.Get(args.ToolName)
		a.mu.RUnlock()

		if filter != nil && !filter.IsAllowed(args.ToolName) {
			return true, "", nil, mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q is blocked by the active profile", args.ToolName))
		}
		if !exists {
			return true, "", nil, mcp.NewToolExecutionErrorResult(fmt.Sprintf("tool %q not found in catalog", args.ToolName))
		}
		if vErr := proxy.ValidateToolArguments(cached.RawTool.Name, cached.RawTool.InputSchema, args.Arguments); vErr != nil {
			res := vErr.ToCallToolResult()
			return true, "", nil, &res
		}
		a.window.RecordUsage(args.ToolName)
		if len(args.Arguments) == 0 {
			args.Arguments = json.RawMessage(`{}`)
		}
		return false, args.ToolName, args.Arguments, nil

	default:
		a.window.RecordUsage(toolName)
		return false, "", nil, nil
	}
}

func formatCatalogMatchLine(t mcp.Tool) string {
	summary := CleanSummaryDescription(t.Description, 100)
	if len(t.InputSchema.Required) > 0 {
		return fmt.Sprintf("- %s: %s (required: %s)", t.Name, summary, strings.Join(t.InputSchema.Required, ", "))
	}
	return fmt.Sprintf("- %s: %s", t.Name, summary)
}

func formatToolSchemasResponse(store *SchemaStore, filter FilterEngine, names []string) string {
	var sections []string
	for _, name := range names {
		if filter != nil && !filter.IsAllowed(name) {
			sections = append(sections, fmt.Sprintf("### %s\nBlocked by active profile.", name))
			continue
		}
		cached, ok := store.Get(name)
		if !ok {
			sections = append(sections, fmt.Sprintf("### %s\nNot found in catalog.", name))
			continue
		}
		schemaJSON, err := json.MarshalIndent(cached.PrunedTool, "", "  ")
		if err != nil {
			schemaJSON, _ = json.Marshal(cached.RawTool)
		}
		sections = append(sections, fmt.Sprintf("### %s\n%s", name, string(schemaJSON)))
	}
	return strings.Join(sections, "\n\n")
}
