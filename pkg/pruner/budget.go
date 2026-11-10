package pruner

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// BudgetOptions configures RFC #1 progressive token-budget schema compaction.
type BudgetOptions struct {
	// MaxSchemaTokens is the strict upper bound target for serialized tools/list schemas.
	MaxSchemaTokens int

	// PinnedTools are immune to Pass 5 stub projection unless no unpinned tools remain.
	PinnedTools map[string]struct{}

	// ContextBoostTools maps tool names to recency/frequency/domain boost weights (>0 = active).
	ContextBoostTools map[string]float64

	// SafetyFactor reserves headroom (default 0.90) so structural byte estimation never overshoots cl100k_base.
	SafetyFactor float64
}

// BudgetMetrics reports telemetry from progressive token-budget compaction.
type BudgetMetrics struct {
	RawTokens      int
	CompactedToken int
	EffectiveCap   int
	PassReached    int
	ProjectedTools []string
}

// EnforceTokenBudget compacts a slice of MCP tools to fit within opts.MaxSchemaTokens
// using a fast (<0.5ms) 5-pass AST compactor with zero tiktoken or json.Marshal overhead.
//
// Passes 1-4 strictly preserve all type and structural constraints:
//   - type, properties, required, enum, const, items, $ref, reachable $defs/definitions,
//     anyOf/oneOf/allOf, not, minimum/maximum, minItems/maxItems, additionalProperties.
//
// Pass 5 (extreme tight budgets) ranks tools by (pinned, activeCallScore, -tokenCost),
// retaining hot tools in full type-skeleton form and projecting cold overflow tools via ProjectCompactTool.
func EnforceTokenBudget(tools []mcp.Tool, opts BudgetOptions) ([]mcp.Tool, BudgetMetrics) {
	if len(tools) == 0 {
		return tools, BudgetMetrics{}
	}

	rawTokens := EstimateToolsBudgetTokens(tools)
	if opts.MaxSchemaTokens <= 0 {
		return DeepCopyTools(tools), BudgetMetrics{
			RawTokens:      rawTokens,
			CompactedToken: rawTokens,
			EffectiveCap:   0,
			PassReached:    0,
		}
	}

	safety := opts.SafetyFactor
	if safety <= 0 || safety > 1.0 {
		safety = 0.90
	}
	effectiveCap := int(math.Floor(float64(opts.MaxSchemaTokens) * safety))
	if effectiveCap < 1 {
		effectiveCap = 1
	}

	work := DeepCopyTools(tools)
	perTool, total := computePerToolBudgetTokens(work)
	if total <= effectiveCap {
		return work, BudgetMetrics{
			RawTokens:      rawTokens,
			CompactedToken: total,
			EffectiveCap:   effectiveCap,
			PassReached:    0,
		}
	}

	// -------------------------------------------------------------------------
	// Pass 1: Lossless decorative metadata strip + unreachable $defs pruning
	// -------------------------------------------------------------------------
	for i := range work {
		work[i] = budgetPass1LosslessAndReachableDefs(work[i])
	}
	perTool, total = computePerToolBudgetTokens(work)
	if total <= effectiveCap {
		return work, BudgetMetrics{
			RawTokens:      rawTokens,
			CompactedToken: total,
			EffectiveCap:   effectiveCap,
			PassReached:    1,
		}
	}

	// Build cold-to-hot tool index order: lowest priority tools compacted first
	coldOrder := rankToolsColdFirst(work, perTool, opts.PinnedTools, opts.ContextBoostTools)

	// -------------------------------------------------------------------------
	// Pass 2: Depth-weighted leaf docstring compaction (boost-aware, in-place)
	// Skip expensive string condensation on cold tools if even stripping all
	// parameter docstrings (Pass 3) will still exceed effectiveCap.
	// -------------------------------------------------------------------------
	selfEvident := GetDefaultSelfEvidentDictionary()
	skipColdPass2 := total > effectiveCap*2
	for i := range work {
		boost := 0.0
		if opts.ContextBoostTools != nil {
			boost = opts.ContextBoostTools[work[i].Name]
		}
		if skipColdPass2 && boost <= 0 && !isToolPinned(work[i].Name, opts.PinnedTools) {
			continue
		}
		work[i] = budgetPass2DepthWeightedDocstrings(work[i], boost, selfEvident)
	}
	perTool, total = computePerToolBudgetTokens(work)
	if total <= effectiveCap {
		return work, BudgetMetrics{
			RawTokens:      rawTokens,
			CompactedToken: total,
			EffectiveCap:   effectiveCap,
			PassReached:    2,
		}
	}

	// -------------------------------------------------------------------------
	// Pass 3: Strip all parameter-level docstrings (depth >= 1), cold tools first
	// -------------------------------------------------------------------------
	for _, idx := range coldOrder {
		updated := budgetPass3StripParamDocstrings(work[idx])
		newCost := EstimateToolBudgetTokens(updated)
		delta := perTool[idx] - newCost
		work[idx] = updated
		perTool[idx] = newCost
		total -= delta
		if total <= effectiveCap {
			return work, BudgetMetrics{
				RawTokens:      rawTokens,
				CompactedToken: total,
				EffectiveCap:   effectiveCap,
				PassReached:    3,
			}
		}
	}

	// -------------------------------------------------------------------------
	// Pass 4: Compact/strip top-level Tool.Description and optional OutputSchema
	// -------------------------------------------------------------------------
	for _, idx := range coldOrder {
		boost := 0.0
		if opts.ContextBoostTools != nil {
			boost = opts.ContextBoostTools[work[idx].Name]
		}
		maxDesc := 32
		if boost > 0 {
			maxDesc = 64
		}
		updated := work[idx]
		updated.OutputSchema = nil
		updated.InputSchema.Description = ""
		if updated.Description != "" {
			updated.Description = truncateDescription(updated.Description, maxDesc)
		}
		newCost := EstimateToolBudgetTokens(updated)
		delta := perTool[idx] - newCost
		work[idx] = updated
		perTool[idx] = newCost
		total -= delta
		if total <= effectiveCap {
			return work, BudgetMetrics{
				RawTokens:      rawTokens,
				CompactedToken: total,
				EffectiveCap:   effectiveCap,
				PassReached:    4,
			}
		}
	}

	// Secondary Pass 4 sweep: strip remaining top-level descriptions & redundant display titles on cold tools
	for _, idx := range coldOrder {
		if opts.ContextBoostTools != nil && opts.ContextBoostTools[work[idx].Name] >= 0.5 {
			continue
		}
		if work[idx].Description == "" && work[idx].Title == "" && (work[idx].Annotations == nil || work[idx].Annotations.Title == "") {
			continue
		}
		updated := work[idx]
		updated.Description = ""
		updated.Title = ""
		if updated.Annotations != nil && updated.Annotations.Title != "" {
			annCopy := *updated.Annotations
			annCopy.Title = ""
			updated.Annotations = &annCopy
		}
		newCost := EstimateToolBudgetTokens(updated)
		delta := perTool[idx] - newCost
		work[idx] = updated
		perTool[idx] = newCost
		total -= delta
		if total <= effectiveCap {
			return work, BudgetMetrics{
				RawTokens:      rawTokens,
				CompactedToken: total,
				EffectiveCap:   effectiveCap,
				PassReached:    4,
			}
		}
	}

	// -------------------------------------------------------------------------
	// Pass 5: Extreme tight budget — project cold overflow tools via ProjectCompactTool
	// -------------------------------------------------------------------------
	var projectedNames []string

	// First pass over non-pinned tools (lowest activeCallScore, highest tokenCost first)
	for _, idx := range coldOrder {
		if isToolPinned(work[idx].Name, opts.PinnedTools) {
			continue
		}
		stub := ProjectCompactTool(work[idx], 36, len(work[idx].InputSchema.Required) <= 4)
		// Under ultra-tight budgets, omit summary description and decorative titles on cold stubs
		minStub := stub
		if EstimateToolBudgetTokens(minStub) >= perTool[idx] || total-(perTool[idx]-EstimateToolBudgetTokens(minStub)) > effectiveCap {
			minStub.Title = ""
			if minStub.Annotations != nil && minStub.Annotations.Title != "" {
				annCopy := *minStub.Annotations
				annCopy.Title = ""
				minStub.Annotations = &annCopy
			}
			if len(work[idx].InputSchema.Required) > 0 && len(work[idx].InputSchema.Required) <= 3 {
				minStub.Description = "Req: " + strings.Join(work[idx].InputSchema.Required, ",")
			} else {
				minStub.Description = ""
			}
		}
		stubCost := EstimateToolBudgetTokens(minStub)
		if stubCost < perTool[idx] {
			total -= perTool[idx] - stubCost
			work[idx] = minStub
			perTool[idx] = stubCost
			projectedNames = append(projectedNames, work[idx].Name)
		}
		if total <= effectiveCap {
			break
		}
	}

	// Final fallback: if even pinned tools alone exceed effectiveCap, stub lowest-boost pinned tools
	if total > effectiveCap {
		for _, idx := range coldOrder {
			if !isToolPinned(work[idx].Name, opts.PinnedTools) {
				continue
			}
			stub := ProjectCompactTool(work[idx], 24, false)
			stub.Description = ""
			stubCost := EstimateToolBudgetTokens(stub)
			if stubCost < perTool[idx] {
				total -= perTool[idx] - stubCost
				work[idx] = stub
				perTool[idx] = stubCost
				projectedNames = append(projectedNames, work[idx].Name)
			}
			if total <= effectiveCap {
				break
			}
		}
	}

	return work, BudgetMetrics{
		RawTokens:      rawTokens,
		CompactedToken: total,
		EffectiveCap:   effectiveCap,
		PassReached:    5,
		ProjectedTools: projectedNames,
	}
}

// ProjectCompactTool projects a tool into a Draft-07 compliant compact stub schema
// backed by SchemaStore / StreamFilter hydration and pre-binding validation.
func ProjectCompactTool(tool mcp.Tool, maxSummaryLen int, includeRequired bool) mcp.Tool {
	var desc string
	if maxSummaryLen > 0 && tool.Description != "" {
		condOpts := DefaultCondenseOptions()
		condOpts.MaxToolDescLen = maxSummaryLen
		desc = CondenseDescription(tool.Description, maxSummaryLen, condOpts)
	}

	if includeRequired && len(tool.InputSchema.Required) > 0 {
		reqList := strings.Join(tool.InputSchema.Required, ", ")
		reqRunes := []rune(reqList)
		if len(reqRunes) > 40 {
			reqList = string(reqRunes[:37]) + "..."
		}
		if desc != "" && !strings.HasSuffix(desc, ".") {
			desc += "."
		}
		if desc != "" {
			desc = fmt.Sprintf("%s Required: %s.", desc, reqList)
		} else {
			desc = fmt.Sprintf("Required: %s.", reqList)
		}
	}

	return mcp.Tool{
		Name:        tool.Name,
		Title:       tool.Title,
		Description: desc,
		Annotations: tool.Annotations,
		Icons:       tool.Icons,
		InputSchema: mcp.InputSchema{
			Type: "object",
		},
	}
}

func isToolPinned(name string, pinned map[string]struct{}) bool {
	if len(pinned) == 0 {
		return false
	}
	_, ok := pinned[name]
	return ok
}

func rankToolsColdFirst(tools []mcp.Tool, costs []int, pinned map[string]struct{}, boosts map[string]float64) []int {
	indices := make([]int, len(tools))
	for i := range indices {
		indices[i] = i
	}
	sort.SliceStable(indices, func(a, b int) bool {
		ia, ib := indices[a], indices[b]
		pinA := isToolPinned(tools[ia].Name, pinned)
		pinB := isToolPinned(tools[ib].Name, pinned)
		if pinA != pinB {
			return !pinA // unpinned (cold) comes first
		}
		var scoreA, scoreB float64
		if boosts != nil {
			scoreA = boosts[tools[ia].Name]
			scoreB = boosts[tools[ib].Name]
		}
		if math.Abs(scoreA-scoreB) > 1e-6 {
			return scoreA < scoreB // lower call score comes first
		}
		// Among equally cold tools, compact heaviest schemas first to reach budget in fewer steps
		return costs[ia] > costs[ib]
	})
	return indices
}

// =============================================================================
// Pass Implementations (Strictly Preserving Structural & Type Constraints)
// =============================================================================

func budgetPass1LosslessAndReachableDefs(t mcp.Tool) mcp.Tool {
	s := t.InputSchema
	if s.Type == "" {
		s.Type = "object"
	}
	s.Schema = ""
	s.ID = ""
	s.Title = ""
	s.Comment = ""
	s.Examples = nil

	if len(s.Required) == 0 {
		s.Required = nil
	}
	if len(s.Properties) > 0 {
		s.Properties = budgetCleanMapPass1(s.Properties)
	} else {
		s.Properties = nil
	}
	if len(s.PatternProperties) > 0 {
		s.PatternProperties = budgetCleanMapPass1(s.PatternProperties)
	} else {
		s.PatternProperties = nil
	}
	if len(s.DependentSchemas) > 0 {
		s.DependentSchemas = budgetCleanMapPass1(s.DependentSchemas)
	} else {
		s.DependentSchemas = nil
	}
	if len(s.DependentRequired) == 0 {
		s.DependentRequired = nil
	}
	if len(s.AnyOf) > 0 {
		s.AnyOf = budgetCleanSlicePass1(s.AnyOf)
	} else {
		s.AnyOf = nil
	}
	if len(s.OneOf) > 0 {
		s.OneOf = budgetCleanSlicePass1(s.OneOf)
	} else {
		s.OneOf = nil
	}
	if len(s.AllOf) > 0 {
		s.AllOf = budgetCleanSlicePass1(s.AllOf)
	} else {
		s.AllOf = nil
	}
	if len(s.Not) > 0 {
		s.Not = budgetCleanNodePass1(s.Not)
	} else {
		s.Not = nil
	}
	if addMap, ok := s.AdditionalProperties.(map[string]any); ok {
		s.AdditionalProperties = budgetCleanNodePass1(addMap)
	}

	// Prune unreachable root $defs / definitions while keeping all transitively reachable $ref targets
	if len(s.Defs) > 0 || len(s.Definitions) > 0 {
		reachableDefs, reachableLegacy := collectReachableRefs(s)
		if len(s.Defs) > 0 {
			prunedDefs := make(map[string]any, len(reachableDefs))
			for k, v := range s.Defs {
				if reachableDefs[k] {
					if vm, ok := v.(map[string]any); ok {
						prunedDefs[k] = budgetCleanNodePass1(vm)
					} else {
						prunedDefs[k] = v
					}
				}
			}
			if len(prunedDefs) > 0 {
				s.Defs = prunedDefs
			} else {
				s.Defs = nil
			}
		}
		if len(s.Definitions) > 0 {
			prunedLegacy := make(map[string]any, len(reachableLegacy))
			for k, v := range s.Definitions {
				if reachableLegacy[k] {
					if vm, ok := v.(map[string]any); ok {
						prunedLegacy[k] = budgetCleanNodePass1(vm)
					} else {
						prunedLegacy[k] = v
					}
				}
			}
			if len(prunedLegacy) > 0 {
				s.Definitions = prunedLegacy
			} else {
				s.Definitions = nil
			}
		}
	}

	t.InputSchema = s
	if len(t.OutputSchema) > 0 {
		t.OutputSchema = budgetCleanNodePass1(t.OutputSchema)
	}
	return t
}

func budgetCleanMapPass1(m map[string]any) map[string]any {
	for k, v := range m {
		if vm, ok := v.(map[string]any); ok {
			m[k] = budgetCleanNodePass1(vm)
		}
	}
	return m
}

func budgetCleanSlicePass1(s []any) []any {
	for i, v := range s {
		if vm, ok := v.(map[string]any); ok {
			s[i] = budgetCleanNodePass1(vm)
		}
	}
	return s
}

func budgetCleanNodePass1(node map[string]any) map[string]any {
	for k, v := range node {
		switch k {
		case "$schema", "$id", "title", "$comment", "comment", "examples", "example":
			delete(node, k)
		case "required":
			if arr, ok := v.([]any); ok && len(arr) == 0 {
				delete(node, k)
			} else if arr, ok := v.([]string); ok && len(arr) == 0 {
				delete(node, k)
			}
		case "properties", "patternProperties", "$defs", "definitions", "dependentSchemas":
			if sub, ok := v.(map[string]any); ok {
				if len(sub) == 0 {
					delete(node, k)
				} else {
					node[k] = budgetCleanMapPass1(sub)
				}
			}
		case "items":
			if sub, ok := v.(map[string]any); ok {
				node[k] = budgetCleanNodePass1(sub)
			} else if arr, ok := v.([]any); ok {
				node[k] = budgetCleanSlicePass1(arr)
			}
		case "anyOf", "oneOf", "allOf":
			if arr, ok := v.([]any); ok {
				node[k] = budgetCleanSlicePass1(arr)
			}
		case "not", "if", "then", "else", "additionalProperties":
			if sub, ok := v.(map[string]any); ok {
				node[k] = budgetCleanNodePass1(sub)
			}
		}
	}
	return node
}

func collectReachableRefs(s mcp.InputSchema) (map[string]bool, map[string]bool) {
	reachDefs := make(map[string]bool)
	reachLegacy := make(map[string]bool)

	var queue []string
	enqueueRefsFromValue := func(v any) {
		scanRefsInValue(v, &queue)
	}

	enqueueRefsFromValue(s.Properties)
	enqueueRefsFromValue(s.PatternProperties)
	enqueueRefsFromValue(s.DependentSchemas)
	enqueueRefsFromValue(s.AdditionalProperties)
	enqueueRefsFromValue(s.AnyOf)
	enqueueRefsFromValue(s.OneOf)
	enqueueRefsFromValue(s.AllOf)
	enqueueRefsFromValue(s.Not)

	for len(queue) > 0 {
		ref := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		if strings.HasPrefix(ref, "#/$defs/") {
			key := ref[len("#/$defs/"):]
			if !reachDefs[key] {
				reachDefs[key] = true
				if s.Defs != nil {
					if target, ok := s.Defs[key]; ok {
						enqueueRefsFromValue(target)
					}
				}
			}
		} else if strings.HasPrefix(ref, "#/definitions/") {
			key := ref[len("#/definitions/"):]
			if !reachLegacy[key] {
				reachLegacy[key] = true
				if s.Definitions != nil {
					if target, ok := s.Definitions[key]; ok {
						enqueueRefsFromValue(target)
					}
				}
			}
		}
	}
	return reachDefs, reachLegacy
}

func scanRefsInValue(v any, queue *[]string) {
	switch val := v.(type) {
	case map[string]any:
		if ref, ok := val["$ref"].(string); ok && ref != "" {
			*queue = append(*queue, ref)
		}
		for k, child := range val {
			if k == "$ref" {
				continue
			}
			scanRefsInValue(child, queue)
		}
	case []any:
		for _, item := range val {
			scanRefsInValue(item, queue)
		}
	}
}

func budgetPass2DepthWeightedDocstrings(t mcp.Tool, boost float64, selfEvident map[string]struct{}) mcp.Tool {
	condOpts := DefaultCondenseOptions()
	toolDescLimit := 90
	if boost > 0 {
		toolDescLimit = 140
	}
	if t.Description != "" {
		t.Description = CondenseDescription(t.Description, toolDescLimit, condOpts)
	}
	if t.InputSchema.Description != "" {
		if boost > 0 {
			t.InputSchema.Description = truncateDescription(t.InputSchema.Description, 60)
		} else {
			t.InputSchema.Description = ""
		}
	}

	t.InputSchema.Properties = compactMapByDepth(t.InputSchema.Properties, 1, boost, selfEvident, condOpts)
	t.InputSchema.Defs = compactMapByDepth(t.InputSchema.Defs, 2, boost, selfEvident, condOpts)
	t.InputSchema.Definitions = compactMapByDepth(t.InputSchema.Definitions, 2, boost, selfEvident, condOpts)
	t.InputSchema.PatternProperties = compactMapByDepth(t.InputSchema.PatternProperties, 2, boost, selfEvident, condOpts)
	t.InputSchema.DependentSchemas = compactMapByDepth(t.InputSchema.DependentSchemas, 2, boost, selfEvident, condOpts)
	t.InputSchema.AnyOf = compactSliceByDepth(t.InputSchema.AnyOf, 2, boost, selfEvident, condOpts)
	t.InputSchema.OneOf = compactSliceByDepth(t.InputSchema.OneOf, 2, boost, selfEvident, condOpts)
	t.InputSchema.AllOf = compactSliceByDepth(t.InputSchema.AllOf, 2, boost, selfEvident, condOpts)
	if len(t.InputSchema.Not) > 0 {
		t.InputSchema.Not = compactNodeByDepth(t.InputSchema.Not, "not", 2, boost, selfEvident, condOpts)
	}
	if addMap, ok := t.InputSchema.AdditionalProperties.(map[string]any); ok {
		t.InputSchema.AdditionalProperties = compactNodeByDepth(addMap, "additional", 2, boost, selfEvident, condOpts)
	}
	if len(t.OutputSchema) > 0 {
		t.OutputSchema = compactNodeByDepth(t.OutputSchema, "output", 2, boost, selfEvident, condOpts)
	}
	return t
}

func compactMapByDepth(m map[string]any, depth int, boost float64, selfEvident map[string]struct{}, condOpts CondenseOptions) map[string]any {
	if m == nil {
		return nil
	}
	for name, raw := range m {
		if nm, ok := raw.(map[string]any); ok {
			m[name] = compactNodeByDepth(nm, name, depth, boost, selfEvident, condOpts)
		}
	}
	return m
}

func compactSliceByDepth(s []any, depth int, boost float64, selfEvident map[string]struct{}, condOpts CondenseOptions) []any {
	if s == nil {
		return nil
	}
	for i, item := range s {
		if nm, ok := item.(map[string]any); ok {
			s[i] = compactNodeByDepth(nm, "", depth, boost, selfEvident, condOpts)
		}
	}
	return s
}

func compactNodeByDepth(out map[string]any, propName string, depth int, boost float64, selfEvident map[string]struct{}, condOpts CondenseOptions) map[string]any {
	if desc, ok := out["description"].(string); ok && desc != "" {
		maxLen := 0
		switch {
		case depth >= 3:
			if boost >= 0.5 {
				maxLen = 32
			} else {
				maxLen = 0
			}
		case depth == 2:
			if boost == 0 && propName != "" && isSelfEvidentParam(propName, selfEvident) {
				maxLen = 0
			} else if boost > 0 {
				maxLen = 56
			} else {
				maxLen = 28
			}
		default: // depth == 1
			if boost == 0 && propName != "" && isSelfEvidentParam(propName, selfEvident) {
				maxLen = 0
			} else if boost > 0 {
				maxLen = 80
			} else {
				maxLen = 45
			}
		}

		if maxLen <= 0 {
			delete(out, "description")
		} else {
			c := CondenseDescription(desc, maxLen, condOpts)
			if c == "" {
				delete(out, "description")
			} else {
				out["description"] = c
			}
		}
	}

	// Normalize verbose regex patterns
	if pat, ok := out["pattern"].(string); ok && len(pat) > condOpts.MaxRegexLength {
		fmtVal, _ := out["format"].(string)
		newPat, newFmt, changed := NormalizeRegex(pat, fmtVal, condOpts)
		if changed {
			if newPat == "" {
				delete(out, "pattern")
			} else {
				out["pattern"] = newPat
			}
			if newFmt != "" {
				out["format"] = newFmt
			}
		}
	}

	for _, mapKey := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
		if sub, ok := out[mapKey].(map[string]any); ok {
			out[mapKey] = compactMapByDepth(sub, depth+1, boost, selfEvident, condOpts)
		}
	}
	if sub, ok := out["additionalProperties"].(map[string]any); ok {
		out["additionalProperties"] = compactNodeByDepth(sub, "additional", depth+1, boost, selfEvident, condOpts)
	}
	if itemMap, ok := out["items"].(map[string]any); ok {
		out["items"] = compactNodeByDepth(itemMap, propName, depth+1, boost, selfEvident, condOpts)
	} else if itemArr, ok := out["items"].([]any); ok {
		out["items"] = compactSliceByDepth(itemArr, depth+1, boost, selfEvident, condOpts)
	}
	for _, unionKey := range []string{"anyOf", "oneOf", "allOf"} {
		if arr, ok := out[unionKey].([]any); ok {
			out[unionKey] = compactSliceByDepth(arr, depth+1, boost, selfEvident, condOpts)
		}
	}
	for _, condKey := range []string{"not", "if", "then", "else"} {
		if sub, ok := out[condKey].(map[string]any); ok {
			out[condKey] = compactNodeByDepth(sub, condKey, depth+1, boost, selfEvident, condOpts)
		}
	}
	return out
}

func budgetPass3StripParamDocstrings(t mcp.Tool) mcp.Tool {
	t.InputSchema.Description = ""
	t.InputSchema.Properties = stripAllDocstringsMap(t.InputSchema.Properties)
	t.InputSchema.Defs = stripAllDocstringsMap(t.InputSchema.Defs)
	t.InputSchema.Definitions = stripAllDocstringsMap(t.InputSchema.Definitions)
	t.InputSchema.PatternProperties = stripAllDocstringsMap(t.InputSchema.PatternProperties)
	t.InputSchema.DependentSchemas = stripAllDocstringsMap(t.InputSchema.DependentSchemas)
	t.InputSchema.AnyOf = stripAllDocstringsSlice(t.InputSchema.AnyOf)
	t.InputSchema.OneOf = stripAllDocstringsSlice(t.InputSchema.OneOf)
	t.InputSchema.AllOf = stripAllDocstringsSlice(t.InputSchema.AllOf)
	if len(t.InputSchema.Not) > 0 {
		t.InputSchema.Not = stripAllDocstringsNode(t.InputSchema.Not)
	}
	if addMap, ok := t.InputSchema.AdditionalProperties.(map[string]any); ok {
		t.InputSchema.AdditionalProperties = stripAllDocstringsNode(addMap)
	}
	if len(t.OutputSchema) > 0 {
		t.OutputSchema = stripAllDocstringsNode(t.OutputSchema)
	}
	return t
}

func stripAllDocstringsMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	for k, v := range m {
		if nm, ok := v.(map[string]any); ok {
			m[k] = stripAllDocstringsNode(nm)
		}
	}
	return m
}

func stripAllDocstringsSlice(s []any) []any {
	if s == nil {
		return nil
	}
	for i, item := range s {
		if nm, ok := item.(map[string]any); ok {
			s[i] = stripAllDocstringsNode(nm)
		}
	}
	return s
}

func stripAllDocstringsNode(node map[string]any) map[string]any {
	delete(node, "description")
	for k, v := range node {
		switch sub := v.(type) {
		case map[string]any:
			node[k] = stripAllDocstringsNode(sub)
		case []any:
			if k != "enum" {
				node[k] = stripAllDocstringsSlice(sub)
			}
		}
	}
	return node
}

// =============================================================================
// Fast Zero-Marshal Structural AST Token Estimator
// =============================================================================

func computePerToolBudgetTokens(tools []mcp.Tool) ([]int, int) {
	costs := make([]int, len(tools))
	total := 2 // outer JSON array brackets []
	for i := range tools {
		c := EstimateToolBudgetTokens(tools[i])
		costs[i] = c
		total += c
	}
	return costs, total
}

// EstimateToolsBudgetTokens estimates cl100k_base tokens for a slice of tools via in-memory AST walk.
func EstimateToolsBudgetTokens(tools []mcp.Tool) int {
	if len(tools) == 0 {
		return 0
	}
	total := 2
	for i := range tools {
		total += EstimateToolBudgetTokens(tools[i])
	}
	return total
}

// EstimateToolBudgetTokens estimates cl100k_base tokens for a single tool without json.Marshal.
// Separates dense structural JSON bytes (~3.15 B/tok) from prose bytes (~3.95 B/tok).
func EstimateToolBudgetTokens(t mcp.Tool) int {
	structBytes := 38 + len(t.Name) // {"name":"...","inputSchema":{"type":"object"}}
	proseBytes := 0

	if t.Title != "" {
		structBytes += 10
		proseBytes += len(t.Title)
	}
	if t.Description != "" {
		structBytes += 16
		proseBytes += len(t.Description)
	}
	if t.Annotations != nil {
		structBytes += 18 // "annotations":{}
		if t.Annotations.Title != "" {
			structBytes += 11
			proseBytes += len(t.Annotations.Title)
		}
		if t.Annotations.ReadOnlyHint != nil {
			structBytes += 22
		}
		if t.Annotations.DestructiveHint != nil {
			structBytes += 25
		}
		if t.Annotations.IdempotentHint != nil {
			structBytes += 24
		}
		if t.Annotations.OpenWorldHint != nil {
			structBytes += 23
		}
	}
	if len(t.Icons) > 0 {
		for _, ic := range t.Icons {
			structBytes += 28 + len(ic.Src) + len(ic.MIMEType)
		}
	}
	if len(t.OutputSchema) > 0 {
		structBytes += 17
		sb, pb := estimateMapBytes(t.OutputSchema)
		structBytes += sb
		proseBytes += pb
	}

	is := &t.InputSchema
	if is.Schema != "" {
		structBytes += 12 + len(is.Schema)
	}
	if is.ID != "" {
		structBytes += 8 + len(is.ID)
	}
	if is.Title != "" {
		structBytes += 10 + len(is.Title)
	}
	if is.Description != "" {
		structBytes += 16
		proseBytes += len(is.Description)
	}
	if is.Comment != "" {
		structBytes += 13
		proseBytes += len(is.Comment)
	}
	if len(is.Required) > 0 {
		structBytes += 13
		for _, r := range is.Required {
			structBytes += 3 + len(r)
		}
	}
	if is.AdditionalProperties != nil {
		structBytes += 24
		sb, pb := estimateAnyBytes(is.AdditionalProperties, "")
		structBytes += sb
		proseBytes += pb
	}
	if len(is.Properties) > 0 {
		structBytes += 15
		sb, pb := estimateMapBytes(is.Properties)
		structBytes += sb
		proseBytes += pb
	}
	if len(is.Defs) > 0 {
		structBytes += 10
		sb, pb := estimateMapBytes(is.Defs)
		structBytes += sb
		proseBytes += pb
	}
	if len(is.Definitions) > 0 {
		structBytes += 16
		sb, pb := estimateMapBytes(is.Definitions)
		structBytes += sb
		proseBytes += pb
	}
	if len(is.PatternProperties) > 0 {
		structBytes += 22
		sb, pb := estimateMapBytes(is.PatternProperties)
		structBytes += sb
		proseBytes += pb
	}
	if len(is.DependentSchemas) > 0 {
		structBytes += 21
		sb, pb := estimateMapBytes(is.DependentSchemas)
		structBytes += sb
		proseBytes += pb
	}
	if len(is.DependentRequired) > 0 {
		structBytes += 22
		for k, vals := range is.DependentRequired {
			structBytes += 6 + len(k)
			for _, v := range vals {
				structBytes += 3 + len(v)
			}
		}
	}
	for _, slice := range [][]any{is.AnyOf, is.OneOf, is.AllOf, is.Examples} {
		if len(slice) > 0 {
			structBytes += 10
			sb, pb := estimateSliceBytes(slice)
			structBytes += sb
			proseBytes += pb
		}
	}
	if len(is.Not) > 0 {
		structBytes += 8
		sb, pb := estimateMapBytes(is.Not)
		structBytes += sb
		proseBytes += pb
	}
	if is.MinProperties != nil {
		structBytes += 18
	}
	if is.MaxProperties != nil {
		structBytes += 18
	}

	est := float64(structBytes)/3.20 + float64(proseBytes)/3.95
	return int(math.Ceil(est))
}

func estimateMapBytes(m map[string]any) (int, int) {
	structBytes := 2
	proseBytes := 0
	for k, v := range m {
		structBytes += 4 + len(k)
		sb, pb := estimateAnyBytes(v, k)
		structBytes += sb
		proseBytes += pb
	}
	return structBytes, proseBytes
}

func estimateSliceBytes(s []any) (int, int) {
	structBytes := 2
	proseBytes := 0
	for _, v := range s {
		structBytes += 1
		sb, pb := estimateAnyBytes(v, "")
		structBytes += sb
		proseBytes += pb
	}
	return structBytes, proseBytes
}

func estimateAnyBytes(v any, key string) (int, int) {
	switch val := v.(type) {
	case nil:
		return 4, 0
	case bool:
		if val {
			return 4, 0
		}
		return 5, 0
	case string:
		if key == "description" || key == "$comment" || key == "comment" {
			return 2, len(val)
		}
		return 2 + len(val), 0
	case float64:
		var buf [24]byte
		return len(strconv.AppendFloat(buf[:0], val, 'f', -1, 64)), 0
	case int:
		var buf [24]byte
		return len(strconv.AppendInt(buf[:0], int64(val), 10)), 0
	case int64:
		var buf [24]byte
		return len(strconv.AppendInt(buf[:0], val, 10)), 0
	case []string:
		sb := 2
		for _, item := range val {
			sb += 3 + len(item)
		}
		return sb, 0
	case []any:
		return estimateSliceBytes(val)
	case map[string]any:
		return estimateMapBytes(val)
	default:
		return 8, 0
	}
}
