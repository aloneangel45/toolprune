package main

import (
	"os"

	"github.com/spf13/cobra"

	presetcmd "github.com/aloneangel45/toolprune/cmd/preset"
)

var (
	// Build metadata populated via -ldflags during build
	Version   = "1.0.0"
	GitCommit = "dev"
	BuildDate = "unknown"

	// Global flag variables
	globalLogLevel string
	globalConfig   string
)

var rootCmd = &cobra.Command{
	Use:   "toolprune",
	Short: "ToolPrune: High-utility, zero-bloat MCP tool router and token squeezer",
	Long: `ToolPrune is a high-performance MCP (Model Context Protocol) proxy and router
that sits between AI clients (Claude Desktop, Cursor) and upstream MCP servers.
It dynamically compresses and prunes verbose tool schemas, achieving ~80% context
token reduction without breaking tool usability.`,
	SilenceUsage:  true,
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		// Environment fallback for global flags
		if !cmd.Flags().Changed("log-level") {
			if env := os.Getenv("TOOLPRUNE_LOG_LEVEL"); env != "" {
				globalLogLevel = env
			}
		}
		if !cmd.Flags().Changed("config") {
			if env := os.Getenv("TOOLPRUNE_CONFIG"); env != "" {
				globalConfig = env
			}
		}
		setupLogger(globalLogLevel)
	},
}

func init() {
	rootCmd.PersistentFlags().StringVar(&globalLogLevel, "log-level", "info", "Logging level (debug, info, warn, error)")
	rootCmd.PersistentFlags().StringVarP(&globalConfig, "config", "c", "", "Path to toolprune.yaml configuration file")

	// Register subcommands
	rootCmd.AddCommand(proxyCmd)
	rootCmd.AddCommand(benchCmd)
	rootCmd.AddCommand(routesCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(presetcmd.NewCommand())
	rootCmd.AddCommand(versionCmd)
}
