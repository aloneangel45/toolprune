package preset

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeJSONC_ZedSettingsCommentsTrailingCommasAndStrings(t *testing.T) {
	// Include UTF-8 BOM, // line comments, /* ... */ block comments, trailing commas in
	// objects and arrays, and tricky string literals containing //, /*, */, escaped quotes, and ", }".
	rawZed := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`// Zed user settings
// Docs: https://zed.dev/docs/configuring-zed
{
  /* Multi-line block comment
     with // nested slashes and commas,
  */
  "theme": "One Dark", // trailing line comment
  "proxy_note": "keep // inline and /* block */ and trailing, } and \"escaped // quote, ]\"",
  "lsp": {
    "gopls": {
      "binary": {
        "arguments": ["-remote=auto",],
      },
    },
  },
  "context_servers": {
    "remote_docs": {
      "url": "https://mcp.example.com/sse/*v1*/?a=1,2", // URL with // and /* */
    },
    "git_mcp": {
      "command": {
        "path": "uvx",
        "args": ["mcp-server-git", "--repository", ".",],
        "env": {
          "GIT_CONFIG_NOSYSTEM": "1",
        },
      },
    },
  },
}
`)...)

	if err := ValidateForClient(ClientZed, rawZed); err != nil {
		t.Fatalf("ValidateForClient(ClientZed) rejected valid JSONC settings.json: %v", err)
	}
	if err := Validate(rawZed); err != nil {
		t.Fatalf("Validate() rejected valid JSONC settings.json: %v", err)
	}

	dir := t.TempDir()
	zedPath := filepath.Join(dir, "Zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zedPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zedPath, rawZed, 0644); err != nil {
		t.Fatal(err)
	}

	if err := ValidateFile(zedPath); err != nil {
		t.Fatalf("ValidateFile(zedPath) failed on JSONC settings.json: %v", err)
	}

	if err := WriteFile(zedPath, DefaultOptions(ClientZed)); err != nil {
		t.Fatalf("WriteFile(zedPath) failed merging into JSONC settings.json: %v", err)
	}

	mergedBytes, err := os.ReadFile(zedPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(mergedBytes, &parsed); err != nil {
		t.Fatalf("merged Zed settings.json is not valid strict JSON: %v", err)
	}

	wantNote := `keep // inline and /* block */ and trailing, } and "escaped // quote, ]"`
	if parsed["proxy_note"] != wantNote {
		t.Errorf("string literal corrupted by JSONC normalizer:\ngot:  %q\nwant: %q", parsed["proxy_note"], wantNote)
	}

	ctxServers, ok := parsed["context_servers"].(map[string]any)
	if !ok {
		t.Fatalf("missing context_servers in merged output: %v", parsed)
	}
	remoteDocs, _ := ctxServers["remote_docs"].(map[string]any)
	if remoteDocs["url"] != "https://mcp.example.com/sse/*v1*/?a=1,2" {
		t.Errorf("URL string corrupted: got %v", remoteDocs["url"])
	}
	if _, hasToolprune := ctxServers["toolprune"]; !hasToolprune {
		t.Errorf("expected toolprune entry merged into context_servers: %v", ctxServers)
	}
}

func TestMergeInto_CommentOnlyZedSettings(t *testing.T) {
	dir := t.TempDir()
	zedPath := filepath.Join(dir, "zed", "settings.json")
	if err := os.MkdirAll(filepath.Dir(zedPath), 0755); err != nil {
		t.Fatal(err)
	}
	commentOnly := "// Zed default settings file\n/* Nothing configured yet */\n"
	if err := os.WriteFile(zedPath, []byte(commentOnly), 0644); err != nil {
		t.Fatal(err)
	}

	merged, err := MergeInto(zedPath, DefaultOptions(ClientZed))
	if err != nil {
		t.Fatalf("MergeInto failed on comment-only Zed settings.json: %v", err)
	}
	if !merged.HasServer("toolprune") {
		t.Fatalf("expected toolprune server in merged config")
	}
}

func TestNormalizeJSONC_RejectsMalformedSyntax(t *testing.T) {
	malformedCases := []struct {
		name  string
		input string
	}{
		{"unterminated block comment", `{"mcpServers":{"a":{"command":"toolprune"}}} /* unclosed`},
		{"double trailing comma in array", `{"mcpServers":{"a":{"command":"toolprune","args":["proxy",,]}}}`},
		{"double trailing comma in object", `{"mcpServers":{"a":{"command":"toolprune",,}}}`},
	}

	for _, tc := range malformedCases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate([]byte(tc.input)); err == nil {
				t.Errorf("expected Validate() to reject %s, got nil", tc.name)
			}
		})
	}
}

func TestInferClientFromPath_CrossPlatformWindowsAndPOSIX(t *testing.T) {
	cases := []struct {
		path   string
		want   Client
		wantOK bool
	}{
		{`C:\Users\alice\AppData\Roaming\Zed\settings.json`, ClientZed, true},
		{`/home/alice/.config/zed/settings.json`, ClientZed, true},
		{`D:\workspace\project\.vscode\mcp.json`, ClientVSCode, true},
		{`/workspace/project/.vscode/mcp.json`, ClientVSCode, true},
		{`D:\workspace\project\.cursor\mcp.json`, ClientCursor, true},
		{`/workspace/project/.cursor/mcp.json`, ClientCursor, true},
		{`C:\workspace\project\.mcp.json`, ClientClaudeCode, true},
		{`/home/alice/.codeium/windsurf/mcp_config.json`, ClientWindsurf, true},
		{`C:\Users\alice\.codeium\windsurf\mcp_config.json`, ClientWindsurf, true},
		{`C:\Users\alice\settings.json`, "", false},
	}

	for _, tc := range cases {
		got, ok := inferClientFromPath(tc.path)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("inferClientFromPath(%q) = (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestInstallPS1_RestrictedPowerShellNoModuleCmdlets(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "install.ps1")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(raw)

	if strings.Contains(script, "Get-FileHash") {
		t.Errorf("install.ps1 must not rely on Get-FileHash (fails in restricted PSModulePath)")
	}
	if strings.Contains(script, "Expand-Archive") {
		t.Errorf("install.ps1 must not rely on Expand-Archive (fails in restricted PSModulePath)")
	}
	if !strings.Contains(script, "[System.Security.Cryptography.SHA256]::Create()") {
		t.Errorf("install.ps1 expected .NET SHA256 implementation")
	}
	if !strings.Contains(script, "[System.IO.Compression.ZipFile]::ExtractToDirectory") {
		t.Errorf("install.ps1 expected .NET ZipFile::ExtractToDirectory implementation")
	}

	if runtime.GOOS != "windows" {
		return
	}
	psExe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not found in PATH")
	}

	// Build a real zip archive containing a dummy toolprune.exe and verify the exact .NET
	// SHA-256 + ZipFile extraction commands work with PSModulePath cleared!
	tmp := t.TempDir()
	zipPath := filepath.Join(tmp, "toolprune-windows-amd64.zip")
	extractDir := filepath.Join(tmp, "extracted")

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	fw, err := zw.Create("toolprune.exe")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("MZ-mock-binary-payload-v1.3.1")
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zipBuf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	wantSum := sha256.Sum256(zipBuf.Bytes())
	wantHex := hex.EncodeToString(wantSum[:])

	psCmd := `
$ErrorActionPreference = "Stop"
$ZipPath = $env:TEST_ZIP_PATH
$ExtractDir = $env:TEST_EXTRACT_DIR
$Sha256 = [System.Security.Cryptography.SHA256]::Create()
try {
    $FileStream = [System.IO.File]::OpenRead($ZipPath)
    try {
        $HashBytes = $Sha256.ComputeHash($FileStream)
    } finally {
        $FileStream.Dispose()
    }
} finally {
    $Sha256.Dispose()
}
$ActualHash = [System.BitConverter]::ToString($HashBytes).Replace("-", "").ToLowerInvariant()
Add-Type -AssemblyName System.IO.Compression.FileSystem
[System.IO.Directory]::CreateDirectory($ExtractDir) | Out-Null
[System.IO.Compression.ZipFile]::ExtractToDirectory($ZipPath, $ExtractDir)
Write-Output $ActualHash
`
	cmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-Command", psCmd)
	// Strip PSModulePath to simulate restricted/non-autoload PowerShell session
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(kv), "PSMODULEPATH=") {
			env = append(env, kv)
		}
	}
	env = append(env, "PSModulePath=", "TEST_ZIP_PATH="+zipPath, "TEST_EXTRACT_DIR="+extractDir)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restricted PowerShell .NET hash+extract failed: %v\noutput: %s", err, string(out))
	}
	gotHex := strings.TrimSpace(string(out))
	if gotHex != wantHex {
		t.Errorf("SHA-256 mismatch: got %q, want %q", gotHex, wantHex)
	}
	extractedBytes, err := os.ReadFile(filepath.Join(extractDir, "toolprune.exe"))
	if err != nil {
		t.Fatalf("expected extracted toolprune.exe: %v", err)
	}
	if !bytes.Equal(extractedBytes, payload) {
		t.Errorf("extracted binary payload mismatch")
	}
}
