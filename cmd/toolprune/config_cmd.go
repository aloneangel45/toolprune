package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/config"
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
	Use:   "patch-claude",
	Short: "Inspect and wrap existing Claude Desktop MCP server configurations",
	RunE: func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(cmd.OutOrStdout(), "Inspecting claude_desktop_config.json...")
		return nil
	},
}

func init() {
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configValidateCmd)
	configCmd.AddCommand(configPatchClaudeCmd)
}
