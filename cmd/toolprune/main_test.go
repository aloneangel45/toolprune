package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	_ = versionCmd.Flags().Set("short", "false")
	_ = versionCmd.Flags().Set("json", "false")

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--short"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("version command failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, Version) {
		t.Errorf("expected version %q in output, got %q", Version, output)
	}
}

func TestVersionJSONCommand(t *testing.T) {
	_ = versionCmd.Flags().Set("short", "false")
	_ = versionCmd.Flags().Set("json", "false")

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--json"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("version --json command failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, `"version":`) || !strings.Contains(output, `"goVersion":`) {
		t.Errorf("expected JSON version output, got %q", output)
	}
}

func TestRoutesCommand(t *testing.T) {
	configPath := "../../toolprune.yaml"
	if _, err := os.Stat(configPath); err != nil {
		configPath = "toolprune.yaml"
	}

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"routes", "--config", configPath, "--profile", "dev", "--filter", "git"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("routes command with flags failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Active Profile: dev") {
		t.Errorf("expected 'Active Profile: dev' in output, got %q", output)
	}
	if !strings.Contains(output, "Active Filter:  git") {
		t.Errorf("expected 'Active Filter:  git' in output, got %q", output)
	}

	// JSON format test
	bufJSON := new(bytes.Buffer)
	rootCmd.SetOut(bufJSON)
	rootCmd.SetErr(bufJSON)
	rootCmd.SetArgs([]string{"routes", "--config", configPath, "--json"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("routes --json failed: %v", err)
	}
}

func TestBenchCommand(t *testing.T) {
	fixturePath := "../../test/fixtures/schemas.json"
	if _, err := os.Stat(fixturePath); err != nil {
		fixturePath = "test/fixtures/schemas.json"
	}

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"bench", "--file", fixturePath, "--format", "json"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("bench command failed: %v", err)
	}
}

func TestConfigValidateCommand(t *testing.T) {
	configPath := "../../toolprune.yaml"
	if _, err := os.Stat(configPath); err != nil {
		configPath = "toolprune.yaml"
	}

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"config", "validate", "--config", configPath})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("config validate failed: %v", err)
	}
}

func TestSetupLogger(t *testing.T) {
	levels := []string{"debug", "info", "warn", "warning", "error", "unknown"}
	for _, lvl := range levels {
		logger := setupLogger(lvl)
		if logger == nil {
			t.Errorf("setupLogger(%q) returned nil", lvl)
		}
	}
}

func TestBindEnvFallback(t *testing.T) {
	t.Setenv("TOOLPRUNE_TEST_ENV", "custom_val")
	var val string
	bindEnvFallback("mode", "TOOLPRUNE_TEST_ENV", func(v string) {
		val = v
	})
	if val != "custom_val" {
		t.Errorf("expected 'custom_val', got %q", val)
	}

	var unsetVal string
	bindEnvFallback("mode", "TOOLPRUNE_NON_EXISTENT_VAR", func(v string) {
		unsetVal = v
	})
	if unsetVal != "" {
		t.Errorf("expected empty string, got %q", unsetVal)
	}
}
