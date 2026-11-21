package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/metrics"
)

type topFlags struct {
	url      string
	once     bool
	interval time.Duration
}

var tFlags topFlags

var topCmd = &cobra.Command{
	Use:   "top [flags]",
	Short: "Live terminal dashboard for ToolPrune token savings, latency, and security telemetry",
	Long: `Connects to a running ToolPrune proxy telemetry endpoint (--metrics-addr) and displays
live per-upstream token pruning savings, response pruning ratios, average latency,
security alerts, and estimated USD saved.

Examples:
  toolprune top
  toolprune top --url http://127.0.0.1:9095/api/stats --once
  toolprune top --interval 2s`,
	RunE: runTop,
}

func init() {
	topCmd.Flags().StringVar(&tFlags.url, "url", "http://127.0.0.1:9095/api/stats", "Telemetry JSON stats URL served by toolprune proxy --metrics-addr")
	topCmd.Flags().BoolVar(&tFlags.once, "once", false, "Print a single telemetry snapshot and exit")
	topCmd.Flags().DurationVar(&tFlags.interval, "interval", time.Second, "Refresh interval in live dashboard mode")
}

func runTop(cmd *cobra.Command, args []string) error {
	targetURL := "http://127.0.0.1:9095/api/stats"
	if cmd.Flags().Changed("url") {
		targetURL = strings.TrimSpace(tFlags.url)
	}
	once := cmd.Flags().Changed("once") && tFlags.once
	interval := time.Second
	if cmd.Flags().Changed("interval") {
		interval = tFlags.interval
	}
	defer resetCommandFlags(cmd)

	if interval <= 0 {
		return fmt.Errorf("invalid --interval %s: must be positive", interval)
	}

	client := &http.Client{Timeout: 5 * time.Second}
	out := cmd.OutOrStdout()

	if once {
		snap, err := fetchTelemetrySnapshot(cmd.Context(), client, targetURL)
		if err != nil {
			return err
		}
		renderTopDashboard(out, targetURL, snap)
		return nil
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	first := true
	for {
		snap, err := fetchTelemetrySnapshot(ctx, client, targetURL)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if !first {
			fmt.Fprint(out, "\033[H\033[2J")
		}
		first = false
		renderTopDashboard(out, targetURL, snap)

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func fetchTelemetrySnapshot(ctx context.Context, client *http.Client, targetURL string) (*metrics.TelemetrySnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid telemetry URL %q: %w", targetURL, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to ToolPrune telemetry endpoint %q: %w (hint: start proxy with --metrics-addr 127.0.0.1:9095)", targetURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telemetry endpoint %q returned HTTP %d", targetURL, resp.StatusCode)
	}

	var snap metrics.TelemetrySnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return nil, fmt.Errorf("failed to decode telemetry JSON from %q: %w", targetURL, err)
	}
	return &snap, nil
}

func renderTopDashboard(w io.Writer, endpoint string, snap *metrics.TelemetrySnapshot) {
	if snap == nil {
		return
	}
	uptime := (time.Duration(snap.UptimeSeconds * float64(time.Second))).Round(time.Second)
	fmt.Fprintf(w, "ToolPrune Live Telemetry (v%s)  |  Endpoint: %s  |  Uptime: %s\n",
		snap.Version, endpoint, uptime)
	fmt.Fprintln(w, strings.Repeat("=", 96))
	fmt.Fprintf(w, "Requests: %d  |  Errors: %d  |  Avg Latency: %.2fms  |  Security Alerts: %d\n",
		snap.Totals.Requests, snap.Totals.Errors, snap.Totals.AvgLatencyMs, snap.Totals.SecurityAlerts)
	fmt.Fprintf(w, "Tokens Saved: %d (%.1f%%)  |  Est. USD Saved: $%.4f (@ $%.2f/1M tok)\n",
		snap.Totals.TokensSaved, snap.Totals.SavingsPercent, snap.Totals.EstimatedUSDSaved, snap.PricePerMillionTokens)
	fmt.Fprintln(w, strings.Repeat("-", 96))

	if len(snap.Upstreams) == 0 {
		fmt.Fprintln(w, "No upstream traffic recorded yet.")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UPSTREAM\tREQS\tERRS\tSCHEMA (RAW->PRUNED)\tRESP (RAW->PRUNED)\tALERTS\tAVG MS\tSAVED %\tUSD SAVED")
	for _, u := range snap.Upstreams {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d->%d\t%d->%d\t%d\t%.2f\t%.1f%%\t$%.4f\n",
			u.Upstream,
			u.Requests,
			u.Errors,
			u.SchemaTokensRaw,
			u.SchemaTokensPruned,
			u.ResponseTokensRaw,
			u.ResponseTokensPruned,
			u.SecurityAlerts,
			u.AvgLatencyMs,
			u.SavingsPercent,
			u.EstimatedUSDSaved,
		)
	}
	fmt.Fprintf(tw, "TOTAL\t%d\t%d\t%d->%d\t%d->%d\t%d\t%.2f\t%.1f%%\t$%.4f\n",
		snap.Totals.Requests,
		snap.Totals.Errors,
		snap.Totals.SchemaTokensRaw,
		snap.Totals.SchemaTokensPruned,
		snap.Totals.ResponseTokensRaw,
		snap.Totals.ResponseTokensPruned,
		snap.Totals.SecurityAlerts,
		snap.Totals.AvgLatencyMs,
		snap.Totals.SavingsPercent,
		snap.Totals.EstimatedUSDSaved,
	)
	_ = tw.Flush()
}
