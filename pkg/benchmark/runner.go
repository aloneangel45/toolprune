package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/router"
)

// BenchmarkOptions configures benchmark execution behavior.
type BenchmarkOptions struct {
	// UseExactTokens forces cl100k BPE counting. If false, calibrated estimator is used.
	UseExactTokens bool `json:"useExactTokens"`

	// ValidateSchemas executes Draft-07 validation guardrails on all pruned schemas.
	ValidateSchemas bool `json:"validateSchemas"`

	// CatalogConfig configures Tier 4 catalog generation.
	CatalogConfig router.CatalogConfig `json:"catalogConfig"`

	// MaxSchemaTokens evaluates RFC #1 progressive token-budget compaction when > 0.
	MaxSchemaTokens int `json:"maxSchemaTokens,omitempty"`

	// IncludeParamBreakdown computes per-parameter token counts for --verbose mode.
	IncludeParamBreakdown bool `json:"includeParamBreakdown,omitempty"`
}

// DefaultBenchmarkOptions returns production defaults for benchmarking.
func DefaultBenchmarkOptions() BenchmarkOptions {
	return BenchmarkOptions{
		UseExactTokens:  true,
		ValidateSchemas: true,
		CatalogConfig:   router.DefaultCatalogConfig(),
	}
}

// TierReport holds metrics for a single pruning tier across the entire toolset.
type TierReport struct {
	Tier             pruner.PruneTier `json:"tier"`
	Name             string           `json:"name"`
	Tokens           int              `json:"tokens"`
	Bytes            int              `json:"bytes"`
	TokensSaved      int              `json:"tokensSaved"`
	SavingsPercent   float64          `json:"savingsPercent"`
	TokensPerTool    float64          `json:"tokensPerTool"`
	ValidDraft07     bool             `json:"validDraft07"`
	ValidationErrors []string         `json:"validationErrors,omitempty"`
}

// ParamTokenStat holds per-parameter token metrics when --verbose is enabled.
type ParamTokenStat struct {
	Name        string `json:"name"`
	Required    bool   `json:"required"`
	RawTokens   int    `json:"rawTokens"`
	Tier3Tokens int    `json:"tier3Tokens"`
}

// ToolReport holds a per-tool breakdown of token counts across all pruning tiers.
type ToolReport struct {
	ToolName          string           `json:"toolName"`
	RawTokens         int              `json:"rawTokens"`
	Tier1Tokens       int              `json:"tier1Tokens"`
	Tier2Tokens       int              `json:"tier2Tokens"`
	Tier3Tokens       int              `json:"tier3Tokens"`
	Tier4Tokens       int              `json:"tier4Tokens"`
	MaxSavingsPercent float64          `json:"maxSavingsPercent"`
	Parameters        []ParamTokenStat `json:"parameters,omitempty"`
}

// BenchmarkSummary highlights top-level outcomes and target qualification.
type BenchmarkSummary struct {
	MaxSavingsPercent  float64          `json:"maxSavingsPercent"`
	MaxTokensSaved     int              `json:"maxTokensSaved"`
	Target80PercentMet bool             `json:"target80PercentMet"`
	RecommendedTier    pruner.PruneTier `json:"recommendedTier"`
}

// Report encapsulates the complete benchmark results across tiers and tools.
type Report struct {
	Timestamp  time.Time        `json:"timestamp"`
	ToolCount  int              `json:"toolCount"`
	Tokenizer  string           `json:"tokenizer"`
	RawTokens  int              `json:"rawTokens"`
	RawBytes   int              `json:"rawBytes"`
	Tiers      []TierReport     `json:"tiers"`
	BudgetTier *TierReport      `json:"budgetTier,omitempty"`
	Tools      []ToolReport     `json:"tools"`
	Summary    BenchmarkSummary `json:"summary"`
}

// RunBenchmark runs the standard benchmark suite on a tool slice using default options.
func RunBenchmark(tools []mcp.Tool) (*Report, error) {
	return RunBenchmarkWithOptions(tools, DefaultBenchmarkOptions())
}

// RunBenchmarkWithOptions runs the benchmark suite using customized options.
func RunBenchmarkWithOptions(tools []mcp.Tool, opts BenchmarkOptions) (*Report, error) {
	if len(tools) == 0 {
		return nil, fmt.Errorf("cannot benchmark empty tool set")
	}

	report := &Report{
		Timestamp: time.Now().UTC(),
		ToolCount: len(tools),
		Tokenizer: DefaultEncoding,
		Tiers:     make([]TierReport, 0, 5),
		Tools:     make([]ToolReport, 0, len(tools)),
	}

	tokenCounter := func(val any) int {
		b, err := json.Marshal(val)
		if err != nil {
			return 0
		}
		if opts.UseExactTokens {
			toks, err := CountTokens(string(b))
			if err == nil {
				return toks
			}
		}
		report.Tokenizer = "calibrated"
		return EstimateTokens(string(b))
	}

	// ------------------------------------------------------------------------
	// Tier 0: Raw Pass-Through (Baseline)
	// ------------------------------------------------------------------------
	rawBytes, _ := json.Marshal(tools)
	rawTokens := tokenCounter(tools)
	report.RawTokens = rawTokens
	report.RawBytes = len(rawBytes)

	t0Valid, t0Errs := validateTools(tools, opts.ValidateSchemas)
	report.Tiers = append(report.Tiers, TierReport{
		Tier:             pruner.TierNone,
		Name:             "Tier 0: Raw Unpruned",
		Tokens:           rawTokens,
		Bytes:            len(rawBytes),
		TokensSaved:      0,
		SavingsPercent:   0.0,
		TokensPerTool:    float64(rawTokens) / float64(len(tools)),
		ValidDraft07:     t0Valid,
		ValidationErrors: t0Errs,
	})

	// ------------------------------------------------------------------------
	// Tier 1: Lossless Minification
	// ------------------------------------------------------------------------
	t1Engine := pruner.New(pruner.DefaultOptions(pruner.TierLossless))
	t1Tools, _, err := t1Engine.PruneToolsWithTier(tools, pruner.TierLossless)
	if err != nil {
		return nil, fmt.Errorf("tier 1 pruning failed: %w", err)
	}
	t1Bytes, _ := json.Marshal(t1Tools)
	t1Tokens := tokenCounter(t1Tools)
	t1Saved := rawTokens - t1Tokens
	t1Pct := calculateSavings(rawTokens, t1Tokens)
	t1Valid, t1Errs := validateTools(t1Tools, opts.ValidateSchemas)

	report.Tiers = append(report.Tiers, TierReport{
		Tier:             pruner.TierLossless,
		Name:             "Tier 1: Lossless Minify",
		Tokens:           t1Tokens,
		Bytes:            len(t1Bytes),
		TokensSaved:      t1Saved,
		SavingsPercent:   t1Pct,
		TokensPerTool:    float64(t1Tokens) / float64(len(tools)),
		ValidDraft07:     t1Valid,
		ValidationErrors: t1Errs,
	})

	// ------------------------------------------------------------------------
	// Tier 2: Description Condensation & Regex Squeezing
	// ------------------------------------------------------------------------
	t2Engine := pruner.New(pruner.DefaultOptions(pruner.TierCompact))
	t2Tools, _, err := t2Engine.PruneToolsWithTier(tools, pruner.TierCompact)
	if err != nil {
		return nil, fmt.Errorf("tier 2 pruning failed: %w", err)
	}
	t2Bytes, _ := json.Marshal(t2Tools)
	t2Tokens := tokenCounter(t2Tools)
	t2Saved := rawTokens - t2Tokens
	t2Pct := calculateSavings(rawTokens, t2Tokens)
	t2Valid, t2Errs := validateTools(t2Tools, opts.ValidateSchemas)

	report.Tiers = append(report.Tiers, TierReport{
		Tier:             pruner.TierCompact,
		Name:             "Tier 2: Condensation",
		Tokens:           t2Tokens,
		Bytes:            len(t2Bytes),
		TokensSaved:      t2Saved,
		SavingsPercent:   t2Pct,
		TokensPerTool:    float64(t2Tokens) / float64(len(tools)),
		ValidDraft07:     t2Valid,
		ValidationErrors: t2Errs,
	})

	// ------------------------------------------------------------------------
	// Tier 3: Aggressive Parameter Pruning
	// ------------------------------------------------------------------------
	t3Engine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	t3Tools, _, err := t3Engine.PruneToolsWithTier(tools, pruner.TierAggressive)
	if err != nil {
		return nil, fmt.Errorf("tier 3 pruning failed: %w", err)
	}
	t3Bytes, _ := json.Marshal(t3Tools)
	t3Tokens := tokenCounter(t3Tools)
	t3Saved := rawTokens - t3Tokens
	t3Pct := calculateSavings(rawTokens, t3Tokens)
	t3Valid, t3Errs := validateTools(t3Tools, opts.ValidateSchemas)

	report.Tiers = append(report.Tiers, TierReport{
		Tier:             pruner.TierAggressive,
		Name:             "Tier 3: Parameter Prune",
		Tokens:           t3Tokens,
		Bytes:            len(t3Bytes),
		TokensSaved:      t3Saved,
		SavingsPercent:   t3Pct,
		TokensPerTool:    float64(t3Tokens) / float64(len(tools)),
		ValidDraft07:     t3Valid,
		ValidationErrors: t3Errs,
	})

	// ------------------------------------------------------------------------
	// Tier 4: Dynamic Catalog Projection
	// ------------------------------------------------------------------------
	t4Tools := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		t4Tools[i] = router.ProjectCompactTool(t, opts.CatalogConfig)
	}
	t4Bytes, _ := json.Marshal(t4Tools)
	t4Tokens := tokenCounter(t4Tools)
	t4Saved := rawTokens - t4Tokens
	t4Pct := calculateSavings(rawTokens, t4Tokens)
	t4Valid, t4Errs := validateTools(t4Tools, opts.ValidateSchemas)

	report.Tiers = append(report.Tiers, TierReport{
		Tier:             pruner.TierDynamic,
		Name:             "Tier 4: Dynamic Catalog",
		Tokens:           t4Tokens,
		Bytes:            len(t4Bytes),
		TokensSaved:      t4Saved,
		SavingsPercent:   t4Pct,
		TokensPerTool:    float64(t4Tokens) / float64(len(tools)),
		ValidDraft07:     t4Valid,
		ValidationErrors: t4Errs,
	})

	// ------------------------------------------------------------------------
	// Optional RFC #1 Token Budget Evaluation
	// ------------------------------------------------------------------------
	if opts.MaxSchemaTokens > 0 {
		bOpts := pruner.DefaultOptions(pruner.TierAggressive)
		bOpts.MaxSchemaTokens = opts.MaxSchemaTokens
		bEngine := pruner.New(bOpts)
		bTools, _, bErr := bEngine.PruneToolsWithTier(tools, pruner.TierAggressive)
		if bErr != nil {
			return nil, fmt.Errorf("token budget pruning failed: %w", bErr)
		}
		bBytes, _ := json.Marshal(bTools)
		bTokens := tokenCounter(bTools)
		bSaved := rawTokens - bTokens
		bPct := calculateSavings(rawTokens, bTokens)
		bValid, bErrs := validateTools(bTools, opts.ValidateSchemas)
		report.BudgetTier = &TierReport{
			Tier:             pruner.PruneTier("budget"),
			Name:             fmt.Sprintf("Budget (<=%d tok)", opts.MaxSchemaTokens),
			Tokens:           bTokens,
			Bytes:            len(bBytes),
			TokensSaved:      bSaved,
			SavingsPercent:   bPct,
			TokensPerTool:    float64(bTokens) / float64(len(tools)),
			ValidDraft07:     bValid,
			ValidationErrors: bErrs,
		}
	}

	// ------------------------------------------------------------------------
	// Per-Tool Breakdown (+ Optional Verbose Per-Parameter Breakdown)
	// ------------------------------------------------------------------------
	for idx, tool := range tools {
		single := []mcp.Tool{tool}
		rawSingle := tokenCounter(single)

		t1Single, _, _ := t1Engine.PruneToolsWithTier(single, pruner.TierLossless)
		t1SingleTok := tokenCounter(t1Single)

		t2Single, _, _ := t2Engine.PruneToolsWithTier(single, pruner.TierCompact)
		t2SingleTok := tokenCounter(t2Single)

		t3Single, _, _ := t3Engine.PruneToolsWithTier(single, pruner.TierAggressive)
		t3SingleTok := tokenCounter(t3Single)

		t4Single := []mcp.Tool{router.ProjectCompactTool(tool, opts.CatalogConfig)}
		t4SingleTok := tokenCounter(t4Single)

		maxSave := calculateSavings(rawSingle, t4SingleTok)

		var paramStats []ParamTokenStat
		if opts.IncludeParamBreakdown && len(tool.InputSchema.Properties) > 0 {
			reqSet := make(map[string]bool, len(tool.InputSchema.Required))
			for _, reqName := range tool.InputSchema.Required {
				reqSet[reqName] = true
			}
			names := make([]string, 0, len(tool.InputSchema.Properties))
			for pName := range tool.InputSchema.Properties {
				names = append(names, pName)
			}
			sort.Strings(names)

			var prunedProps map[string]any
			if idx < len(t3Tools) {
				prunedProps = t3Tools[idx].InputSchema.Properties
			}

			paramStats = make([]ParamTokenStat, 0, len(names))
			for _, pName := range names {
				rawProp := map[string]any{pName: tool.InputSchema.Properties[pName]}
				var t3PropTok int
				if prunedProps != nil {
					if pv, exists := prunedProps[pName]; exists {
						t3PropTok = tokenCounter(map[string]any{pName: pv})
					}
				}
				paramStats = append(paramStats, ParamTokenStat{
					Name:        pName,
					Required:    reqSet[pName],
					RawTokens:   tokenCounter(rawProp),
					Tier3Tokens: t3PropTok,
				})
			}
		}

		report.Tools = append(report.Tools, ToolReport{
			ToolName:          tool.Name,
			RawTokens:         rawSingle,
			Tier1Tokens:       t1SingleTok,
			Tier2Tokens:       t2SingleTok,
			Tier3Tokens:       t3SingleTok,
			Tier4Tokens:       t4SingleTok,
			MaxSavingsPercent: maxSave,
			Parameters:        paramStats,
		})
	}

	// ------------------------------------------------------------------------
	// Summary Formulation
	// ------------------------------------------------------------------------
	report.Summary = BenchmarkSummary{
		MaxSavingsPercent:  t4Pct,
		MaxTokensSaved:     t4Saved,
		Target80PercentMet: t4Pct >= 80.0,
		RecommendedTier:    pruner.TierAggressive,
	}
	if t4Pct >= 80.0 {
		report.Summary.RecommendedTier = pruner.TierDynamic
	}

	return report, nil
}

// validateTools runs Draft-07 validation guardrail across a tool slice.
func validateTools(tools []mcp.Tool, validate bool) (bool, []string) {
	if !validate {
		return true, nil
	}
	var errs []string
	for _, tool := range tools {
		res := pruner.ValidateTool(tool)
		if !res.Valid {
			errs = append(errs, fmt.Sprintf("%s: %v", tool.Name, res.Errors))
		}
	}
	return len(errs) == 0, errs
}

// calculateSavings computes percentage reduction from raw to pruned.
func calculateSavings(raw, pruned int) float64 {
	if raw <= 0 {
		return 0.0
	}
	if pruned >= raw {
		return 0.0
	}
	return (float64(raw-pruned) / float64(raw)) * 100.0
}

// LoadFixtures loads tool definitions from candidates or custom paths.
func LoadFixtures(customPath ...string) ([]mcp.Tool, error) {
	candidates := customPath
	if len(candidates) == 0 {
		candidates = []string{
			"test/fixtures/schemas.json",
			filepath.Join("..", "..", "test", "fixtures", "schemas.json"),
			filepath.Join("test", "fixtures", "schemas.json"),
		}
	}

	var data []byte
	var err error
	for _, p := range candidates {
		data, err = os.ReadFile(p)
		if err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("unable to load schemas fixture from candidates: %w", err)
	}

	var tools []mcp.Tool
	if err := json.Unmarshal(data, &tools); err != nil {
		return nil, fmt.Errorf("failed to unmarshal tools JSON: %w", err)
	}

	return tools, nil
}
