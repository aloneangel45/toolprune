//go:build windows

package e2e_test

import (
	"encoding/json"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
	"golang.org/x/sys/windows"
)

// isProcessAlive checks whether a process is still running on Windows.
func isProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	const stillActive = 259
	return exitCode == stillActive
}

// -----------------------------------------------------------------------------
// Windows Job Object Orphan Prevention & Clean Propagation
// -----------------------------------------------------------------------------

func TestAdversarial_WindowsJobObject_OrphanPrevention(t *testing.T) {
	p, cleanup := spawnProxy(t, "proxy", "--", testserverBin)
	defer cleanup()

	performStandardHandshake(t, p)

	resp := p.Call(t, mcp.MethodToolsCall, mcp.CallToolRequestParams{
		Name:      "system_info",
		Arguments: json.RawMessage(`{}`),
	})
	if resp.Error != nil {
		t.Fatalf("system_info failed: %v", resp.Error)
	}

	var callRes mcp.CallToolResult
	_ = json.Unmarshal(resp.Result, &callRes)
	if len(callRes.Content) == 0 {
		t.Fatalf("empty system_info content")
	}

	var info map[string]any
	_ = json.Unmarshal([]byte(callRes.Content[0].Text), &info)
	childPIDFloat, ok := info["pid"].(float64)
	if !ok {
		t.Fatalf("missing child PID in system_info: %s", callRes.Content[0].Text)
	}
	childPID := int(childPIDFloat)
	proxyPID := p.cmd.Process.Pid

	t.Logf("Proxy PID: %d, Upstream Child PID: %d", proxyPID, childPID)

	if !isProcessAlive(childPID) {
		t.Fatalf("child process %d is not running prior to kill test", childPID)
	}

	killCmd := exec.Command("taskkill", "/F", "/PID", strconv.Itoa(proxyPID))
	if out, err := killCmd.CombinedOutput(); err != nil {
		t.Fatalf("taskkill proxy failed: %v (output: %s)", err, string(out))
	}

	var childDied bool
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessAlive(childPID) {
			childDied = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !childDied {
		_ = exec.Command("taskkill", "/F", "/PID", strconv.Itoa(childPID)).Run()
		t.Fatalf("FAILURE: Windows Job Object failed to terminate child process %d after proxy %d was killed! Orphaned process detected.",
			childPID, proxyPID)
	}

	t.Logf("SUCCESS: Windows Job Object automatically terminated child process %d when proxy died. Zero orphan leak confirmed.", childPID)
}
