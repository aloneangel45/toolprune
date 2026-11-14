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
		Version:     "1.3.1",
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

func TestDoctorInspectsZedAndVSCodeJSONCWithoutFalseWarnings(t *testing.T) {
	tmp := t.TempDir()

	// 1. Real-world Zed settings.json with UTF-8 BOM, // comments, /* */ comments, trailing commas, and URLs
	zedPath := filepath.Join(tmp, "Zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zedPath), 0755); err != nil {
		t.Fatal(err)
	}
	zedJSONC := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`// Zed settings
// https://zed.dev/docs/configuring-zed
{
  /* UI configuration */
  "theme": "Ayu Dark",
  "context_servers": {
    "unwrapped_git": {
      "command": {
        "path": "uvx",
        "args": ["mcp-server-git",],
      },
    },
    "wrapped_tp": {
      "command": {
        "path": "C:\\Users\\dev\\.local\\bin\\toolprune.exe",
        "args": ["proxy", "--", "npx", "server",],
      },
    },
    "remote_sse": {
      "url": "https://mcp.example.com/sse/*live*/",
    },
  },
}
`)...)
	if err := os.WriteFile(zedPath, zedJSONC, 0644); err != nil {
		t.Fatal(err)
	}

	zedRes := inspectSingleClientConfig(clientTarget{
		client:    "zed",
		scope:     "user",
		path:      zedPath,
		schemaKey: "context_servers",
		wrapHint:  "toolprune preset generate --client zed --merge",
	})
	if zedRes.Warning != "" || zedRes.Status == "warn" {
		t.Fatalf("expected valid JSONC Zed settings.json with zero warning, got: %+v", zedRes)
	}
	if zedRes.TotalServers != 3 || zedRes.WrappedServers != 1 || zedRes.UnwrappedCount != 2 {
		t.Errorf("unexpected Zed server counts: %+v", zedRes)
	}

	// 2. Comment-only Zed settings.json should report status="ok"
	commentOnlyPath := filepath.Join(tmp, "ZedEmpty", "settings.json")
	if err := os.MkdirAll(filepath.Dir(commentOnlyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commentOnlyPath, []byte("// Default Zed settings\n"), 0644); err != nil {
		t.Fatal(err)
	}
	emptyRes := inspectSingleClientConfig(clientTarget{
		client:    "zed",
		scope:     "user",
		path:      commentOnlyPath,
		schemaKey: "context_servers",
	})
	if emptyRes.Status != "ok" || emptyRes.Warning != "" {
		t.Errorf("expected comment-only Zed settings.json to be ok, got: %+v", emptyRes)
	}

	// 3. VS Code .vscode/mcp.json with JSONC comments and trailing commas via Run()
	vscodePath := filepath.Join(tmp, ".vscode", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(vscodePath), 0755); err != nil {
		t.Fatal(err)
	}
	vscodeJSONC := `{
  // VS Code MCP config
  "servers": {
    "toolprune": {
      "type": "stdio",
      "command": "toolprune",
      "args": ["proxy",],
    },
  },
}`
	if err := os.WriteFile(vscodePath, []byte(vscodeJSONC), 0644); err != nil {
		t.Fatal(err)
	}
	rep := Run(Options{
		Version:     "1.3.1",
		ProjectDir:  tmp,
		DirExplicit: true,
	})
	foundVSCodeOK := false
	for _, c := range rep.Clients {
		if c.Client == "vscode" && c.Status == "ok" && c.WrappedServers == 1 {
			foundVSCodeOK = true
		}
	}
	if !foundVSCodeOK {
		t.Errorf("expected vscode JSONC config to report status=ok with 1 wrapped server, got %+v", rep.Clients)
	}
}
