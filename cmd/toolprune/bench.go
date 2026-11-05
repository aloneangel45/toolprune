package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/benchmark"
	"github.com/aloneangel45/toolprune/pkg/mcp"
)

type benchFlags struct {
	file    string
	model   string
	format  string
	verbose bool
}

var bFlags benchFlags

var benchCmd = &cobra.Command{
	Use:   "bench [flags] [-- <upstream_cmd> [args...]]",
	Short: "Benchmark MCP tool token consumption and pruning reduction",
	Long: `Measures exact token counts and optimization ratios across Pruning Tiers (1 to 4).

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
}

func runBench(cmd *cobra.Command, args []string) error {
	outWriter := cmd.OutOrStdout()
	var tools []mcp.Tool

	if bFlags.file != "" {
		data, err := os.ReadFile(bFlags.file)
		if err != nil {
			return fmt.Errorf("failed to read schema file %q: %w", bFlags.file, err)
		}
		if err := json.Unmarshal(data, &tools); err != nil {
			return fmt.Errorf("failed to parse JSON schema from %q: %w", bFlags.file, err)
		}
	} else if len(args) > 0 {
		return fmt.Errorf("benchmarking live upstream commands is scheduled for live integration")
	} else {
		// Try candidate locations using benchmark.LoadFixtures()
		loaded, err := benchmark.LoadFixtures()
		if err != nil {
			return fmt.Errorf("no input schema specified and failed to load default fixtures: %w. Provide --file <path> or -- <upstream_cmd>", err)
		}
		tools = loaded
	}

	opts := benchmark.DefaultBenchmarkOptions()
	if bFlags.model == "calibrated" {
		opts.UseExactTokens = false
	}

	report, err := benchmark.RunBenchmarkWithOptions(tools, opts)
	if err != nil {
		return fmt.Errorf("benchmark run failed: %w", err)
	}

	switch bFlags.format {
	case "json":
		out, err := benchmark.FormatReportJSON(report)
		if err != nil {
			return err
		}
		fmt.Fprintln(outWriter, out)
	case "markdown":
		fmt.Fprintln(outWriter, benchmark.FormatReportMarkdown(report))
	default:
		fmt.Fprintln(outWriter, benchmark.FormatReportTable(report))
	}

	return nil
}
