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

func TestBenchCSVVerboseAndBudgetCommand(t *testing.T) {
	fixturePath := "../../test/fixtures/schemas.json"
	if _, err := os.Stat(fixturePath); err != nil {
		fixturePath = "test/fixtures/schemas.json"
	}

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{
		"bench",
		"--file", fixturePath,
		"--format", "csv",
		"--verbose",
		"--max-schema-tokens", "450",
	})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("bench --format csv --verbose --max-schema-tokens failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "record_type,name,tier,tokens") {
		t.Errorf("expected CSV header row, got:\n%s", out)
	}
	if !strings.Contains(out, "Budget (<=450 tok)") {
		t.Errorf("expected Budget row in CSV output, got:\n%s", out)
	}
	if !strings.Contains(out, "parameter,") {
		t.Errorf("expected verbose parameter rows in CSV output, got:\n%s", out)
	}
}

func TestConfigPatchClaudeCommand(t *testing.T) {
	dir := t.TempDir()
	claudeCfgPath := dir + "/claude_desktop_config.json"

	initial := `{
  "globalShortcut": "Ctrl+Space",
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {"GITHUB_PERSONAL_ACCESS_TOKEN": "secret"}
    },
    "already_wrapped": {
      "command": "toolprune",
      "args": ["proxy", "--mode=aggressive", "--", "uvx", "mcp-server-sqlite"]
    }
  }
}`
	if err := os.WriteFile(claudeCfgPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Dry-run must not modify file or create .bak
	dryBuf := new(bytes.Buffer)
	rootCmd.SetOut(dryBuf)
	rootCmd.SetErr(dryBuf)
	rootCmd.SetArgs([]string{"config", "patch-claude", "--path", claudeCfgPath, "--dry-run", "--preset", "strict"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("patch-claude --dry-run failed: %v", err)
	}
	if !strings.Contains(dryBuf.String(), "--preset=strict") {
		t.Errorf("expected --preset=strict in dry-run output, got:\n%s", dryBuf.String())
	}
	if _, err := os.Stat(claudeCfgPath + ".bak"); err == nil {
		t.Errorf("did not expect .bak file on --dry-run")
	}

	// 2. Real patch creates .bak and wraps unwrapped server idempotently
	applyBuf := new(bytes.Buffer)
	rootCmd.SetOut(applyBuf)
	rootCmd.SetErr(applyBuf)
	rootCmd.SetArgs([]string{"config", "patch-claude", "--path", claudeCfgPath, "--mode", "dynamic"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("patch-claude failed: %v", err)
	}

	bakData, err := os.ReadFile(claudeCfgPath + ".bak")
	if err != nil {
		t.Fatalf("expected .bak backup file: %v", err)
	}
	if string(bakData) != initial {
		t.Errorf("backup content mismatch")
	}

	patchedData, err := os.ReadFile(claudeCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	patchedStr := string(patchedData)
	if !strings.Contains(patchedStr, `"globalShortcut": "Ctrl+Space"`) {
		t.Errorf("expected unknown top-level field preserved, got:\n%s", patchedStr)
	}
	if !strings.Contains(patchedStr, `"--mode=dynamic"`) {
		t.Errorf("expected --mode=dynamic in patched file, got:\n%s", patchedStr)
	}
}

func TestResolvePresetTierMapping(t *testing.T) {
	cases := map[string]string{
		"fast":     "lossless",
		"balanced": "compact",
		"strict":   "aggressive",
		"dynamic":  "dynamic",
	}
	for presetName, wantTier := range cases {
		got, err := resolvePresetToMode(presetName)
		if err != nil || got != wantTier {
			t.Errorf("resolvePresetToMode(%q) = (%q, %v), want (%q, nil)", presetName, got, err, wantTier)
		}
	}
	if _, err := resolvePresetToMode("invalid_preset"); err == nil {
		t.Errorf("expected error for invalid preset")
	}
}
