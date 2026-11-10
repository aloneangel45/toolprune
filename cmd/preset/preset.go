package presetcmd

import (
	"fmt"
	"os"
	"path/filepath"

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

// NewCommand builds a fresh 'preset' cobra command hierarchy with isolated flag state.
func NewCommand() *cobra.Command {
	flags := &presetFlags{}

	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Generate MCP client config presets (Cursor, Windsurf)",
		Long: `Generate out-of-the-box MCP client configuration presets that wire AI clients
to a ToolPrune proxy instance.

Targets:
  cursor    .cursor/mcp.json (project-level)
  windsurf  ~/.codeium/windsurf/mcp_config.json (global)`,
	}

	genCmd := &cobra.Command{
		Use:   "generate --client <cursor|windsurf> [flags]",
		Short: "Generate a client MCP config preset for ToolPrune",
		RunE: func(c *cobra.Command, args []string) error {
			return runPresetGenerate(c, flags)
		},
	}

	valCmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a client MCP config file against the expected schema",
		Args:  cobra.ExactArgs(1),
		RunE:  runPresetValidate,
	}

	genCmd.Flags().StringVar(&flags.client, "client", "cursor", "Target client (cursor, windsurf)")
	genCmd.Flags().StringVarP(&flags.output, "output", "o", "", "Output file path (default: client's conventional path; use '-' for stdout)")
	genCmd.Flags().StringVar(&flags.name, "name", "toolprune", "Server name under mcpServers")
	genCmd.Flags().StringVar(&flags.command, "command", "toolprune", "Executable used to launch the ToolPrune proxy")
	genCmd.Flags().StringSliceVar(&flags.args, "args", []string{"proxy"}, "Arguments passed to the proxy command")
	genCmd.Flags().StringSliceVar(&flags.env, "env", nil, "Extra environment variables as KEY=VALUE (repeatable)")
	genCmd.Flags().StringVar(&flags.serverURL, "server-url", "", "Emit a remote {serverUrl} entry instead of a stdio command (e.g. http://127.0.0.1:8080/sse)")
	genCmd.Flags().StringVarP(&flags.config, "config", "c", "", "toolprune.yaml path to pass to the proxy as --config")
	genCmd.Flags().BoolVar(&flags.merge, "merge", false, "Merge into an existing config file at --output, preserving other servers")
	genCmd.Flags().BoolVar(&flags.stdout, "stdout", false, "Print the generated config to stdout instead of writing a file")

	cmd.AddCommand(genCmd)
	cmd.AddCommand(valCmd)
	return cmd
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

func runPresetGenerate(cmd *cobra.Command, prFlags *presetFlags) error {
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
		if dir := filepath.Dir(out); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %q: %w", dir, err)
			}
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
