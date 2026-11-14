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

func TestConfigPatchClaude_CrossPlatformAndModeValidationAndJSONC(t *testing.T) {
	dir := t.TempDir()
	claudeCfgPath := dir + "/claude_desktop_config.json"

	// Include UTF-8 BOM, JSONC comments, trailing commas, and both Windows & POSIX toolprune paths
	initial := "\xef\xbb\xbf" + `{
  // Claude Desktop config with comments
  "mcpServers": {
    "win_wrapped": {
      "command": "C:\\Users\\alice\\.local\\bin\\toolprune.exe",
      "args": ["proxy", "--", "npx", "server-a"],
    },
    "posix_wrapped": {
      "command": "/usr/local/bin/toolprune",
      "args": ["proxy", "--", "uvx", "server-b"],
    },
    "unwrapped_git": {
      "command": "uvx",
      "args": ["mcp-server-git",],
    },
  },
}`
	if err := os.WriteFile(claudeCfgPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Invalid --mode must fail before modifying disk
	badBuf := new(bytes.Buffer)
	rootCmd.SetOut(badBuf)
	rootCmd.SetErr(badBuf)
	rootCmd.SetArgs([]string{"config", "patch-claude", "--path", claudeCfgPath, "--mode", "bogus_mode"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error for invalid --mode bogus_mode, got nil")
	}

	// 2. Valid patch skips both Windows and POSIX toolprune paths and wraps only unwrapped_git
	okBuf := new(bytes.Buffer)
	rootCmd.SetOut(okBuf)
	rootCmd.SetErr(okBuf)
	rootCmd.SetArgs([]string{"config", "patch-claude", "--path", claudeCfgPath, "--mode", "compact"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("patch-claude failed on JSONC config: %v", err)
	}
	if !strings.Contains(okBuf.String(), "(1 wrapped, 2 unchanged)") {
		t.Errorf("expected 1 wrapped and 2 unchanged, got: %s", okBuf.String())
	}
}

func TestCommandFlagIsolationAcrossExecutions(t *testing.T) {
	// 1. Run version --json, then plain version without manual flag reset
	buf1 := new(bytes.Buffer)
	rootCmd.SetOut(buf1)
	rootCmd.SetErr(buf1)
	rootCmd.SetArgs([]string{"version", "--json"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("version --json failed: %v", err)
	}
	if !strings.Contains(buf1.String(), `"version":`) {
		t.Fatalf("expected JSON output, got %q", buf1.String())
	}

	buf2 := new(bytes.Buffer)
	rootCmd.SetOut(buf2)
	rootCmd.SetErr(buf2)
	rootCmd.SetArgs([]string{"version"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("plain version failed: %v", err)
	}
	if strings.Contains(buf2.String(), `"version":`) || !strings.Contains(buf2.String(), "ToolPrune v") {
		t.Errorf("--json flag leaked across version command executions; got:\n%s", buf2.String())
	}

	// 2. Verify TOOLPRUNE_CONFIG env fallback does not leak globalConfig into subsequent executions
	configPath := "../../toolprune.yaml"
	if _, err := os.Stat(configPath); err != nil {
		configPath = "toolprune.yaml"
	}
	t.Setenv("TOOLPRUNE_CONFIG", configPath)
	buf3 := new(bytes.Buffer)
	rootCmd.SetOut(buf3)
	rootCmd.SetErr(buf3)
	rootCmd.SetArgs([]string{"config", "validate"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("config validate with TOOLPRUNE_CONFIG failed: %v", err)
	}

	t.Setenv("TOOLPRUNE_CONFIG", "")
	if globalConfig != "" {
		t.Errorf("globalConfig leaked after command completion: got %q, want empty", globalConfig)
	}
}

func TestSanitizeWrapperUpstreamID(t *testing.T) {
	cases := map[string]string{
		`C:\Program Files\Tools\my__mcp.exe`: "my_mcp.exe",
		`/usr/local/bin/github__server`:      "github_server",
		`npx`:                                "npx",
		`   `:                                "upstream",
	}
	for in, want := range cases {
		if got := sanitizeWrapperUpstreamID(in); got != want {
			t.Errorf("sanitizeWrapperUpstreamID(%q) = %q, want %q", in, got, want)
		}
	}
}
