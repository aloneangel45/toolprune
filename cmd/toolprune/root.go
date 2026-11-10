package main

import (
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	presetcmd "github.com/aloneangel45/toolprune/cmd/preset"
)

var (
	// Build metadata populated via -ldflags during build
	Version   = "1.2.0"
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

// resetCommandFlags resets local and persistent flag values & Changed bits after command execution
// so repeated in-process rootCmd.Execute() invocations do not leak flag state across runs.
func resetCommandFlags(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	resetSet := func(fs *pflag.FlagSet) {
		if fs == nil {
			return
		}
		fs.VisitAll(func(f *pflag.Flag) {
			if f.Changed {
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					_ = sv.Replace(nil)
				} else {
					_ = f.Value.Set(f.DefValue)
				}
				f.Changed = false
			}
		})
	}
	resetSet(cmd.LocalFlags())
	resetSet(rootCmd.PersistentFlags())
}
