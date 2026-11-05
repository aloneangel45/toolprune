package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

var (
	shortVersion bool
	jsonVersion  bool
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print ToolPrune version and build telemetry",
	Run: func(cmd *cobra.Command, args []string) {
		outWriter := cmd.OutOrStdout()
		isJSON, _ := cmd.Flags().GetBool("json")
		isShort, _ := cmd.Flags().GetBool("short")

		if isJSON {
			info := map[string]string{
				"version":   Version,
				"commit":    GitCommit,
				"buildDate": BuildDate,
				"goVersion": runtime.Version(),
				"os":        runtime.GOOS,
				"arch":      runtime.GOARCH,
			}
			out, _ := json.MarshalIndent(info, "", "  ")
			fmt.Fprintln(outWriter, string(out))
			return
		}

		if isShort {
			fmt.Fprintln(outWriter, Version)
			return
		}

		fmt.Fprintf(outWriter, "ToolPrune v%s\n", Version)
		fmt.Fprintf(outWriter, "  Commit:     %s\n", GitCommit)
		fmt.Fprintf(outWriter, "  Build Date: %s\n", BuildDate)
		fmt.Fprintf(outWriter, "  Go Version: %s\n", runtime.Version())
		fmt.Fprintf(outWriter, "  OS/Arch:    %s/%s\n", runtime.GOOS, runtime.GOARCH)
	},
}

func init() {
	versionCmd.Flags().BoolVar(&shortVersion, "short", false, "Print version number only")
	versionCmd.Flags().BoolVar(&jsonVersion, "json", false, "Output version telemetry in JSON")
}
