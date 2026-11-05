//go:build windows

package proxy

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsJobData struct {
	jobHandle windows.Handle
}

func configurePlatformCmd(cmd *exec.Cmd) {
	// Standard Windows process creation
}

func attachPlatformSupervision(s *Supervisor) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return errors.New("cannot attach job object to nil process")
	}

	pid := s.cmd.Process.Pid

	// 1. Create anonymous Job Object
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject failed: %w", err)
	}

	// 2. Configure JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}

	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("SetInformationJobObject failed: %w", err)
	}

	// 3. Open process handle with required permissions
	hProcess, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(pid),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("OpenProcess(pid=%d) failed: %w", pid, err)
	}
	defer windows.CloseHandle(hProcess)

	// 4. Assign process to Job Object
	if err := windows.AssignProcessToJobObject(job, hProcess); err != nil {
		_ = windows.CloseHandle(job)
		return fmt.Errorf("AssignProcessToJobObject failed: %w", err)
	}

	s.platformData = &windowsJobData{jobHandle: job}
	s.logger.Debug("attached subprocess to Windows Job Object with KILL_ON_JOB_CLOSE",
		"server", s.cfg.Name,
		"pid", pid,
	)
	return nil
}

func terminatePlatformProcessTree(s *Supervisor) error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}

	pid := s.cmd.Process.Pid

	// Execute taskkill /F /T /PID <pid> to terminate the full tree
	killCmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	output, err := killCmd.CombinedOutput()
	if err != nil {
		outStr := strings.ToLower(string(output))
		// If process was already terminated, taskkill returns an error containing "not found"
		if strings.Contains(outStr, "not found") || strings.Contains(outStr, "bulunamadı") {
			return nil
		}
		return fmt.Errorf("taskkill error: %w (output: %s)", err, strings.TrimSpace(string(output)))
	}

	return nil
}

func cleanupPlatformSupervision(s *Supervisor) {
	if data, ok := s.platformData.(*windowsJobData); ok && data != nil {
		if data.jobHandle != 0 {
			_ = windows.CloseHandle(data.jobHandle)
			data.jobHandle = 0
		}
	}
}

// ResolveExecutable finds the target executable on Windows, taking into account .cmd, .bat, and .exe wrappers.
func ResolveExecutable(name string) (string, error) {
	// If path is absolute or has directory separator, check directly
	if strings.ContainsAny(name, `/\`) {
		return exec.LookPath(name)
	}

	// First try standard LookPath (which honors PATHEXT)
	path, err := exec.LookPath(name)
	if err == nil {
		return path, nil
	}

	// Fallback check with explicit Windows extensions
	for _, ext := range []string{".cmd", ".exe", ".bat"} {
		if p, err := exec.LookPath(name + ext); err == nil {
			return p, nil
		}
	}

	return "", err
}
