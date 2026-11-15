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

func TestInstallSH_ZeroCarriageReturnBytes(t *testing.T) {
	scriptPath := filepath.Join("..", "..", "install.sh")
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	if bytes.Contains(raw, []byte{'\r'}) {
		t.Fatalf("install.sh contains CR (\\r) bytes; must be pure LF (\\n)")
	}
	script := string(raw)
	if strings.Count(script, `mkdir -p "${INSTALL_DIR}"`) < 2 {
		t.Errorf("install.sh must run mkdir -p \"${INSTALL_DIR}\" in both root and non-root branches")
	}
	if !strings.Contains(script, `.tmp.$$`) || !strings.Contains(script, `chmod 0755`) || !strings.Contains(script, `mv -f`) {
		t.Errorf("install.sh must perform atomic install via cp to .tmp.$$, chmod 0755, and mv -f")
	}
}

func TestWindowsPowerShellEmptyPSModulePath(t *testing.T) {
	scriptPath, err := filepath.Abs(filepath.Join("..", "..", "install.ps1"))
	if err != nil {
		t.Fatalf("abs install.ps1: %v", err)
	}
	raw, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	script := string(raw)

	forbiddenCmdlets := []string{
		"Get-FileHash",
		"Expand-Archive",
		"Join-Path",
		"Write-Host",
		"Write-Error",
		"Invoke-RestMethod",
		"Invoke-WebRequest",
		"New-Item",
		"Get-Content",
		"Add-Type",
		"Get-ChildItem",
		"Select-Object",
		"Test-Path",
		"Copy-Item",
		"Remove-Item",
		"Where-Object",
		"Out-Null",
	}
	for _, cmdlet := range forbiddenCmdlets {
		if strings.Contains(script, cmdlet) {
			t.Errorf("install.ps1 must not rely on %s (fails when PSModulePath is empty or polluted)", cmdlet)
		}
	}
	if strings.Contains(script, "\nexit ") || strings.Contains(script, " exit ") {
		t.Errorf("install.ps1 must use throw inside function wrapper rather than exit")
	}
	if !strings.Contains(script, "[System.Security.Cryptography.SHA256]::Create()") {
		t.Errorf("install.ps1 expected .NET SHA256 implementation")
	}
	if !strings.Contains(script, "[System.IO.Compression.ZipFile]::ExtractToDirectory") {
		t.Errorf("install.ps1 expected .NET ZipFile::ExtractToDirectory implementation")
	}
	if !strings.Contains(script, "-bor [Net.SecurityProtocolType]::Tls12") {
		t.Errorf("install.ps1 expected additive TLS 1.2 flag (-bor Tls12)")
	}
	if !strings.Contains(script, `LoadWithPartialName("System.IO.Compression.FileSystem")`) {
		t.Errorf("install.ps1 expected LoadWithPartialName fallback for System.IO.Compression.FileSystem")
	}

	if runtime.GOOS != "windows" {
		return
	}
	psExe, err := exec.LookPath("powershell.exe")
	if err != nil {
		t.Skip("powershell.exe not found in PATH")
	}

	arch := "amd64"
	if runtime.GOARCH == "arm64" {
		arch = "arm64"
	}
	archiveName := "toolprune-windows-" + arch + ".zip"

	assetDir := t.TempDir()
	zipPath := filepath.Join(assetDir, archiveName)

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	fw, err := zw.Create("toolprune-windows-" + arch + ".exe")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("MZ-mock-binary-payload-v1.3.2")
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zipPath, zipBuf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(zipBuf.Bytes())
	sumHex := hex.EncodeToString(sum[:])
	checksumContent := sumHex + "  " + archiveName + "\n"
	if err := os.WriteFile(filepath.Join(assetDir, "checksums.txt"), []byte(checksumContent), 0644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		psModulePath string
	}{
		{"empty_PSModulePath", ""},
		{"pwsh7_polluted_PSModulePath", `C:\Program Files\PowerShell\7\Modules;C:\nonexistent_modules_dir`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installDir := filepath.Join(t.TempDir(), "nested", "bin")
			if err := os.MkdirAll(installDir, 0755); err != nil {
				t.Fatal(err)
			}
			// Pre-populate a ReadOnly toolprune.exe to ensure ReadOnly attribute is cleared before overwrite.
			destExe := filepath.Join(installDir, "toolprune.exe")
			if err := os.WriteFile(destExe, []byte("old-readonly-binary"), 0444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.Chmod(destExe, 0666)
			})

			cmd := exec.Command(psExe, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
			env := make([]string, 0, len(os.Environ())+4)
			for _, kv := range os.Environ() {
				upper := strings.ToUpper(kv)
				if strings.HasPrefix(upper, "PSMODULEPATH=") ||
					strings.HasPrefix(upper, "TOOLPRUNE_ASSET_DIR=") ||
					strings.HasPrefix(upper, "TOOLPRUNE_INSTALL_DIR=") ||
					strings.HasPrefix(upper, "TOOLPRUNE_SKIP_PATH_UPDATE=") {
					continue
				}
				env = append(env, kv)
			}
			env = append(env,
				"PSModulePath="+tc.psModulePath,
				"TOOLPRUNE_ASSET_DIR="+assetDir,
				"TOOLPRUNE_INSTALL_DIR="+installDir,
				"TOOLPRUNE_SKIP_PATH_UPDATE=1",
			)
			cmd.Env = env

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("install.ps1 failed with %s: %v\noutput:\n%s", tc.name, err, string(out))
			}

			gotBytes, err := os.ReadFile(destExe)
			if err != nil {
				t.Fatalf("expected installed toolprune.exe: %v", err)
			}
			if !bytes.Equal(gotBytes, payload) {
				t.Fatalf("installed binary payload mismatch: got %q, want %q", string(gotBytes), string(payload))
			}
		})
	}
}

func TestInstallPS1_RestrictedPowerShellNoModuleCmdlets(t *testing.T) {
	TestWindowsPowerShellEmptyPSModulePath(t)
}
