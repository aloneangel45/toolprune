//go:build !windows

package proxy

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func configurePlatformCmd(cmd *exec.Cmd) {
	// Put child in its own process group (PGID == PID)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}
}

func attachPlatformSupervision(s *Supervisor) error {
	// POSIX process group configured via SysProcAttr before start
	return nil
}

func terminatePlatformProcessTree(s *Supervisor) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}

	pid := s.cmd.Process.Pid
	pgid := pid // Because Setpgid is true, PGID == PID

	// Stage 1: Send SIGTERM to entire process group (-pgid)
	_ = syscall.Kill(-pgid, syscall.SIGTERM)

	// Await termination with short grace period
	done := make(chan struct{})
	go func() {
		// Poll process state
		for {
			err := syscall.Kill(-pgid, 0)
			if err != nil {
				// Process group no longer exists
				close(done)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	select {
	case <-done:
		return nil
	case <-time.After(1 * time.Second):
		// Stage 2: Escalate to SIGKILL to entire process group (-pgid)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return fmt.Errorf("process group %d did not terminate on SIGTERM, killed with SIGKILL", pgid)
	}
}

func cleanupPlatformSupervision(s *Supervisor) {
	// No OS handles to close on POSIX
}

// ResolveExecutable resolves executable using standard exec.LookPath on Unix.
func ResolveExecutable(name string) (string, error) {
	return exec.LookPath(name)
}
