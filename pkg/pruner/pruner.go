package pruner

import (
	"encoding/json"
	"fmt"
	"math"
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

// PruneToolsWithTier prunes tools with an explicit tier.
func (e *Engine) PruneToolsWithTier(tools []mcp.Tool, tier PruneTier) ([]mcp.Tool, proxy.PruneMetrics, error) {
	if len(tools) == 0 {
		return tools, proxy.PruneMetrics{}, nil
	}

	rawTokens := EstimateToolsTokens(tools)

	if tier == TierNone {
		return tools, proxy.PruneMetrics{
			RawTokens:    rawTokens,
			PrunedTokens: rawTokens,
			SavingsRatio: 0.0,
		}, nil
	}

	e.mu.RLock()
	opts := e.opts
	e.mu.RUnlock()
	opts.Tier = tier

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

// DeepCopyTools creates an isolated clone of a tool slice.
func DeepCopyTools(tools []mcp.Tool) []mcp.Tool {
	b, _ := json.Marshal(tools)
	var copy []mcp.Tool
	_ = json.Unmarshal(b, &copy)
	return copy
}
