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
