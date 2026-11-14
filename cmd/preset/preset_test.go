package presetcmd_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	presetcmd "github.com/aloneangel45/toolprune/cmd/preset"
)

func TestPresetGenerateCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	nestedOut := filepath.Join(dir, "nested", ".cursor", "mcp.json")

	cmd := presetcmd.NewCommand()
	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"generate", "--client", "cursor", "--output", nestedOut})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("preset generate failed on missing parent dir: %v", err)
	}

	raw, err := os.ReadFile(nestedOut)
	if err != nil {
		t.Fatalf("expected generated file at %s: %v", nestedOut, err)
	}
	if !strings.Contains(string(raw), `"toolprune"`) {
		t.Errorf("expected toolprune entry in generated preset, got:\n%s", string(raw))
	}
}

func TestPresetMergePreservesUnknownSiblingFieldsAndValidateURL(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, "mcp.json")

	existing := `{
  "editorTheme": "midnight",
  "mcpServers": {
    "remote_cursor": {
      "url": "http://127.0.0.1:8080/mcp",
      "disabled": false,
      "autoApprove": ["search"]
    }
  }
}`
	if err := os.WriteFile(outPath, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	// 1. Validate existing config with "url"
	valCmd := presetcmd.NewCommand()
	valBuf := new(bytes.Buffer)
	valCmd.SetOut(valBuf)
	valCmd.SetErr(valBuf)
	valCmd.SetArgs([]string{"validate", outPath})
	if err := valCmd.Execute(); err != nil {
		t.Fatalf("preset validate should accept 'url' remote server field: %v", err)
	}

	// 2. Merge toolprune into existing config and verify sibling fields remain intact
	genCmd := presetcmd.NewCommand()
	genBuf := new(bytes.Buffer)
	genCmd.SetOut(genBuf)
	genCmd.SetErr(genBuf)
	genCmd.SetArgs([]string{"generate", "--client", "cursor", "--output", outPath, "--merge"})
	if err := genCmd.Execute(); err != nil {
		t.Fatalf("preset generate --merge failed: %v", err)
	}

	mergedRaw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(mergedRaw, &parsed); err != nil {
		t.Fatalf("invalid merged JSON: %v", err)
	}
	if parsed["editorTheme"] != "midnight" {
		t.Errorf("expected top-level unknown field editorTheme='midnight' preserved, got %v", parsed["editorTheme"])
	}
	servers, _ := parsed["mcpServers"].(map[string]any)
	remote, ok := servers["remote_cursor"].(map[string]any)
	if !ok {
		t.Fatalf("expected remote_cursor preserved in mcpServers: %v", servers)
	}
	if remote["url"] != "http://127.0.0.1:8080/mcp" {
		t.Errorf("expected url preserved, got %v", remote["url"])
	}
	if remote["disabled"] != false {
		t.Errorf("expected sibling field disabled=false preserved, got %v", remote["disabled"])
	}
	if _, ok := servers["toolprune"]; !ok {
		t.Errorf("expected toolprune server added after --merge")
	}
}

func TestPresetVSCodeMergeAndValidate(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, ".vscode", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "inputs": [
    {"type": "promptString", "id": "gh-token", "password": true}
  ],
  "servers": {
    "filesystem": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
    }
  }
}`
	if err := os.WriteFile(outPath, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}

	genCmd := presetcmd.NewCommand()
	genCmd.SetOut(new(bytes.Buffer))
	genCmd.SetErr(new(bytes.Buffer))
	genCmd.SetArgs([]string{"generate", "--client", "vscode", "--output", outPath, "--merge", "--env", "GITHUB_TOKEN=${input:gh-token}"})
	if err := genCmd.Execute(); err != nil {
		t.Fatalf("preset generate --client vscode --merge failed: %v", err)
	}

	valCmd := presetcmd.NewCommand()
	valCmd.SetOut(new(bytes.Buffer))
	valCmd.SetErr(new(bytes.Buffer))
	valCmd.SetArgs([]string{"validate", "--client", "vscode", outPath})
	if err := valCmd.Execute(); err != nil {
		t.Fatalf("preset validate --client vscode failed: %v", err)
	}

	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["inputs"].([]any); !ok {
		t.Errorf("expected VS Code inputs array preserved, got %v", parsed["inputs"])
	}
	servers := parsed["servers"].(map[string]any)
	if _, ok := servers["filesystem"]; !ok {
		t.Errorf("expected filesystem server preserved in VS Code servers")
	}
	tp := servers["toolprune"].(map[string]any)
	if tp["type"] != "stdio" || tp["command"] != "toolprune" {
		t.Errorf("unexpected toolprune server in VS Code servers: %v", tp)
	}
}

func TestPresetClaudeCodeAndZedGenerateValidate(t *testing.T) {
	dir := t.TempDir()

	// 1. Claude Code (.mcp.json)
	claudePath := filepath.Join(dir, ".mcp.json")
	ccCmd := presetcmd.NewCommand()
	ccCmd.SetOut(new(bytes.Buffer))
	ccCmd.SetErr(new(bytes.Buffer))
	ccCmd.SetArgs([]string{"generate", "--client", "claude-code", "--output", claudePath})
	if err := ccCmd.Execute(); err != nil {
		t.Fatalf("claude-code generate failed: %v", err)
	}

	valCC := presetcmd.NewCommand()
	valCC.SetOut(new(bytes.Buffer))
	valCC.SetErr(new(bytes.Buffer))
	valCC.SetArgs([]string{"validate", claudePath})
	if err := valCC.Execute(); err != nil {
		t.Fatalf("claude-code validate failed: %v", err)
	}

	// 2. Zed (settings.json) — must preserve existing editor keys even without --merge!
	zedPath := filepath.Join(dir, "Zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zedPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zedPath, []byte(`{"theme":"Ayu Dark","tab_size":2}`), 0644); err != nil {
		t.Fatal(err)
	}

	zedCmd := presetcmd.NewCommand()
	zedCmd.SetOut(new(bytes.Buffer))
	zedCmd.SetErr(new(bytes.Buffer))
	zedCmd.SetArgs([]string{"generate", "--client", "zed", "--output", zedPath})
	if err := zedCmd.Execute(); err != nil {
		t.Fatalf("zed generate failed: %v", err)
	}

	valZed := presetcmd.NewCommand()
	valZed.SetOut(new(bytes.Buffer))
	valZed.SetErr(new(bytes.Buffer))
	valZed.SetArgs([]string{"validate", "--client", "zed", "--input", zedPath})
	if err := valZed.Execute(); err != nil {
		t.Fatalf("zed validate failed: %v", err)
	}

	zedRaw, err := os.ReadFile(zedPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsedZed map[string]any
	if err := json.Unmarshal(zedRaw, &parsedZed); err != nil {
		t.Fatal(err)
	}
	if parsedZed["theme"] != "Ayu Dark" || parsedZed["tab_size"] != float64(2) {
		t.Errorf("expected Zed theme and tab_size preserved, got %v", parsedZed)
	}
	if _, ok := parsedZed["context_servers"].(map[string]any); !ok {
		t.Errorf("expected Zed context_servers added, got %v", parsedZed)
	}
}

func TestPresetEnvCommaValuesAndEmptyKeyRejection(t *testing.T) {
	cmd := presetcmd.NewCommand()
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{
		"generate",
		"--client", "cursor",
		"--stdout",
		"--env", "ALLOWED_ORIGINS=http://localhost:3000,http://127.0.0.1:8080",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("preset generate with comma-containing --env failed: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal stdout JSON failed: %v", err)
	}
	tp := parsed["mcpServers"].(map[string]any)["toolprune"].(map[string]any)
	envMap := tp["env"].(map[string]any)
	if envMap["ALLOWED_ORIGINS"] != "http://localhost:3000,http://127.0.0.1:8080" {
		t.Errorf("comma-containing --env value corrupted: got %v", envMap["ALLOWED_ORIGINS"])
	}

	badCmd := presetcmd.NewCommand()
	badCmd.SetOut(new(bytes.Buffer))
	badCmd.SetErr(new(bytes.Buffer))
	badCmd.SetArgs([]string{"generate", "--client", "cursor", "--stdout", "--env", "=missing_key"})
	if err := badCmd.Execute(); err == nil {
		t.Errorf("expected error for empty --env key '=missing_key'")
	}
}
