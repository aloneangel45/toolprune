package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aloneangel45/toolprune/pkg/config"
	"github.com/aloneangel45/toolprune/pkg/mcp"
	"github.com/aloneangel45/toolprune/pkg/proxy"
	"github.com/aloneangel45/toolprune/pkg/pruner"
	"github.com/aloneangel45/toolprune/pkg/router"
)

type proxyFlags struct {
	mode      string
	transport string
	port      int
	host      string
	profile   string
	timeout   time.Duration
	stats     bool
	namespace string
}

var pFlags proxyFlags

var proxyCmd = &cobra.Command{
	Use:   "proxy [flags] [-- <upstream_cmd> [args...]]",
	Short: "Start the ToolPrune proxy engine",
	Long: `Starts the ToolPrune proxy in either zero-config wrapper mode or standalone multi-upstream mode.

Modes:
  1. Zero-Config Wrapper:
     toolprune proxy --mode=aggressive -- npx -y @modelcontextprotocol/server-github
     Transparently wraps any upstream MCP server command.

  2. Standalone Multi-Upstream:
     toolprune proxy --config toolprune.yaml
     Orchestrates, namespaces, and multiplexes multiple upstream servers.`,
	RunE: runProxy,
}

func init() {
	proxyCmd.Flags().StringVarP(&pFlags.mode, "mode", "m", "aggressive", "Pruning tier (none, lossless, compact, aggressive, dynamic)")
	proxyCmd.Flags().StringVarP(&pFlags.transport, "transport", "t", "stdio", "Client transport type (stdio, sse)")
	proxyCmd.Flags().IntVarP(&pFlags.port, "port", "p", 8080, "Listen port for HTTP/SSE transport")
	proxyCmd.Flags().StringVar(&pFlags.host, "host", "127.0.0.1", "Listen host address for HTTP/SSE transport")
	proxyCmd.Flags().StringVar(&pFlags.profile, "profile", "full", "Active routing profile (full, read_only, dev, minimal)")
	proxyCmd.Flags().DurationVar(&pFlags.timeout, "timeout", 60*time.Second, "Request timeout duration")
	proxyCmd.Flags().BoolVar(&pFlags.stats, "stats", true, "Print token savings statistics summary on exit")
	proxyCmd.Flags().StringVar(&pFlags.namespace, "namespace", "auto", "Namespacing mode for multi-upstream (auto, always, on_collision, none)")
}

func runProxy(cmd *cobra.Command, args []string) error {
	logger := slog.Default()

	// Environment variable fallbacks
	if !cmd.Flags().Changed("mode") {
		bindEnvFallback("mode", "TOOLPRUNE_MODE", func(v string) { pFlags.mode = v })
	}
	if !cmd.Flags().Changed("transport") {
		bindEnvFallback("transport", "TOOLPRUNE_TRANSPORT", func(v string) { pFlags.transport = v })
	}
	if !cmd.Flags().Changed("profile") {
		bindEnvFallback("profile", "TOOLPRUNE_PROFILE", func(v string) { pFlags.profile = v })
	}
	if !cmd.Flags().Changed("namespace") {
		bindEnvFallback("namespace", "TOOLPRUNE_NAMESPACE", func(v string) { pFlags.namespace = v })
	}

	// Set up two-stage signal handling context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Handle second Ctrl+C for immediate termination
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		// First signal: graceful shutdown initiates via ctx.Done()
		<-sigCh
		// Second signal: force exit
		fmt.Fprintln(os.Stderr, "\nReceived second interrupt. Forcing immediate termination...")
		os.Exit(130)
	}()

	// Determine execution mode:
	// If arguments exist after '--', run Mode A (Zero-Config Transparent Wrapper)
	if len(args) > 0 {
		return runWrapperMode(ctx, args, logger)
	}

	// Otherwise, check for configuration file (Mode B: Multi-Upstream Standalone Mode)
	configPath := globalConfig
	if configPath == "" {
		configPath = config.FindConfigFile()
	}

	if configPath != "" {
		return runMultiUpstreamMode(ctx, configPath, logger)
	}

	return fmt.Errorf("no upstream command or configuration specified\n\nUsage:\n  toolprune proxy [flags] -- <upstream_cmd> [args...]\n  toolprune proxy --config toolprune.yaml\n\nRun 'toolprune proxy --help' for details")
}

// runWrapperMode runs transparent single-upstream wrapper proxy.
func runWrapperMode(ctx context.Context, cmdArgs []string, logger *slog.Logger) error {
	cmdName := cmdArgs[0]
	cmdRest := cmdArgs[1:]

	logger.Info("starting ToolPrune in zero-config wrapper mode",
		"command", cmdName,
		"args", cmdRest,
		"pruneMode", pFlags.mode,
		"transport", pFlags.transport,
	)

	// Configure upstream process supervisor
	supCfg := proxy.SupervisorConfig{
		Name:         filepath.Base(cmdName),
		Command:      cmdName,
		Args:         cmdRest,
		Env:          nil, // inherits current process environment
		GracePeriod:  2 * time.Second,
		Logger:       logger,
		RingCapacity: 100,
	}
	supervisor := proxy.NewSupervisor(supCfg)

	if err := supervisor.Start(ctx); err != nil {
		return fmt.Errorf("failed to start upstream process %q: %w", cmdName, err)
	}
	defer func() {
		_ = supervisor.Stop()
	}()

	// Configure pruner engine
	tier := pruner.PruneTier(pFlags.mode)
	pruneOpts := pruner.DefaultOptions(tier)
	pruneEngine := pruner.New(pruneOpts)

	// Configure proxy engine
	proxyCfg := proxy.ProxyConfig{
		PruneMode:      pFlags.mode,
		RequestTimeout: pFlags.timeout,
		InjectProxyInfo: true,
	}
	p := proxy.NewProxy(proxyCfg, supervisor, pruneEngine)
	p.Start()
	defer p.Shutdown()

	// Dispatch client transport
	if strings.ToLower(pFlags.transport) == "sse" {
		return runSSETransport(ctx, p, logger)
	}
	return runStdioTransport(ctx, p, supervisor, logger)
}

// runStdioTransport connects os.Stdin / os.Stdout to proxy.
func runStdioTransport(ctx context.Context, p *proxy.Proxy, supervisor *proxy.Supervisor, logger *slog.Logger) error {
	stdioOpts := proxy.StdioOptions{
		In:     os.Stdin,
		Out:    os.Stdout,
		Logger: logger,
	}
	stdioTransport := proxy.NewStdioTransport(stdioOpts)
	defer func() { _ = stdioTransport.Close() }()

	session := proxy.NewStdioSession(stdioTransport)
	p.RegisterClient(session)
	defer p.UnregisterClient(session.ID())

	// Read loop in background
	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stdioTransport.ReadMessage()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, proxy.ErrTransportClosed) {
					errCh <- nil
					return
				}
				errCh <- err
				return
			}
			if err := p.HandleClientMessage(session, msg); err != nil {
				logger.Warn("error handling client message", "err", err)
			}
		}
	}()

	var supDone <-chan struct{}
	if supervisor != nil {
		supDone = supervisor.Done()
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down proxy from signal context")
		return nil
	case <-supDone:
		logger.Info("upstream subprocess terminated; exiting stdio transport")
		return supervisor.Wait()
	case err := <-errCh:
		if err != nil {
			logger.Error("stdio transport read failed", "err", err)
			return err
		}
		logger.Info("client disconnected (stdio EOF)")
		return nil
	}
}

// runSSETransport starts the HTTP/SSE server.
func runSSETransport(ctx context.Context, p *proxy.Proxy, logger *slog.Logger) error {
	sseCfg := proxy.SSEServerConfig{
		Host:            pFlags.host,
		Port:            pFlags.port,
		BasePath:        "",
		CORSOrigins:     []string{"*"},
		KeepAlivePeriod: 15 * time.Second,
		MaxBodySize:     10 * 1024 * 1024,
	}
	sseServer := proxy.NewSSEServer(sseCfg, p)
	if err := sseServer.Start(); err != nil {
		return fmt.Errorf("failed to start SSE server: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sseServer.Shutdown(shutdownCtx)
	}()

	logger.Info("ToolPrune SSE server ready", "endpoint", fmt.Sprintf("http://%s:%d/sse", pFlags.host, pFlags.port))

	<-ctx.Done()
	logger.Info("shutting down SSE server")
	return nil
}

// runMultiUpstreamMode starts multi-upstream multiplexer from config file.
func runMultiUpstreamMode(ctx context.Context, configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration %q: %w", configPath, err)
	}

	logger.Info("loaded configuration", "path", configPath, "upstreams", len(cfg.Upstreams))

	// Instantiate router
	rCfg := router.RouterConfig{
		NamespaceMode:  router.NamespaceMode(cfg.Routing.NamespaceMode),
		PruneMode:      cfg.Pruning.Tier,
		ActiveProfile:  cfg.Routing.DefaultProfile,
		MaxActiveTools: cfg.Routing.ActiveWindowSize,
		EnableCatalog:  cfg.Pruning.Tier == "dynamic",
	}
	if pFlags.mode != "" && pFlags.mode != "aggressive" {
		rCfg.PruneMode = pFlags.mode
	}

	tier := pruner.PruneTier(rCfg.PruneMode)
	pruneEngine := pruner.New(pruner.DefaultOptions(tier))
	r := router.NewRouter(rCfg, pruneEngine, logger)
	defer func() { _ = r.Close() }()

	// Spawn and register each upstream server
	for _, u := range cfg.Upstreams {
		if u.Disabled {
			continue
		}
		id := u.ID
		supCfg := proxy.SupervisorConfig{
			Name:         id,
			Command:      u.Command,
			Args:         u.Args,
			Env:          u.Env,
			Dir:          u.Dir,
			GracePeriod:  2 * time.Second,
			Logger:       logger,
			RingCapacity: 100,
		}
		sup := proxy.NewSupervisor(supCfg)
		if err := sup.Start(ctx); err != nil {
			return fmt.Errorf("failed to start upstream %q: %w", id, err)
		}
		defer func() { _ = sup.Stop() }()

		// Perform initial handshake and sync tools
		tools, err := performUpstreamHandshake(sup)
		if err != nil {
			logger.Warn("handshake with upstream failed, continuing", "upstream", id, "err", err)
		}

		_ = r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{
			ID:        id,
			Prefix:    u.Prefix,
			Transport: sup,
		})
		r.Multiplexer().SyncUpstreamTools(ctx, id, tools)
	}

	logger.Info("all upstreams registered and synchronized", "activeTools", len(r.Multiplexer().ListAllTools()))

	// If stdio mode, run stdio router message loop
	if strings.ToLower(pFlags.transport) == "stdio" {
		return runRouterStdioLoop(ctx, r, logger)
	}

	// For SSE mode, wire up SSE with Router handler
	return fmt.Errorf("multi-upstream SSE mode initialized")
}

// performUpstreamHandshake performs standard MCP initialize + tools/list exchange.
func performUpstreamHandshake(transport proxy.UpstreamTransport) ([]mcp.Tool, error) {
	initReq, _ := mcp.NewRequest(mcp.NewIntID(1), mcp.MethodInitialize, mcp.InitializeRequestParams{
		ProtocolVersion: mcp.LatestProtocolVersion,
		ClientInfo:      mcp.Implementation{Name: "ToolPrune", Version: Version},
	})
	if err := transport.Send(initReq); err != nil {
		return nil, err
	}

	// Receive initialize response
	_, err := transport.Receive()
	if err != nil {
		return nil, err
	}

	// Send initialized notification
	initNotif, _ := mcp.NewNotification(mcp.MethodNotificationsInitialized, nil)
	_ = transport.Send(initNotif)

	// Send tools/list request
	listReq, _ := mcp.NewRequest(mcp.NewIntID(2), mcp.MethodToolsList, nil)
	if err := transport.Send(listReq); err != nil {
		return nil, err
	}

	listResp, err := transport.Receive()
	if err != nil {
		return nil, err
	}

	var res mcp.ListToolsResult
	if err := json.Unmarshal(listResp.Result, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// runRouterStdioLoop dispatches stdio messages to unified router.
func runRouterStdioLoop(ctx context.Context, r *router.Router, logger *slog.Logger) error {
	stdioTransport := proxy.NewStdioTransport(proxy.StdioOptions{In: os.Stdin, Out: os.Stdout, Logger: logger})
	defer func() { _ = stdioTransport.Close() }()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		msg, err := stdioTransport.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, proxy.ErrTransportClosed) {
				return nil
			}
			return err
		}

		// Dispatch by method
		if msg.IsRequest() {
			switch msg.Method {
			case mcp.MethodInitialize:
				initRes := mcp.InitializeResult{
					ProtocolVersion: mcp.LatestProtocolVersion,
					Capabilities: mcp.ServerCapabilities{
						Tools: &mcp.ToolsCapability{ListChanged: true},
					},
					ServerInfo:   mcp.Implementation{Name: "ToolPrune Unified Proxy", Version: Version},
					Instructions: "ToolPrune multiplexed MCP proxy: tools pruned and dynamically routed.",
				}
				resp, _ := mcp.NewResponse(*msg.ID, initRes)
				_ = stdioTransport.WriteMessage(resp)

			case mcp.MethodToolsList:
				resp, err := r.HandleToolsList(ctx, msg)
				if err != nil {
					resp = mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, err.Error())
				}
				_ = stdioTransport.WriteMessage(resp)

			case mcp.MethodToolsCall:
				// First check internal discovery meta-tools
				if internalResp, ok := r.HandleInternalToolCall(msg); ok {
					_ = stdioTransport.WriteMessage(internalResp)
					continue
				}

				// Route to upstream
				targetTransport, rewrittenMsg, err := r.RouteToolCall(msg)
				if err != nil {
					_ = stdioTransport.WriteMessage(mcp.NewErrorMessage(msg.ID, mcp.CodeMethodNotFound, err.Error()))
					continue
				}

				// Forward rewritten request to upstream
				if err := targetTransport.Send(rewrittenMsg); err != nil {
					_ = stdioTransport.WriteMessage(mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, err.Error()))
					continue
				}

				// Receive response and forward back to client
				upResp, err := targetTransport.Receive()
				if err != nil {
					_ = stdioTransport.WriteMessage(mcp.NewErrorMessage(msg.ID, mcp.CodeInternalError, err.Error()))
					continue
				}
				upResp.ID = msg.ID // Restore original client ID
				_ = stdioTransport.WriteMessage(upResp)

			case mcp.MethodPing:
				resp, _ := mcp.NewResponse(*msg.ID, map[string]any{})
				_ = stdioTransport.WriteMessage(resp)

			default:
				_ = stdioTransport.WriteMessage(mcp.NewErrorMessage(msg.ID, mcp.CodeMethodNotFound, fmt.Sprintf("unsupported method %q", msg.Method)))
			}
		}
	}
}
