package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// BudgetContext carries dynamic call-context weights and token limits into the pruning engine.
type BudgetContext struct {
	MaxSchemaTokens   int
	PinnedTools       map[string]struct{}
	ContextBoostTools map[string]float64
}

// BudgetPruneMetrics extends PruneMetrics with RFC #1 budget pass diagnostics.
type BudgetPruneMetrics struct {
	PruneMetrics
	PassReached    int
	ProjectedTools []string
	CacheHit       bool
}

// ContextBudgetEngine is an optional extension of PruneEngine supporting RFC #1 token budgets.
type ContextBudgetEngine interface {
	PruneEngine
	PruneToolsWithContext(tools []mcp.Tool, ctx BudgetContext) ([]mcp.Tool, BudgetPruneMetrics, error)
}

// EngineConfigFingerprinter allows PruneEngine implementations to expose live runtime option changes to the cache key.
type EngineConfigFingerprinter interface {
	ConfigFingerprint() uint64
}

type callRecord struct {
	calls   uint64
	lastSeq uint64
	domain  string
}

type domainRecord struct {
	calls   uint64
	lastSeq uint64
}

type rawToolRecord struct {
	rawTool   mcp.Tool
	compacted bool
	stubbed   bool
}

type filterCacheEntry struct {
	key        uint64
	tools      []mcp.Tool
	metrics    BudgetPruneMetrics
	stubbedMap map[string]bool
}

const maxFilterCacheEntries = 32

// StreamFilterConfig configures the RFC #1 adaptive JSON-RPC stream filter.
type StreamFilterConfig struct {
	PruneMode       string
	MaxSchemaTokens int
	PinnedTools     []string
}

// StreamFilter intercepts tools/list and tools/call to enforce dynamic token budgets,
// track call-context affinity, cache fingerprinted lists, and pre-validate compacted calls.
type StreamFilter struct {
	cfgMu  sync.RWMutex
	cfg    StreamFilterConfig
	pinned map[string]struct{}

	seq       atomic.Uint64
	contextMu sync.RWMutex
	toolStats map[string]*callRecord
	domStats  map[string]*domainRecord

	regMu       sync.RWMutex
	rawRegistry map[string]rawToolRecord

	cacheMu sync.RWMutex
	cache   []filterCacheEntry
}

// NewStreamFilter creates an initialized adaptive StreamFilter.
func NewStreamFilter(cfg StreamFilterConfig) *StreamFilter {
	pinned := make(map[string]struct{}, len(cfg.PinnedTools))
	for _, p := range cfg.PinnedTools {
		if p != "" {
			pinned[p] = struct{}{}
		}
	}
	return &StreamFilter{
		cfg:         cfg,
		pinned:      pinned,
		toolStats:   make(map[string]*callRecord, 32),
		domStats:    make(map[string]*domainRecord, 16),
		rawRegistry: make(map[string]rawToolRecord, 64),
		cache:       make([]filterCacheEntry, 0, maxFilterCacheEntries),
	}
}

// UpdateConfig dynamically updates budget and mode settings and clears stale cache entries.
func (sf *StreamFilter) UpdateConfig(cfg StreamFilterConfig) {
	pinned := make(map[string]struct{}, len(cfg.PinnedTools))
	for _, p := range cfg.PinnedTools {
		if p != "" {
			pinned[p] = struct{}{}
		}
	}
	sf.cfgMu.Lock()
	sf.cfg = cfg
	sf.pinned = pinned
	sf.cfgMu.Unlock()

	sf.cacheMu.Lock()
	sf.cache = sf.cache[:0]
	sf.cacheMu.Unlock()
}

// InvalidateCache flushes cached tools/list projections (e.g. when upstream tools change).
func (sf *StreamFilter) InvalidateCache() {
	sf.cacheMu.Lock()
	sf.cache = sf.cache[:0]
	sf.cacheMu.Unlock()
}

// RecordToolCall records a tools/call invocation for recency and domain-affinity boosting (<5us).
func (sf *StreamFilter) RecordToolCall(toolName string) {
	if toolName == "" {
		return
	}
	curSeq := sf.seq.Add(1)
	dom := extractToolDomain(toolName)

	sf.contextMu.Lock()
	rec, ok := sf.toolStats[toolName]
	if !ok {
		rec = &callRecord{domain: dom}
		sf.toolStats[toolName] = rec
	}
	rec.calls++
	rec.lastSeq = curSeq

	if dom != "" {
		dRec, dOk := sf.domStats[dom]
		if !dOk {
			dRec = &domainRecord{}
			sf.domStats[dom] = dRec
		}
		dRec.calls++
		dRec.lastSeq = curSeq
	}
	sf.contextMu.Unlock()
}

// RegisterRawTools explicitly registers unpruned tools and whether their exposed schemas are compacted/stubbed.
func (sf *StreamFilter) RegisterRawTools(rawTools []mcp.Tool, compacted bool, stubbedSet map[string]bool) {
	sf.regMu.Lock()
	for _, rt := range rawTools {
		sf.rawRegistry[rt.Name] = rawToolRecord{
			rawTool:   rt,
			compacted: compacted || stubbedSet[rt.Name],
			stubbed:   stubbedSet[rt.Name],
		}
	}
	sf.regMu.Unlock()
}

// ComputeBoostScores calculates recency-decayed call and domain-affinity scores for tools.
// Also returns a quantized FNV-1a context signature for cache keying.
func (sf *StreamFilter) ComputeBoostScores(tools []mcp.Tool) (map[string]float64, uint64) {
	curSeq := sf.seq.Load()
	if curSeq == 0 || len(tools) == 0 {
		return nil, 0
	}

	sf.contextMu.RLock()
	if len(sf.toolStats) == 0 {
		sf.contextMu.RUnlock()
		return nil, 0
	}

	scores := make(map[string]float64, len(tools))
	h := fnv.New64a()

	for i := range tools {
		name := tools[i].Name
		var score float64
		if st, ok := sf.toolStats[name]; ok && st.calls > 0 {
			age := float64(curSeq - st.lastSeq)
			recency := math.Exp(-0.18 * age)
			freq := 0.25 * math.Log1p(float64(st.calls))
			score += 1.0*recency + freq
		}
		dom := extractToolDomain(name)
		if dom != "" {
			if dst, ok := sf.domStats[dom]; ok && dst.calls > 0 {
				domAge := float64(curSeq - dst.lastSeq)
				score += 0.35 * math.Exp(-0.22*domAge)
			}
		}
		if score >= 0.05 {
			scores[name] = score
			// Quantize into 8 discrete bands so minor decay jitter reuses cache while rank shifts bust cache
			band := uint8(math.Min(score*4.0, 32.0))
			_, _ = h.Write([]byte(name))
			_, _ = h.Write([]byte{0, band})
		}
	}
	sf.contextMu.RUnlock()

	if len(scores) == 0 {
		return nil, 0
	}
	return scores, h.Sum64()
}

// FilterToolsList applies tier pruning + RFC #1 progressive token-budget compaction with FNV-1a fast-path caching.
func (sf *StreamFilter) FilterToolsList(rawTools []mcp.Tool, engine PruneEngine) ([]mcp.Tool, BudgetPruneMetrics, error) {
	if engine == nil {
		return rawTools, BudgetPruneMetrics{}, nil
	}

	sf.cfgMu.RLock()
	cfg := sf.cfg
	pinned := sf.pinned
	sf.cfgMu.RUnlock()

	if cfg.PruneMode == "none" && cfg.MaxSchemaTokens <= 0 {
		sf.RegisterRawTools(rawTools, false, nil)
		return rawTools, BudgetPruneMetrics{}, nil
	}

	var boostScores map[string]float64
	var ctxSig uint64
	if cfg.MaxSchemaTokens > 0 {
		boostScores, ctxSig = sf.ComputeBoostScores(rawTools)
	}

	var engineFp uint64
	if fpEngine, ok := engine.(EngineConfigFingerprinter); ok {
		engineFp = fpEngine.ConfigFingerprint()
	}

	cacheKey := fingerprintToolsRequest(rawTools, cfg.PruneMode, cfg.MaxSchemaTokens, ctxSig, engineFp)

	// Fast-path FNV-1a cache lookup (<0.05ms)
	sf.cacheMu.RLock()
	for i := len(sf.cache) - 1; i >= 0; i-- {
		if sf.cache[i].key == cacheKey {
			hit := sf.cache[i]
			sf.cacheMu.RUnlock()
			outMetrics := hit.metrics
			outMetrics.CacheHit = true
			return hit.tools, outMetrics, nil
		}
	}
	sf.cacheMu.RUnlock()

	var pruned []mcp.Tool
	var bMetrics BudgetPruneMetrics
	var err error

	if ctxEngine, ok := engine.(ContextBudgetEngine); ok && cfg.MaxSchemaTokens > 0 {
		pruned, bMetrics, err = ctxEngine.PruneToolsWithContext(rawTools, BudgetContext{
			MaxSchemaTokens:   cfg.MaxSchemaTokens,
			PinnedTools:       pinned,
			ContextBoostTools: boostScores,
		})
	} else {
		var pMetrics PruneMetrics
		pruned, pMetrics, err = engine.PruneTools(rawTools)
		bMetrics = BudgetPruneMetrics{PruneMetrics: pMetrics}
	}
	if err != nil {
		return nil, BudgetPruneMetrics{}, err
	}

	stubbedSet := make(map[string]bool, len(bMetrics.ProjectedTools))
	for _, name := range bMetrics.ProjectedTools {
		stubbedSet[name] = true
	}
	compactedForValidation := cfg.MaxSchemaTokens > 0 || cfg.PruneMode == "dynamic" || len(stubbedSet) > 0
	sf.RegisterRawTools(rawTools, compactedForValidation, stubbedSet)

	// Store in bounded cache
	sf.cacheMu.Lock()
	if len(sf.cache) >= maxFilterCacheEntries {
		copy(sf.cache, sf.cache[1:])
		sf.cache[len(sf.cache)-1] = filterCacheEntry{
			key:        cacheKey,
			tools:      pruned,
			metrics:    bMetrics,
			stubbedMap: stubbedSet,
		}
	} else {
		sf.cache = append(sf.cache, filterCacheEntry{
			key:        cacheKey,
			tools:      pruned,
			metrics:    bMetrics,
			stubbedMap: stubbedSet,
		})
	}
	sf.cacheMu.Unlock()

	return pruned, bMetrics, nil
}

// InterceptToolCall records call context and performs fast (<0.1ms) recursive pre-binding validation
// against the cached raw InputSchema when schemas were budget-compacted or stubbed.
// Returns a non-nil self-correcting CallToolResult{IsError: true} if validation fails.
func (sf *StreamFilter) InterceptToolCall(toolName string, rawArgs json.RawMessage) *mcp.CallToolResult {
	if toolName == "" {
		return nil
	}

	// Track recency and domain affinity so subsequent tools/list turns boost this tool
	sf.RecordToolCall(toolName)

	sf.regMu.RLock()
	rec, ok := sf.rawRegistry[toolName]
	sf.regMu.RUnlock()

	if !ok || !rec.compacted {
		return nil
	}

	if vErr := ValidateToolArguments(toolName, rec.rawTool.InputSchema, rawArgs); vErr != nil {
		res := vErr.ToCallToolResult()
		return &res
	}
	return nil
}

// GetRawTool retrieves a cached raw upstream tool definition if present.
func (sf *StreamFilter) GetRawTool(toolName string) (mcp.Tool, bool) {
	sf.regMu.RLock()
	rec, ok := sf.rawRegistry[toolName]
	sf.regMu.RUnlock()
	if !ok {
		return mcp.Tool{}, false
	}
	return rec.rawTool, true
}

func extractToolDomain(name string) string {
	if idx := strings.Index(name, "__"); idx > 0 {
		return name[:idx+2]
	}
	for _, sep := range []byte{'_', '-', '.'} {
		if idx := strings.IndexByte(name, sep); idx >= 2 {
			return name[:idx+1]
		}
	}
	return ""
}

func fingerprintToolsRequest(tools []mcp.Tool, mode string, maxTokens int, ctxSig uint64, engineFp uint64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mode))
	var buf [16]byte
	b := strconv.AppendInt(buf[:0], int64(maxTokens), 10)
	_, _ = h.Write(b)
	b = strconv.AppendUint(buf[:0], ctxSig, 16)
	_, _ = h.Write(b)
	b = strconv.AppendUint(buf[:0], engineFp, 16)
	_, _ = h.Write(b)

	for i := range tools {
		t := &tools[i]
		_, _ = h.Write([]byte{0x1e})
		_, _ = h.Write([]byte(t.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(t.Description))
		b = strconv.AppendInt(buf[:0], int64(len(t.InputSchema.Properties)), 10)
		_, _ = h.Write(b)
		b = strconv.AppendInt(buf[:0], int64(len(t.InputSchema.Required)), 10)
		_, _ = h.Write(b)
		b = strconv.AppendInt(buf[:0], int64(len(t.InputSchema.Defs)+len(t.InputSchema.Definitions)), 10)
		_, _ = h.Write(b)
	}
	return h.Sum64()
}

// =============================================================================
// Fast Recursive Pre-Binding Schema Validator (<0.1ms)
// =============================================================================

// PreBindViolation describes a single argument validation failure with its violated sub-schema.
type PreBindViolation struct {
	Path      string
	Reason    string
	SubSchema map[string]any
}

// PreBindValidationError aggregates argument validation failures for self-correcting LLM feedback.
type PreBindValidationError struct {
	ToolName   string
	Violations []PreBindViolation
	RootSchema mcp.InputSchema
}

// Error formats a concise, self-correcting diagnostic including exact violated sub-schemas.
func (e *PreBindValidationError) Error() string {
	var sb strings.Builder
	sb.WriteString("ToolPrune Pre-Binding Schema Validation Failed for \"")
	sb.WriteString(e.ToolName)
	sb.WriteString("\":\n")

	limit := len(e.Violations)
	if limit > 5 {
		limit = 5
	}
	for i := 0; i < limit; i++ {
		v := e.Violations[i]
		sb.WriteString("- ")
		if v.Path != "" {
			sb.WriteString(v.Path)
			sb.WriteString(": ")
		}
		sb.WriteString(v.Reason)
		if len(v.SubSchema) > 0 {
			compact := stripDescriptionsFromMap(v.SubSchema, 0)
			if b, err := json.Marshal(compact); err == nil {
				sb.WriteString(" | expected schema: ")
				sb.Write(b)
			}
		}
		sb.WriteByte('\n')
	}
	if len(e.Violations) > limit {
		sb.WriteString(fmt.Sprintf("- ... and %d additional violation(s)\n", len(e.Violations)-limit))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// ToCallToolResult wraps the validation error as an MCP CallToolResult{IsError: true}.
func (e *PreBindValidationError) ToCallToolResult() mcp.CallToolResult {
	return mcp.CallToolResult{
		IsError: true,
		Content: []mcp.ContentItem{
			{
				Type: "text",
				Text: e.Error(),
			},
		},
	}
}

type validationCtx struct {
	defs       map[string]any
	legacyDefs map[string]any
	violations []PreBindViolation
}

// ValidateToolArguments validates JSON-RPC tools/call arguments against an unpruned mcp.InputSchema.
// Supports nested objects, required fields, arrays/tuples, enums, const, numeric/string/array bounds,
// local $ref -> $defs/definitions resolution, and anyOf/oneOf/allOf combinators.
func ValidateToolArguments(toolName string, schema mcp.InputSchema, rawArgs json.RawMessage) *PreBindValidationError {
	var args map[string]any
	trimmed := bytes.TrimSpace(rawArgs)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		args = make(map[string]any)
	} else {
		if trimmed[0] != '{' {
			return &PreBindValidationError{
				ToolName: toolName,
				Violations: []PreBindViolation{{
					Path:   "$",
					Reason: "arguments must be a JSON object",
				}},
				RootSchema: schema,
			}
		}
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber()
		if err := dec.Decode(&args); err != nil {
			return &PreBindValidationError{
				ToolName: toolName,
				Violations: []PreBindViolation{{
					Path:   "$",
					Reason: fmt.Sprintf("malformed arguments JSON: %v", err),
				}},
				RootSchema: schema,
			}
		}
	}

	vctx := &validationCtx{
		defs:       schema.Defs,
		legacyDefs: schema.Definitions,
	}

	rootMap := inputSchemaToNodeMap(schema)
	validateNodeValue(vctx, args, rootMap, "$", 0)

	if len(vctx.violations) == 0 {
		return nil
	}
	return &PreBindValidationError{
		ToolName:   toolName,
		Violations: vctx.violations,
		RootSchema: schema,
	}
}

func inputSchemaToNodeMap(s mcp.InputSchema) map[string]any {
	m := make(map[string]any, 12)
	if s.Type != "" {
		m["type"] = s.Type
	} else {
		m["type"] = "object"
	}
	if len(s.Properties) > 0 {
		m["properties"] = s.Properties
	}
	if len(s.Required) > 0 {
		m["required"] = s.Required
	}
	if s.AdditionalProperties != nil {
		m["additionalProperties"] = s.AdditionalProperties
	}
	if len(s.AnyOf) > 0 {
		m["anyOf"] = s.AnyOf
	}
	if len(s.OneOf) > 0 {
		m["oneOf"] = s.OneOf
	}
	if len(s.AllOf) > 0 {
		m["allOf"] = s.AllOf
	}
	if len(s.Not) > 0 {
		m["not"] = s.Not
	}
	if len(s.DependentRequired) > 0 {
		m["dependentRequired"] = s.DependentRequired
	}
	if s.MinProperties != nil {
		m["minProperties"] = float64(*s.MinProperties)
	}
	if s.MaxProperties != nil {
		m["maxProperties"] = float64(*s.MaxProperties)
	}
	return m
}

func validateNodeValue(vctx *validationCtx, val any, node map[string]any, path string, depth int) {
	if node == nil || depth > 32 || len(vctx.violations) >= 8 {
		return
	}

	// Scoped local $defs / definitions override if present on sub-node
	prevDefs, prevLegacy := vctx.defs, vctx.legacyDefs
	if subDefs, ok := node["$defs"].(map[string]any); ok && len(subDefs) > 0 {
		vctx.defs = mergeDefs(vctx.defs, subDefs)
	}
	if subLegacy, ok := node["definitions"].(map[string]any); ok && len(subLegacy) > 0 {
		vctx.legacyDefs = mergeDefs(vctx.legacyDefs, subLegacy)
	}
	defer func() {
		vctx.defs = prevDefs
		vctx.legacyDefs = prevLegacy
	}()

	// 1. Resolve local $ref (#/$defs/... or #/definitions/...)
	if refStr, ok := node["$ref"].(string); ok && refStr != "" {
		if target := resolveLocalRef(refStr, vctx.defs, vctx.legacyDefs); target != nil {
			validateNodeValue(vctx, val, target, path, depth+1)
		}
	}

	// 2. Combinators: allOf, anyOf, oneOf, not
	if allOf, ok := node["allOf"].([]any); ok {
		for _, branch := range allOf {
			if bMap, ok := branch.(map[string]any); ok {
				validateNodeValue(vctx, val, bMap, path, depth+1)
			}
		}
	}
	if anyOf, ok := node["anyOf"].([]any); ok && len(anyOf) > 0 {
		if !matchesAnyBranch(vctx, val, anyOf, depth+1) {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    "value did not satisfy any allowed schema in anyOf",
				SubSchema: map[string]any{"anyOf": anyOf},
			})
			return
		}
	}
	if oneOf, ok := node["oneOf"].([]any); ok && len(oneOf) > 0 {
		matches := countMatchingBranches(vctx, val, oneOf, depth+1)
		if matches == 0 {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    "value did not match any branch in oneOf",
				SubSchema: map[string]any{"oneOf": oneOf},
			})
			return
		}
	}
	if notSchema, ok := node["not"].(map[string]any); ok && len(notSchema) > 0 {
		probe := &validationCtx{defs: vctx.defs, legacyDefs: vctx.legacyDefs}
		validateNodeValue(probe, val, notSchema, path, depth+1)
		if len(probe.violations) == 0 {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    "value matched forbidden 'not' schema",
				SubSchema: map[string]any{"not": notSchema},
			})
			return
		}
	}

	// 3. Type check
	if rawType, hasType := node["type"]; hasType {
		if !checkValueMatchesSchemaType(val, rawType) {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("invalid type: expected %v, got %s", rawType, jsonValueTypeName(val)),
				SubSchema: node,
			})
			return
		}
	}

	// 4. Const & Enum checks
	if constVal, hasConst := node["const"]; hasConst {
		if !jsonScalarEqual(val, constVal) {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("value must equal const %v", constVal),
				SubSchema: node,
			})
			return
		}
	}
	if rawEnum, hasEnum := node["enum"]; hasEnum {
		if !checkEnumMatch(val, rawEnum) {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("value %v is not in allowed enum %v", val, rawEnum),
				SubSchema: node,
			})
			return
		}
	}

	// 5. Type-specific constraints
	switch typed := val.(type) {
	case json.Number:
		if f, err := typed.Float64(); err == nil {
			checkNumericBounds(vctx, f, node, path)
		}
	case float64:
		checkNumericBounds(vctx, typed, node, path)
	case string:
		runeLen := len([]rune(typed))
		if minLen, ok := asFloat(node["minLength"]); ok && float64(runeLen) < minLen {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("string length %d is below minLength %d", runeLen, int(minLen)),
				SubSchema: node,
			})
		}
		if maxLen, ok := asFloat(node["maxLength"]); ok && float64(runeLen) > maxLen {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("string length %d exceeds maxLength %d", runeLen, int(maxLen)),
				SubSchema: node,
			})
		}
	case []any:
		if minItems, ok := asFloat(node["minItems"]); ok && float64(len(typed)) < minItems {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("array length %d is below minItems %d", len(typed), int(minItems)),
				SubSchema: node,
			})
		}
		if maxItems, ok := asFloat(node["maxItems"]); ok && float64(len(typed)) > maxItems {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("array length %d exceeds maxItems %d", len(typed), int(maxItems)),
				SubSchema: node,
			})
		}
		if itemMap, ok := node["items"].(map[string]any); ok {
			for idx, elem := range typed {
				elemPath := fmt.Sprintf("%s[%d]", path, idx)
				validateNodeValue(vctx, elem, itemMap, elemPath, depth+1)
			}
		} else if tupleSchemas, ok := node["items"].([]any); ok {
			for idx := 0; idx < len(typed) && idx < len(tupleSchemas); idx++ {
				if tMap, ok := tupleSchemas[idx].(map[string]any); ok {
					elemPath := fmt.Sprintf("%s[%d]", path, idx)
					validateNodeValue(vctx, typed[idx], tMap, elemPath, depth+1)
				}
			}
		}
	case map[string]any:
		validateObjectValue(vctx, typed, node, path, depth)
	}
}

func validateObjectValue(vctx *validationCtx, obj map[string]any, node map[string]any, path string, depth int) {
	if minProps, ok := asFloat(node["minProperties"]); ok && float64(len(obj)) < minProps {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("object has %d properties, minimum required is %d", len(obj), int(minProps)),
			SubSchema: node,
		})
	}
	if maxProps, ok := asFloat(node["maxProperties"]); ok && float64(len(obj)) > maxProps {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("object has %d properties, maximum allowed is %d", len(obj), int(maxProps)),
			SubSchema: node,
		})
	}

	props, _ := node["properties"].(map[string]any)
	reqNames := extractStringList(node["required"])
	for _, req := range reqNames {
		if v, exists := obj[req]; !exists || v == nil {
			var propSchema map[string]any
			if props != nil {
				if pm, ok := props[req].(map[string]any); ok {
					if refStr, hasRef := pm["$ref"].(string); hasRef {
						if resolved := resolveLocalRef(refStr, vctx.defs, vctx.legacyDefs); resolved != nil {
							propSchema = resolved
						} else {
							propSchema = pm
						}
					} else {
						propSchema = pm
					}
				}
			}
			if propSchema == nil {
				propSchema = node
			}
			fieldPath := joinJSONPath(path, req)
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      fieldPath,
				Reason:    fmt.Sprintf("missing required property %q", req),
				SubSchema: propSchema,
			})
		}
	}

	// dependentRequired check
	switch depReq := node["dependentRequired"].(type) {
	case map[string][]string:
		for trigger, needed := range depReq {
			if _, present := obj[trigger]; present {
				for _, req := range needed {
					if _, ok := obj[req]; !ok {
						vctx.violations = append(vctx.violations, PreBindViolation{
							Path:      joinJSONPath(path, req),
							Reason:    fmt.Sprintf("property %q is required when %q is present", req, trigger),
							SubSchema: node,
						})
					}
				}
			}
		}
	case map[string]any:
		for trigger, rawNeeded := range depReq {
			if _, present := obj[trigger]; present {
				for _, req := range extractStringList(rawNeeded) {
					if _, ok := obj[req]; !ok {
						vctx.violations = append(vctx.violations, PreBindViolation{
							Path:      joinJSONPath(path, req),
							Reason:    fmt.Sprintf("property %q is required when %q is present", req, trigger),
							SubSchema: node,
						})
					}
				}
			}
		}
	}

	// Deterministic key order for reproducible error reporting
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	addPropsFalse := false
	var addPropsSchema map[string]any
	if b, ok := node["additionalProperties"].(bool); ok && !b {
		addPropsFalse = true
	} else if m, ok := node["additionalProperties"].(map[string]any); ok {
		addPropsSchema = m
	}

	_, hasPatternProps := node["patternProperties"].(map[string]any)

	for _, k := range keys {
		val := obj[k]
		fieldPath := joinJSONPath(path, k)
		if props != nil {
			if propDef, defined := props[k]; defined {
				if pm, ok := propDef.(map[string]any); ok {
					validateNodeValue(vctx, val, pm, fieldPath, depth+1)
				}
				continue
			}
		}
		if addPropsSchema != nil {
			validateNodeValue(vctx, val, addPropsSchema, fieldPath, depth+1)
		} else if addPropsFalse && props != nil && !hasPatternProps {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      fieldPath,
				Reason:    fmt.Sprintf("unknown property %q is not permitted (additionalProperties: false)", k),
				SubSchema: node,
			})
		}
	}
}

func matchesAnyBranch(vctx *validationCtx, val any, branches []any, depth int) bool {
	for _, b := range branches {
		bMap, ok := b.(map[string]any)
		if !ok {
			continue
		}
		probe := &validationCtx{defs: vctx.defs, legacyDefs: vctx.legacyDefs}
		validateNodeValue(probe, val, bMap, "$", depth)
		if len(probe.violations) == 0 {
			return true
		}
	}
	return false
}

func countMatchingBranches(vctx *validationCtx, val any, branches []any, depth int) int {
	matched := 0
	for _, b := range branches {
		bMap, ok := b.(map[string]any)
		if !ok {
			continue
		}
		probe := &validationCtx{defs: vctx.defs, legacyDefs: vctx.legacyDefs}
		validateNodeValue(probe, val, bMap, "$", depth)
		if len(probe.violations) == 0 {
			matched++
		}
	}
	return matched
}

func resolveLocalRef(ref string, defs, legacyDefs map[string]any) map[string]any {
	const defsPrefix = "#/$defs/"
	const legacyPrefix = "#/definitions/"
	if strings.HasPrefix(ref, defsPrefix) && defs != nil {
		key := ref[len(defsPrefix):]
		if target, ok := defs[key].(map[string]any); ok {
			return target
		}
	}
	if strings.HasPrefix(ref, legacyPrefix) && legacyDefs != nil {
		key := ref[len(legacyPrefix):]
		if target, ok := legacyDefs[key].(map[string]any); ok {
			return target
		}
	}
	return nil
}

func mergeDefs(base, extra map[string]any) map[string]any {
	if len(base) == 0 {
		return extra
	}
	merged := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

func checkNumericBounds(vctx *validationCtx, val float64, node map[string]any, path string) {
	if minVal, ok := asFloat(node["minimum"]); ok && val < minVal {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("value %v is less than minimum %v", val, minVal),
			SubSchema: node,
		})
	}
	if maxVal, ok := asFloat(node["maximum"]); ok && val > maxVal {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("value %v exceeds maximum %v", val, maxVal),
			SubSchema: node,
		})
	}
	if exMin, ok := asFloat(node["exclusiveMinimum"]); ok && val <= exMin {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("value %v must be strictly greater than exclusiveMinimum %v", val, exMin),
			SubSchema: node,
		})
	}
	if exMax, ok := asFloat(node["exclusiveMaximum"]); ok && val >= exMax {
		vctx.violations = append(vctx.violations, PreBindViolation{
			Path:      path,
			Reason:    fmt.Sprintf("value %v must be strictly less than exclusiveMaximum %v", val, exMax),
			SubSchema: node,
		})
	}
}

func checkValueMatchesSchemaType(val any, rawType any) bool {
	switch t := rawType.(type) {
	case string:
		return matchesSingleJSONType(val, t)
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok && matchesSingleJSONType(val, s) {
				return true
			}
		}
		return false
	case []string:
		for _, s := range t {
			if matchesSingleJSONType(val, s) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func matchesSingleJSONType(val any, expected string) bool {
	switch expected {
	case "null":
		return val == nil
	case "string":
		_, ok := val.(string)
		return ok
	case "boolean":
		_, ok := val.(bool)
		return ok
	case "number":
		switch n := val.(type) {
		case json.Number:
			_, err := n.Float64()
			return err == nil
		case float64, float32, int, int64:
			return true
		default:
			return false
		}
	case "integer":
		switch n := val.(type) {
		case json.Number:
			if _, err := n.Int64(); err == nil {
				return true
			}
			f, err := n.Float64()
			return err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f
		case float64:
			return !math.IsNaN(n) && !math.IsInf(n, 0) && math.Trunc(n) == n
		case int, int64:
			return true
		default:
			return false
		}
	case "array":
		_, ok := val.([]any)
		return ok
	case "object":
		_, ok := val.(map[string]any)
		return ok
	default:
		return true
	}
}

func jsonValueTypeName(val any) string {
	if val == nil {
		return "null"
	}
	switch v := val.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		if _, err := v.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case float64:
		if math.Trunc(v) == v {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", val)
	}
}

func checkEnumMatch(val any, rawEnum any) bool {
	switch enums := rawEnum.(type) {
	case []any:
		for _, candidate := range enums {
			if jsonScalarEqual(val, candidate) {
				return true
			}
		}
		return false
	case []string:
		s, ok := val.(string)
		if !ok {
			return false
		}
		for _, candidate := range enums {
			if s == candidate {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func jsonScalarEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if fa, okA := asFloat(a); okA {
		if fb, okB := asFloat(b); okB {
			return fa == fb
		}
		return false
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func extractStringList(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func joinJSONPath(base, prop string) string {
	if base == "" || base == "$" {
		return "$." + prop
	}
	return base + "." + prop
}

func stripDescriptionsFromMap(m map[string]any, depth int) map[string]any {
	if m == nil || depth > 6 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k == "description" || k == "$comment" || k == "examples" || k == "example" || k == "title" {
			continue
		}
		switch sub := v.(type) {
		case map[string]any:
			out[k] = stripDescriptionsFromMap(sub, depth+1)
		case []any:
			arr := make([]any, len(sub))
			for i, item := range sub {
				if im, ok := item.(map[string]any); ok {
					arr[i] = stripDescriptionsFromMap(im, depth+1)
				} else {
					arr[i] = item
				}
			}
			out[k] = arr
		default:
			out[k] = v
		}
	}
	return out
}
