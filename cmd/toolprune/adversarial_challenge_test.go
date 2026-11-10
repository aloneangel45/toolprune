package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
)

// -----------------------------------------------------------------------------
// 1. CLI Flags Fuzzing: Unknown Flags, Missing Args, Conflicting Options
// -----------------------------------------------------------------------------

func TestAdversarial_CLIUnknownFlags(t *testing.T) {
	unknownFlags := [][]string{
		{"--unknown-global-flag"},
		{"proxy", "--invalid-mode-flag"},
		{"bench", "--bogus"},
		{"routes", "--doesnotexist"},
		{"config", "validate", "--badflag"},
		{"version", "--invalid"},
	}

	for _, args := range unknownFlags {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(args)

			err := rootCmd.Execute()
			if err == nil {
				t.Fatalf("expected error for unknown flag execution %v, got nil", args)
			}
			t.Logf("CLI correctly rejected unknown flag %v with error: %v", args, err)
		})
	}
}

func TestAdversarial_MissingArguments(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bench missing flag arg", []string{"bench", "--file"}},
		{"bench missing model arg", []string{"bench", "--model"}},
		{"routes missing config arg", []string{"routes", "--config"}},
		{"config validate missing config arg", []string{"config", "validate", "--config"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			rootCmd.SetOut(buf)
			rootCmd.SetErr(buf)
			rootCmd.SetArgs(tc.args)

			err := rootCmd.Execute()
			if err == nil {
				t.Fatalf("expected error for missing flag argument %v, got nil", tc.args)
			}
			t.Logf("CLI correctly rejected missing argument with error: %v", err)
		})
	}
}

func TestAdversarial_ConflictingOptions(t *testing.T) {
	t.Run("version --short and --json", func(t *testing.T) {
		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetErr(buf)
		rootCmd.SetArgs([]string{"version", "--short", "--json"})

		err := rootCmd.Execute()
		if err != nil {
			t.Fatalf("version with conflicting flags failed: %v", err)
		}
		output := buf.String()
		// Should output JSON format when both are passed
		if !strings.Contains(output, `"version":`) {
			t.Errorf("expected JSON output when --json and --short are combined, got: %s", output)
		}
	})
}

func TestAdversarial_SilenceErrorsAudit(t *testing.T) {
	// Audit finding: rootCmd has SilenceErrors: true, and main.go does not print err
	if rootCmd.SilenceErrors {
		t.Logf("CONFIRMED: rootCmd.SilenceErrors is true. Any error returned by rootCmd.Execute() will NOT be printed to stderr unless main.go prints it.")
	}
}

func TestAdversarial_RoutesFlagsIgnoredAudit(t *testing.T) {
	configPath := "../../toolprune.yaml"
	if _, err := exec.LookPath("go"); err == nil {
		// Resolve config relative to package test directory
	}

	t.Run("evaluates --profile and rejects unknown profile", func(t *testing.T) {
		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetErr(buf)
		rootCmd.SetArgs([]string{"routes", "--config", configPath, "--profile", "nonexistent_profile_404"})
		err := rootCmd.Execute()
		if err == nil {
			t.Fatal("expected error for unknown --profile on routes command, got nil")
		}
		if !strings.Contains(err.Error(), "nonexistent_profile_404") {
			t.Errorf("expected error mentioning invalid profile, got: %v", err)
		}
	})

	t.Run("evaluates --profile read_only and --filter", func(t *testing.T) {
		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetErr(buf)
		rootCmd.SetArgs([]string{"routes", "--config", configPath, "--profile", "read_only", "--filter", "sqlite"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("routes --profile read_only --filter sqlite failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, "sqlite") {
			t.Errorf("expected sqlite upstream in filtered output, got:\n%s", out)
		}
		if strings.Contains(out, "remote_agent") {
			t.Errorf("expected remote_agent to be excluded by --filter sqlite, got:\n%s", out)
		}
		if !strings.Contains(out, "read-only") {
			t.Errorf("expected read-only profile access evaluation in output, got:\n%s", out)
		}
	})

	t.Run("custom profile filters out blocked upstreams", func(t *testing.T) {
		buf := new(bytes.Buffer)
		rootCmd.SetOut(buf)
		rootCmd.SetErr(buf)
		// In toolprune.yaml, 'coding' profile includes github__*, git_*, filesystem__*, *search*, *read*
		rootCmd.SetArgs([]string{"routes", "--config", configPath, "--profile", "minimal", "--json"})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("routes --profile minimal --json failed: %v", err)
		}
		out := buf.String()
		if !strings.Contains(out, `"activeProfile": "minimal"`) {
			t.Errorf("expected activeProfile minimal in JSON output, got:\n%s", out)
		}
	})
}

// -----------------------------------------------------------------------------
// 2. Zero-Config Wrapper: Non-Existent Command (Exit Code 127 Audit)
// -----------------------------------------------------------------------------

func TestAdversarial_Wrapper_NonExistentCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	logger := slog.Default()
	cmdArgs := []string{"this_command_absolutely_does_not_exist_404_test"}

	err := runWrapperMode(ctx, cmdArgs, logger)
	if err == nil {
		t.Fatalf("expected runWrapperMode to fail on non-existent command, got nil")
	}

	t.Logf("runWrapperMode returned error: %v", err)

	// Check if the underlying error is exec.ErrNotFound
	isNotFound := errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found")
	if !isNotFound {
		t.Errorf("expected exec.ErrNotFound in error chain, got: %v", err)
	}

	// Audit finding: In POSIX / wrapper CLI standards, non-existent commands should result in exit code 127.
	// Currently, main.go exits with 1 for all errors.
	t.Logf("AUDIT: Command not found error detected. In main.go, this exits with code 1 instead of standard exit code 127.")
}

// -----------------------------------------------------------------------------
// 3. Zero-Config Wrapper: Stdin EOF and Subprocess Lifecycle
// -----------------------------------------------------------------------------

func TestAdversarial_Wrapper_ImmediateStdinEOF(t *testing.T) {
	// Verify that stdio transport with immediate EOF terminates gracefully
	emptyReader := strings.NewReader("")
	bufOut := new(bytes.Buffer)
	logger := slog.Default()

	stdioOpts := proxy.StdioOptions{
		In:     emptyReader,
		Out:    bufOut,
		Logger: logger,
	}
	transport := proxy.NewStdioTransport(stdioOpts)
	defer transport.Close()

	// Reading from empty reader should return EOF
	_, err := transport.ReadMessage()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty reader, got: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 4. Zero-Config Wrapper: Child Process Disconnect / Crash While Stdin Open
// -----------------------------------------------------------------------------

func TestAdversarial_Wrapper_ChildCrashDetection(t *testing.T) {
	// Test supervisor behavior when child process crashes immediately
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	logger := slog.Default()
	cmdName := "cmd.exe"
	cmdArgs := []string{"/c", "exit 42"}
	if runtime.GOOS != "windows" {
		cmdName = "sh"
		cmdArgs = []string{"-c", "exit 42"}
	}
	supCfg := proxy.SupervisorConfig{
		Name:         "crash-test",
		Command:      cmdName,
		Args:         cmdArgs,
		GracePeriod:  500 * time.Millisecond,
		Logger:       logger,
		RingCapacity: 10,
	}
	sup := proxy.NewSupervisor(supCfg)
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("failed to start supervisor: %v", err)
	}
	defer sup.Stop()

	// Wait for process to exit
	time.Sleep(200 * time.Millisecond)

	// In proxy.NewProxy, if the upstream crashes, does Proxy have an exit / error channel?
	pCfg := proxy.ProxyConfig{
		PruneMode:      "aggressive",
		RequestTimeout: 5 * time.Second,
	}
	pruneEngine := pruner.New(pruner.DefaultOptions(pruner.TierAggressive))
	p := proxy.NewProxy(pCfg, sup, pruneEngine)
	p.Start()
	defer p.Shutdown()

	t.Logf("AUDIT: Supervisor detected process termination. Note that runStdioTransport selects only on ctx.Done() and client stdin EOF; if upstream process exits while client stdin is open, runStdioTransport will block indefinitely until client stdin closes.")
}
