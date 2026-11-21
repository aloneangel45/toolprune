// Package metrics implements thread-safe live token, latency, error, security alert,
// and USD savings telemetry with Prometheus (/metrics) and JSON (/api/stats) endpoints.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultPricePerMillionTokens is the default blended LLM input-token price ($3.00 / 1M tokens)
// used to estimate USD savings from pruned schema and response tokens.
const DefaultPricePerMillionTokens = 3.00

// UpstreamStats captures point-in-time telemetry metrics for a single upstream server (or aggregate total).
type UpstreamStats struct {
	Upstream             string  `json:"upstream"`
	Requests             uint64  `json:"requests"`
	Errors               uint64  `json:"errors"`
	SchemaTokensRaw      uint64  `json:"schema_tokens_raw"`
	SchemaTokensPruned   uint64  `json:"schema_tokens_pruned"`
	ResponseTokensRaw    uint64  `json:"response_tokens_raw"`
	ResponseTokensPruned uint64  `json:"response_tokens_pruned"`
	SecurityAlerts       uint64  `json:"security_alerts"`
	AvgLatencyMs         float64 `json:"avg_latency_ms"`
	TokensSaved          uint64  `json:"tokens_saved"`
	SavingsPercent       float64 `json:"savings_percent"`
	EstimatedUSDSaved    float64 `json:"estimated_usd_saved"`
}

// TelemetrySnapshot is the JSON response served by GET /api/stats and consumed by `toolprune top`.
type TelemetrySnapshot struct {
	Version               string          `json:"version"`
	StartedAt             time.Time       `json:"started_at"`
	UptimeSeconds         float64         `json:"uptime_seconds"`
	PricePerMillionTokens float64         `json:"price_per_million_tokens"`
	Totals                UpstreamStats   `json:"totals"`
	Upstreams             []UpstreamStats `json:"upstreams"`
}

type upstreamCounter struct {
	requests             uint64
	errors               uint64
	schemaTokensRaw      uint64
	schemaTokensPruned   uint64
	responseTokensRaw    uint64
	responseTokensPruned uint64
	securityAlerts       uint64
	totalLatencyNs       uint64
	latencySamples       uint64
}

// Registry provides lock-guarded, race-free live telemetry tracking per upstream.
type Registry struct {
	mu                    sync.RWMutex
	version               string
	startedAt             time.Time
	pricePerMillionTokens float64
	upstreams             map[string]*upstreamCounter
}

// NewRegistry creates a new live telemetry registry.
func NewRegistry(version string) *Registry {
	if version == "" {
		version = "1.4.1"
	}
	return &Registry{
		version:               version,
		startedAt:             time.Now().UTC(),
		pricePerMillionTokens: DefaultPricePerMillionTokens,
		upstreams:             make(map[string]*upstreamCounter),
	}
}

// SetPricePerMillionTokens configures the USD per 1M tokens rate used for savings estimation.
func (r *Registry) SetPricePerMillionTokens(usd float64) {
	if r == nil || math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return
	}
	r.mu.Lock()
	r.pricePerMillionTokens = usd
	r.mu.Unlock()
}

func (r *Registry) getOrCreateLocked(upstream string) *upstreamCounter {
	if upstream == "" {
		upstream = "default"
	}
	c, ok := r.upstreams[upstream]
	if !ok {
		c = &upstreamCounter{}
		r.upstreams[upstream] = c
	}
	return c
}

// RecordRequest records an MCP RPC request, its latency, and whether it ended in an error.
func (r *Registry) RecordRequest(upstream string, latency time.Duration, isErr bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	c := r.getOrCreateLocked(upstream)
	c.requests++
	if isErr {
		c.errors++
	}
	if latency >= 0 {
		c.totalLatencyNs += uint64(latency.Nanoseconds())
		c.latencySamples++
	}
	r.mu.Unlock()
}

// RecordSchemaPruning records raw and pruned token counts for a tools/list response.
func (r *Registry) RecordSchemaPruning(upstream string, rawTokens, prunedTokens int) {
	if r == nil {
		return
	}
	if rawTokens < 0 {
		rawTokens = 0
	}
	if prunedTokens < 0 {
		prunedTokens = 0
	}
	r.mu.Lock()
	c := r.getOrCreateLocked(upstream)
	c.schemaTokensRaw += uint64(rawTokens)
	c.schemaTokensPruned += uint64(prunedTokens)
	r.mu.Unlock()
}

// RecordResponsePruning records raw and pruned token counts for a tools/call response.
func (r *Registry) RecordResponsePruning(upstream string, rawTokens, prunedTokens int) {
	if r == nil {
		return
	}
	if rawTokens < 0 {
		rawTokens = 0
	}
	if prunedTokens < 0 {
		prunedTokens = 0
	}
	r.mu.Lock()
	c := r.getOrCreateLocked(upstream)
	c.responseTokensRaw += uint64(rawTokens)
	c.responseTokensPruned += uint64(prunedTokens)
	r.mu.Unlock()
}

// RecordSecurityAlert increments the security alert counter for an upstream.
func (r *Registry) RecordSecurityAlert(upstream string, count int) {
	if r == nil || count <= 0 {
		return
	}
	r.mu.Lock()
	c := r.getOrCreateLocked(upstream)
	c.securityAlerts += uint64(count)
	r.mu.Unlock()
}

// Snapshot returns a deterministic, thread-safe snapshot of all telemetry metrics.
func (r *Registry) Snapshot() TelemetrySnapshot {
	if r == nil {
		return TelemetrySnapshot{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.upstreams))
	for name := range r.upstreams {
		names = append(names, name)
	}
	sort.Strings(names)

	upstreams := make([]UpstreamStats, 0, len(names))
	var tot upstreamCounter

	for _, name := range names {
		c := r.upstreams[name]
		upstreams = append(upstreams, buildUpstreamStats(name, c, r.pricePerMillionTokens))
		tot.requests += c.requests
		tot.errors += c.errors
		tot.schemaTokensRaw += c.schemaTokensRaw
		tot.schemaTokensPruned += c.schemaTokensPruned
		tot.responseTokensRaw += c.responseTokensRaw
		tot.responseTokensPruned += c.responseTokensPruned
		tot.securityAlerts += c.securityAlerts
		tot.totalLatencyNs += c.totalLatencyNs
		tot.latencySamples += c.latencySamples
	}

	return TelemetrySnapshot{
		Version:               r.version,
		StartedAt:             r.startedAt,
		UptimeSeconds:         sanitizeFloat(time.Since(r.startedAt).Seconds()),
		PricePerMillionTokens: sanitizeFloat(r.pricePerMillionTokens),
		Totals:                buildUpstreamStats("total", &tot, r.pricePerMillionTokens),
		Upstreams:             upstreams,
	}
}

func sanitizeFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func buildUpstreamStats(name string, c *upstreamCounter, pricePerMillion float64) UpstreamStats {
	rawTotal := c.schemaTokensRaw + c.responseTokensRaw
	prunedTotal := c.schemaTokensPruned + c.responseTokensPruned
	var saved uint64
	if rawTotal > prunedTotal {
		saved = rawTotal - prunedTotal
	}
	var savingsPct float64
	if rawTotal > 0 {
		savingsPct = sanitizeFloat(float64(saved) * 100.0 / float64(rawTotal))
	}
	var avgMs float64
	if c.latencySamples > 0 {
		avgMs = sanitizeFloat((float64(c.totalLatencyNs) / float64(c.latencySamples)) / 1e6)
	}
	usdSaved := sanitizeFloat((float64(saved) / 1_000_000.0) * sanitizeFloat(pricePerMillion))

	return UpstreamStats{
		Upstream:             name,
		Requests:             c.requests,
		Errors:               c.errors,
		SchemaTokensRaw:      c.schemaTokensRaw,
		SchemaTokensPruned:   c.schemaTokensPruned,
		ResponseTokensRaw:    c.responseTokensRaw,
		ResponseTokensPruned: c.responseTokensPruned,
		SecurityAlerts:       c.securityAlerts,
		AvgLatencyMs:         avgMs,
		TokensSaved:          saved,
		SavingsPercent:       savingsPct,
		EstimatedUSDSaved:    usdSaved,
	}
}

// RenderPrometheus writes telemetry counters and gauges in Prometheus 0.0.4 text exposition format.
func (r *Registry) RenderPrometheus(w io.Writer) {
	snap := r.Snapshot()

	fmt.Fprintln(w, "# HELP toolprune_uptime_seconds ToolPrune process uptime in seconds.")
	fmt.Fprintln(w, "# TYPE toolprune_uptime_seconds gauge")
	fmt.Fprintf(w, "toolprune_uptime_seconds %.3f\n", snap.UptimeSeconds)

	fmt.Fprintln(w, "# HELP toolprune_requests_total Total MCP RPC requests handled per upstream.")
	fmt.Fprintln(w, "# TYPE toolprune_requests_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_requests_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.Requests)
	}

	fmt.Fprintln(w, "# HELP toolprune_errors_total Total MCP RPC errors per upstream.")
	fmt.Fprintln(w, "# TYPE toolprune_errors_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_errors_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.Errors)
	}

	fmt.Fprintln(w, "# HELP toolprune_schema_tokens_raw_total Raw tools/list schema tokens before pruning.")
	fmt.Fprintln(w, "# TYPE toolprune_schema_tokens_raw_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_schema_tokens_raw_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.SchemaTokensRaw)
	}

	fmt.Fprintln(w, "# HELP toolprune_schema_tokens_pruned_total Pruned tools/list schema tokens sent to clients.")
	fmt.Fprintln(w, "# TYPE toolprune_schema_tokens_pruned_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_schema_tokens_pruned_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.SchemaTokensPruned)
	}

	fmt.Fprintln(w, "# HELP toolprune_response_tokens_raw_total Raw tools/call response tokens before pruning.")
	fmt.Fprintln(w, "# TYPE toolprune_response_tokens_raw_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_response_tokens_raw_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.ResponseTokensRaw)
	}

	fmt.Fprintln(w, "# HELP toolprune_response_tokens_pruned_total Pruned tools/call response tokens sent to clients.")
	fmt.Fprintln(w, "# TYPE toolprune_response_tokens_pruned_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_response_tokens_pruned_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.ResponseTokensPruned)
	}

	fmt.Fprintln(w, "# HELP toolprune_security_alerts_total Total security poisoning and rug-pull alerts detected.")
	fmt.Fprintln(w, "# TYPE toolprune_security_alerts_total counter")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_security_alerts_total{upstream=\"%s\"} %d\n", escapePromLabel(u.Upstream), u.SecurityAlerts)
	}

	fmt.Fprintln(w, "# HELP toolprune_avg_latency_ms Average upstream request latency in milliseconds.")
	fmt.Fprintln(w, "# TYPE toolprune_avg_latency_ms gauge")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(w, "toolprune_avg_latency_ms{upstream=\"%s\"} %.3f\n", escapePromLabel(u.Upstream), u.AvgLatencyMs)
	}

	fmt.Fprintln(w, "# HELP toolprune_estimated_usd_saved Estimated USD saved via token pruning.")
	fmt.Fprintln(w, "# TYPE toolprune_estimated_usd_saved gauge")
	fmt.Fprintf(w, "toolprune_estimated_usd_saved %.6f\n", snap.Totals.EstimatedUSDSaved)
}

// escapePromLabel escapes label values per Prometheus 0.0.4 text exposition format:
// backslash -> \\, double-quote -> \", newline -> \n.
func escapePromLabel(v string) string {
	var b strings.Builder
	b.Grow(len(v))
	for _, r := range v {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Handler returns an http.Handler serving /metrics (Prometheus), /api/stats (JSON), and /healthz.
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		r.RenderPrometheus(w)
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(r.Snapshot())
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Server wraps an HTTP listener serving telemetry endpoints.
type Server struct {
	httpServer *http.Server
	ln         net.Listener
}

// StartServer binds addr and serves /metrics and /api/stats in the background until ctx or Shutdown.
func StartServer(addr string, reg *Registry, logger *slog.Logger) (*Server, error) {
	if strings.TrimSpace(addr) == "" {
		return nil, fmt.Errorf("metrics listen address cannot be empty")
	}
	if reg == nil {
		reg = NewRegistry("")
	}
	if logger == nil {
		logger = slog.Default()
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to bind metrics server on %q: %w", addr, err)
	}

	srv := &http.Server{
		Handler:           reg.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Error("telemetry metrics server exited with error", "addr", ln.Addr().String(), "err", err)
		}
	}()

	logger.Info("ToolPrune telemetry server listening",
		"addr", ln.Addr().String(),
		"metrics", fmt.Sprintf("http://%s/metrics", ln.Addr().String()),
		"stats", fmt.Sprintf("http://%s/api/stats", ln.Addr().String()),
	)

	return &Server{
		httpServer: srv,
		ln:         ln,
	}, nil
}

// Addr returns the bound listener network address.
func (s *Server) Addr() net.Addr {
	if s == nil || s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// Shutdown gracefully closes the telemetry HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}
