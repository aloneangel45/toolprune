package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
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
	routesCmd.Flags().StringVar(&rFlags.profile, "profile", "", "Evaluate routes under specific profile (full, read_only, dev, minimal, or custom)")
	routesCmd.Flags().BoolVar(&rFlags.asJSON, "json", false, "Output route table in JSON format")
	routesCmd.Flags().StringVar(&rFlags.filter, "filter", "", "Filter upstream or tool routes matching substring, glob, or regex")
}

type evaluatedRouteEntry struct {
	config.UpstreamConfig
	EffectivePrefix string `json:"effectivePrefix"`
	ProfileAccess   string `json:"profileAccess"`
}

type routesJSONOutput struct {
	NamespaceMode  string                `json:"namespaceMode"`
	DefaultTier    string                `json:"defaultTier"`
	ActiveProfile  string                `json:"activeProfile"`
	ActiveFilter   string                `json:"activeFilter,omitempty"`
	ProfileInclude []string              `json:"profileInclude,omitempty"`
	ProfileExclude []string              `json:"profileExclude,omitempty"`
	GlobalExclude  []string              `json:"globalExclude,omitempty"`
	Upstreams      []evaluatedRouteEntry `json:"upstreams"`
}

func evaluateUpstreamProfileAccess(tf *router.ToolFilter, prefix string, disabled bool) (access string, anyAllowed bool) {
	if disabled {
		return "disabled", false
	}
	if tf == nil {
		return "full", true
	}
	readProbe := prefix + "__list_items"
	getProbe := prefix + "__get_item"
	writeProbe := prefix + "__create_item"
	deleteProbe := prefix + "__delete_item"

	readOK := tf.IsAllowed(readProbe) || tf.IsAllowed(getProbe)
	writeOK := tf.IsAllowed(writeProbe)
	deleteOK := tf.IsAllowed(deleteProbe)

	switch {
	case readOK && writeOK && deleteOK:
		return "full", true
	case readOK && writeOK && !deleteOK:
		return "read/write (safe)", true
	case readOK && !writeOK:
		return "read-only", true
	case writeOK || deleteOK:
		return "restricted", true
	default:
		return "blocked", false
	}
}

func runRoutes(cmd *cobra.Command, args []string) error {
	defer resetCommandFlags(cmd)
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

	profileChanged := cmd.Flags().Changed("profile")
	filterChanged := cmd.Flags().Changed("filter")
	jsonChanged := cmd.Flags().Changed("json")

	profileArg := ""
	if profileChanged {
		profileArg = strings.TrimSpace(rFlags.profile)
	}
	filterArg := ""
	if filterChanged {
		filterArg = strings.TrimSpace(rFlags.filter)
	}
	asJSON := jsonChanged && rFlags.asJSON

	activeProfile := cfg.Routing.DefaultProfile
	if profileArg != "" {
		activeProfile = profileArg
		cfg.Routing.DefaultProfile = activeProfile
	}

	filterCfg := cfg.ToFilterConfig()
	filterCfg.ActiveProfile = activeProfile
	toolFilter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		return fmt.Errorf("invalid routing profile %q: %w", activeProfile, err)
	}

	var filterRe *regexp.Regexp
	if filterArg != "" {
		var compileErr error
		if strings.ContainsAny(filterArg, "*?") && !strings.Contains(filterArg, ".*") {
			reStr := router.GlobToRegex(filterArg)
			filterRe, compileErr = regexp.Compile("(?i)" + reStr)
		} else {
			filterRe, compileErr = regexp.Compile("(?i)" + filterArg)
		}
		if compileErr != nil {
			filterRe = regexp.MustCompile("(?i)" + regexp.QuoteMeta(filterArg))
		}
	}

	totalConfigured := len(cfg.Upstreams)
	evaluated := make([]evaluatedRouteEntry, 0, totalConfigured)
	for _, u := range cfg.Upstreams {
		effPrefix := strings.TrimSpace(u.Prefix)
		if effPrefix == "" {
			effPrefix = u.ID
		}

		if filterRe != nil {
			target := u.Command
			if (u.Transport == "sse" || u.Transport == "streamable-http") && u.URL != "" {
				target = u.URL
			}
			matched := filterRe.MatchString(u.ID) ||
				filterRe.MatchString(effPrefix) ||
				filterRe.MatchString(target) ||
				filterRe.MatchString(u.Description) ||
				filterRe.MatchString(effPrefix+"__")
			if !matched {
				continue
			}
		}

		access, allowed := evaluateUpstreamProfileAccess(toolFilter, effPrefix, u.Disabled)
		if profileChanged && !u.Disabled && !allowed {
			continue
		}

		evaluated = append(evaluated, evaluatedRouteEntry{
			UpstreamConfig:  u,
			EffectivePrefix: effPrefix,
			ProfileAccess:   access,
		})
	}

	activeRules := filterCfg.Profiles[activeProfile]

	if asJSON {
		payload := routesJSONOutput{
			NamespaceMode:  cfg.Routing.NamespaceMode,
			DefaultTier:    cfg.Pruning.Tier,
			ActiveProfile:  activeProfile,
			ActiveFilter:   filterArg,
			ProfileInclude: activeRules.Include,
			ProfileExclude: activeRules.Exclude,
			GlobalExclude:  filterCfg.GlobalExclude,
			Upstreams:      evaluated,
		}
		out, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(outWriter, string(out))
		return nil
	}

	w := tabwriter.NewWriter(outWriter, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "UPSTREAM\tENDPOINT\tPREFIX\tACCESS\tDISABLED")
	for _, entry := range evaluated {
		endpoint := entry.Command
		if (entry.Transport == "sse" || entry.Transport == "streamable-http") && entry.URL != "" {
			endpoint = entry.URL
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\n",
			entry.ID, endpoint, entry.EffectivePrefix, entry.ProfileAccess, entry.Disabled)
	}
	_ = w.Flush()

	fmt.Fprintf(outWriter, "\nNamespace Mode: %s\nDefault Tier:   %s\nActive Profile: %s\n",
		cfg.Routing.NamespaceMode, cfg.Pruning.Tier, activeProfile)
	if len(activeRules.Include) > 0 {
		fmt.Fprintf(outWriter, "Include Rules:  %s\n", strings.Join(activeRules.Include, ", "))
	}
	if len(activeRules.Exclude) > 0 {
		fmt.Fprintf(outWriter, "Exclude Rules:  %s\n", strings.Join(activeRules.Exclude, ", "))
	}
	if filterArg != "" {
		fmt.Fprintf(outWriter, "Active Filter:  %s (%d/%d matched)\n", filterArg, len(evaluated), totalConfigured)
	}

	return nil
}
