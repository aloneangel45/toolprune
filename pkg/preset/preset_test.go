package preset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/config"
)

func TestSupportedClientsAndParseClient(t *testing.T) {
	supported := SupportedClients()
	wantClients := []Client{ClientCursor, ClientWindsurf, ClientVSCode, ClientClaudeCode, ClientZed}
	if len(supported) != len(wantClients) {
		t.Fatalf("SupportedClients() len = %d, want %d", len(supported), len(wantClients))
	}
	for i, c := range wantClients {
		if supported[i] != c {
			t.Errorf("SupportedClients()[%d] = %q, want %q", i, supported[i], c)
		}
	}

	for _, name := range []string{"cursor", "Cursor", " windsurf ", "vscode", "VSCode", "claude-code", "CLAUDE-CODE", "zed", "Zed"} {
		if _, err := ParseClient(name); err != nil {
			t.Errorf("ParseClient(%q) unexpected error: %v", name, err)
		}
	}
	for _, invalid := range []string{"", "emacs", "unknown-ide"} {
		if _, err := ParseClient(invalid); err == nil {
			t.Errorf("ParseClient(%q) expected error, got nil", invalid)
		}
	}
}

func TestGenerateCursorStdio(t *testing.T) {
	cfg, err := Generate(DefaultOptions(ClientCursor))
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	spec, ok := cfg.MCPServers["toolprune"]
	if !ok {
		t.Fatal("missing toolprune server entry")
	}
	if spec.Command != "toolprune" {
		t.Errorf("command = %q, want toolprune", spec.Command)
	}
	if len(spec.Args) != 1 || spec.Args[0] != "proxy" {
		t.Errorf("args = %v, want [proxy]", spec.Args)
	}
	if spec.ServerURL != "" {
		t.Errorf("serverUrl = %q, want empty for stdio", spec.ServerURL)
	}
}

func TestGenerateWindsurfRemote(t *testing.T) {
	o := DefaultOptions(ClientWindsurf)
	o.ServerURL = "http://127.0.0.1:8080/sse"
	cfg, err := Generate(o)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	spec := cfg.MCPServers["toolprune"]
	if spec.ServerURL != "http://127.0.0.1:8080/sse" {
		t.Errorf("serverUrl = %q", spec.ServerURL)
	}
	if spec.Command != "" {
		t.Errorf("command = %q, want empty for remote entry", spec.Command)
	}
}

func TestGenerateVSCodeStdioAndRemote(t *testing.T) {
	// 1. Stdio
	o := DefaultOptions(ClientVSCode)
	o.Env = map[string]string{"GITHUB_TOKEN": "${input:gh_token}"}
	data, err := GenerateJSON(o)
	if err != nil {
		t.Fatalf("GenerateJSON(vscode) failed: %v", err)
	}
	if err := ValidateForClient(ClientVSCode, data); err != nil {
		t.Fatalf("ValidateForClient(vscode) failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, hasMCP := parsed["mcpServers"]; hasMCP {
		t.Errorf("vscode config must use top-level 'servers', not 'mcpServers': %s", string(data))
	}
	servers, ok := parsed["servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level 'servers' object, got %v", parsed)
	}
	tp, _ := servers["toolprune"].(map[string]any)
	if tp["type"] != "stdio" || tp["command"] != "toolprune" {
		t.Errorf("unexpected vscode stdio server entry: %v", tp)
	}

	// 2. Remote SSE
	oRemote := DefaultOptions(ClientVSCode)
	oRemote.ServerURL = "http://127.0.0.1:8080/sse"
	remoteData, err := GenerateJSON(oRemote)
	if err != nil {
		t.Fatalf("GenerateJSON(vscode remote) failed: %v", err)
	}
	if err := ValidateForClient(ClientVSCode, remoteData); err != nil {
		t.Fatalf("ValidateForClient(vscode remote) failed: %v", err)
	}
	var parsedRemote map[string]any
	_ = json.Unmarshal(remoteData, &parsedRemote)
	tpRemote := parsedRemote["servers"].(map[string]any)["toolprune"].(map[string]any)
	if tpRemote["type"] != "sse" || tpRemote["url"] != "http://127.0.0.1:8080/sse" {
		t.Errorf("unexpected vscode remote server entry: %v", tpRemote)
	}
}

func TestGenerateClaudeCodeStdioAndRemote(t *testing.T) {
	// 1. Stdio
	o := DefaultOptions(ClientClaudeCode)
	o.ConfigPath = "toolprune.yaml"
	data, err := GenerateJSON(o)
	if err != nil {
		t.Fatalf("GenerateJSON(claude-code) failed: %v", err)
	}
	if err := ValidateForClient(ClientClaudeCode, data); err != nil {
		t.Fatalf("ValidateForClient(claude-code) failed: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(data, &parsed)
	servers := parsed["mcpServers"].(map[string]any)
	tp := servers["toolprune"].(map[string]any)
	if tp["type"] != "stdio" || tp["command"] != "toolprune" {
		t.Errorf("unexpected claude-code stdio entry: %v", tp)
	}

	// 2. Remote SSE
	oRemote := DefaultOptions(ClientClaudeCode)
	oRemote.ServerURL = "http://127.0.0.1:8080/sse"
	remoteData, err := GenerateJSON(oRemote)
	if err != nil {
		t.Fatalf("GenerateJSON(claude-code remote) failed: %v", err)
	}
	if err := ValidateForClient(ClientClaudeCode, remoteData); err != nil {
		t.Fatalf("ValidateForClient(claude-code remote) failed: %v", err)
	}
	var parsedRemote map[string]any
	_ = json.Unmarshal(remoteData, &parsedRemote)
	tpRemote := parsedRemote["mcpServers"].(map[string]any)["toolprune"].(map[string]any)
	if tpRemote["type"] != "sse" || tpRemote["url"] != "http://127.0.0.1:8080/sse" {
		t.Errorf("unexpected claude-code remote entry: %v", tpRemote)
	}
}

func TestGenerateZedContextServers(t *testing.T) {
	o := DefaultOptions(ClientZed)
	o.ConfigPath = "toolprune.yaml"
	o.Env = map[string]string{"GITHUB_TOKEN": "tok"}
	data, err := GenerateJSON(o)
	if err != nil {
		t.Fatalf("GenerateJSON(zed) failed: %v", err)
	}
	if err := ValidateForClient(ClientZed, data); err != nil {
		t.Fatalf("ValidateForClient(zed) failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	ctxServers, ok := parsed["context_servers"].(map[string]any)
	if !ok {
		t.Fatalf("expected top-level context_servers, got %v", parsed)
	}
	tp := ctxServers["toolprune"].(map[string]any)
	cmdObj, ok := tp["command"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested command object in zed context_servers, got %v", tp)
	}
	if cmdObj["path"] != "toolprune" {
		t.Errorf("command.path = %v, want toolprune", cmdObj["path"])
	}
}

func TestGenerateConfigPathAppended(t *testing.T) {
	o := DefaultOptions(ClientCursor)
	o.ConfigPath = "toolprune.yaml"
	cfg, err := Generate(o)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	args := cfg.MCPServers["toolprune"].Args
	want := []string{"proxy", "--config", "toolprune.yaml"}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %v, want %v", args, want)
	}
}

func TestGenerateJSONValidAndParseableAllClients(t *testing.T) {
	for _, client := range SupportedClients() {
		t.Run(string(client), func(t *testing.T) {
			data, err := GenerateJSON(DefaultOptions(client))
			if err != nil {
				t.Fatalf("GenerateJSON(%s) failed: %v", client, err)
			}
			if err := Validate(data); err != nil {
				t.Errorf("generated %s JSON failed Validate: %v", client, err)
			}
			if err := ValidateForClient(client, data); err != nil {
				t.Errorf("generated %s JSON failed ValidateForClient: %v", client, err)
			}
			var round ConfigFile
			if err := json.Unmarshal(data, &round); err != nil {
				t.Fatalf("generated %s JSON does not round-trip: %v", client, err)
			}
			if !round.HasServer("toolprune") {
				t.Errorf("round-tripped %s ConfigFile missing toolprune server", client)
			}
		})
	}
}

func TestGenerateFromConfig(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Upstreams = []config.UpstreamConfig{{ID: "github", Transport: "stdio", Command: "npx"}}
	out, err := GenerateFromConfig(cfg, ClientWindsurf, "toolprune.yaml")
	if err != nil {
		t.Fatalf("GenerateFromConfig failed: %v", err)
	}
	spec := out.MCPServers["toolprune"]
	if spec.Command != "toolprune" {
		t.Errorf("command = %q", spec.Command)
	}
	joined := strings.Join(spec.Args, " ")
	if !strings.Contains(joined, "--config toolprune.yaml") {
		t.Errorf("args %v missing --config toolprune.yaml", spec.Args)
	}
	if _, err := GenerateFromConfig(nil, ClientCursor, ""); err == nil {
		t.Error("GenerateFromConfig(nil) expected error")
	}
}

func TestMergeIntoPreservesExistingServers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	existing := `{"mcpServers":{"other":{"command":"foo","args":["--bar"]}}}`
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	merged, err := MergeInto(path, DefaultOptions(ClientCursor))
	if err != nil {
		t.Fatalf("MergeInto failed: %v", err)
	}
	if _, ok := merged.MCPServers["other"]; !ok {
		t.Error("existing 'other' server was dropped")
	}
	if _, ok := merged.MCPServers["toolprune"]; !ok {
		t.Error("toolprune server missing after merge")
	}
}

func TestMergeVSCodePreservesInputsAndServerExtras(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".vscode", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "inputs": [
    {"type": "promptString", "id": "api-key", "password": true}
  ],
  "servers": {
    "github": {
      "type": "stdio",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "envFile": "${workspaceFolder}/.env"
    },
    "toolprune": {
      "type": "stdio",
      "command": "old-toolprune",
      "dev": {"watch": "src/**/*.go"}
    }
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, DefaultOptions(ClientVSCode)); err != nil {
		t.Fatalf("WriteFile(vscode) failed: %v", err)
	}
	if err := ValidateFile(path); err != nil {
		t.Fatalf("ValidateFile(vscode) failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	inputs, ok := parsed["inputs"].([]any)
	if !ok || len(inputs) != 1 {
		t.Errorf("expected VS Code inputs array preserved, got %v", parsed["inputs"])
	}
	servers := parsed["servers"].(map[string]any)
	gh := servers["github"].(map[string]any)
	if gh["envFile"] != "${workspaceFolder}/.env" {
		t.Errorf("expected github.envFile preserved, got %v", gh["envFile"])
	}
	tp := servers["toolprune"].(map[string]any)
	if tp["command"] != "toolprune" {
		t.Errorf("expected toolprune command updated, got %v", tp["command"])
	}
	if _, hasDev := tp["dev"]; !hasDev {
		t.Errorf("expected toolprune.dev extra field preserved, got %v", tp)
	}
}

func TestMergeZedSettingsPreservesAllEditorSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "Zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	existing := `{
  "theme": "One Dark",
  "buffer_font_size": 15,
  "vim_mode": true,
  "lsp": {
    "gopls": {
      "initialization_options": {"staticcheck": true}
    }
  },
  "context_servers": {
    "existing_mcp": {
      "settings": {"workspace": "main"},
      "command": {
        "path": "uvx",
        "args": ["mcp-server-git"]
      }
    }
  }
}`
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, DefaultOptions(ClientZed)); err != nil {
		t.Fatalf("WriteFile(zed) failed: %v", err)
	}
	if err := ValidateFile(path); err != nil {
		t.Fatalf("ValidateFile(zed) failed: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["theme"] != "One Dark" || parsed["vim_mode"] != true || parsed["buffer_font_size"] != float64(15) {
		t.Errorf("Zed editor settings were not preserved: %v", parsed)
	}
	if _, hasLSP := parsed["lsp"].(map[string]any); !hasLSP {
		t.Errorf("Zed lsp block was dropped: %v", parsed)
	}
	ctxServers := parsed["context_servers"].(map[string]any)
	existingMCP := ctxServers["existing_mcp"].(map[string]any)
	if _, hasSettings := existingMCP["settings"]; !hasSettings {
		t.Errorf("existing_mcp.settings was dropped: %v", existingMCP)
	}
	tp := ctxServers["toolprune"].(map[string]any)
	cmdObj := tp["command"].(map[string]any)
	if cmdObj["path"] != "toolprune" {
		t.Errorf("expected toolprune command.path, got %v", cmdObj)
	}
}

func TestMergeIntoMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "mcp.json")
	merged, err := MergeInto(path, DefaultOptions(ClientCursor))
	if err != nil {
		t.Fatalf("MergeInto failed: %v", err)
	}
	if len(merged.MCPServers) != 1 {
		t.Errorf("expected 1 server, got %d", len(merged.MCPServers))
	}
}

func TestWriteFileCreatesDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".cursor", "mcp.json")
	if err := WriteFile(path, DefaultOptions(ClientCursor)); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Errorf("written file failed Validate: %v", err)
	}
}

func TestValidateSchema(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid stdio", `{"mcpServers":{"a":{"command":"toolprune","args":["proxy"]}}}`, false},
		{"valid remote", `{"mcpServers":{"a":{"serverUrl":"http://127.0.0.1:8080/sse"}}}`, false},
		{"valid remote cursor url", `{"mcpServers":{"a":{"url":"http://127.0.0.1:8080/mcp"}}}`, false},
		{"valid with env", `{"mcpServers":{"a":{"command":"x","env":{"K":"V"}}}}`, false},
		{"valid vscode stdio", `{"inputs":[],"servers":{"a":{"type":"stdio","command":"toolprune","args":["proxy"]}}}`, false},
		{"valid vscode sse", `{"servers":{"a":{"type":"sse","url":"http://127.0.0.1:8080/sse"}}}`, false},
		{"valid claude-code stdio", `{"mcpServers":{"a":{"type":"stdio","command":"toolprune","args":["proxy"]}}}`, false},
		{"valid zed settings", `{"theme":"One Dark","context_servers":{"a":{"command":{"path":"toolprune","args":["proxy"]}}}}`, false},
		{"missing mcpServers", `{}`, true},
		{"empty mcpServers", `{"mcpServers":{}}`, true},
		{"empty vscode servers", `{"servers":{}}`, true},
		{"empty zed context_servers", `{"context_servers":{}}`, true},
		{"mcpServers not object", `{"mcpServers":[]}`, true},
		{"server without command or url", `{"mcpServers":{"a":{}}}`, true},
		{"vscode stdio without command", `{"servers":{"a":{"type":"stdio"}}}`, true},
		{"vscode sse without url", `{"servers":{"a":{"type":"sse"}}}`, true},
		{"vscode unsupported type", `{"servers":{"a":{"type":"grpc","command":"x"}}}`, true},
		{"zed command string instead of object", `{"context_servers":{"a":{"command":"toolprune"}}}`, true},
		{"zed command missing path", `{"context_servers":{"a":{"command":{"args":["proxy"]}}}}`, true},
		{"empty command", `{"mcpServers":{"a":{"command":""}}}`, true},
		{"bad args type", `{"mcpServers":{"a":{"command":"x","args":"nope"}}}`, true},
		{"bad env type", `{"mcpServers":{"a":{"command":"x","env":["K"]}}}`, true},
		{"not json", `nope`, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate([]byte(tc.input))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateFileClientMismatch(t *testing.T) {
	dir := t.TempDir()
	vscodeFile := filepath.Join(dir, ".vscode", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(vscodeFile), 0755); err != nil {
		t.Fatal(err)
	}
	// Put a Cursor-style "mcpServers" document inside .vscode/mcp.json -> ValidateFile should flag mismatch!
	if err := os.WriteFile(vscodeFile, []byte(`{"mcpServers":{"a":{"command":"toolprune"}}}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(vscodeFile); err == nil {
		t.Error("expected ValidateFile(.vscode/mcp.json) to reject 'mcpServers' in favor of 'servers'")
	}
}

func TestDefaultPath(t *testing.T) {
	p, err := DefaultPath(ClientCursor)
	if err != nil || p != filepath.Join(".cursor", "mcp.json") {
		t.Errorf("cursor default path = %q, %v", p, err)
	}
	p, err = DefaultPath(ClientVSCode)
	if err != nil || p != filepath.Join(".vscode", "mcp.json") {
		t.Errorf("vscode default path = %q, %v", p, err)
	}
	p, err = DefaultPath(ClientClaudeCode)
	if err != nil || p != ".mcp.json" {
		t.Errorf("claude-code default path = %q, %v", p, err)
	}
	p, err = DefaultPath(ClientWindsurf)
	if err != nil || !strings.Contains(p, filepath.Join(".codeium", "windsurf")) {
		t.Errorf("windsurf default path = %q, %v", p, err)
	}

	// Verify Zed platform-specific path logic deterministically
	winZed, err := defaultPathForEnv(ClientZed, "windows", `C:\Users\Test\AppData\Roaming`, `C:\Users\Test`, nil)
	if err != nil || winZed != filepath.Join(`C:\Users\Test\AppData\Roaming`, "Zed", "settings.json") {
		t.Errorf("windows zed APPDATA path = %q, %v", winZed, err)
	}
	winZedFallback, err := defaultPathForEnv(ClientZed, "windows", "", `C:\Users\Test`, nil)
	if err != nil || winZedFallback != filepath.Join(`C:\Users\Test`, ".config", "zed", "settings.json") {
		t.Errorf("windows zed fallback path = %q, %v", winZedFallback, err)
	}
	unixZed, err := defaultPathForEnv(ClientZed, "linux", "/ignored", "/home/alice", nil)
	if err != nil || unixZed != filepath.Join("/home/alice", ".config", "zed", "settings.json") {
		t.Errorf("linux zed path = %q, %v", unixZed, err)
	}
}
