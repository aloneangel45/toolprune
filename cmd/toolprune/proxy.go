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
	"strconv"
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
	mode            string
	preset          string
	maxSchemaTokens int
	transport       string
	port            int
	host            string
	profile         string
	timeout         time.Duration
	stats           bool
	namespace       string
}

var pFlags proxyFlags

var proxyCmd = &cobra.Command{
	Use:   "proxy [flags] [-- <upstream_cmd> [args...]]",
	Short: "Start the ToolPrune proxy engine",
	Long: `Starts the ToolPrune proxy in either zero-config wrapper mode or standalone multi-upstream mode.

Modes:
  1. Zero-Config Wrapper:
     toolprune proxy --mode=aggressive -- npx -y @modelcontextprotocol/server-github
     toolprune proxy --preset=balanced --max-schema-tokens=2000 -- npx -y @modelcontextprotocol/server-github
     Transparently wraps any upstream MCP server command.

  2. Standalone Multi-Upstream:
     toolprune proxy --config toolprune.yaml
     Orchestrates, namespaces, and multiplexes multiple upstream servers over stdio, SSE, or Streamable HTTP.`,
	RunE: runProxy,
}

func init() {
	proxyCmd.Flags().StringVarP(&pFlags.mode, "mode", "m", "aggressive", "Pruning tier (none, lossless, compact, aggressive, dynamic)")
	proxyCmd.Flags().StringVar(&pFlags.preset, "preset", "", "Pruning preset alias (fast, balanced, strict, dynamic)")
	proxyCmd.Flags().IntVar(&pFlags.maxSchemaTokens, "max-schema-tokens", 0, "Maximum token budget for tools/list schemas (0 = unlimited)")
	proxyCmd.Flags().StringVarP(&pFlags.transport, "transport", "t", "stdio", "Client transport type (stdio, sse)")
	proxyCmd.Flags().IntVarP(&pFlags.port, "port", "p", 8080, "Listen port for HTTP/SSE and Streamable HTTP transport")
	proxyCmd.Flags().StringVar(&pFlags.host, "host", "127.0.0.1", "Listen host address for HTTP/SSE transport")
	proxyCmd.Flags().StringVar(&pFlags.profile, "profile", "full", "Active routing profile (full, read_only, dev, minimal)")
	proxyCmd.Flags().DurationVar(&pFlags.timeout, "timeout", 60*time.Second, "Request timeout duration")
	proxyCmd.Flags().BoolVar(&pFlags.stats, "stats", true, "Print token savings statistics summary to stderr on exit")
	proxyCmd.Flags().StringVar(&pFlags.namespace, "namespace", "auto", "Namespacing mode for multi-upstream (auto, always, on_collision, none)")
}

// resolvePresetToMode maps user-friendly preset aliases to canonical pruning tiers.
func resolvePresetToMode(preset string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fast", "lossless":
		return "lossless", nil
	case "balanced", "compact":
		return "compact", nil
	case "strict", "aggressive":
		return "aggressive", nil
	case "dynamic":
		return "dynamic", nil
	case "none":
		return "none", nil
	default:
		return "", fmt.Errorf("invalid --preset %q: must be one of fast, balanced, strict, dynamic", preset)
	}
}

func collectChangedFlags(cmd *cobra.Command) map[string]bool {
	changed := make(map[string]bool)
	if cmd == nil {
		return changed
	}
	for _, name := range []string{
		"mode", "preset", "max-schema-tokens", "transport",
		"port", "host", "profile", "timeout", "stats", "namespace",
	} {
		if cmd.Flags().Changed(name) {
			changed[name] = true
		}
	}
	return changed
}

func runProxy(cmd *cobra.Command, args []string) error {
	logger := slog.Default()
	changed := collectChangedFlags(cmd)

	// Resolve --preset if explicitly specified or via TOOLPRUNE_PRESET
	presetVal := pFlags.preset
	if !changed["preset"] {
		if envPreset := strings.TrimSpace(os.Getenv("TOOLPRUNE_PRESET")); envPreset != "" {
			presetVal = envPreset
		}
	}
	if presetVal != "" {
		resolvedMode, err := resolvePresetToMode(presetVal)
		if err != nil {
			return err
		}
		if !changed["mode"] {
			pFlags.mode = resolvedMode
			changed["preset_resolved"] = true
		}
	}

	// Environment variable fallbacks for wrapper mode when flags were not explicitly set
	if !changed["mode"] && !changed["preset_resolved"] {
		bindEnvFallback("mode", "TOOLPRUNE_MODE", func(v string) { pFlags.mode = v })
		bindEnvFallback("mode", "TOOLPRUNE_TIER", func(v string) { pFlags.mode = v })
	}
	if !changed["max-schema-tokens"] {
		bindEnvFallback("max-schema-tokens", "TOOLPRUNE_MAX_SCHEMA_TOKENS", func(v string) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
				pFlags.maxSchemaTokens = n
			}
		})
	}
	if !changed["transport"] {
		bindEnvFallback("transport", "TOOLPRUNE_TRANSPORT", func(v string) { pFlags.transport = v })
	}
	if !changed["host"] {
		bindEnvFallback("host", "TOOLPRUNE_HOST", func(v string) { pFlags.host = v })
	}
	if !changed["port"] {
		bindEnvFallback("port", "TOOLPRUNE_PORT", func(v string) {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
				pFlags.port = n
			}
		})
	}
	if !changed["profile"] {
		bindEnvFallback("profile", "TOOLPRUNE_PROFILE", func(v string) { pFlags.profile = v })
	}
	if !changed["namespace"] {
		bindEnvFallback("namespace", "TOOLPRUNE_NAMESPACE", func(v string) { pFlags.namespace = v })
	}
	if !changed["stats"] {
		bindEnvFallback("stats", "TOOLPRUNE_STATS", func(v string) {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				pFlags.stats = b
			}
		})
	}

	// Set up two-stage signal handling context
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		_, ok := <-sigCh
		if !ok {
			return
		}
		_, ok = <-sigCh
		if !ok {
			return
		}
		fmt.Fprintln(os.Stderr, "\nReceived second interrupt. Forcing immediate termination...")
		os.Exit(130)
	}()

	// Mode A: Zero-Config Transparent Wrapper
	if len(args) > 0 {
		return runWrapperModeWithFlags(ctx, args, changed, logger)
	}

	// Mode B: Standalone Multi-Upstream Mode from toolprune.yaml
	configPath := globalConfig
	if configPath == "" {
		configPath = config.FindConfigFile()
	}

	if configPath != "" {
		return runMultiUpstreamModeWithFlags(ctx, configPath, changed, logger)
	}

	return fmt.Errorf("no upstream command or configuration specified\n\nUsage:\n  toolprune proxy [flags] -- <upstream_cmd> [args...]\n  toolprune proxy --config toolprune.yaml\n\nRun 'toolprune proxy --help' for details")
}

// runWrapperMode runs transparent single-upstream wrapper proxy (preserved signature for direct callers/tests).
func runWrapperMode(ctx context.Context, cmdArgs []string, logger *slog.Logger) error {
	return runWrapperModeWithFlags(ctx, cmdArgs, nil, logger)
}

func runWrapperModeWithFlags(ctx context.Context, cmdArgs []string, changed map[string]bool, logger *slog.Logger) error {
	cmdName := cmdArgs[0]
	cmdRest := cmdArgs[1:]

	cfg := config.DefaultConfig()
	cfg.ApplyEnvOverrides()

	mode := pFlags.mode
	if mode == "" {
		mode = cfg.Pruning.Tier
	}
	cfg.Pruning.Tier = mode
	if pFlags.maxSchemaTokens > 0 || (changed != nil && changed["max-schema-tokens"]) {
		cfg.Pruning.MaxSchemaTokens = pFlags.maxSchemaTokens
	}
	if pFlags.profile != "" {
		cfg.Routing.DefaultProfile = pFlags.profile
	}
	if pFlags.transport != "" {
		cfg.Server.Transport = pFlags.transport
	}
	if pFlags.host != "" {
		cfg.Server.Host = pFlags.host
	}
	if pFlags.port > 0 || (changed != nil && changed["port"]) {
		cfg.Server.Port = pFlags.port
	}
	if pFlags.timeout > 0 {
		cfg.Server.Timeout = config.Duration(pFlags.timeout)
	}

	logger.Info("starting ToolPrune in zero-config wrapper mode",
		"command", cmdName,
		"args", cmdRest,
		"pruneMode", cfg.Pruning.Tier,
		"maxSchemaTokens", cfg.Pruning.MaxSchemaTokens,
		"profile", cfg.Routing.DefaultProfile,
		"transport", cfg.Server.Transport,
	)

	upSpec := config.UpstreamConfig{
		ID:        filepath.Base(cmdName),
		Transport: "stdio",
		Command:   cmdName,
		Args:      cmdRest,
	}
	supCfg := upSpec.ToSupervisorConfig(logger)
	supCfg.GracePeriod = 2 * time.Second
	supervisor := proxy.NewSupervisor(supCfg)

	if err := supervisor.Start(ctx); err != nil {
		return fmt.Errorf("failed to start upstream process %q: %w", cmdName, err)
	}
	defer func() {
		_ = supervisor.Stop()
	}()

	filterCfg := cfg.ToFilterConfig()
	toolFilter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		return fmt.Errorf("invalid routing profile %q: %w", cfg.Routing.DefaultProfile, err)
	}

	pruneOpts := cfg.ToPrunerOptions()
	pruneEngine := pruner.New(pruneOpts)

	proxyCfg := cfg.ToProxyConfig()
	proxyCfg.ToolFilterFunc = toolFilter.IsAllowed

	var p *proxy.Proxy
	var notifier *router.DebouncedNotifier

	if strings.EqualFold(cfg.Pruning.Tier, "dynamic") {
		notifier = router.NewDebouncedNotifier(nil, 50*time.Millisecond)
		defer notifier.Stop()
		adapter, aErr := router.NewWrapperCatalogAdapter(
			cfg.Routing.ActiveWindowSize,
			cfg.Routing.PinnedTools,
			toolFilter,
			pruneEngine,
			notifier,
			func() {
				if p != nil && p.StreamFilter() != nil {
					p.StreamFilter().InvalidateCache()
				}
				notifier.Trigger()
			},
		)
		if aErr != nil {
			return fmt.Errorf("failed to initialize dynamic catalog adapter: %w", aErr)
		}
		proxyCfg.DynamicCatalog = adapter
	}

	p = proxy.NewProxy(proxyCfg, supervisor, pruneEngine)
	if notifier != nil {
		notifier.SetBroadcaster(p.BroadcastNotification)
	}
	p.Start()
	defer p.Shutdown()

	if pFlags.stats {
		defer func() {
			printSessionStats(os.Stderr, "wrapper", p.Stats())
		}()
	}

	if strings.EqualFold(cfg.Server.Transport, "sse") {
		sseCfg := cfg.ToSSEServerConfig()
		return runSSEDispatcher(ctx, sseCfg, p, logger)
	}
	return runStdioTransport(ctx, p, supervisor, logger)
}

// runStdioTransport connects os.Stdin / os.Stdout to single-upstream Proxy with JSON-RPC error recovery.
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

	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stdioTransport.ReadMessage()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, proxy.ErrTransportClosed) {
					errCh <- nil
					return
				}
				if rpcErrResp := proxy.ErrorToJSONRPCResponse(err); rpcErrResp != nil {
					_ = stdioTransport.WriteMessage(rpcErrResp)
					continue
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

// runSSEDispatcher starts the HTTP/SSE and Streamable HTTP (/mcp) server for any MessageDispatcher.
func runSSEDispatcher(ctx context.Context, sseCfg proxy.SSEServerConfig, dispatcher proxy.MessageDispatcher, logger *slog.Logger) error {
	sseServer := proxy.NewSSEServerWithDispatcher(sseCfg, dispatcher)
	if err := sseServer.Start(); err != nil {
		return fmt.Errorf("failed to start SSE/HTTP server: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = sseServer.Shutdown(shutdownCtx)
	}()

	boundAddr := fmt.Sprintf("%s:%d", sseCfg.Host, sseCfg.Port)
	if sseServer.Addr() != nil {
		boundAddr = sseServer.Addr().String()
	}
	logger.Info("ToolPrune HTTP/SSE and Streamable HTTP server ready",
		"sseEndpoint", fmt.Sprintf("http://%s%s/sse", boundAddr, sseCfg.BasePath),
		"mcpEndpoint", fmt.Sprintf("http://%s/mcp", boundAddr),
	)

	<-ctx.Done()
	logger.Info("shutting down SSE/HTTP server")
	return nil
}

// runMultiUpstreamMode starts multi-upstream multiplexer from config file.
func runMultiUpstreamMode(ctx context.Context, configPath string, logger *slog.Logger) error {
	return runMultiUpstreamModeWithFlags(ctx, configPath, nil, logger)
}

func runMultiUpstreamModeWithFlags(ctx context.Context, configPath string, changed map[string]bool, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load configuration %q: %w", configPath, err)
	}

	// Only override configuration file settings for CLI flags explicitly passed by the caller.
	if changed != nil {
		if changed["mode"] || changed["preset"] || changed["preset_resolved"] {
			cfg.Pruning.Tier = pFlags.mode
		}
		if changed["max-schema-tokens"] {
			cfg.Pruning.MaxSchemaTokens = pFlags.maxSchemaTokens
		}
		if changed["transport"] {
			cfg.Server.Transport = pFlags.transport
		}
		if changed["host"] {
			cfg.Server.Host = pFlags.host
		}
		if changed["port"] {
			cfg.Server.Port = pFlags.port
		}
		if changed["profile"] {
			cfg.Routing.DefaultProfile = pFlags.profile
		}
		if changed["timeout"] {
			cfg.Server.Timeout = config.Duration(pFlags.timeout)
		}
		if changed["namespace"] {
			cfg.Routing.NamespaceMode = pFlags.namespace
		}
		if err := config.Validate(cfg); err != nil {
			return fmt.Errorf("invalid CLI flag override: %w", err)
		}
	}

	logger.Info("loaded configuration",
		"path", configPath,
		"upstreams", len(cfg.Upstreams),
		"tier", cfg.Pruning.Tier,
		"maxSchemaTokens", cfg.Pruning.MaxSchemaTokens,
		"profile", cfg.Routing.DefaultProfile,
		"namespace", cfg.Routing.NamespaceMode,
		"transport", cfg.Server.Transport,
	)

	rCfg := cfg.ToRouterConfig()
	pruneOpts := cfg.ToPrunerOptions()
	pruneEngine := pruner.New(pruneOpts)
	r := router.NewRouter(rCfg, pruneEngine, logger)

	gw := router.NewRouterGateway(r, router.GatewayConfig{
		RequestTimeout:  time.Duration(cfg.Server.Timeout),
		InjectProxyInfo: cfg.Server.InjectProxyInfo,
		ServerVersion:   Version,
		Logger:          logger,
	})
	defer gw.Shutdown()

	filterCfg := cfg.ToFilterConfig()
	toolFilter, err := router.NewToolFilter(filterCfg)
	if err != nil {
		return fmt.Errorf("failed to initialize tool filter: %w", err)
	}
	r.AttachFilter(toolFilter)

	winCfg := cfg.ToWindowConfig()
	var toolWindow *router.ToolWindow
	if winCfg.Enabled {
		toolWindow, err = router.NewToolWindow(winCfg, r.Multiplexer(), gw.Notifier())
		if err != nil {
			return fmt.Errorf("failed to initialize active tool window: %w", err)
		}
		r.AttachWindow(toolWindow)
	}

	// Spawn/connect and handshake each enabled upstream server before starting Multiplexer reader loops
	for _, u := range cfg.Upstreams {
		if u.Disabled {
			continue
		}

		handshakeTimeout := time.Duration(u.Timeout)
		if handshakeTimeout <= 0 {
			handshakeTimeout = time.Duration(cfg.Server.Timeout)
		}
		if handshakeTimeout <= 0 {
			handshakeTimeout = 15 * time.Second
		}

		var upTransport proxy.UpstreamTransport
		if strings.EqualFold(u.Transport, "sse") {
			sseClient, cErr := proxy.NewSSEClientTransport(proxy.SSEClientConfig{
				Name:           u.ID,
				URL:            u.URL,
				Headers:        proxy.ExtractHTTPHeadersFromEnv(u.Env),
				ConnectTimeout: handshakeTimeout,
				RequestTimeout: time.Duration(cfg.Server.Timeout),
				Logger:         logger,
			})
			if cErr != nil {
				return fmt.Errorf("invalid SSE upstream %q: %w", u.ID, cErr)
			}
			if sErr := sseClient.Start(ctx); sErr != nil {
				return fmt.Errorf("failed to connect SSE upstream %q (%s): %w", u.ID, u.URL, sErr)
			}
			upTransport = sseClient
		} else {
			supCfg := u.ToSupervisorConfig(logger)
			sup := proxy.NewSupervisor(supCfg)
			if sErr := sup.Start(ctx); sErr != nil {
				return fmt.Errorf("failed to start upstream %q: %w", u.ID, sErr)
			}
			upTransport = sup
		}

		tools, hErr := performUpstreamHandshakeWithTimeout(ctx, upTransport, handshakeTimeout)
		if hErr != nil {
			logger.Warn("handshake with upstream failed, closing upstream and continuing",
				"upstream", u.ID,
				"err", hErr,
			)
			_ = upTransport.Close()
			continue
		}

		if regErr := r.Multiplexer().RegisterUpstream(ctx, router.UpstreamConfig{
			ID:          u.ID,
			Prefix:      u.Prefix,
			Description: u.Description,
			Transport:   upTransport,
		}); regErr != nil {
			_ = upTransport.Close()
			return fmt.Errorf("failed to register upstream %q: %w", u.ID, regErr)
		}
		r.Multiplexer().SyncUpstreamTools(ctx, u.ID, tools)
	}

	// Pre-seed up to MaxActive allowed tools into the window for non-dynamic tiers so initial tools/list is populated
	if toolWindow != nil && !rCfg.EnableCatalog {
		seeded := 0
		for _, t := range r.Multiplexer().ListAllTools() {
			if seeded >= winCfg.MaxActive {
				break
			}
			if toolFilter == nil || toolFilter.IsAllowed(t.Name) {
				if promoted, _, pErr := toolWindow.Promote(t.Name); pErr == nil && promoted {
					seeded++
				}
			}
		}
	}

	if err := gw.Start(); err != nil {
		return fmt.Errorf("failed to start router gateway: %w", err)
	}

	if pFlags.stats {
		defer func() {
			printSessionStats(os.Stderr, "router", r.Stats())
		}()
	}

	logger.Info("all upstreams registered and synchronized", "exposedTools", len(r.Multiplexer().ListAllTools()))

	if strings.EqualFold(cfg.Server.Transport, "sse") {
		sseCfg := cfg.ToSSEServerConfig()
		return runSSEDispatcher(ctx, sseCfg, gw, logger)
	}

	return runGatewayStdioLoop(ctx, gw, logger)
}

// performUpstreamHandshake performs standard MCP initialize + paginated tools/list exchange with a 15s default timeout.
func performUpstreamHandshake(transport proxy.UpstreamTransport) ([]mcp.Tool, error) {
	return performUpstreamHandshakeWithTimeout(context.Background(), transport, 15*time.Second)
}

// performUpstreamHandshakeWithTimeout performs MCP initialize + paginated tools/list while
// safely skipping interleaved startup notifications and enforcing a hard deadline.
func performUpstreamHandshakeWithTimeout(parentCtx context.Context, transport proxy.UpstreamTransport, timeout time.Duration) ([]mcp.Tool, error) {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	type handshakeResult struct {
		tools []mcp.Tool
		err   error
	}
	resCh := make(chan handshakeResult, 1)

	go func() {
		recvForID := func(expected mcp.RequestID) (*mcp.Message, error) {
			for {
				msg, err := transport.Receive()
				if err != nil {
					return nil, err
				}
				if msg == nil || msg.IsNotification() {
					// Ignore startup notifications (e.g. notifications/message, notifications/tools/list_changed)
					continue
				}
				if msg.IsResponse() && msg.ID != nil && msg.ID.Equal(expected) {
					if msg.Error != nil {
						return nil, msg.Error
					}
					return msg, nil
				}
			}
		}

		initID := mcp.NewIntID(1)
		initReq, err := mcp.NewRequest(initID, mcp.MethodInitialize, mcp.InitializeRequestParams{
			ProtocolVersion: mcp.LatestProtocolVersion,
			ClientInfo:      mcp.Implementation{Name: "ToolPrune", Version: Version},
		})
		if err != nil {
			resCh <- handshakeResult{err: err}
			return
		}
		if err := transport.Send(initReq); err != nil {
			resCh <- handshakeResult{err: err}
			return
		}
		if _, err := recvForID(initID); err != nil {
			resCh <- handshakeResult{err: fmt.Errorf("initialize failed: %w", err)}
			return
		}

		initNotif, _ := mcp.NewNotification(mcp.MethodNotificationsInitialized, nil)
		_ = transport.Send(initNotif)

		var allTools []mcp.Tool
		cursor := ""
		const maxPages = 100
		for page := 0; page < maxPages; page++ {
			reqID := mcp.NewIntID(int64(2 + page))
			var params any
			if cursor != "" {
				params = mcp.ListToolsRequestParams{Cursor: cursor}
			}
			listReq, lErr := mcp.NewRequest(reqID, mcp.MethodToolsList, params)
			if lErr != nil {
				resCh <- handshakeResult{err: lErr}
				return
			}
			if sErr := transport.Send(listReq); sErr != nil {
				resCh <- handshakeResult{err: sErr}
				return
			}
			listResp, rErr := recvForID(reqID)
			if rErr != nil {
				resCh <- handshakeResult{err: fmt.Errorf("tools/list page %d failed: %w", page+1, rErr)}
				return
			}
			var pageRes mcp.ListToolsResult
			if uErr := json.Unmarshal(listResp.Result, &pageRes); uErr != nil {
				resCh <- handshakeResult{err: fmt.Errorf("unmarshal tools/list result: %w", uErr)}
				return
			}
			allTools = append(allTools, pageRes.Tools...)
			if pageRes.NextCursor == "" || pageRes.NextCursor == cursor {
				break
			}
			cursor = pageRes.NextCursor
		}

		resCh <- handshakeResult{tools: allTools}
	}()

	select {
	case res := <-resCh:
		return res.tools, res.err
	case <-ctx.Done():
		_ = transport.Close()
		return nil, fmt.Errorf("upstream handshake timed out after %s: %w", timeout, ctx.Err())
	}
}

// runGatewayStdioLoop connects os.Stdin / os.Stdout to RouterGateway asynchronously with parse error recovery.
func runGatewayStdioLoop(ctx context.Context, gw *router.RouterGateway, logger *slog.Logger) error {
	stdioTransport := proxy.NewStdioTransport(proxy.StdioOptions{
		In:     os.Stdin,
		Out:    os.Stdout,
		Logger: logger,
	})
	defer func() { _ = stdioTransport.Close() }()

	session := proxy.NewStdioSession(stdioTransport)
	gw.RegisterClient(session)
	defer gw.UnregisterClient(session.ID())

	errCh := make(chan error, 1)
	go func() {
		for {
			msg, err := stdioTransport.ReadMessage()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, proxy.ErrTransportClosed) {
					errCh <- nil
					return
				}
				if rpcErrResp := proxy.ErrorToJSONRPCResponse(err); rpcErrResp != nil {
					_ = stdioTransport.WriteMessage(rpcErrResp)
					continue
				}
				errCh <- err
				return
			}
			if hErr := gw.HandleClientMessage(session, msg); hErr != nil {
				logger.Warn("router gateway error handling message", "err", hErr)
			}
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down multi-upstream stdio gateway")
		return nil
	case err := <-errCh:
		if err != nil {
			logger.Error("stdio gateway read failed", "err", err)
			return err
		}
		logger.Info("client disconnected from multi-upstream stdio gateway (EOF)")
		return nil
	}
}

func printSessionStats(w io.Writer, modeLabel string, stats proxy.SessionStatsSnapshot) {
	if w == nil {
		return
	}
	fmt.Fprintf(
		w,
		"[toolprune stats] mode=%s lists=%d calls=%d rejected=%d cache_hits=%d raw_tokens=%d pruned_tokens=%d saved=%d (%.1f%%)\n",
		modeLabel,
		stats.ListsIntercepted,
		stats.CallsIntercepted,
		stats.PreBindRejections,
		stats.CacheHits,
		stats.RawTokensTotal,
		stats.PrunedTokensTotal,
		stats.TokensSaved(),
		stats.SavingsPercent(),
	)
}
