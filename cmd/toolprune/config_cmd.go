package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/doctor"
	"github.com/aloneangel45/toolprune/pkg/preset"
)

var (
	patchClaudePath   string
	patchClaudeDryRun bool
	patchClaudeMode   string
	patchClaudePreset string
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Configuration management and client setup helpers",
	Long:  `Manage toolprune.yaml configurations, validate syntax, and patch Claude Desktop / Cursor settings.`,
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize a new toolprune.yaml configuration file",
	RunE: func(cmd *cobra.Command, args []string) error {
		defer resetCommandFlags(cmd)
		targetFile := "toolprune.yaml"
		if _, err := os.Stat(targetFile); err == nil {
			return fmt.Errorf("%s already exists in current directory", targetFile)
		}
		content := config.GenerateDefaultConfig()
		if err := os.WriteFile(targetFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write %s: %w", targetFile, err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Created default configuration in %s\n", targetFile)
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate toolprune.yaml syntax and check upstream commands",
	RunE: func(cmd *cobra.Command, args []string) error {
		defer resetCommandFlags(cmd)
		configPath := globalConfig
		if configPath == "" {
			configPath = config.FindConfigFile()
		}
		if configPath == "" {
			return fmt.Errorf("no configuration file found to validate")
		}
		cfg, err := config.Load(configPath)
		if err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
		if err := config.Validate(cfg); err != nil {
			return fmt.Errorf("configuration semantic error: %w", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Configuration %s is valid. %d upstreams defined.\n", configPath, len(cfg.Upstreams))
		return nil
	},
}

var configPatchClaudeCmd = &cobra.Command{
	Use:   "patch-claude [flags]",
	Short: "Inspect and wrap existing Claude Desktop MCP server configurations",
	Long: `Reads Claude Desktop's claude_desktop_config.json, wraps stdio MCP servers with
'toolprune proxy', creates a .bak backup before modifying disk, or prints a preview with --dry-run.`,
	RunE: runConfigPatchClaude,
}

func init() {
	configPatchClaudeCmd.Flags().StringVar(&patchClaudePath, "path", "", "Path to claude_desktop_config.json (default: OS standard location)")
	configPatchClaudeCmd.Flags().BoolVar(&patchClaudeDryRun, "dry-run", false, "Preview patched JSON without modifying files on disk")
	configPatchClaudeCmd.Flags().StringVar(&patchClaudeMode, "mode", "aggressive", "Pruning mode passed to wrapped servers (lossless, compact, aggressive, dynamic)")
	configPatchClaudeCmd.Flags().StringVar(&patchClaudePreset, "preset", "", "Optional preset passed to wrapped servers (fast, balanced, strict, dynamic)")

	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configPatchClaudeCmd)
}

func defaultClaudeConfigPath() (string, error) {
	switch runtime.GOOS {
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "Claude", "claude_desktop_config.json"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

func isToolpruneBinary(cmdName string) bool {
	return doctor.IsToolpruneBinary(cmdName)
}

func isValidPatchMode(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "none", "lossless", "compact", "aggressive", "dynamic":
		return true
	default:
		return false
	}
}

func atomicWriteFile(targetPath string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(targetPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".toolprune-patch-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func runConfigPatchClaude(cmd *cobra.Command, args []string) error {
	defer resetCommandFlags(cmd)
	targetPath := ""
	if cmd.Flags().Changed("path") {
		targetPath = strings.TrimSpace(patchClaudePath)
	}
	if targetPath == "" {
		var err error
		targetPath, err = defaultClaudeConfigPath()
		if err != nil {
			return fmt.Errorf("failed to locate Claude Desktop config path: %w", err)
		}
	}

	dryRun := cmd.Flags().Changed("dry-run") && patchClaudeDryRun
	mode := "aggressive"
	if cmd.Flags().Changed("mode") {
		candidate := strings.ToLower(strings.TrimSpace(patchClaudeMode))
		if !isValidPatchMode(candidate) {
			return fmt.Errorf("invalid --mode %q: must be one of none, lossless, compact, aggressive, dynamic", patchClaudeMode)
		}
		mode = candidate
	}
	presetVal := ""
	if cmd.Flags().Changed("preset") && strings.TrimSpace(patchClaudePreset) != "" {
		if _, err := resolvePresetToMode(patchClaudePreset); err != nil {
			return err
		}
		presetVal = strings.ToLower(strings.TrimSpace(patchClaudePreset))
	}

	origBytes, err := os.ReadFile(targetPath)
	if err != nil {
		return fmt.Errorf("failed to read Claude config %q: %w", targetPath, err)
	}

	var root map[string]any
	if err := json.Unmarshal(preset.NormalizeJSONC(origBytes), &root); err != nil {
		return fmt.Errorf("invalid JSON in %q: %w", targetPath, err)
	}
	if root == nil {
		root = make(map[string]any)
	}

	serversRaw, ok := root["mcpServers"]
	if !ok || serversRaw == nil {
		return fmt.Errorf("no \"mcpServers\" object found in %q", targetPath)
	}
	serversMap, ok := serversRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("\"mcpServers\" in %q must be a JSON object", targetPath)
	}

	names := make([]string, 0, len(serversMap))
	for name := range serversMap {
		names = append(names, name)
	}
	sort.Strings(names)

	wrappedCount := 0
	skippedCount := 0

	for _, name := range names {
		entryMap, ok := serversMap[name].(map[string]any)
		if !ok {
			skippedCount++
			continue
		}
		origCmd, hasCmd := entryMap["command"].(string)
		if !hasCmd || strings.TrimSpace(origCmd) == "" || isToolpruneBinary(origCmd) {
			skippedCount++
			continue
		}

		var origArgs []string
		if rawArgs, ok := entryMap["args"].([]any); ok {
			for _, item := range rawArgs {
				if s, ok := item.(string); ok {
					origArgs = append(origArgs, s)
				} else {
					origArgs = append(origArgs, fmt.Sprintf("%v", item))
				}
			}
		}

		newArgs := []string{"proxy"}
		if presetVal != "" {
			newArgs = append(newArgs, "--preset="+presetVal)
		} else {
			newArgs = append(newArgs, "--mode="+mode)
		}
		newArgs = append(newArgs, "--", origCmd)
		newArgs = append(newArgs, origArgs...)

		entryMap["command"] = "toolprune"
		entryMap["args"] = newArgs
		serversMap[name] = entryMap
		wrappedCount++
	}

	patchedBytes, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal patched Claude config: %w", err)
	}
	patchedBytes = append(patchedBytes, '\n')

	if dryRun {
		fmt.Fprint(cmd.OutOrStdout(), string(patchedBytes))
		return nil
	}

	backupPath := targetPath + ".bak"
	if err := atomicWriteFile(backupPath, origBytes, 0600); err != nil {
		return fmt.Errorf("failed to create backup %q: %w", backupPath, err)
	}

	if err := atomicWriteFile(targetPath, patchedBytes, 0600); err != nil {
		return fmt.Errorf("failed to write patched config %q: %w", targetPath, err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Patched %s (%d wrapped, %d unchanged). Backup saved to %s\n",
		targetPath, wrappedCount, skippedCount, backupPath)
	return nil
}
