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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

// SSEClientConfig configures an outbound HTTP/SSE or Streamable HTTP MCP client transport.
type SSEClientConfig struct {
	Name           string
	URL            string
	Headers        map[string]string
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

// SSEClientTransport implements UpstreamTransport over outbound HTTP+SSE or Streamable HTTP.
type SSEClientTransport struct {
	cfg       SSEClientConfig
	baseURL   *url.URL
	client    *http.Client
	sseClient *http.Client
	logger    *slog.Logger

	mu          sync.RWMutex
	postURL     string
	sessionID   string
	sseRespBody io.ReadCloser

	recvCh    chan *mcp.Message
	ctx       context.Context
	cancel    context.CancelFunc
	closed    atomic.Bool
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewSSEClientTransport creates a new outbound HTTP/SSE upstream transport.
func NewSSEClientTransport(cfg SSEClientConfig) (*SSEClientTransport, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	if rawURL == "" {
		return nil, errors.New("SSE upstream URL cannot be empty")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid SSE upstream URL %q: %w", rawURL, err)
	}
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 15 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 60 * time.Second
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &SSEClientTransport{
		cfg:     cfg,
		baseURL: parsed,
		postURL: rawURL,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
		},
		sseClient: &http.Client{
			// No overall Timeout on long-lived SSE GET stream
			Transport: &http.Transport{
				ResponseHeaderTimeout: cfg.ConnectTimeout,
			},
		},
		logger: logger,
		recvCh: make(chan *mcp.Message, 256),
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// ExtractHTTPHeadersFromEnv extracts optional HTTP headers from upstream env configuration
// (such as AUTHORIZATION, BEARER_TOKEN, API_KEY, or HTTP_HEADER_<NAME>).
func ExtractHTTPHeadersFromEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	headers := make(map[string]string)
	for k, v := range env {
		if strings.TrimSpace(v) == "" {
			continue
		}
		upper := strings.ToUpper(strings.TrimSpace(k))
		switch {
		case upper == "AUTHORIZATION":
			headers["Authorization"] = v
		case upper == "BEARER_TOKEN":
			if strings.HasPrefix(strings.ToLower(v), "bearer ") {
				headers["Authorization"] = v
			} else {
				headers["Authorization"] = "Bearer " + v
			}
		case upper == "X_API_KEY" || upper == "API_KEY":
			headers["X-API-Key"] = v
		case strings.HasPrefix(upper, "HTTP_HEADER_"):
			rawName := strings.TrimPrefix(k, k[:len("HTTP_HEADER_")])
			headerName := strings.ReplaceAll(rawName, "_", "-")
			if headerName != "" {
				headers[headerName] = v
			}
		}
	}
	return headers
}

// Start establishes the SSE event stream if supported by the remote endpoint, or falls back
// cleanly to stateless/session Streamable HTTP POST mode if GET returns 405 Method Not Allowed.
func (t *SSEClientTransport) Start(parentCtx context.Context) error {
	reqCtx, cancel := context.WithCancel(t.ctx)
	go func() {
		select {
		case <-parentCtx.Done():
			cancel()
		case <-t.ctx.Done():
			cancel()
		}
	}()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, t.baseURL.String(), nil)
	if err != nil {
		cancel()
		return fmt.Errorf("create SSE GET request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	t.applyCustomHeaders(req)

	resp, err := t.sseClient.Do(req)
	if err != nil {
		cancel()
		return fmt.Errorf("connect SSE upstream %q: %w", t.baseURL.String(), err)
	}

	// Streamable HTTP servers that only accept POST /mcp return 405 on GET before session initialization.
	if resp.StatusCode == http.StatusMethodNotAllowed {
		_ = resp.Body.Close()
		cancel()
		t.logger.Info("remote upstream uses POST-first Streamable HTTP", "upstream", t.cfg.Name, "url", t.baseURL.String())
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		bodySnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return fmt.Errorf("SSE upstream %q returned HTTP %d: %s", t.baseURL.String(), resp.StatusCode, strings.TrimSpace(string(bodySnippet)))
	}

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	t.mu.Lock()
	t.sseRespBody = resp.Body
	t.mu.Unlock()

	endpointReady := make(chan struct{})
	var readyOnce sync.Once
	markReady := func() {
		readyOnce.Do(func() {
			close(endpointReady)
		})
	}

	// If Mcp-Session-Id was already returned on GET /mcp, endpoint is baseURL immediately.
	if strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")) != "" {
		markReady()
	}

	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer cancel()
		t.readSSEStream(resp.Body, markReady)
	}()

	// For classic /sse endpoints, wait briefly for the initial `event: endpoint` frame.
	if strings.HasSuffix(strings.ToLower(t.baseURL.Path), "/sse") {
		select {
		case <-endpointReady:
		case <-time.After(t.cfg.ConnectTimeout):
			_ = t.Close()
			return fmt.Errorf("timed out waiting for SSE endpoint event from %q", t.baseURL.String())
		case <-parentCtx.Done():
			_ = t.Close()
			return parentCtx.Err()
		}
	}

	return nil
}

func (t *SSEClientTransport) readSSEStream(body io.ReadCloser, markReady func()) {
	defer func() {
		_ = body.Close()
		t.closeRecvChannel()
	}()

	reader := bufio.NewReaderSize(body, 64*1024)
	var eventType string
	var dataLines []string

	dispatchEvent := func() {
		if len(dataLines) == 0 {
			eventType = ""
			return
		}
		payload := strings.Join(dataLines, "\n")
		ev := eventType
		if ev == "" {
			ev = "message"
		}
		eventType = ""
		dataLines = dataLines[:0]

		switch ev {
		case "endpoint":
			resolved := t.resolveEndpointURI(strings.TrimSpace(payload))
			t.mu.Lock()
			t.postURL = resolved
			t.mu.Unlock()
			markReady()

		case "message":
			markReady()
			trimmed := strings.TrimSpace(payload)
			if trimmed == "" {
				return
			}
			var msg mcp.Message
			if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
				t.logger.Warn("failed to unmarshal SSE upstream message", "upstream", t.cfg.Name, "err", err)
				return
			}
			t.enqueueMessage(&msg)
		}
	}

	for {
		line, err := readBoundedSSELine(reader, DefaultMaxMessageSize)
		if errors.Is(err, mcp.ErrMessageTooLarge) {
			t.logger.Warn("SSE upstream event line exceeded max size", "upstream", t.cfg.Name)
			continue
		}
		if err == nil || len(line) > 0 {
			if line == "" {
				dispatchEvent()
			} else if strings.HasPrefix(line, ":") {
				// SSE comment / keepalive ping
			} else if strings.HasPrefix(line, "event:") {
				eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data := strings.TrimPrefix(line, "data:")
				data = strings.TrimPrefix(data, " ")
				dataLines = append(dataLines, data)
			}
		}

		if err != nil {
			if len(dataLines) > 0 {
				dispatchEvent()
			}
			return
		}
	}
}

func (t *SSEClientTransport) resolveEndpointURI(rawEndpoint string) string {
	ref, err := url.Parse(rawEndpoint)
	if err != nil {
		return rawEndpoint
	}
	return t.baseURL.ResolveReference(ref).String()
}

func (t *SSEClientTransport) applyCustomHeaders(req *http.Request) {
	for k, v := range t.cfg.Headers {
		req.Header.Set(k, v)
	}
}

func (t *SSEClientTransport) enqueueMessage(msg *mcp.Message) {
	if msg == nil || t.closed.Load() {
		return
	}
	select {
	case t.recvCh <- msg:
	case <-t.ctx.Done():
	}
}

func (t *SSEClientTransport) closeRecvChannel() {
	t.closeOnce.Do(func() {
		t.closed.Store(true)
		close(t.recvCh)
	})
}

// Send posts a JSON-RPC message to the remote MCP endpoint and captures any inline Streamable HTTP response.
func (t *SSEClientTransport) Send(msg *mcp.Message) error {
	if t.closed.Load() {
		return io.ErrClosedPipe
	}

	payload, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal outbound SSE message: %w", err)
	}

	t.mu.RLock()
	postURL := t.postURL
	sessionID := t.sessionID
	t.mu.RUnlock()

	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, postURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("create outbound POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	t.applyCustomHeaders(req)

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("POST to SSE upstream %q failed: %w", postURL, err)
	}
	defer resp.Body.Close()

	if sid := strings.TrimSpace(resp.Header.Get("Mcp-Session-Id")); sid != "" {
		t.mu.Lock()
		t.sessionID = sid
		t.mu.Unlock()
	}

	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("SSE upstream %q returned HTTP %d: %s", postURL, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		t.parseInlineSSEResponse(resp.Body)
		return nil
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, int64(DefaultMaxMessageSize)))
	if err != nil {
		return fmt.Errorf("read Streamable HTTP response: %w", err)
	}
	trimmed := bytes.TrimSpace(bodyBytes)
	if len(trimmed) == 0 {
		return nil
	}

	var respMsg mcp.Message
	if err := json.Unmarshal(trimmed, &respMsg); err != nil {
		return fmt.Errorf("unmarshal Streamable HTTP response: %w", err)
	}
	t.enqueueMessage(&respMsg)
	return nil
}

func (t *SSEClientTransport) parseInlineSSEResponse(r io.Reader) {
	scanner := bufio.NewReaderSize(r, 64*1024)
	var dataLines []string
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		raw := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if raw == "" {
			return
		}
		var msg mcp.Message
		if json.Unmarshal([]byte(raw), &msg) == nil {
			t.enqueueMessage(&msg)
		}
	}
	for {
		line, err := readBoundedSSELine(scanner, DefaultMaxMessageSize)
		if err == nil || len(line) > 0 {
			if line == "" {
				flush()
			} else if strings.HasPrefix(line, "data:") {
				d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
				dataLines = append(dataLines, d)
			}
		}
		if err != nil {
			flush()
			return
		}
	}
}

// readBoundedSSELine reads a single newline-delimited SSE line (preserving empty delimiter lines)
// while enforcing maxBytes before heap allocation.
func readBoundedSSELine(r *bufio.Reader, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxMessageSize
	}
	rawCap := maxBytes + 64
	frag, err := r.ReadSlice('\n')
	if err == nil {
		trimmed := strings.TrimRight(string(frag), "\r\n")
		if len(trimmed) > maxBytes {
			return "", mcp.ErrMessageTooLarge
		}
		return trimmed, nil
	}
	if !errors.Is(err, bufio.ErrBufferFull) {
		trimmed := strings.TrimRight(string(frag), "\r\n")
		if len(trimmed) > maxBytes {
			return "", mcp.ErrMessageTooLarge
		}
		return trimmed, err
	}

	if len(frag) > rawCap {
		for {
			_, dErr := r.ReadSlice('\n')
			if !errors.Is(dErr, bufio.ErrBufferFull) {
				break
			}
		}
		return "", mcp.ErrMessageTooLarge
	}

	acc := make([]byte, 0, rawCap)
	acc = append(acc, frag...)
	for {
		part, partErr := r.ReadSlice('\n')
		if len(acc)+len(part) > rawCap {
			if errors.Is(partErr, bufio.ErrBufferFull) {
				for {
					_, dErr := r.ReadSlice('\n')
					if !errors.Is(dErr, bufio.ErrBufferFull) {
						break
					}
				}
			}
			return "", mcp.ErrMessageTooLarge
		}
		acc = append(acc, part...)
		if partErr == nil {
			break
		}
		if errors.Is(partErr, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(partErr, io.EOF) {
			break
		}
		return "", partErr
	}
	trimmed := strings.TrimRight(string(acc), "\r\n")
	if len(trimmed) > maxBytes {
		return "", mcp.ErrMessageTooLarge
	}
	return trimmed, nil
}

// Receive blocks until the next JSON-RPC message arrives from the remote server or the transport closes.
func (t *SSEClientTransport) Receive() (*mcp.Message, error) {
	msg, ok := <-t.recvCh
	if !ok {
		return nil, io.EOF
	}
	return msg, nil
}

// Close terminates the remote SSE connection and releases background goroutines.
func (t *SSEClientTransport) Close() error {
	t.cancel()
	t.mu.Lock()
	body := t.sseRespBody
	t.sseRespBody = nil
	t.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
	t.closeRecvChannel()
	t.wg.Wait()
	return nil
}
