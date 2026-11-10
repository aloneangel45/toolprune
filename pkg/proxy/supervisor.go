package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

const (
	supervisorReadBufferSize = 64 * 1024
	maxStderrLineBytes       = 256 * 1024
	stdoutChannelCapacity    = 1024
)

type ProcessState int32

const (
	StateCreated ProcessState = iota
	StateRunning
	StateStopping
	StateTerminated
	StateFailed
)

func (s ProcessState) String() string {
	switch s {
	case StateCreated:
		return "Created"
	case StateRunning:
		return "Running"
	case StateStopping:
		return "Stopping"
	case StateTerminated:
		return "Terminated"
	case StateFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// SupervisorConfig defines configuration for starting an upstream MCP process.
type SupervisorConfig struct {
	Name         string            // Descriptive name (e.g., "github", "sqlite")
	Command      string            // Executable path or command name
	Args         []string          // Command line arguments
	Env          map[string]string // Additional environment variables
	Dir          string            // Working directory
	GracePeriod  time.Duration     // Grace period before forced termination (default 2s)
	Logger       *slog.Logger      // Diagnostic logger (writes to os.Stderr)
	RingCapacity int               // Number of trailing stderr lines to retain (default 100)
}

// StderrRingBuffer maintains a thread-safe circular buffer of recent stderr log lines.
type StderrRingBuffer struct {
	mu       sync.RWMutex
	lines    []string
	capacity int
	head     int
	count    int
}

// NewStderrRingBuffer initializes a new circular buffer.
func NewStderrRingBuffer(capacity int) *StderrRingBuffer {
	if capacity <= 0 {
		capacity = 100
	}
	return &StderrRingBuffer{
		lines:    make([]string, capacity),
		capacity: capacity,
	}
}

// Append adds a new log line to the ring buffer.
func (r *StderrRingBuffer) Append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines[r.head] = line
	r.head = (r.head + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
	}
}

// Lines returns a snapshot of the buffered lines in chronological order.
func (r *StderrRingBuffer) Lines() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]string, r.count)
	start := (r.head - r.count + r.capacity) % r.capacity
	for i := 0; i < r.count; i++ {
		result[i] = r.lines[(start+i)%r.capacity]
	}
	return result
}

type stdoutItem struct {
	msg *mcp.Message
	err error
}

// Supervisor supervises an upstream MCP server subprocess.
type Supervisor struct {
	cfg            SupervisorConfig
	cmd            *exec.Cmd
	stdinPipe      io.WriteCloser
	stdoutPipe     io.ReadCloser
	stderrPipe     io.ReadCloser
	pipesCloseOnce sync.Once
	stdoutCh       chan stdoutItem
	stopDrainCh    chan struct{}
	stopDrainOnce  sync.Once
	stderrRing     *StderrRingBuffer
	logger         *slog.Logger
	state          atomic.Int32
	writeMu        sync.Mutex
	wg             sync.WaitGroup
	platformData   any // Platform-specific handle (e.g. WindowsJob)
	waitErr        error
	waitErrMu      sync.Mutex
	doneCh         chan struct{}
}

// NewSupervisor instantiates a Supervisor with the provided configuration.
func NewSupervisor(cfg SupervisorConfig) *Supervisor {
	if cfg.GracePeriod <= 0 {
		cfg.GracePeriod = 2 * time.Second
	}
	if cfg.RingCapacity <= 0 {
		cfg.RingCapacity = 100
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	s := &Supervisor{
		cfg:         cfg,
		stderrRing:  NewStderrRingBuffer(cfg.RingCapacity),
		logger:      logger,
		doneCh:      make(chan struct{}),
		stopDrainCh: make(chan struct{}),
	}
	s.state.Store(int32(StateCreated))
	return s
}

// Start launches the upstream process, configures pipes, starts stdout/stderr draining,
// and sets up platform-specific process tree supervision.
func (s *Supervisor) Start(ctx context.Context) error {
	if !s.state.CompareAndSwap(int32(StateCreated), int32(StateRunning)) {
		return fmt.Errorf("supervisor already started (state: %s)", ProcessState(s.state.Load()))
	}

	execPath, err := ResolveExecutable(s.cfg.Command)
	if err != nil {
		s.state.Store(int32(StateFailed))
		close(s.doneCh)
		return fmt.Errorf("resolve command %q: %w", s.cfg.Command, err)
	}

	s.cmd = exec.CommandContext(ctx, execPath, s.cfg.Args...)
	s.cmd.Dir = s.cfg.Dir

	// Configure environment variables
	env := os.Environ()
	for k, v := range s.cfg.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	s.cmd.Env = env

	// Platform-specific process attributes (e.g., Setpgid on Unix)
	configurePlatformCmd(s.cmd)

	// Set up pipes
	stdinPipe, err := s.cmd.StdinPipe()
	if err != nil {
		s.state.Store(int32(StateFailed))
		close(s.doneCh)
		return fmt.Errorf("cmd.StdinPipe: %w", err)
	}
	s.stdinPipe = stdinPipe

	stdoutPipe, err := s.cmd.StdoutPipe()
	if err != nil {
		s.state.Store(int32(StateFailed))
		close(s.doneCh)
		return fmt.Errorf("cmd.StdoutPipe: %w", err)
	}
	s.stdoutPipe = stdoutPipe

	stderrPipe, err := s.cmd.StderrPipe()
	if err != nil {
		s.state.Store(int32(StateFailed))
		close(s.doneCh)
		return fmt.Errorf("cmd.StderrPipe: %w", err)
	}
	s.stderrPipe = stderrPipe

	s.stdoutCh = make(chan stdoutItem, stdoutChannelCapacity)

	// Start child process
	if err := s.cmd.Start(); err != nil {
		s.state.Store(int32(StateFailed))
		close(s.stdoutCh)
		close(s.doneCh)
		return fmt.Errorf("cmd.Start (%s %v): %w", execPath, s.cfg.Args, err)
	}

	s.logger.Info("upstream subprocess started",
		"server", s.cfg.Name,
		"pid", s.cmd.Process.Pid,
		"command", execPath,
	)

	// Attach platform-specific supervision (Windows Job Objects)
	if err := attachPlatformSupervision(s); err != nil {
		s.logger.Warn("failed to attach platform supervision, proceeding with fallback",
			"server", s.cfg.Name,
			"pid", s.cmd.Process.Pid,
			"error", err,
		)
	}

	// Drain both stdout and stderr completely BEFORE calling cmd.Wait()
	// per os/exec contract so final buffered stdout/stderr frames are never truncated.
	var pipeWg sync.WaitGroup
	pipeWg.Add(2)

	s.wg.Add(3)
	go func() {
		defer s.wg.Done()
		defer pipeWg.Done()
		s.drainStdout(stdoutPipe)
	}()

	go func() {
		defer s.wg.Done()
		defer pipeWg.Done()
		s.drainStderr(stderrPipe)
	}()

	go func() {
		defer s.wg.Done()
		defer close(s.doneCh)
		pipeWg.Wait()
		err := s.cmd.Wait()
		s.waitErrMu.Lock()
		s.waitErr = err
		s.waitErrMu.Unlock()
		s.state.Store(int32(StateTerminated))
	}()

	return nil
}

func (s *Supervisor) closeReadPipes() {
	s.pipesCloseOnce.Do(func() {
		if s.stdoutPipe != nil {
			_ = s.stdoutPipe.Close()
		}
		if s.stderrPipe != nil {
			_ = s.stderrPipe.Close()
		}
	})
}

func (s *Supervisor) signalStopDrain() {
	s.stopDrainOnce.Do(func() {
		close(s.stopDrainCh)
	})
}

func (s *Supervisor) pushStdoutItem(item stdoutItem) bool {
	select {
	case s.stdoutCh <- item:
		return true
	case <-s.stopDrainCh:
		return false
	}
}

// drainStdout continuously reads bounded JSON-RPC lines from upstream stdout before cmd.Wait() closes the pipe.
func (s *Supervisor) drainStdout(stdout io.ReadCloser) {
	defer close(s.stdoutCh)

	reader := bufio.NewReaderSize(stdout, supervisorReadBufferSize)
	for {
		line, err := mcp.ReadBoundedLine(reader, DefaultMaxMessageSize)
		if errors.Is(err, mcp.ErrMessageTooLarge) {
			s.logger.Warn("upstream stdout message exceeded maximum size",
				"server", s.cfg.Name,
				"maxBytes", DefaultMaxMessageSize,
			)
			if !s.pushStdoutItem(stdoutItem{err: ErrMessageTooLarge}) {
				return
			}
			continue
		}

		if len(line) > 0 {
			trimmed := bytes.TrimSpace(line)
			if len(trimmed) > 0 {
				var msg mcp.Message
				if unmarshalErr := json.Unmarshal(trimmed, &msg); unmarshalErr != nil {
					s.logger.Warn("unmarshal upstream JSON-RPC failed",
						"server", s.cfg.Name,
						"error", unmarshalErr,
						"raw_snippet", truncateSnippet(trimmed, 128),
					)
					if !s.pushStdoutItem(stdoutItem{err: fmt.Errorf("unmarshal upstream JSON-RPC: %w", unmarshalErr)}) {
						return
					}
				} else {
					if !s.pushStdoutItem(stdoutItem{msg: &msg}) {
						return
					}
				}
			}
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				_ = s.pushStdoutItem(stdoutItem{err: err})
			}
			return
		}
	}
}

// drainStderr continuously drains upstream stderr with bounded line memory to prevent pipe saturation and OOM.
func (s *Supervisor) drainStderr(stderr io.ReadCloser) {
	reader := bufio.NewReaderSize(stderr, supervisorReadBufferSize)
	for {
		line, err := mcp.ReadBoundedLine(reader, maxStderrLineBytes)
		if errors.Is(err, mcp.ErrMessageTooLarge) {
			s.stderrRing.Append("[stderr line exceeded 256 KiB cap, truncated]")
			continue
		}
		if len(line) > 0 {
			cleanLine := strings.TrimRight(string(line), "\r\n")
			s.stderrRing.Append(cleanLine)
			s.logger.Debug("upstream stderr",
				"server", s.cfg.Name,
				"msg", cleanLine,
			)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				s.logger.Debug("upstream stderr pipe closed",
					"server", s.cfg.Name,
					"error", err,
				)
			}
			break
		}
	}
}

// ReadMessage reads a single newline-delimited JSON-RPC 2.0 message from the upstream server's stdout.
// Messages buffered right before process termination remain readable until drained.
func (s *Supervisor) ReadMessage() (*mcp.Message, error) {
	if s.stdoutCh == nil {
		return nil, fmt.Errorf("supervisor not running (state: %s)", ProcessState(s.state.Load()))
	}

	item, ok := <-s.stdoutCh
	if !ok {
		return nil, io.EOF
	}
	if item.err != nil {
		return nil, item.err
	}
	return item.msg, nil
}

// WriteMessage sends a JSON-RPC 2.0 message to the upstream server's stdin.
// Writes are thread-safe and serialized under writeMu.
func (s *Supervisor) WriteMessage(msg *mcp.Message) error {
	currentState := ProcessState(s.state.Load())
	if currentState != StateRunning {
		return fmt.Errorf("cannot write to upstream in state: %s", currentState)
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message for upstream: %w", err)
	}

	payload := make([]byte, len(data)+1)
	copy(payload, data)
	payload[len(data)] = '\n'

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	_, err = s.stdinPipe.Write(payload)
	return err
}

// CloseStdin closes the upstream process's stdin pipe to signal graceful shutdown via EOF.
func (s *Supervisor) CloseStdin() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.stdinPipe != nil {
		return s.stdinPipe.Close()
	}
	return nil
}

// Stop initiates a clean shutdown sequence:
// 1. Closes upstream stdin (EOF).
// 2. Waits up to GracePeriod for clean exit.
// 3. Escalates to platform-specific process tree termination if timeout expires.
func (s *Supervisor) Stop() error {
	if !s.state.CompareAndSwap(int32(StateRunning), int32(StateStopping)) {
		// Already stopping or stopped
		return s.Wait()
	}

	s.logger.Info("stopping upstream subprocess", "server", s.cfg.Name, "pid", s.Pid())

	// Step 1: Forward graceful stdin EOF
	_ = s.CloseStdin()

	// Step 2: Await graceful termination with timeout
	select {
	case <-s.doneCh:
		s.logger.Info("upstream subprocess exited cleanly", "server", s.cfg.Name)
	case <-time.After(s.cfg.GracePeriod):
		s.logger.Warn("grace period expired, forcefully terminating process tree",
			"server", s.cfg.Name,
			"pid", s.Pid(),
			"grace_period", s.cfg.GracePeriod,
		)
		// Step 3: Forcefully kill entire process tree and unblock any stalled pipe drainers
		killErr := terminatePlatformProcessTree(s)
		if killErr != nil {
			s.logger.Error("error during forced process tree termination",
				"server", s.cfg.Name,
				"error", killErr,
			)
		}
		s.signalStopDrain()
		s.closeReadPipes()
		// Await final process cleanup
		<-s.doneCh
	}

	// Clean up platform handles (e.g. close Job Object)
	cleanupPlatformSupervision(s)

	// Wait for all pipe drainers and wait coordinator to exit
	s.wg.Wait()

	s.state.Store(int32(StateTerminated))

	s.waitErrMu.Lock()
	waitErr := s.waitErr
	s.waitErrMu.Unlock()

	return waitErr
}

// Done returns a channel that is closed when the supervised process exits.
func (s *Supervisor) Done() <-chan struct{} {
	return s.doneCh
}

// Wait blocks until the supervised process exits and all I/O streams are fully drained.
func (s *Supervisor) Wait() error {
	<-s.doneCh
	s.wg.Wait()
	s.waitErrMu.Lock()
	defer s.waitErrMu.Unlock()
	return s.waitErr
}

// Send satisfies the UpstreamTransport interface.
func (s *Supervisor) Send(msg *mcp.Message) error {
	return s.WriteMessage(msg)
}

// Receive satisfies the UpstreamTransport interface.
func (s *Supervisor) Receive() (*mcp.Message, error) {
	return s.ReadMessage()
}

// Close satisfies the UpstreamTransport interface.
func (s *Supervisor) Close() error {
	return s.Stop()
}

// TrailingStderr returns the captured stderr log lines from the ring buffer.
func (s *Supervisor) TrailingStderr() []string {
	return s.stderrRing.Lines()
}

// Pid returns the subprocess PID, or 0 if not running.
func (s *Supervisor) Pid() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// State returns the current lifecycle state of the supervisor.
func (s *Supervisor) State() ProcessState {
	return ProcessState(s.state.Load())
}
