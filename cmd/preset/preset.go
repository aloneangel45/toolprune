package presetcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

type validateFlags struct {
	client string
	input  string
}

// NewCommand builds a fresh 'preset' cobra command hierarchy with isolated flag state.
func NewCommand() *cobra.Command {
	flags := &presetFlags{}
	vFlags := &validateFlags{}

	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Generate MCP client config presets (Cursor, Windsurf, VS Code, Claude Code, Zed)",
		Long: `Generate out-of-the-box MCP client configuration presets that wire AI clients
to a ToolPrune proxy instance while preserving existing sibling keys.

Targets:
  cursor       .cursor/mcp.json (project-level)
  windsurf     ~/.codeium/windsurf/mcp_config.json (global)
  vscode       .vscode/mcp.json (project-level, "servers" schema)
  claude-code  .mcp.json (project-level, "mcpServers" schema)
  zed          ~/.config/zed/settings.json or %APPDATA%\Zed\settings.json ("context_servers")`,
	}

	genCmd := &cobra.Command{
		Use:   "generate --client <cursor|windsurf|vscode|claude-code|zed> [flags]",
		Short: "Generate a client MCP config preset for ToolPrune",
		RunE: func(c *cobra.Command, args []string) error {
			return runPresetGenerate(c, flags)
		},
	}

	valCmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Validate a client MCP config file against the expected schema",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runPresetValidate(c, args, vFlags)
		},
	}

	genCmd.Flags().StringVar(&flags.client, "client", "cursor", "Target client (cursor, windsurf, vscode, claude-code, zed)")
	genCmd.Flags().StringVarP(&flags.output, "output", "o", "", "Output file path (default: client's conventional path; use '-' for stdout)")
	genCmd.Flags().StringVar(&flags.name, "name", "toolprune", "Server name under the client's MCP servers map")
	genCmd.Flags().StringVar(&flags.command, "command", "toolprune", "Executable used to launch the ToolPrune proxy")
	genCmd.Flags().StringSliceVar(&flags.args, "args", []string{"proxy"}, "Arguments passed to the proxy command")
	genCmd.Flags().StringSliceVar(&flags.env, "env", nil, "Extra environment variables as KEY=VALUE (repeatable)")
	genCmd.Flags().StringVar(&flags.serverURL, "server-url", "", "Emit a remote server entry instead of a stdio command (e.g. http://127.0.0.1:8080/sse)")
	genCmd.Flags().StringVarP(&flags.config, "config", "c", "", "toolprune.yaml path to pass to the proxy as --config")
	genCmd.Flags().BoolVar(&flags.merge, "merge", false, "Merge into an existing config file at --output, preserving other servers and keys")
	genCmd.Flags().BoolVar(&flags.stdout, "stdout", false, "Print the generated config to stdout instead of writing a file")

	valCmd.Flags().StringVar(&vFlags.client, "client", "", "Optional target client schema to enforce (cursor, windsurf, vscode, claude-code, zed)")
	valCmd.Flags().StringVarP(&vFlags.input, "input", "i", "", "Path to MCP client config file to validate (alternative to positional <file>)")

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

	// Always preserve existing settings.json keys when writing to Zed, or whenever --merge is passed.
	if prFlags.merge || client == preset.ClientZed {
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

func runPresetValidate(cmd *cobra.Command, args []string, vFlags *validateFlags) error {
	target := strings.TrimSpace(vFlags.input)
	if len(args) > 0 {
		target = args[0]
	}
	if target == "" {
		return fmt.Errorf("missing config file path: provide <file> or --input <file>")
	}

	var err error
	if strings.TrimSpace(vFlags.client) != "" {
		c, parseErr := preset.ParseClient(vFlags.client)
		if parseErr != nil {
			return parseErr
		}
		err = preset.ValidateFile(target, c)
	} else {
		err = preset.ValidateFile(target)
	}
	if err != nil {
		return fmt.Errorf("invalid MCP client config %q: %w", target, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "MCP client config %s is valid.\n", target)
	return nil
}
