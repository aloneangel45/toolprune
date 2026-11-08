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
// conforms to the client MCP config schema enforced by preset.Validate.
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
			// The preset must register a toolprune server.
			cfg, err := preset.Generate(preset.DefaultOptions(preset.Client(e.Name())))
			if err != nil {
				t.Fatalf("client %q not supported: %v", e.Name(), err)
			}
			if _, ok := cfg.MCPServers["toolprune"]; !ok {
				t.Errorf("client %q preset missing toolprune entry", e.Name())
			}
		}
	}
	if checked == 0 {
		t.Error("no example JSON files found under examples/")
	}
}

// TestGeneratedPresetMatchesExample ensures the generator output for the default
// options validates and contains the toolprune stdio entry.
func TestGeneratedPresetMatchesExample(t *testing.T) {
	data, err := preset.GenerateJSON(preset.DefaultOptions(preset.ClientCursor))
	if err != nil {
		t.Fatal(err)
	}
	if err := preset.Validate(data); err != nil {
		t.Fatalf("generated preset invalid: %v", err)
	}
	if !strings.Contains(string(data), `"toolprune"`) {
		t.Error("generated preset missing toolprune server name")
	}
}
