package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/doctor"
)

func isolateUserEnv(t *testing.T) (homeDir, appDataDir string) {
	t.Helper()
	homeDir = t.TempDir()
	appDataDir = filepath.Join(homeDir, "AppData", "Roaming")
	if err := os.MkdirAll(appDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("APPDATA", appDataDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(homeDir, ".config"))
	return homeDir, appDataDir
}

func TestDoctorCleanMachineExitsZero(t *testing.T) {
	isolateUserEnv(t)
	projDir := t.TempDir()

	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", projDir})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("expected doctor on clean machine to exit 0, got error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Overall Status: HEALTHY") {
		t.Errorf("expected HEALTHY status on clean machine, got:\n%s", out)
	}

	// JSON output on clean machine
	jsonBuf := new(bytes.Buffer)
	rootCmd.SetOut(jsonBuf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", projDir, "--json"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("doctor --json on clean machine failed: %v", err)
	}

	var rep doctor.Report
	if err := json.Unmarshal(jsonBuf.Bytes(), &rep); err != nil {
		t.Fatalf("invalid doctor --json output: %v\nraw:\n%s", err, jsonBuf.String())
	}
	if !rep.Healthy {
		t.Errorf("expected rep.Healthy=true, got false: %+v", rep)
	}
	if rep.Config.Found {
		t.Errorf("expected rep.Config.Found=false on clean dir, got %+v", rep.Config)
	}
}

func TestDoctorFullScanWithMockConfigsAndReadOnlyGuarantee(t *testing.T) {
	homeDir, appDataDir := isolateUserEnv(t)
	projDir := t.TempDir()

	// Create a fake toolprune binary on PATH so binary check reports inPath=true
	binDir := t.TempDir()
	tpBinName := "toolprune"
	if runtime.GOOS == "windows" {
		tpBinName = "toolprune.exe"
	}
	fakeTP := filepath.Join(binDir, tpBinName)
	if err := os.WriteFile(fakeTP, []byte("fake-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+origPath)

	// Valid toolprune.yaml using 'go' (which exists on PATH during go test), SSE, Streamable HTTP, and a disabled upstream
	yamlCfg := `version: "1"
server:
  transport: "stdio"
pruning:
  tier: "aggressive"
upstreams:
  - id: "go_tool"
    transport: "stdio"
    command: "go"
    args: ["version"]
  - id: "remote_sse"
    transport: "sse"
    url: "http://127.0.0.1:8080/sse"
  - id: "remote_http"
    transport: "streamable-http"
    url: "https://example.com/mcp"
  - id: "disabled_srv"
    transport: "stdio"
    command: "missing_binary_that_is_disabled"
    disabled: true
`
	cfgFile := filepath.Join(projDir, "toolprune.yaml")
	if err := os.WriteFile(cfgFile, []byte(yamlCfg), 0644); err != nil {
		t.Fatal(err)
	}

	// Mock project-level IDE configs
	cursorFile := filepath.Join(projDir, ".cursor", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(cursorFile), 0755)
	cursorJSON := `{"mcpServers":{"toolprune":{"command":"toolprune","args":["proxy"]},"github_raw":{"command":"npx","args":["-y","@mcp/github"]}}}`
	_ = os.WriteFile(cursorFile, []byte(cursorJSON), 0644)

	vscodeFile := filepath.Join(projDir, ".vscode", "mcp.json")
	_ = os.MkdirAll(filepath.Dir(vscodeFile), 0755)
	vscodeJSON := `{"inputs":[],"servers":{"postgres":{"type":"stdio","command":"npx","args":["@mcp/postgres"]}}}`
	_ = os.WriteFile(vscodeFile, []byte(vscodeJSON), 0644)

	claudeCodeFile := filepath.Join(projDir, ".mcp.json")
	claudeCodeJSON := `{"mcpServers":{"toolprune":{"type":"stdio","command":"toolprune","args":["proxy"]}}}`
	_ = os.WriteFile(claudeCodeFile, []byte(claudeCodeJSON), 0644)

	// Mock user-level IDE configs (Claude Desktop + Windsurf + Zed)
	claudeDesktopFile, err := doctor.DefaultClaudeDesktopPath()
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Dir(claudeDesktopFile), 0755)
	claudeDesktopJSON := `{"mcpServers":{"sqlite":{"command":"uvx","args":["mcp-server-sqlite"]}}}`
	_ = os.WriteFile(claudeDesktopFile, []byte(claudeDesktopJSON), 0644)

	windsurfFile := filepath.Join(homeDir, ".codeium", "windsurf", "mcp_config.json")
	_ = os.MkdirAll(filepath.Dir(windsurfFile), 0755)
	windsurfJSON := `{"mcpServers":{"toolprune":{"serverUrl":"http://127.0.0.1:8080/sse"}}}`
	_ = os.WriteFile(windsurfFile, []byte(windsurfJSON), 0644)

	zedFile := filepath.Join(homeDir, ".config", "zed", "settings.json")
	if runtime.GOOS == "windows" {
		zedFile = filepath.Join(appDataDir, "Zed", "settings.json")
	}
	_ = os.MkdirAll(filepath.Dir(zedFile), 0755)
	zedJSON := `{"theme":"One Dark","context_servers":{"git_mcp":{"command":{"path":"uvx","args":["mcp-server-git"]}}}}`
	_ = os.WriteFile(zedFile, []byte(zedJSON), 0644)

	// 1. Human-readable output check
	humanBuf := new(bytes.Buffer)
	rootCmd.SetOut(humanBuf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", projDir})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("doctor human run failed: %v\noutput:\n%s", err, humanBuf.String())
	}
	humanOut := humanBuf.String()
	for _, wantSub := range []string{
		"toolprune found on PATH",
		"go_tool",
		"toolprune preset generate --client cursor",
		"toolprune preset generate --client vscode",
		"toolprune config patch-claude",
		"toolprune preset generate --client zed",
		"Overall Status: HEALTHY",
	} {
		if !strings.Contains(humanOut, wantSub) {
			t.Errorf("expected %q in human doctor output, got:\n%s", wantSub, humanOut)
		}
	}

	// 2. Structured JSON output check
	jsonBuf := new(bytes.Buffer)
	rootCmd.SetOut(jsonBuf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", projDir, "--json"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("doctor --json failed: %v", err)
	}

	var rep doctor.Report
	if err := json.Unmarshal(jsonBuf.Bytes(), &rep); err != nil {
		t.Fatalf("invalid JSON from doctor: %v", err)
	}
	if !rep.Healthy || !rep.Binary.InPath || !rep.Config.Valid {
		t.Fatalf("expected healthy report with binary in PATH and valid config: %+v", rep)
	}
	if len(rep.Config.Upstreams) != 4 {
		t.Errorf("expected 4 upstream checks, got %d", len(rep.Config.Upstreams))
	}

	byClient := map[string]doctor.ClientConfigCheck{}
	for _, c := range rep.Clients {
		byClient[c.Client] = c
	}
	if c := byClient["cursor"]; !c.Found || c.TotalServers != 2 || c.WrappedServers != 1 || c.UnwrappedCount != 1 {
		t.Errorf("unexpected cursor check: %+v", c)
	}
	if c := byClient["vscode"]; !c.Found || c.TotalServers != 1 || c.UnwrappedCount != 1 || !strings.Contains(c.Hint, "--client vscode") {
		t.Errorf("unexpected vscode check: %+v", c)
	}
	if c := byClient["claude-code"]; !c.Found || c.TotalServers != 1 || c.WrappedServers != 1 || c.UnwrappedCount != 0 {
		t.Errorf("unexpected claude-code check: %+v", c)
	}
	if c := byClient["claude-desktop"]; !c.Found || c.UnwrappedCount != 1 || c.Hint != "toolprune config patch-claude" {
		t.Errorf("unexpected claude-desktop check: %+v", c)
	}
	if c := byClient["windsurf"]; !c.Found || c.WrappedServers != 1 || c.UnwrappedCount != 0 {
		t.Errorf("unexpected windsurf check: %+v", c)
	}
	if c := byClient["zed"]; !c.Found || c.UnwrappedCount != 1 || !strings.Contains(c.Hint, "--client zed") {
		t.Errorf("unexpected zed check: %+v", c)
	}

	// 3. Verify strict read-only guarantee: none of the IDE files were modified
	verifyUnchanged := map[string]string{
		cursorFile:        cursorJSON,
		vscodeFile:        vscodeJSON,
		claudeCodeFile:    claudeCodeJSON,
		claudeDesktopFile: claudeDesktopJSON,
		windsurfFile:      windsurfJSON,
		zedFile:           zedJSON,
	}
	for p, want := range verifyUnchanged {
		gotBytes, rErr := os.ReadFile(p)
		if rErr != nil {
			t.Fatalf("failed reading %s after doctor: %v", p, rErr)
		}
		if string(gotBytes) != want {
			t.Errorf("doctor mutated %s!\nwant: %s\ngot:  %s", p, want, string(gotBytes))
		}
	}
}

func TestDoctorFailsOnMissingUpstreamExecutableOrInvalidConfig(t *testing.T) {
	isolateUserEnv(t)

	// Case A: Missing stdio upstream executable on PATH
	dirMissingBin := t.TempDir()
	badUpstreamYAML := `version: "1"
upstreams:
  - id: "ghost"
    transport: "stdio"
    command: "definitely_nonexistent_mcp_executable_987654"
`
	if err := os.WriteFile(filepath.Join(dirMissingBin, "toolprune.yaml"), []byte(badUpstreamYAML), 0644); err != nil {
		t.Fatal(err)
	}

	jsonBuf := new(bytes.Buffer)
	rootCmd.SetOut(jsonBuf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", dirMissingBin, "--json"})
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected non-zero error when stdio upstream executable is missing from PATH")
	}

	var rep doctor.Report
	if uErr := json.Unmarshal(jsonBuf.Bytes(), &rep); uErr != nil {
		t.Fatalf("expected valid JSON output even on unhealthy exit: %v\nraw:\n%s", uErr, jsonBuf.String())
	}
	if rep.Healthy {
		t.Error("expected rep.Healthy=false for missing upstream executable")
	}
	if len(rep.Errors) == 0 || !strings.Contains(rep.Errors[0], "definitely_nonexistent_mcp_executable_987654") {
		t.Errorf("expected missing executable error in report, got: %+v", rep.Errors)
	}

	// Case B: Malformed toolprune.yaml via --config
	dirBadYAML := t.TempDir()
	badCfgPath := filepath.Join(dirBadYAML, "broken.yaml")
	if err := os.WriteFile(badCfgPath, []byte("version: \"1\"\npruning:\n  tier: \"invalid_tier_xyz\"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	humanBuf := new(bytes.Buffer)
	rootCmd.SetOut(humanBuf)
	rootCmd.SetErr(new(bytes.Buffer))
	rootCmd.SetArgs([]string{"doctor", "--dir", dirBadYAML, "--config", badCfgPath})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected non-zero error when toolprune.yaml has invalid pruning tier")
	}
	if !strings.Contains(humanBuf.String(), "UNHEALTHY") {
		t.Errorf("expected UNHEALTHY in output, got:\n%s", humanBuf.String())
	}
}
