package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRegistry_ConcurrentRecordingAndSnapshot(t *testing.T) {
	reg := NewRegistry("1.4.0")
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			up := "github"
			if idx%2 == 1 {
				up = "postgres"
			}
			for j := 0; j < 50; j++ {
				reg.RecordRequest(up, 10*time.Millisecond, j%10 == 0)
				reg.RecordSchemaPruning(up, 1000, 200)
				reg.RecordResponsePruning(up, 500, 100)
				if j == 0 {
					reg.RecordSecurityAlert(up, 1)
				}
			}
		}(i)
	}
	wg.Wait()

	snap := reg.Snapshot()
	if snap.Version != "1.4.0" {
		t.Fatalf("expected version 1.4.0, got %q", snap.Version)
	}
	if len(snap.Upstreams) != 2 {
		t.Fatalf("expected 2 upstreams, got %d", len(snap.Upstreams))
	}
	if snap.Totals.Requests != 400 {
		t.Errorf("expected 400 requests, got %d", snap.Totals.Requests)
	}
	if snap.Totals.Errors != 40 {
		t.Errorf("expected 40 errors, got %d", snap.Totals.Errors)
	}
	if snap.Totals.SchemaTokensRaw != 400000 || snap.Totals.SchemaTokensPruned != 80000 {
		t.Errorf("unexpected schema tokens: raw=%d pruned=%d", snap.Totals.SchemaTokensRaw, snap.Totals.SchemaTokensPruned)
	}
	if snap.Totals.ResponseTokensRaw != 200000 || snap.Totals.ResponseTokensPruned != 40000 {
		t.Errorf("unexpected response tokens: raw=%d pruned=%d", snap.Totals.ResponseTokensRaw, snap.Totals.ResponseTokensPruned)
	}
	if snap.Totals.SecurityAlerts != 8 {
		t.Errorf("expected 8 security alerts, got %d", snap.Totals.SecurityAlerts)
	}
	if snap.Totals.TokensSaved != 480000 {
		t.Errorf("expected 480000 saved tokens, got %d", snap.Totals.TokensSaved)
	}
	if snap.Totals.AvgLatencyMs < 9.9 || snap.Totals.AvgLatencyMs > 10.1 {
		t.Errorf("expected avg latency ~10ms, got %.3f", snap.Totals.AvgLatencyMs)
	}
	// 480,000 tokens * $3.00 / 1,000,000 = $1.44
	if snap.Totals.EstimatedUSDSaved < 1.43 || snap.Totals.EstimatedUSDSaved > 1.45 {
		t.Errorf("expected ~$1.44 USD saved, got %.4f", snap.Totals.EstimatedUSDSaved)
	}
}

func TestRegistry_HTTPHandlersPrometheusAndJSON(t *testing.T) {
	reg := NewRegistry("1.4.0")
	reg.RecordRequest("github", 25*time.Millisecond, false)
	reg.RecordSchemaPruning("github", 2000, 400)
	reg.RecordResponsePruning("github", 1000, 250)
	reg.RecordSecurityAlert("github", 2)

	ts := httptest.NewServer(reg.Handler())
	defer ts.Close()

	// 1. GET /metrics (Prometheus text format)
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /metrics, got %d", resp.StatusCode)
	}
	prom := string(bodyBytes)
	for _, expected := range []string{
		`# TYPE toolprune_requests_total counter`,
		`toolprune_requests_total{upstream="github"} 1`,
		`toolprune_schema_tokens_raw_total{upstream="github"} 2000`,
		`toolprune_schema_tokens_pruned_total{upstream="github"} 400`,
		`toolprune_response_tokens_raw_total{upstream="github"} 1000`,
		`toolprune_response_tokens_pruned_total{upstream="github"} 250`,
		`toolprune_security_alerts_total{upstream="github"} 2`,
		`toolprune_avg_latency_ms{upstream="github"} 25.000`,
		`toolprune_estimated_usd_saved`,
	} {
		if !strings.Contains(prom, expected) {
			t.Errorf("missing Prometheus line %q in:\n%s", expected, prom)
		}
	}

	// 2. GET /api/stats (JSON)
	resp2, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats failed: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /api/stats, got %d", resp2.StatusCode)
	}
	var snap TelemetrySnapshot
	if err := json.NewDecoder(resp2.Body).Decode(&snap); err != nil {
		t.Fatalf("decode /api/stats failed: %v", err)
	}
	if snap.Totals.Requests != 1 || snap.Totals.SecurityAlerts != 2 || len(snap.Upstreams) != 1 {
		t.Fatalf("unexpected /api/stats snapshot: %+v", snap)
	}
}

func TestStartServerAndShutdown(t *testing.T) {
	reg := NewRegistry("1.4.0")
	srv, err := StartServer("127.0.0.1:0", reg, nil)
	if err != nil {
		t.Fatalf("StartServer failed: %v", err)
	}
	addr := srv.Addr().String()
	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
}

func TestRegistry_PrometheusLabelEscapingAndNaNInfGuards(t *testing.T) {
	reg := NewRegistry("1.4.1")
	// Special chars: backslash, quote, newline, tab, and Unicode
	trickyUpstream := "srv\\prod\"alpha\nbeta\t日本語"
	reg.RecordRequest(trickyUpstream, 5*time.Millisecond, false)

	// Invalid NaN / Inf / negative prices must be rejected so JSON/Prometheus never emit NaN/Inf
	reg.SetPricePerMillionTokens(math.NaN())
	reg.SetPricePerMillionTokens(math.Inf(1))
	reg.SetPricePerMillionTokens(math.Inf(-1))
	reg.SetPricePerMillionTokens(-5.0)

	// Zero-sample / zero-token upstream must not divide by zero
	reg.RecordSecurityAlert("zero_div", 1)

	var promBuf bytes.Buffer
	reg.RenderPrometheus(&promBuf)
	prom := promBuf.String()

	// Strict Prometheus 0.0.4 label escaping: \ -> \\, " -> \", \n -> \n (and NO Go %q escapes like \t or \u)
	wantLabel := `upstream="srv\\prod\"alpha\nbeta	日本語"`
	if !strings.Contains(prom, wantLabel) {
		t.Errorf("expected Prometheus 0.0.4 escaped label %s in:\n%s", wantLabel, prom)
	}
	if strings.Contains(prom, `\t`) || strings.Contains(prom, `\u`) {
		t.Errorf("Prometheus output must not contain Go %%q escape sequences (\\t or \\u):\n%s", prom)
	}

	// Verify /api/stats marshals cleanly to valid JSON with zero NaN/Inf
	ts := httptest.NewServer(reg.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/stats")
	if err != nil {
		t.Fatalf("GET /api/stats failed: %v", err)
	}
	defer resp.Body.Close()
	var snap TelemetrySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("expected valid JSON from /api/stats after NaN/Inf guard: %v", err)
	}
	if snap.PricePerMillionTokens != DefaultPricePerMillionTokens {
		t.Errorf("expected default price %.2f preserved, got %.2f", DefaultPricePerMillionTokens, snap.PricePerMillionTokens)
	}
	for _, u := range snap.Upstreams {
		if math.IsNaN(u.AvgLatencyMs) || math.IsInf(u.AvgLatencyMs, 0) || math.IsNaN(u.SavingsPercent) || math.IsInf(u.SavingsPercent, 0) {
			t.Errorf("non-finite float metric detected for %q: %+v", u.Upstream, u)
		}
	}
}

func TestStartServer_TimeoutsConfigured(t *testing.T) {
	srv, err := StartServer("127.0.0.1:0", NewRegistry("1.4.1"), nil)
	if err != nil {
		t.Fatalf("StartServer failed: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if srv.httpServer.ReadHeaderTimeout <= 0 || srv.httpServer.IdleTimeout <= 0 || srv.httpServer.ReadTimeout <= 0 || srv.httpServer.WriteTimeout <= 0 {
		t.Fatalf("metrics http.Server missing required timeouts: ReadHeader=%v Read=%v Write=%v Idle=%v",
			srv.httpServer.ReadHeaderTimeout, srv.httpServer.ReadTimeout, srv.httpServer.WriteTimeout, srv.httpServer.IdleTimeout)
	}
}
