package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/doctor"
)

type doctorFlags struct {
	asJSON bool
	dir    string
}

var docFlags doctorFlags

var doctorCmd = &cobra.Command{
	Use:   "doctor [flags]",
	Short: "Run local MCP environment, upstream, and client preset health checks",
	Long: `Performs an actionable, strictly read-only health check of your local ToolPrune setup:
  1. Verifies ToolPrune binary build metadata and PATH discoverability.
  2. Validates toolprune.yaml (if present) and checks upstream executables/URLs.
  3. Scans project and user MCP client configs (.cursor/mcp.json, .vscode/mcp.json,
     .mcp.json, Windsurf, Claude Desktop, Zed) for unwrapped MCP servers.`,
	RunE: runDoctor,
}

func init() {
	doctorCmd.Flags().BoolVar(&docFlags.asJSON, "json", false, "Output health check report in structured JSON format")
	doctorCmd.Flags().StringVar(&docFlags.dir, "dir", "", "Target project directory to scan (default: current working directory)")
}

func runDoctor(cmd *cobra.Command, args []string) error {
	cfgPath := globalConfig
	asJSON := cmd.Flags().Changed("json") && docFlags.asJSON
	dirExplicit := cmd.Flags().Changed("dir")
	dirVal := ""
	if dirExplicit {
		dirVal = strings.TrimSpace(docFlags.dir)
	}
	defer func() {
		resetCommandFlags(cmd)
		globalConfig = ""
	}()

	rep := doctor.Run(doctor.Options{
		Version:     Version,
		Commit:      GitCommit,
		BuildDate:   BuildDate,
		ProjectDir:  dirVal,
		DirExplicit: dirExplicit,
		ConfigPath:  cfgPath,
	})

	outWriter := cmd.OutOrStdout()
	if asJSON {
		data, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal doctor report: %w", err)
		}
		fmt.Fprintln(outWriter, string(data))
	} else {
		doctor.FormatReport(outWriter, rep)
	}

	if !rep.Healthy {
		return fmt.Errorf("doctor detected %d issue(s): %s", len(rep.Errors), strings.Join(rep.Errors, "; "))
	}
	return nil
}
