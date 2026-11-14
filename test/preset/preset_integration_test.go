package preset_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/preset"
)

// repoRoot walks up from the test working directory to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// TestExamplePresetsSchemaCompatible verifies every sample config under examples/
// conforms to the client MCP config schema enforced by preset.Validate and ValidateFile.
func TestExamplePresetsSchemaCompatible(t *testing.T) {
	root := repoRoot(t)
	examples := filepath.Join(root, "examples")
	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatalf("examples dir missing: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		client, err := preset.ParseClient(e.Name())
		if err != nil {
			t.Fatalf("example directory %q does not match a supported client: %v", e.Name(), err)
		}
		dir := filepath.Join(examples, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			checked++
			path := filepath.Join(dir, f.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			if err := preset.Validate(data); err != nil {
				t.Errorf("example %s failed schema validation: %v", path, err)
			}
			if err := preset.ValidateFile(path, client); err != nil {
				t.Errorf("example %s failed ValidateFile(%s): %v", path, client, err)
			}
			// The preset must register a toolprune server.
			cfg, err := preset.Generate(preset.DefaultOptions(client))
			if err != nil {
				t.Fatalf("client %q not supported: %v", e.Name(), err)
			}
			if !cfg.HasServer("toolprune") {
				t.Errorf("client %q preset missing toolprune entry", e.Name())
			}
		}
	}
	if checked < 4 {
		t.Errorf("expected at least 4 example JSON files (cursor, windsurf, vscode, claude-code), found %d", checked)
	}
}

// TestGeneratedPresetMatchesAllClients ensures generator output for all supported clients
// validates cleanly in memory and on disk via WriteFile + ValidateFile.
func TestGeneratedPresetMatchesAllClients(t *testing.T) {
	for _, client := range preset.SupportedClients() {
		t.Run(string(client), func(t *testing.T) {
			data, err := preset.GenerateJSON(preset.DefaultOptions(client))
			if err != nil {
				t.Fatal(err)
			}
			if err := preset.Validate(data); err != nil {
				t.Fatalf("generated %s preset invalid: %v", client, err)
			}
			if !strings.Contains(string(data), `"toolprune"`) {
				t.Errorf("generated %s preset missing toolprune server name", client)
			}

			tmp := t.TempDir()
			outPath := filepath.Join(tmp, string(client), "config.json")
			if err := preset.WriteFile(outPath, preset.DefaultOptions(client)); err != nil {
				t.Fatalf("WriteFile(%s) failed: %v", client, err)
			}
			if err := preset.ValidateFile(outPath, client); err != nil {
				t.Fatalf("ValidateFile(%s) failed: %v", client, err)
			}
		})
	}
}
