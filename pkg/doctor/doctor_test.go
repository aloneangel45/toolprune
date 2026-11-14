package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsToolpruneBinary(t *testing.T) {
	for _, valid := range []string{"toolprune", "toolprune.exe", "/usr/local/bin/toolprune", `C:\Users\dev\.local\bin\toolprune.exe`} {
		if !IsToolpruneBinary(valid) {
			t.Errorf("IsToolpruneBinary(%q) = false, want true", valid)
		}
	}
	for _, other := range []string{"npx", "uvx", "node", ""} {
		if IsToolpruneBinary(other) {
			t.Errorf("IsToolpruneBinary(%q) = true, want false", other)
		}
	}
}

func TestClassifyZedAndStandardEntries(t *testing.T) {
	// Zed extension entry without command or url should not be counted as unwrapped MCP subprocess
	counted, wrapped := classifyZedEntry("postgres-ext", map[string]any{"settings": map[string]any{"db": "local"}})
	if counted || wrapped {
		t.Errorf("expected extension-only Zed entry to be ignored, got counted=%v wrapped=%v", counted, wrapped)
	}

	// Docker wrapper running toolprune container should be recognized as wrapped
	dockerEntry := map[string]any{
		"command": "docker",
		"args":    []any{"run", "-i", "--rm", "ghcr.io/aloneangel45/toolprune:latest", "proxy"},
	}
	if !classifyStandardEntry("github_docker", dockerEntry) {
		t.Error("expected docker toolprune wrapper entry to be classified as wrapped")
	}

	// Malformed IDE JSON should yield status="warn" without panicking or failing Healthy
	tmp := t.TempDir()
	badCursor := filepath.Join(tmp, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(badCursor), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badCursor, []byte("{not-json"), 0644); err != nil {
		t.Fatal(err)
	}

	rep := Run(Options{
		Version:     "1.3.0",
		ProjectDir:  tmp,
		DirExplicit: true,
	})
	if !rep.Healthy {
		t.Errorf("malformed optional IDE config should warn, not fail Healthy: %+v", rep)
	}
	foundWarn := false
	for _, c := range rep.Clients {
		if c.Client == "cursor" && c.Status == "warn" && strings.Contains(c.Warning, "not valid JSON") {
			foundWarn = true
		}
	}
	if !foundWarn {
		t.Errorf("expected warn status on malformed cursor mcp.json, got %+v", rep.Clients)
	}
}
