package presetcmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/preset"
)

type presetFlags struct {
	client    string
	output    string
	name      string
	command   string
	args      []string
	env       []string
	serverURL string
	config    string
	merge     bool
	stdout    bool
}

var prFlags presetFlags

var presetCmd = &cobra.Command{
	Use:   "preset",
	Short: "Generate MCP client config presets (Cursor, Windsurf)",
	Long: `Generate out-of-the-box MCP client configuration presets that wire AI clients
to a ToolPrune proxy instance.

Targets:
  cursor    .cursor/mcp.json (project-level)
  windsurf  ~/.codeium/windsurf/mcp_config.json (global)`,
}

var presetGenerateCmd = &cobra.Command{
	Use:   "generate --client <cursor|windsurf> [flags]",
	Short: "Generate a client MCP config preset for ToolPrune",
	RunE:  runPresetGenerate,
}

var presetValidateCmd = &cobra.Command{
	Use:   "validate <file>",
	Short: "Validate a client MCP config file against the expected schema",
	Args:  cobra.ExactArgs(1),
	RunE:  runPresetValidate,
}

func NewCommand() *cobra.Command {
	presetGenerateCmd.Flags().StringVar(&prFlags.client, "client", "cursor", "Target client (cursor, windsurf)")
	presetGenerateCmd.Flags().StringVarP(&prFlags.output, "output", "o", "", "Output file path (default: client's conventional path; use '-' for stdout)")
	presetGenerateCmd.Flags().StringVar(&prFlags.name, "name", "toolprune", "Server name under mcpServers")
	presetGenerateCmd.Flags().StringVar(&prFlags.command, "command", "toolprune", "Executable used to launch the ToolPrune proxy")
	presetGenerateCmd.Flags().StringSliceVar(&prFlags.args, "args", []string{"proxy"}, "Arguments passed to the proxy command")
	presetGenerateCmd.Flags().StringSliceVar(&prFlags.env, "env", nil, "Extra environment variables as KEY=VALUE (repeatable)")
	presetGenerateCmd.Flags().StringVar(&prFlags.serverURL, "server-url", "", "Emit a remote {serverUrl} entry instead of a stdio command (e.g. http://127.0.0.1:8080/sse)")
	presetGenerateCmd.Flags().StringVarP(&prFlags.config, "config", "c", "", "toolprune.yaml path to pass to the proxy as --config")
	presetGenerateCmd.Flags().BoolVar(&prFlags.merge, "merge", false, "Merge into an existing config file at --output, preserving other servers")
	presetGenerateCmd.Flags().BoolVar(&prFlags.stdout, "stdout", false, "Print the generated config to stdout instead of writing a file")

	presetCmd.AddCommand(presetGenerateCmd)
	presetCmd.AddCommand(presetValidateCmd)
	return presetCmd
}

func parseEnvPairs(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := splitKeyValue(p)
		if !ok {
			return nil, fmt.Errorf("invalid --env entry %q: expected KEY=VALUE", p)
		}
		env[k] = v
	}
	return env, nil
}

func splitKeyValue(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

func runPresetGenerate(cmd *cobra.Command, args []string) error {
	client, err := preset.ParseClient(prFlags.client)
	if err != nil {
		return err
	}
	env, err := parseEnvPairs(prFlags.env)
	if err != nil {
		return err
	}

	opts := preset.Options{
		Client:     client,
		ServerName: prFlags.name,
		Command:    prFlags.command,
		Args:       prFlags.args,
		Env:        env,
		ServerURL:  prFlags.serverURL,
		ConfigPath: prFlags.config,
	}

	// If a toolprune config was supplied, derive args from it for standalone mode.
	if prFlags.config != "" {
		if _, err := config.Load(prFlags.config); err != nil {
			return fmt.Errorf("cannot use --config: %w", err)
		}
	}

	out := prFlags.output
	if prFlags.stdout || out == "-" {
		data, err := preset.GenerateJSON(opts)
		if err != nil {
			return err
		}
		fmt.Fprint(cmd.OutOrStdout(), string(data))
		return nil
	}

	if out == "" {
		def, err := preset.DefaultPath(client)
		if err != nil {
			return err
		}
		out = def
	}

	if prFlags.merge {
		if err := preset.WriteFile(out, opts); err != nil {
			return err
		}
	} else {
		data, err := preset.GenerateJSON(opts)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, data, 0644); err != nil {
			return fmt.Errorf("failed to write %q: %w", out, err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s preset for ToolPrune to %s\n", client, out)
	return nil
}

func runPresetValidate(cmd *cobra.Command, args []string) error {
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("failed to read %q: %w", args[0], err)
	}
	if err := preset.Validate(data); err != nil {
		return fmt.Errorf("invalid MCP client config %q: %w", args[0], err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "MCP client config %s is valid.\n", args[0])
	return nil
}
