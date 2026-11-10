package pruner

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"sync"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

// Engine orchestrates schema pruning transformations across Tiers 0 through 3.
type Engine struct {
	mu   sync.RWMutex
	opts Options
}

// New creates a new pruning engine.
func New(opts ...Options) *Engine {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	} else {
		opt = DefaultOptions(TierAggressive)
	}
	return &Engine{opts: opt}
}

// SetOptions dynamically reconfigures the pruning engine.
func (e *Engine) SetOptions(opts Options) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opts = opts
}

// GetOptions retrieves the current pruning engine configuration.
func (e *Engine) GetOptions() Options {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.opts
}

// ConfigFingerprint returns a fast FNV-1a hash of the active Engine options for stream cache keying.
func (e *Engine) ConfigFingerprint() uint64 {
	e.mu.RLock()
	opts := e.opts
	e.mu.RUnlock()

	h := fnv.New64a()
	_, _ = h.Write([]byte(opts.Tier))
	var buf [16]byte
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(opts.MaxSchemaTokens), 10))
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(opts.Condense.MaxToolDescLen), 10))
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(opts.Condense.MaxParamDescLen), 10))
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(opts.Params.MaxNonSelfEvidentLen), 10))
	if opts.StripSelfEvident {
		_, _ = h.Write([]byte{1})
	} else {
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// PruneTools satisfies the proxy.PruneEngine interface.
func (e *Engine) PruneTools(tools []mcp.Tool) ([]mcp.Tool, proxy.PruneMetrics, error) {
	e.mu.RLock()
	opts := e.opts
	e.mu.RUnlock()

	return e.PruneToolsWithTier(tools, opts.Tier)
}

// PruneToolsForProfile prunes tools for a named profile or tier string.
func (e *Engine) PruneToolsForProfile(tools []mcp.Tool, profile string) ([]mcp.Tool, error) {
	tier := PruneTier(profile)
	if tier == "" {
		tier = TierAggressive
	}
	pruned, _, err := e.PruneToolsWithTier(tools, tier)
	return pruned, err
}

// PruneToolsWithContext satisfies proxy.ContextBudgetEngine for fast (<0.5ms) RFC #1 budget enforcement.
func (e *Engine) PruneToolsWithContext(tools []mcp.Tool, ctx proxy.BudgetContext) ([]mcp.Tool, proxy.BudgetPruneMetrics, error) {
	if len(tools) == 0 {
		return tools, proxy.BudgetPruneMetrics{}, nil
	}

	e.mu.RLock()
	opts := e.opts
	e.mu.RUnlock()

	maxTokens := ctx.MaxSchemaTokens
	if maxTokens <= 0 {
		maxTokens = opts.MaxSchemaTokens
	}

	if maxTokens <= 0 {
		pruned, pm, err := e.PruneToolsWithTier(tools, opts.Tier)
		return pruned, proxy.BudgetPruneMetrics{PruneMetrics: pm}, err
	}

	// Preserve numeric/array/additionalProperties constraints so EnforceTokenBudget retains full structural fidelity
	tierOpts := opts
	tierOpts.Minify.StripAdditionalPropsFalse = false
	tierOpts.Params.StripValidators = false

	var base []mcp.Tool
	if tierOpts.Tier == TierNone {
		base = DeepCopyTools(tools)
	} else {
		base = make([]mcp.Tool, len(tools))
		cloned := DeepCopyTools(tools)
		for i, tool := range cloned {
			pt, err := e.pruneSingleTool(tool, tierOpts)
			if err != nil {
				return nil, proxy.BudgetPruneMetrics{}, fmt.Errorf("failed to prune tool %q: %w", tool.Name, err)
			}
			base[i] = pt
		}
	}

	rawEst := EstimateToolsBudgetTokens(tools)
	compacted, bMetrics := EnforceTokenBudget(base, BudgetOptions{
		MaxSchemaTokens:   maxTokens,
		PinnedTools:       ctx.PinnedTools,
		ContextBoostTools: ctx.ContextBoostTools,
	})

	if opts.StrictDraft07 {
		for _, pt := range compacted {
			res := ValidateTool(pt)
			if !res.Valid {
				return nil, proxy.BudgetPruneMetrics{}, fmt.Errorf("budget-compacted tool %q failed Draft-07 validation: %v", pt.Name, res.Errors)
			}
		}
	}

	var ratio float64
	if rawEst > 0 && rawEst >= bMetrics.CompactedToken {
		ratio = float64(rawEst-bMetrics.CompactedToken) / float64(rawEst)
	}

	return compacted, proxy.BudgetPruneMetrics{
		PruneMetrics: proxy.PruneMetrics{
			RawTokens:    rawEst,
			PrunedTokens: bMetrics.CompactedToken,
			SavingsRatio: ratio,
		},
		PassReached:    bMetrics.PassReached,
		ProjectedTools: bMetrics.ProjectedTools,
	}, nil
}

// PruneToolsWithTier prunes tools with an explicit tier.
func (e *Engine) PruneToolsWithTier(tools []mcp.Tool, tier PruneTier) ([]mcp.Tool, proxy.PruneMetrics, error) {
	if len(tools) == 0 {
		return tools, proxy.PruneMetrics{}, nil
	}

	e.mu.RLock()
	opts := e.opts
	e.mu.RUnlock()
	opts.Tier = tier

	if opts.MaxSchemaTokens > 0 {
		compacted, bm, err := e.PruneToolsWithContext(tools, proxy.BudgetContext{
			MaxSchemaTokens: opts.MaxSchemaTokens,
		})
		return compacted, bm.PruneMetrics, err
	}

	rawTokens := EstimateToolsTokens(tools)

	if tier == TierNone {
		return tools, proxy.PruneMetrics{
			RawTokens:    rawTokens,
			PrunedTokens: rawTokens,
			SavingsRatio: 0.0,
		}, nil
	}

	// Deep-copy tools to enforce immutability
	cloned := DeepCopyTools(tools)
	pruned := make([]mcp.Tool, len(cloned))

	for i, tool := range cloned {
		pTool, err := e.pruneSingleTool(tool, opts)
		if err != nil {
			return nil, proxy.PruneMetrics{}, fmt.Errorf("failed to prune tool %q: %w", tool.Name, err)
		}

		// Validation Guardrail Check
		if opts.StrictDraft07 {
			res := ValidateTool(pTool)
			if !res.Valid {
				return nil, proxy.PruneMetrics{}, fmt.Errorf("pruned tool %q failed Draft-07 validation: %v", tool.Name, res.Errors)
			}
		}

		pruned[i] = pTool
	}

	prunedTokens := EstimateToolsTokens(pruned)
	var savingsRatio float64
	if rawTokens > 0 {
		savingsRatio = float64(rawTokens-prunedTokens) / float64(rawTokens)
	}

	return pruned, proxy.PruneMetrics{
		RawTokens:    rawTokens,
		PrunedTokens: prunedTokens,
		SavingsRatio: savingsRatio,
	}, nil
}

// pruneSingleTool routes transformations through progressive tiers.
func (e *Engine) pruneSingleTool(tool mcp.Tool, opts Options) (mcp.Tool, error) {
	switch opts.Tier {
	case TierNone:
		return tool, nil

	case TierLossless:
		return MinifyTool(tool, opts.Minify), nil

	case TierCompact:
		minified := MinifyTool(tool, opts.Minify)
		condensed := CondenseTool(minified, opts.Condense)
		return condensed, nil

	case TierAggressive, TierDynamic:
		minified := MinifyTool(tool, opts.Minify)
		condensed := CondenseTool(minified, opts.Condense)
		pruned := PruneParamsTool(condensed, opts.Params)
		return pruned, nil

	default:
		return tool, fmt.Errorf("unknown prune tier: %s", opts.Tier)
	}
}

// PruneTool applies the pruning pipeline to a single tool based on given options.
func PruneTool(tool mcp.Tool, opts ...any) (mcp.Tool, error) {
	var opt Options
	if len(opts) > 0 {
		switch v := opts[0].(type) {
		case Options:
			opt = v
		case *Options:
			if v != nil {
				opt = *v
			}
		case PruneTier:
			opt = DefaultOptions(v)
		default:
			opt = DefaultOptions(TierAggressive)
		}
	} else {
		opt = DefaultOptions(TierAggressive)
	}

	engine := New(opt)
	return engine.pruneSingleTool(tool, opt)
}

// PruneToolsList applies the pruning pipeline to a slice of tools.
func PruneToolsList(tools []mcp.Tool, opts ...any) ([]mcp.Tool, error) {
	var opt Options
	if len(opts) > 0 {
		switch v := opts[0].(type) {
		case Options:
			opt = v
		case *Options:
			if v != nil {
				opt = *v
			}
		case PruneTier:
			opt = DefaultOptions(v)
		default:
			opt = DefaultOptions(TierAggressive)
		}
	} else {
		opt = DefaultOptions(TierAggressive)
	}

	engine := New(opt)
	pruned, _, err := engine.PruneToolsWithTier(tools, opt.Tier)
	return pruned, err
}

// EstimateToolsTokens provides calibrated token estimation (zero-allocation BPE approximation).
// Formula: tokens = round(len(compactJSON) / 4.45) based on 207-tool cl100k empirical analysis.
func EstimateToolsTokens(tools []mcp.Tool) int {
	b, err := json.Marshal(tools)
	if err != nil {
		return 0
	}
	return int(math.Round(float64(len(b)) / 4.45))
}

// DeepCopyTools creates an isolated structural clone of a tool slice without JSON reflection overhead.
func DeepCopyTools(tools []mcp.Tool) []mcp.Tool {
	if tools == nil {
		return nil
	}
	out := make([]mcp.Tool, len(tools))
	for i := range tools {
		out[i] = CloneTool(tools[i])
	}
	return out
}

// CloneTool creates an isolated deep copy of a single mcp.Tool.
func CloneTool(t mcp.Tool) mcp.Tool {
	out := mcp.Tool{
		Name:         t.Name,
		Title:        t.Title,
		Description:  t.Description,
		InputSchema:  CloneInputSchema(t.InputSchema),
		OutputSchema: CloneSchemaMap(t.OutputSchema),
	}
	if t.Annotations != nil {
		ann := &mcp.ToolAnnotations{
			Title: t.Annotations.Title,
		}
		if t.Annotations.ReadOnlyHint != nil {
			v := *t.Annotations.ReadOnlyHint
			ann.ReadOnlyHint = &v
		}
		if t.Annotations.DestructiveHint != nil {
			v := *t.Annotations.DestructiveHint
			ann.DestructiveHint = &v
		}
		if t.Annotations.IdempotentHint != nil {
			v := *t.Annotations.IdempotentHint
			ann.IdempotentHint = &v
		}
		if t.Annotations.OpenWorldHint != nil {
			v := *t.Annotations.OpenWorldHint
			ann.OpenWorldHint = &v
		}
		out.Annotations = ann
	}
	if t.Icons != nil {
		icons := make([]mcp.ToolIcon, len(t.Icons))
		for i, ic := range t.Icons {
			icons[i] = mcp.ToolIcon{
				Src:      ic.Src,
				MIMEType: ic.MIMEType,
				Theme:    ic.Theme,
			}
			if ic.Sizes != nil {
				icons[i].Sizes = append([]string(nil), ic.Sizes...)
			}
		}
		out.Icons = icons
	}
	return out
}

// CloneInputSchema creates an isolated deep copy of an mcp.InputSchema.
func CloneInputSchema(s mcp.InputSchema) mcp.InputSchema {
	out := mcp.InputSchema{
		Type:                 s.Type,
		Properties:           CloneSchemaMap(s.Properties),
		AdditionalProperties: CloneSchemaValue(s.AdditionalProperties),
		Schema:               s.Schema,
		ID:                   s.ID,
		Title:                s.Title,
		Description:          s.Description,
		Comment:              s.Comment,
		Examples:             CloneSchemaSlice(s.Examples),
		Defs:                 CloneSchemaMap(s.Defs),
		Definitions:          CloneSchemaMap(s.Definitions),
		AnyOf:                CloneSchemaSlice(s.AnyOf),
		OneOf:                CloneSchemaSlice(s.OneOf),
		AllOf:                CloneSchemaSlice(s.AllOf),
		Not:                  CloneSchemaMap(s.Not),
		PatternProperties:    CloneSchemaMap(s.PatternProperties),
		DependentSchemas:     CloneSchemaMap(s.DependentSchemas),
	}
	if s.Required != nil {
		out.Required = append([]string(nil), s.Required...)
	}
	if s.DependentRequired != nil {
		dr := make(map[string][]string, len(s.DependentRequired))
		for k, v := range s.DependentRequired {
			if v != nil {
				dr[k] = append([]string(nil), v...)
			} else {
				dr[k] = nil
			}
		}
		out.DependentRequired = dr
	}
	if s.MinProperties != nil {
		v := *s.MinProperties
		out.MinProperties = &v
	}
	if s.MaxProperties != nil {
		v := *s.MaxProperties
		out.MaxProperties = &v
	}
	return out
}

// CloneSchemaMap recursively deep-copies a map[string]any schema node.
func CloneSchemaMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = CloneSchemaValue(v)
	}
	return out
}

// CloneSchemaSlice recursively deep-copies a []any schema slice.
func CloneSchemaSlice(s []any) []any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = CloneSchemaValue(v)
	}
	return out
}

// CloneSchemaValue deep-copies common JSON Schema Go values.
func CloneSchemaValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return CloneSchemaMap(val)
	case []any:
		return CloneSchemaSlice(val)
	case []string:
		return append([]string(nil), val...)
	case map[string]string:
		cp := make(map[string]string, len(val))
		for k, s := range val {
			cp[k] = s
		}
		return cp
	default:
		// Primitives (string, float64, int, bool, json.Number) are immutable value types in Go.
		return val
	}
}
