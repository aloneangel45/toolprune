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

	// Await termination with short grace period; stopPoll guarantees the polling goroutine never leaks.
	done := make(chan struct{})
	stopPoll := make(chan struct{})
	defer close(stopPoll)

	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := syscall.Kill(-pgid, 0); err != nil {
				close(done)
				return
			}
			select {
			case <-stopPoll:
				return
			case <-ticker.C:
			}
		}
	}()

	timer := time.NewTimer(1 * time.Second)
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-timer.C:
		// Stage 2: Escalate to SIGKILL to entire process group (-pgid)
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return fmt.Errorf("process group %d did not terminate on SIGTERM, killed with SIGKILL", pgid)
	}
}

func cleanupPlatformSupervision(s *Supervisor) {
	// No OS handles to close on POSIX (idempotent no-op)
}

// ResolveExecutable resolves executable using standard exec.LookPath on Unix.
func ResolveExecutable(name string) (string, error) {
	return exec.LookPath(name)
}
