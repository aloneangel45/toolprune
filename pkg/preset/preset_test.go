package preset

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aloneangel45/toolprune/pkg/config"
)

func TestParseClient(t *testing.T) {
	for _, name := range []string{"cursor", "Cursor", " windsurf "} {
		if _, err := ParseClient(name); err != nil {
			t.Errorf("ParseClient(%q) unexpected error: %v", name, err)
		}
	}
	if _, err := ParseClient("vscode"); err == nil {
		t.Error("ParseClient(vscode) expected error, got nil")
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

func TestGenerateJSONValidAndParseable(t *testing.T) {
	data, err := GenerateJSON(DefaultOptions(ClientCursor))
	if err != nil {
		t.Fatalf("GenerateJSON failed: %v", err)
	}
	if err := Validate(data); err != nil {
		t.Errorf("generated JSON failed Validate: %v", err)
	}
	var round ConfigFile
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("generated JSON does not round-trip: %v", err)
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
		{"valid with env", `{"mcpServers":{"a":{"command":"x","env":{"K":"V"}}}}`, false},
		{"missing mcpServers", `{}`, true},
		{"empty mcpServers", `{"mcpServers":{}}`, true},
		{"mcpServers not object", `{"mcpServers":[]}`, true},
		{"server without command or url", `{"mcpServers":{"a":{}}}`, true},
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

func TestDefaultPath(t *testing.T) {
	p, err := DefaultPath(ClientCursor)
	if err != nil || p != filepath.Join(".cursor", "mcp.json") {
		t.Errorf("cursor default path = %q, %v", p, err)
	}
	p, err = DefaultPath(ClientWindsurf)
	if err != nil || !strings.Contains(p, filepath.Join(".codeium", "windsurf")) {
		t.Errorf("windsurf default path = %q, %v", p, err)
	}
}
