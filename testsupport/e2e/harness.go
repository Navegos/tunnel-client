package e2e

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"go.uber.org/fx/fxtest"

	"go.openai.org/api/tunnel-client/pkg/app"
	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/types"
	"go.openai.org/api/tunnel-client/testsupport/mockmcpserver"
	"go.openai.org/api/tunnel-client/testsupport/mocktunnelservice"
)

type harnessConfig struct {
	apiKey              string
	tunnelID            types.TunnelID
	controlPlaneOptions []mocktunnelservice.Option
	mcpOptions          []mockmcpserver.Option
	clientCustomizer    func(*config.Config)
	scenarioTimeout     time.Duration
	logWriter           io.Writer
	mcpTransportKind    config.MCPTransportKind
}

// HarnessOption customizes the E2E harness configuration.
type HarnessOption func(*harnessConfig)

// WithAPIKey overrides the API key used between the client and mock control plane.
func WithAPIKey(key string) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.apiKey = key
	}
}

// WithTunnelID overrides the tunnel identifier advertised to the client.
func WithTunnelID(id types.TunnelID) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.tunnelID = id
	}
}

// WithControlPlaneOptions forwards additional options to the mock control plane.
func WithControlPlaneOptions(opts ...mocktunnelservice.Option) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.controlPlaneOptions = append(cfg.controlPlaneOptions, opts...)
	}
}

// WithMCPOptions forwards additional options to the mock MCP server.
func WithMCPOptions(opts ...mockmcpserver.Option) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.mcpOptions = append(cfg.mcpOptions, opts...)
	}
}

// WithClientConfig allows tests to customize the derived tunnel-client config.
func WithClientConfig(fn func(*config.Config)) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.clientCustomizer = fn
	}
}

// WithScenarioTimeout overrides the time ExecuteScenarious waits for the
// scripted tunnel commands to drain before failing the test.
func WithScenarioTimeout(timeout time.Duration) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.scenarioTimeout = timeout
	}
}

// WithLogWriter overrides the writer used by the tunnel-client logging module.
// The harness always tees writes into an internal buffer so logs can be dumped
// when ExecuteScenarious encounters an error.
func WithLogWriter(w io.Writer) HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.logWriter = w
	}
}

// WithInMemoryMCPTransport uses the in-memory MCP transport for this harness.
func WithInMemoryMCPTransport() HarnessOption {
	return func(cfg *harnessConfig) {
		cfg.mcpTransportKind = config.MCPTransportInMemory
	}
}

// Harness wires together the mock control plane, mock MCP server, and a running tunnel-client.
type Harness struct {
	ControlPlane  *mocktunnelservice.MockTunnelService
	MCP           *mockmcpserver.MockMCPServer
	cfg           *config.Config
	app           *fxtest.App
	waitTimeout   time.Duration
	tunnelStarted bool
	mcpStarted    bool
	inMemoryMCP   *mcp.InMemoryTransport
	stdioMCP      *mcp.IOTransport
	logWriter     io.Writer
	logBuffer     *bytes.Buffer
}

// NewHarness configures the mocks and client wiring using the provided options.
func NewHarness(t testing.TB, opts ...HarnessOption) *Harness {
	t.Helper()

	cfg := harnessConfig{
		apiKey:           "test-api-key",
		tunnelID:         types.TunnelID("tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		scenarioTimeout:  4 * time.Second,
		mcpTransportKind: config.MCPTransportHTTPStreamable,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	controlPlaneOpts := append([]mocktunnelservice.Option{},
		mocktunnelservice.WithAPIKey(cfg.apiKey),
		mocktunnelservice.WithTunnelID(string(cfg.tunnelID)),
	)
	controlPlaneOpts = append(controlPlaneOpts, cfg.controlPlaneOptions...)
	controlPlane := mocktunnelservice.NewMockTunnelService(controlPlaneOpts...)

	mcpServer := mockmcpserver.NewMockMCPServer(cfg.mcpOptions...)

	clientCfg := &config.Config{
		ControlPlane: config.ControlPlaneConfig{
			BaseURL:             nil,
			TunnelID:            cfg.tunnelID,
			APIKey:              cfg.apiKey,
			MaxInFlightRequests: 10,
			PollTimeout:         100 * time.Millisecond,
			PollBackoffMin:      50 * time.Millisecond,
			PollBackoffMax:      300 * time.Millisecond,
		},
		Logging: config.LoggingConfig{
			Level:  slog.LevelError,
			Format: config.LogFormatStructText,
		},
		Health: config.HealthConfig{
			ListenAddr: "127.0.0.1:0",
		},
		MCP: config.MCPConfig{
			ServerURL:             nil,
			TransportKind:         cfg.mcpTransportKind,
			ConnectionMaxTTL:      time.Minute,
			MaxConcurrentRequests: 1,
		},
	}
	if cfg.clientCustomizer != nil {
		cfg.clientCustomizer(clientCfg)
	}

	var logBuf bytes.Buffer
	var logWriter io.Writer = &logBuf
	if cfg.logWriter != nil {
		logWriter = io.MultiWriter(cfg.logWriter, &logBuf)
	}

	return &Harness{
		ControlPlane: controlPlane,
		MCP:          mcpServer,
		cfg:          clientCfg,
		waitTimeout:  cfg.scenarioTimeout,
		logWriter:    logWriter,
		logBuffer:    &logBuf,
	}
}

// ExecuteScenarious orchestrates the tunnel lifecycle, waits for the control
// plane queue to drain, and then shuts everything down before returning.
func (h *Harness) ExecuteScenarious(t testing.TB) {
	t.Helper()
	if h.ControlPlane == nil || h.MCP == nil || h.cfg == nil {
		t.Fatalf("harness not initialized")
	}
	defer h.shutdown(t)
	h.startControlPlane(t)
	h.startMCPServer(t)
	h.startClient(t)
	timeout := h.waitTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := h.ControlPlane.WaitUntilIdle(ctx); err != nil {
		h.dumpFailureState(t, err)
		t.Fatalf("scenario did not complete before timeout: %v", err)
	}
}

func (h *Harness) dumpFailureState(t testing.TB, cause error) {
	if t == nil {
		return
	}
	t.Helper()
	if cause != nil {
		t.Logf("scenario error: %v", cause)
	}
	if h.ControlPlane != nil {
		pending, awaiting := h.ControlPlane.PendingScriptDebugInfo()
		for i, cmd := range pending {
			t.Logf("pending command[%d]: %s", i, string(cmd))
		}
		for i, cmd := range awaiting {
			t.Logf("awaiting response[%d]: %s", i, string(cmd))
		}
	}
	if h.logBuffer != nil {
		if h.logBuffer.Len() == 0 {
			t.Log("tunnel-client logs: <empty>")
		} else {
			t.Logf("tunnel-client logs:\n%s", h.logBuffer.String())
		}
	}
}

func (h *Harness) startControlPlane(t testing.TB) {
	t.Helper()
	if h.tunnelStarted {
		return
	}
	h.ControlPlane.Start(t)
	if h.ControlPlane.BaseURL() == nil {
		t.Fatalf("mock control plane failed to expose a base URL")
	}
	h.tunnelStarted = true
}

func (h *Harness) startMCPServer(t testing.TB) {
	t.Helper()
	if h.mcpStarted {
		return
	}
	switch h.cfg.MCP.TransportKind {
	case config.MCPTransportInMemory:
		h.inMemoryMCP = h.MCP.StartInMemory(t)
	case config.MCPTransportStdio:
		h.stdioMCP = h.MCP.StartStdio(t)
	default:
		h.MCP.Start(t)
		if h.MCP.BaseURL() == nil {
			t.Fatalf("mock MCP server failed to expose a base URL")
		}
	}
	h.mcpStarted = true
}

func (h *Harness) startClient(t testing.TB) {
	t.Helper()
	if h.app != nil {
		t.Fatalf("tunnel-client already running")
		return
	}
	cfg := h.cloneConfig()
	if cfg == nil {
		t.Fatalf("missing tunnel-client configuration")
		return
	}
	ctrlURL := h.ControlPlane.BaseURL()
	if ctrlURL == nil {
		t.Fatalf("control plane must be started before the client")
		return
	}
	cfg.ControlPlane.BaseURL = ctrlURL
	transportKind := cfg.MCP.TransportKind
	if transportKind == "" {
		transportKind = config.MCPTransportHTTPStreamable
	}
	switch transportKind {
	case config.MCPTransportHTTPStreamable:
		mcpURL := h.MCP.BaseURL()
		if mcpURL == nil {
			t.Fatalf("mock MCP server must be started before the client")
			return
		}
		cfg.MCP.ServerURL = mcpURL
		if err := cfg.MCP.BootstrapOAuthResourceMetadataURLs(); err != nil {
			t.Fatalf("bootstrap MCP OAuth metadata URLs: %v", err)
		}
	case config.MCPTransportInMemory, config.MCPTransportStdio:
		if transportKind == config.MCPTransportInMemory && h.inMemoryMCP == nil {
			t.Fatalf("mock MCP in-memory transport must be started before the client")
			return
		}
		if transportKind == config.MCPTransportStdio && h.stdioMCP == nil {
			t.Fatalf("mock MCP stdio transport must be started before the client")
			return
		}
	default:
		t.Fatalf("unsupported MCP transport kind: %s", transportKind)
		return
	}
	logWriter := h.logWriter
	if logWriter == nil {
		logWriter = io.Discard
	}
	options := []fx.Option{
		fx.Provide(func() io.Writer { return logWriter }),
		fx.WithLogger(func(*slog.Logger) fxevent.Logger { return fxevent.NopLogger }),
	}
	if h.inMemoryMCP != nil {
		options = append(options, fx.Supply(h.inMemoryMCP))
	}
	if h.stdioMCP != nil {
		options = append(options, fx.Supply(h.stdioMCP))
	}
	app := fxtest.New(t, app.Options(cfg, options...)...)
	app.RequireStart()
	h.app = app
}

func (h *Harness) shutdown(t testing.TB) {
	t.Helper()
	if h.app != nil {
		h.app.RequireStop()
		h.app = nil
	}
	if h.MCP != nil && h.mcpStarted {
		h.MCP.Close()
		h.mcpStarted = false
	}
	if h.ControlPlane != nil && h.tunnelStarted {
		h.ControlPlane.Close()
		h.tunnelStarted = false
	}
}

func (h *Harness) cloneConfig() *config.Config {
	if h.cfg == nil {
		return nil
	}
	clone := *h.cfg
	clone.ControlPlane = h.cfg.ControlPlane
	if h.cfg.ControlPlane.ExtraHeaders != nil {
		clone.ControlPlane.ExtraHeaders = make(map[string]string, len(h.cfg.ControlPlane.ExtraHeaders))
		for k, v := range h.cfg.ControlPlane.ExtraHeaders {
			clone.ControlPlane.ExtraHeaders[k] = v
		}
	}
	clone.Logging = h.cfg.Logging
	clone.Health = h.cfg.Health
	clone.Process = h.cfg.Process
	clone.MCP = h.cfg.MCP
	return &clone
}
