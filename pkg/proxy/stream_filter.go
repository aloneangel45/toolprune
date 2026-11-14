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
	compacted  bool
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
			rawTool:   cloneMCPTool(rt),
			compacted: compacted || stubbedSet[rt.Name],
			stubbed:   stubbedSet[rt.Name],
		}
	}
	sf.regMu.Unlock()
}

func (sf *StreamFilter) markCompactedTools(forceAll bool, stubbedSet map[string]bool) {
	if !forceAll && len(stubbedSet) == 0 {
		return
	}
	sf.regMu.Lock()
	for name, rec := range sf.rawRegistry {
		if forceAll || stubbedSet[name] {
			rec.compacted = true
			if stubbedSet[name] {
				rec.stubbed = true
			}
			sf.rawRegistry[name] = rec
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

// FilterToolsList applies tier pruning + RFC #1 progressive token-budget compaction with FNV-1a fast-path caching,
// and registers rawTools in the pre-binding validation registry.
func (sf *StreamFilter) FilterToolsList(rawTools []mcp.Tool, engine PruneEngine) ([]mcp.Tool, BudgetPruneMetrics, error) {
	return sf.filterToolsListCore(rawTools, engine, true)
}

// FilterProjectedToolsList enforces RFC #1 progressive token-budget compaction on an already-projected
// or pre-pruned tool slice (such as Tier 4 dynamic catalog stubs + meta-tools) WITHOUT overwriting
// the unpruned raw schemas previously registered via RegisterRawTools.
func (sf *StreamFilter) FilterProjectedToolsList(projectedTools []mcp.Tool, engine PruneEngine) ([]mcp.Tool, BudgetPruneMetrics, error) {
	return sf.filterToolsListCore(projectedTools, engine, false)
}

func (sf *StreamFilter) filterToolsListCore(inputTools []mcp.Tool, engine PruneEngine, registerRaw bool) ([]mcp.Tool, BudgetPruneMetrics, error) {
	if engine == nil {
		return cloneToolsSlice(inputTools), BudgetPruneMetrics{}, nil
	}

	sf.cfgMu.RLock()
	cfg := sf.cfg
	pinned := sf.pinned
	sf.cfgMu.RUnlock()

	if cfg.PruneMode == "none" && cfg.MaxSchemaTokens <= 0 {
		if registerRaw {
			sf.RegisterRawTools(inputTools, false, nil)
		}
		return cloneToolsSlice(inputTools), BudgetPruneMetrics{}, nil
	}

	var boostScores map[string]float64
	var ctxSig uint64
	if cfg.MaxSchemaTokens > 0 {
		boostScores, ctxSig = sf.ComputeBoostScores(inputTools)
	}

	var engineFp uint64
	if fpEngine, ok := engine.(EngineConfigFingerprinter); ok {
		engineFp = fpEngine.ConfigFingerprint()
	}

	cacheKey := fingerprintToolsRequest(inputTools, cfg.PruneMode, cfg.MaxSchemaTokens, pinned, ctxSig, engineFp)

	// Fast-path FNV-1a cache lookup (<0.05ms)
	sf.cacheMu.RLock()
	for i := len(sf.cache) - 1; i >= 0; i-- {
		if sf.cache[i].key == cacheKey {
			hit := sf.cache[i]
			sf.cacheMu.RUnlock()
			if registerRaw {
				sf.RegisterRawTools(inputTools, hit.compacted, hit.stubbedMap)
			} else {
				sf.markCompactedTools(hit.compacted, hit.stubbedMap)
			}
			outMetrics := hit.metrics
			outMetrics.CacheHit = true
			return cloneToolsSlice(hit.tools), outMetrics, nil
		}
	}
	sf.cacheMu.RUnlock()

	var pruned []mcp.Tool
	var bMetrics BudgetPruneMetrics
	var err error

	if ctxEngine, ok := engine.(ContextBudgetEngine); ok && cfg.MaxSchemaTokens > 0 {
		pruned, bMetrics, err = ctxEngine.PruneToolsWithContext(inputTools, BudgetContext{
			MaxSchemaTokens:   cfg.MaxSchemaTokens,
			PinnedTools:       pinned,
			ContextBoostTools: boostScores,
		})
	} else {
		var pMetrics PruneMetrics
		pruned, pMetrics, err = engine.PruneTools(inputTools)
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
	if registerRaw {
		sf.RegisterRawTools(inputTools, compactedForValidation, stubbedSet)
	} else {
		sf.markCompactedTools(compactedForValidation, stubbedSet)
	}

	cachedTools := cloneToolsSlice(pruned)

	// Store in bounded cache
	sf.cacheMu.Lock()
	entry := filterCacheEntry{
		key:        cacheKey,
		tools:      cachedTools,
		metrics:    bMetrics,
		compacted:  compactedForValidation,
		stubbedMap: stubbedSet,
	}
	if len(sf.cache) >= maxFilterCacheEntries {
		copy(sf.cache, sf.cache[1:])
		sf.cache[len(sf.cache)-1] = entry
	} else {
		sf.cache = append(sf.cache, entry)
	}
	sf.cacheMu.Unlock()

	return cloneToolsSlice(pruned), bMetrics, nil
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

func fingerprintToolsRequest(tools []mcp.Tool, mode string, maxTokens int, pinned map[string]struct{}, ctxSig uint64, engineFp uint64) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mode))
	_, _ = h.Write([]byte{0})
	var buf [24]byte
	b := strconv.AppendInt(buf[:0], int64(maxTokens), 10)
	_, _ = h.Write(b)
	_, _ = h.Write([]byte{0})
	b = strconv.AppendUint(buf[:0], ctxSig, 16)
	_, _ = h.Write(b)
	_, _ = h.Write([]byte{0})
	b = strconv.AppendUint(buf[:0], engineFp, 16)
	_, _ = h.Write(b)

	if len(pinned) > 0 {
		pinKeys := make([]string, 0, len(pinned))
		for k := range pinned {
			pinKeys = append(pinKeys, k)
		}
		sort.Strings(pinKeys)
		for _, k := range pinKeys {
			_, _ = h.Write([]byte{0x1d})
			_, _ = h.Write([]byte(k))
		}
	}

	for i := range tools {
		t := &tools[i]
		_, _ = h.Write([]byte{0x1e})
		_, _ = h.Write([]byte(t.Name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(t.Description))
		_, _ = h.Write([]byte{0})
		hashInputSchemaDeterministic(h, &t.InputSchema)
	}
	return h.Sum64()
}

func hashInputSchemaDeterministic(h interface{ Write([]byte) (int, error) }, s *mcp.InputSchema) {
	if s == nil {
		return
	}
	_, _ = h.Write([]byte(s.Type))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(s.Description))
	_, _ = h.Write([]byte{0})
	for _, r := range s.Required {
		_, _ = h.Write([]byte(r))
		_, _ = h.Write([]byte{0x1f})
	}
	_, _ = h.Write([]byte{0})
	hashAnyDeterministic(h, s.AdditionalProperties)
	hashMapDeterministic(h, s.Properties)
	hashMapDeterministic(h, s.Defs)
	hashMapDeterministic(h, s.Definitions)
	hashSliceDeterministic(h, s.AllOf)
	hashSliceDeterministic(h, s.AnyOf)
	hashSliceDeterministic(h, s.OneOf)
	hashMapDeterministic(h, s.Not)
	hashMapDeterministic(h, s.PatternProperties)
	hashMapDeterministic(h, s.DependentSchemas)
	if len(s.DependentRequired) > 0 {
		depKeys := make([]string, 0, len(s.DependentRequired))
		for k := range s.DependentRequired {
			depKeys = append(depKeys, k)
		}
		sort.Strings(depKeys)
		for _, k := range depKeys {
			_, _ = h.Write([]byte(k))
			hashAnyDeterministic(h, s.DependentRequired[k])
		}
	}
	if s.MinProperties != nil {
		hashAnyDeterministic(h, *s.MinProperties)
	}
	if s.MaxProperties != nil {
		hashAnyDeterministic(h, *s.MaxProperties)
	}
}

func hashMapDeterministic(h interface{ Write([]byte) (int, error) }, m map[string]any) {
	if len(m) == 0 {
		_, _ = h.Write([]byte{0})
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	_, _ = h.Write([]byte{'{'})
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{':'})
		hashAnyDeterministic(h, m[k])
		_, _ = h.Write([]byte{','})
	}
	_, _ = h.Write([]byte{'}'})
}

func hashSliceDeterministic(h interface{ Write([]byte) (int, error) }, s []any) {
	if len(s) == 0 {
		_, _ = h.Write([]byte{0})
		return
	}
	_, _ = h.Write([]byte{'['})
	for _, elem := range s {
		hashAnyDeterministic(h, elem)
		_, _ = h.Write([]byte{','})
	}
	_, _ = h.Write([]byte{']'})
}

func hashAnyDeterministic(h interface{ Write([]byte) (int, error) }, v any) {
	switch val := v.(type) {
	case nil:
		_, _ = h.Write([]byte{'N'})
	case bool:
		if val {
			_, _ = h.Write([]byte{'T'})
		} else {
			_, _ = h.Write([]byte{'F'})
		}
	case string:
		_, _ = h.Write([]byte{'S'})
		_, _ = h.Write([]byte(val))
		_, _ = h.Write([]byte{0})
	case float64:
		var buf [24]byte
		_, _ = h.Write([]byte{'D'})
		_, _ = h.Write(strconv.AppendFloat(buf[:0], val, 'g', -1, 64))
		_, _ = h.Write([]byte{0})
	case int:
		var buf [24]byte
		_, _ = h.Write([]byte{'I'})
		_, _ = h.Write(strconv.AppendInt(buf[:0], int64(val), 10))
		_, _ = h.Write([]byte{0})
	case json.Number:
		_, _ = h.Write([]byte{'J'})
		_, _ = h.Write([]byte(val.String()))
		_, _ = h.Write([]byte{0})
	case []string:
		_, _ = h.Write([]byte{'['})
		for _, item := range val {
			_, _ = h.Write([]byte(item))
			_, _ = h.Write([]byte{0x1f})
		}
		_, _ = h.Write([]byte{']'})
	case []any:
		hashSliceDeterministic(h, val)
	case map[string]any:
		hashMapDeterministic(h, val)
	default:
		_, _ = fmt.Fprintf(interfaceWriter{h}, "%T:%v;", val, val)
	}
}

type interfaceWriter struct {
	w interface{ Write([]byte) (int, error) }
}

func (iw interfaceWriter) Write(p []byte) (int, error) {
	return iw.w.Write(p)
}

func cloneToolsSlice(tools []mcp.Tool) []mcp.Tool {
	if tools == nil {
		return nil
	}
	out := make([]mcp.Tool, len(tools))
	for i := range tools {
		out[i] = cloneMCPTool(tools[i])
	}
	return out
}

func cloneMCPTool(t mcp.Tool) mcp.Tool {
	cp := t
	cp.OutputSchema = cloneAnyMap(t.OutputSchema)
	if t.Icons != nil {
		cp.Icons = make([]mcp.ToolIcon, len(t.Icons))
		for i, ic := range t.Icons {
			cp.Icons[i] = ic
			if ic.Sizes != nil {
				cp.Icons[i].Sizes = append([]string(nil), ic.Sizes...)
			}
		}
	}
	if t.Annotations != nil {
		ann := *t.Annotations
		cp.Annotations = &ann
	}
	cp.InputSchema = mcp.InputSchema{
		Type:                 t.InputSchema.Type,
		Properties:           cloneAnyMap(t.InputSchema.Properties),
		AdditionalProperties: cloneAnyValue(t.InputSchema.AdditionalProperties),
		Schema:               t.InputSchema.Schema,
		ID:                   t.InputSchema.ID,
		Title:                t.InputSchema.Title,
		Description:          t.InputSchema.Description,
		Comment:              t.InputSchema.Comment,
		Examples:             cloneAnySlice(t.InputSchema.Examples),
		Defs:                 cloneAnyMap(t.InputSchema.Defs),
		Definitions:          cloneAnyMap(t.InputSchema.Definitions),
		AnyOf:                cloneAnySlice(t.InputSchema.AnyOf),
		OneOf:                cloneAnySlice(t.InputSchema.OneOf),
		AllOf:                cloneAnySlice(t.InputSchema.AllOf),
		Not:                  cloneAnyMap(t.InputSchema.Not),
		PatternProperties:    cloneAnyMap(t.InputSchema.PatternProperties),
		DependentSchemas:     cloneAnyMap(t.InputSchema.DependentSchemas),
	}
	if t.InputSchema.Required != nil {
		cp.InputSchema.Required = append([]string(nil), t.InputSchema.Required...)
	}
	if t.InputSchema.DependentRequired != nil {
		cp.InputSchema.DependentRequired = make(map[string][]string, len(t.InputSchema.DependentRequired))
		for k, v := range t.InputSchema.DependentRequired {
			cp.InputSchema.DependentRequired[k] = append([]string(nil), v...)
		}
	}
	if t.InputSchema.MinProperties != nil {
		v := *t.InputSchema.MinProperties
		cp.InputSchema.MinProperties = &v
	}
	if t.InputSchema.MaxProperties != nil {
		v := *t.InputSchema.MaxProperties
		cp.InputSchema.MaxProperties = &v
	}
	return cp
}

func cloneAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAnyValue(v)
	}
	return out
}

func cloneAnySlice(s []any) []any {
	if s == nil {
		return nil
	}
	out := make([]any, len(s))
	for i := range s {
		out[i] = cloneAnyValue(s[i])
	}
	return out
}

func cloneAnyValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		return cloneAnySlice(typed)
	case []string:
		return append([]string(nil), typed...)
	default:
		return v
	}
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
		if matches > 1 {
			vctx.violations = append(vctx.violations, PreBindViolation{
				Path:      path,
				Reason:    fmt.Sprintf("value matched %d branches in oneOf; must match exactly 1", matches),
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
		var rawPropSchema map[string]any
		var propSchema map[string]any
		if props != nil {
			if pm, ok := props[req].(map[string]any); ok {
				rawPropSchema = pm
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
		v, exists := obj[req]
		if !exists || (v == nil && !schemaExplicitlyAllowsNull(vctx, rawPropSchema, depth+1)) {
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

func schemaExplicitlyAllowsNull(vctx *validationCtx, propSchema map[string]any, depth int) bool {
	if len(propSchema) == 0 || depth > 32 {
		return false
	}
	target := propSchema
	if refStr, ok := propSchema["$ref"].(string); ok && refStr != "" {
		if resolved := resolveLocalRef(refStr, vctx.defs, vctx.legacyDefs); resolved != nil {
			target = resolved
		}
	}
	_, hasType := target["type"]
	_, hasAnyOf := target["anyOf"]
	_, hasOneOf := target["oneOf"]
	_, hasEnum := target["enum"]
	_, hasConst := target["const"]
	if !hasType && !hasAnyOf && !hasOneOf && !hasEnum && !hasConst {
		return false
	}
	probe := &validationCtx{defs: vctx.defs, legacyDefs: vctx.legacyDefs}
	validateNodeValue(probe, nil, propSchema, "$", depth)
	return len(probe.violations) == 0
}

func resolveLocalRef(ref string, defs, legacyDefs map[string]any) map[string]any {
	const defsPrefix = "#/$defs/"
	const legacyPrefix = "#/definitions/"
	if strings.HasPrefix(ref, defsPrefix) && defs != nil {
		key := unescapeJSONPointerToken(ref[len(defsPrefix):])
		if target, ok := defs[key].(map[string]any); ok {
			return target
		}
	}
	if strings.HasPrefix(ref, legacyPrefix) && legacyDefs != nil {
		key := unescapeJSONPointerToken(ref[len(legacyPrefix):])
		if target, ok := legacyDefs[key].(map[string]any); ok {
			return target
		}
	}
	return nil
}

func unescapeJSONPointerToken(tok string) string {
	if !strings.Contains(tok, "~") {
		return tok
	}
	s := strings.ReplaceAll(tok, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
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
