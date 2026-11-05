package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/router"
)

type routesFlags struct {
	profile string
	asJSON  bool
	filter  string
}

var rFlags routesFlags

var routesCmd = &cobra.Command{
	Use:   "routes [flags]",
	Short: "Inspect registered upstreams and routing table",
	Long:  `Inspects configured upstream MCP servers, namespaced tools, active filters, and routing rules.`,
	RunE:  runRoutes,
}

func init() {
	routesCmd.Flags().StringVar(&rFlags.profile, "profile", "", "Evaluate routes under specific profile (full, read_only, dev)")
	routesCmd.Flags().BoolVar(&rFlags.asJSON, "json", false, "Output route table in JSON format")
	routesCmd.Flags().StringVar(&rFlags.filter, "filter", "", "Filter tool names matching substring or regex")
}

func runRoutes(cmd *cobra.Command, args []string) error {
	outWriter := cmd.OutOrStdout()
	configPath := globalConfig
	if configPath == "" {
		configPath = config.FindConfigFile()
	}
	if configPath == "" {
		return fmt.Errorf("no configuration file found. Specify --config <path> or run 'toolprune config init'")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration %q: %w", configPath, err)
	}

	activeProfile := cfg.Routing.DefaultProfile
	if rFlags.profile != "" {
		activeProfile = rFlags.profile
		cfg.Routing.DefaultProfile = activeProfile
	}

	var filterRe *regexp.Regexp
	if rFlags.filter != "" {
		reStr := router.GlobToRegex(rFlags.filter)
		var compileErr error
		filterRe, compileErr = regexp.Compile("(?i)" + reStr)
		if compileErr != nil {
			filterRe = regexp.MustCompile("(?i)" + regexp.QuoteMeta(rFlags.filter))
		}
	}

	filteredUpstreams := make([]config.UpstreamConfig, 0, len(cfg.Upstreams))
	for _, u := range cfg.Upstreams {
		if filterRe != nil {
			if !filterRe.MatchString(u.ID) && !filterRe.MatchString(u.Prefix) && !filterRe.MatchString(u.Command) {
				continue
			}
		}
		filteredUpstreams = append(filteredUpstreams, u)
	}

	if rFlags.asJSON {
		cfg.Upstreams = filteredUpstreams
		out, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Fprintln(outWriter, string(out))
		return nil
	}

	w := tabwriter.NewWriter(outWriter, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "UPSTREAM\tCOMMAND\tPREFIX\tDISABLED")
	for _, u := range filteredUpstreams {
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", u.ID, u.Command, u.Prefix, u.Disabled)
	}
	_ = w.Flush()

	fmt.Fprintf(outWriter, "\nNamespace Mode: %s\nDefault Tier:   %s\nActive Profile: %s\n",
		cfg.Routing.NamespaceMode, cfg.Pruning.Tier, activeProfile)
	if rFlags.filter != "" {
		fmt.Fprintf(outWriter, "Active Filter:  %s (%d/%d matched)\n", rFlags.filter, len(filteredUpstreams), len(cfg.Upstreams))
	}

	return nil
}
