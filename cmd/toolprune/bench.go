package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
)

type benchFlags struct {
	file            string
	model           string
	format          string
	verbose         bool
	maxSchemaTokens int
}

var bFlags benchFlags

var benchCmd = &cobra.Command{
	Use:   "bench [flags] [-- <upstream_cmd> [args...]]",
	Short: "Benchmark MCP tool token consumption and pruning reduction",
	Long: `Measures exact token counts and optimization ratios across Pruning Tiers (1 to 4)
and optional RFC #1 token budgets (--max-schema-tokens).

Can evaluate:
  1. A static JSON schema fixtures file:
     toolprune bench --file test/fixtures/schemas.json

  2. A live upstream server command:
     toolprune bench -- npx -y @modelcontextprotocol/server-github`,
	RunE: runBench,
}

func init() {
	benchCmd.Flags().StringVarP(&bFlags.file, "file", "f", "", "Path to JSON schema fixtures file")
	benchCmd.Flags().StringVar(&bFlags.model, "model", "cl100k_base", "Tokenizer model (cl100k_base, calibrated)")
	benchCmd.Flags().StringVar(&bFlags.format, "format", "table", "Output format (table, json, markdown, csv)")
	benchCmd.Flags().BoolVarP(&bFlags.verbose, "verbose", "v", false, "Display per-parameter token breakdown")
	benchCmd.Flags().IntVar(&bFlags.maxSchemaTokens, "max-schema-tokens", 0, "Evaluate RFC #1 progressive token-budget cap (env: TOOLPRUNE_MAX_SCHEMA_TOKENS)")
}

func benchmarkLiveUpstream(parentCtx context.Context, cmdArgs []string, logger *slog.Logger) ([]mcp.Tool, error) {
	ctx, cancel := context.WithTimeout(parentCtx, 30*time.Second)
	defer cancel()

	sup := proxy.NewSupervisor(proxy.SupervisorConfig{
		Name:         "bench-upstream",
		Command:      cmdArgs[0],
		Args:         cmdArgs[1:],
		GracePeriod:  2 * time.Second,
		Logger:       logger,
		RingCapacity: 100,
	})
	if err := sup.Start(ctx); err != nil {
		return nil, fmt.Errorf("failed to start live upstream %q: %w", cmdArgs[0], err)
	}
	defer func() {
		_ = sup.Stop()
	}()

	tools, err := performUpstreamHandshakeWithTimeout(ctx, sup, 20*time.Second)
	if err != nil {
		return nil, fmt.Errorf("live upstream handshake failed: %w", err)
	}
	return tools, nil
}

func runBench(cmd *cobra.Command, args []string) error {
	defer resetCommandFlags(cmd)
	outWriter := cmd.OutOrStdout()
	logger := setupLogger(globalLogLevel)

	fileArg := ""
	if cmd.Flags().Changed("file") {
		fileArg = strings.TrimSpace(bFlags.file)
	}
	modelArg := "cl100k_base"
	if cmd.Flags().Changed("model") {
		modelArg = strings.ToLower(strings.TrimSpace(bFlags.model))
	}
	formatArg := "table"
	if cmd.Flags().Changed("format") {
		formatArg = strings.ToLower(strings.TrimSpace(bFlags.format))
	}
	verboseArg := cmd.Flags().Changed("verbose") && bFlags.verbose

	maxBudget := 0
	if cmd.Flags().Changed("max-schema-tokens") {
		maxBudget = bFlags.maxSchemaTokens
	} else if envVal := strings.TrimSpace(os.Getenv("TOOLPRUNE_MAX_SCHEMA_TOKENS")); envVal != "" {
		if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
			maxBudget = parsed
		}
	}

	var tools []mcp.Tool
	if fileArg != "" {
		data, err := os.ReadFile(fileArg)
		if err != nil {
			return fmt.Errorf("failed to read schema file %q: %w", fileArg, err)
		}
		if err := json.Unmarshal(data, &tools); err != nil {
			return fmt.Errorf("failed to parse JSON schema from %q: %w", fileArg, err)
		}
	} else if len(args) > 0 {
		liveTools, err := benchmarkLiveUpstream(cmd.Context(), args, logger)
		if err != nil {
			return err
		}
		tools = liveTools
	} else {
		loaded, err := benchmark.LoadFixtures()
		if err != nil {
			return fmt.Errorf("no input schema specified and failed to load default fixtures: %w. Provide --file <path> or -- <upstream_cmd>", err)
		}
		tools = loaded
	}

	opts := benchmark.DefaultBenchmarkOptions()
	if modelArg == "calibrated" {
		opts.UseExactTokens = false
	}
	opts.IncludeParamBreakdown = verboseArg
	opts.MaxSchemaTokens = maxBudget

	report, err := benchmark.RunBenchmarkWithOptions(tools, opts)
	if err != nil {
		return fmt.Errorf("benchmark run failed: %w", err)
	}

	switch formatArg {
	case "json":
		out, err := benchmark.FormatReportJSON(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(outWriter, out)
	case "markdown", "md":
		fmt.Fprintln(outWriter, benchmark.FormatReportMarkdown(report))
	case "csv":
		out, err := benchmark.FormatReportCSV(report)
		if err != nil {
			return err
		}
		fmt.Fprint(outWriter, out)
	case "table":
		fmt.Fprintln(outWriter, benchmark.FormatReportTable(report))
	default:
		return fmt.Errorf("unsupported benchmark format %q: must be table, json, markdown, or csv", formatArg)
	}

	return nil
}
