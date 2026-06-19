package localproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/app"
	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/controlplane/wiretypes"
	"go.openai.org/api/tunnel-client/pkg/health"
	"go.openai.org/api/tunnel-client/pkg/mcpclient"
	"go.openai.org/api/tunnel-client/pkg/types"
)

// BackendName selects the local proxy engine used by Start.
type BackendName string

const (
	BackendAuto BackendName = "auto"
	BackendRust BackendName = "rust"
	BackendGo   BackendName = "go"
)

const (
	DefaultListenAddr                = "127.0.0.1:0"
	DefaultHealthListenAddr          = ""
	DefaultEnabledHealthListenAddr   = "127.0.0.1:0"
	DefaultTunnelID                  = "tunnel_22222222222222222222222222222222"
	DefaultBackend                   = BackendAuto
	DefaultReadinessTimeout          = 10 * time.Second
	DefaultResponseTimeout           = 30 * time.Second
	DefaultClientLastSeenTimeout     = 20 * time.Second
	DefaultControlPlanePollTimeout   = 500 * time.Millisecond
	DefaultControlPlanePollGuardrail = 100 * time.Millisecond
	localControlPlaneAPIKeyEnv       = "CONTROL_PLANE_API_KEY"
	localControlPlaneAPIKey          = "local-tunnel-client-dev-proxy"
	tunnelClientPollBackoffMin       = 10 * time.Millisecond
	tunnelClientPollBackoffMax       = 50 * time.Millisecond
	defaultClientShutdownGracePeriod = 5 * time.Second
	defaultPollProbeDelay            = 25 * time.Millisecond
)

var blockedRequestMCPHeaders = map[string]struct{}{
	"accept-encoding":                   {},
	"cf-connecting-ip":                  {},
	"connection":                        {},
	"cookie":                            {},
	"content-length":                    {},
	"content-type":                      {},
	"forwarded":                         {},
	"host":                              {},
	"proxy-authorization":               {},
	"proxy-connection":                  {},
	"transfer-encoding":                 {},
	"x-custom-cf-witness-actor":         {},
	"x-custom-cf-witness-authorization": {},
	"x-forwarded-for":                   {},
	"x-openai-actor-authorization":      {},
	"x-openai-authorization":            {},
	"x-openai-authorization-error":      {},
	"x-openai-internal-caller":          {},
	"x-openai-skip-auth":                {},
	"x-original-forwarded-for":          {},
	"x-real-ip":                         {},
	"x-tunnel-traffic-source":           {},
	"user-agent":                        {},
}

// Options configures a local control plane plus in-process tunnel-client runtime.
type Options struct {
	ListenAddr    string
	TunnelID      types.TunnelID
	MCPServerURLs []string
	MCPCommands   []string
	Profile       string
	ProfileFile   string
	ProfileDir    string
	// HealthListenAddr optionally starts the embedded tunnel-client health/admin
	// listener. Empty means no health/admin listener.
	HealthListenAddr string
	// HealthURLFile writes the embedded tunnel-client health/admin base URL. If
	// set without HealthListenAddr, Start uses an ephemeral loopback listener.
	HealthURLFile string
	URLFile       string
	// Backend selects the local proxy backend. Empty defaults to auto.
	Backend BackendName
	// BackendFactories registers optional per-call backends linked into this
	// binary. Process-wide linked backends can also be registered with
	// RegisterBackendFactory.
	BackendFactories []BackendFactory
	// ResponseTimeout bounds how long local MCP ingress waits for a tunnel-client response.
	ResponseTimeout time.Duration
	// ClientLastSeenTimeout is used by readiness and local diagnostics.
	ClientLastSeenTimeout   time.Duration
	ReadinessTimeout        time.Duration
	ControlPlanePollTimeout time.Duration
	PollDeadlineGuardrail   time.Duration
	LookupEnv               func(string) (string, bool)
	Stdout                  io.Writer
	Stderr                  io.Writer
}

// Info is printed by the CLI and consumed by integration tests.
type Info struct {
	TunnelID               string `json:"tunnel_id"`
	MCPURL                 string `json:"mcp_url"`
	ControlPlaneBaseURL    string `json:"control_plane_base_url"`
	ControlPlaneTransport  string `json:"control_plane_transport"`
	ControlPlaneUnixSocket string `json:"control_plane_unix_socket,omitempty"`
	HealthURL              string `json:"health_url,omitempty"`
	Backend                string `json:"backend"`
}

// Proxy owns the local control plane and in-process tunnel-client.
type Proxy struct {
	info                Info
	clientApp           *fx.App
	backend             Backend
	cleanupControlPlane func()
	stopOnce            sync.Once
}

// BackendOptions configures a local proxy backend.
type BackendOptions struct {
	ListenAddr             string
	ControlPlaneUnixSocket string
	TunnelID               types.TunnelID
	APIKey                 string
	ResponseTimeout        time.Duration
	ClientLastSeenTimeout  time.Duration
	ReadinessTimeout       time.Duration
	Stderr                 io.Writer
}

// BackendFactory starts a linked local proxy backend.
type BackendFactory interface {
	Name() BackendName
	StartBackend(context.Context, BackendOptions) (Backend, error)
}

var registeredBackendFactories struct {
	sync.RWMutex
	factories []BackendFactory
}

// RegisterBackendFactory registers an optional local proxy backend linked into
// the current binary.
func RegisterBackendFactory(factory BackendFactory) {
	if factory == nil {
		return
	}
	registeredBackendFactories.Lock()
	defer registeredBackendFactories.Unlock()
	registeredBackendFactories.factories = append(registeredBackendFactories.factories, factory)
}

// Backend is the local control plane used by the tunnel-client runtime.
type Backend interface {
	Name() BackendName
	InfoBackend() string
	IngressBaseURL() *url.URL
	ControlPlaneBaseURL() *url.URL
	ControlPlaneUnixSocket() string
	WaitForTunnelClient(context.Context, time.Duration, types.TunnelID) error
	Wait(context.Context) error
	Stop(context.Context) error
}

// Start starts the selected local control plane and tunnel-client runtime.
func Start(ctx context.Context, opts Options) (*Proxy, error) {
	if ctx == nil {
		return nil, errors.New("local proxy context is nil")
	}
	opts = applyDefaults(opts)
	if err := validateOptions(opts); err != nil {
		return nil, err
	}

	backendFactory, err := resolveBackendFactory(opts.Backend, linkedBackendFactories(opts.BackendFactories))
	if err != nil {
		return nil, err
	}

	controlPlaneUnixSocket, cleanupControlPlane := prepareControlPlaneUnixSocket()
	backend, err := backendFactory.StartBackend(ctx, BackendOptions{
		ListenAddr:             opts.ListenAddr,
		ControlPlaneUnixSocket: controlPlaneUnixSocket,
		TunnelID:               opts.TunnelID,
		APIKey:                 localControlPlaneAPIKey,
		ResponseTimeout:        opts.ResponseTimeout,
		ClientLastSeenTimeout:  opts.ClientLastSeenTimeout,
		ReadinessTimeout:       opts.ReadinessTimeout,
		Stderr:                 opts.Stderr,
	})
	if err != nil && controlPlaneUnixSocket != "" {
		if cleanupControlPlane != nil {
			cleanupControlPlane()
		}
		cleanupControlPlane = nil
		_, _ = fmt.Fprintf(opts.Stderr, "warning: local proxy %s backend unix control-plane listener unavailable; falling back to TCP: %v\n", backendFactory.Name(), err)
		backend, err = backendFactory.StartBackend(ctx, BackendOptions{
			ListenAddr:            opts.ListenAddr,
			TunnelID:              opts.TunnelID,
			APIKey:                localControlPlaneAPIKey,
			ResponseTimeout:       opts.ResponseTimeout,
			ClientLastSeenTimeout: opts.ClientLastSeenTimeout,
			ReadinessTimeout:      opts.ReadinessTimeout,
			Stderr:                opts.Stderr,
		})
	}
	if err != nil {
		if cleanupControlPlane != nil {
			cleanupControlPlane()
		}
		return nil, err
	}

	proxy := &Proxy{
		backend:             backend,
		cleanupControlPlane: cleanupControlPlane,
	}
	defer func() {
		if err != nil {
			_ = proxy.Stop(context.Background())
		}
	}()

	cfg, err := buildClientConfig(opts, backend.ControlPlaneBaseURL(), backend.ControlPlaneUnixSocket())
	if err != nil {
		return nil, err
	}

	var probeState *mcpclient.ProbeState
	var healthService health.Service
	healthEnabled := opts.HealthListenAddr != ""
	clientApp := app.NewWithRuntime(
		cfg,
		app.RuntimeOptions{DisableHealthAdmin: !healthEnabled},
		fx.Provide(func() io.Writer { return opts.Stderr }),
		fx.Populate(&probeState, &healthService),
	)
	startCtx, cancel := context.WithTimeout(ctx, opts.ReadinessTimeout)
	defer cancel()
	if err = clientApp.Start(startCtx); err != nil {
		return nil, fmt.Errorf("start tunnel-client runtime: %w", err)
	}
	proxy.clientApp = clientApp

	if err = waitForMCPProbe(ctx, opts.ReadinessTimeout, probeState); err != nil {
		return nil, err
	}
	if err = backend.WaitForTunnelClient(ctx, opts.ReadinessTimeout, opts.TunnelID); err != nil {
		return nil, err
	}

	healthURL := ""
	if healthEnabled && healthService != nil {
		if addr, addrErr := healthService.Addr(opts.ReadinessTimeout); addrErr == nil {
			healthURL = "http://" + addr + "/readyz"
		}
	}
	controlPlaneTransport := "tcp"
	if backend.ControlPlaneUnixSocket() != "" {
		controlPlaneTransport = "unix"
	}
	proxy.info = Info{
		TunnelID:               opts.TunnelID.String(),
		MCPURL:                 backend.IngressBaseURL().JoinPath("v1", "mcp", opts.TunnelID.String()).String(),
		ControlPlaneBaseURL:    backend.ControlPlaneBaseURL().String(),
		ControlPlaneTransport:  controlPlaneTransport,
		ControlPlaneUnixSocket: backend.ControlPlaneUnixSocket(),
		HealthURL:              healthURL,
		Backend:                backend.InfoBackend(),
	}
	if opts.URLFile != "" {
		if err = writeInfoFile(opts.URLFile, proxy.info); err != nil {
			return nil, err
		}
	}
	return proxy, nil
}

// Info returns the local proxy connection details.
func (p *Proxy) Info() Info {
	if p == nil {
		return Info{}
	}
	return p.info
}

// Stop shuts down the tunnel-client runtime and local control plane.
func (p *Proxy) Stop(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var stopErr error
	p.stopOnce.Do(func() {
		if p.clientApp != nil {
			clientCtx, cancel := context.WithTimeout(ctx, defaultClientShutdownGracePeriod)
			defer cancel()
			if err := p.clientApp.Stop(clientCtx); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("stop tunnel-client runtime: %w", err))
			}
		}
		if p.backend != nil {
			if err := p.backend.Stop(ctx); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("stop local control plane: %w", err))
			}
		}
		if p.cleanupControlPlane != nil {
			p.cleanupControlPlane()
		}
	})
	return stopErr
}

// Wait blocks until the context is canceled or the local control plane exits unexpectedly.
func (p *Proxy) Wait(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if p.backend == nil {
		<-ctx.Done()
		return p.Stop(context.Background())
	}
	err := p.backend.Wait(ctx)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return p.Stop(context.Background())
	}
	return err
}

func applyDefaults(opts Options) Options {
	if opts.ListenAddr == "" {
		opts.ListenAddr = DefaultListenAddr
	}
	if opts.Backend == "" {
		opts.Backend = DefaultBackend
	}
	if opts.TunnelID == "" {
		opts.TunnelID = DefaultTunnelID
	}
	if opts.HealthListenAddr == "" && opts.HealthURLFile != "" {
		opts.HealthListenAddr = DefaultEnabledHealthListenAddr
	}
	if opts.ResponseTimeout <= 0 {
		opts.ResponseTimeout = DefaultResponseTimeout
	}
	if opts.ClientLastSeenTimeout <= 0 {
		opts.ClientLastSeenTimeout = DefaultClientLastSeenTimeout
	}
	if opts.ReadinessTimeout <= 0 {
		opts.ReadinessTimeout = DefaultReadinessTimeout
	}
	if opts.ControlPlanePollTimeout <= 0 {
		opts.ControlPlanePollTimeout = DefaultControlPlanePollTimeout
	}
	if opts.PollDeadlineGuardrail <= 0 {
		opts.PollDeadlineGuardrail = DefaultControlPlanePollGuardrail
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	return opts
}

func validateOptions(opts Options) error {
	if err := config.ValidateTunnelID(opts.TunnelID.String()); err != nil {
		return err
	}
	hasDirectMCP := len(opts.MCPServerURLs) > 0 || len(opts.MCPCommands) > 0
	hasProfile := opts.Profile != "" || opts.ProfileFile != ""
	if !hasDirectMCP && !hasProfile {
		return errors.New("set --mcp-server-url, --mcp-command, --profile, or --profile-file")
	}
	if opts.Profile != "" && opts.ProfileFile != "" {
		return errors.New("--profile and --profile-file are mutually exclusive")
	}
	if opts.ResponseTimeout <= 0 {
		return errors.New("response timeout must be positive")
	}
	if opts.ClientLastSeenTimeout <= 0 {
		return errors.New("client last-seen timeout must be positive")
	}
	if opts.ReadinessTimeout <= 0 {
		return errors.New("readiness timeout must be positive")
	}
	return nil
}

func resolveBackendFactory(backend BackendName, factories []BackendFactory) (BackendFactory, error) {
	allFactories := append([]BackendFactory{goBackendFactory{}}, factories...)
	switch backend {
	case BackendAuto:
		if factory := findBackendFactory(allFactories, BackendRust); factory != nil {
			return factory, nil
		}
		if factory := findBackendFactory(allFactories, BackendGo); factory != nil {
			return factory, nil
		}
		return nil, errors.New("no local proxy backend is registered")
	case BackendGo:
		if factory := findBackendFactory(allFactories, BackendGo); factory != nil {
			return factory, nil
		}
		return nil, errors.New("go local proxy backend is unavailable in this build")
	case BackendRust:
		if factory := findBackendFactory(allFactories, BackendRust); factory != nil {
			return factory, nil
		}
		return nil, errors.New("rust local proxy backend is unavailable in this build")
	default:
		return nil, fmt.Errorf("unknown local proxy backend %q (supported: auto, rust, go)", backend)
	}
}

func findBackendFactory(factories []BackendFactory, name BackendName) BackendFactory {
	for i := len(factories) - 1; i >= 0; i-- {
		factory := factories[i]
		if factory == nil {
			continue
		}
		if factory.Name() == name {
			return factory
		}
	}
	return nil
}

func linkedBackendFactories(callFactories []BackendFactory) []BackendFactory {
	registeredBackendFactories.RLock()
	defer registeredBackendFactories.RUnlock()
	factories := make([]BackendFactory, 0, len(registeredBackendFactories.factories)+len(callFactories))
	factories = append(factories, registeredBackendFactories.factories...)
	factories = append(factories, callFactories...)
	return factories
}

func buildClientConfig(opts Options, controlPlaneURL *url.URL, controlPlaneUnixSocket string) (*config.Config, error) {
	args := []string{
		"--control-plane.base-url", controlPlaneURL.String(),
		"--control-plane.tunnel-id", opts.TunnelID.String(),
		"--control-plane.poll-timeout", opts.ControlPlanePollTimeout.String(),
		"--control-plane.poll-deadline-guardrail", opts.PollDeadlineGuardrail.String(),
		"--open-web-ui=false",
	}
	if opts.HealthListenAddr != "" {
		args = append(args, "--health.listen-addr", opts.HealthListenAddr)
	}
	if opts.HealthURLFile != "" {
		args = append(args, "--health.url-file", opts.HealthURLFile)
	}
	if opts.Profile != "" {
		args = append(args, "--profile", opts.Profile)
	}
	if opts.ProfileFile != "" {
		args = append(args, "--profile-file", opts.ProfileFile)
	}
	if opts.ProfileDir != "" {
		args = append(args, "--profile-dir", opts.ProfileDir)
	}
	for _, serverURL := range opts.MCPServerURLs {
		args = append(args, "--mcp.server-url", serverURL)
	}
	for _, command := range opts.MCPCommands {
		args = append(args, "--mcp.command", command)
	}

	cfg, err := config.Load(args, overlayEnv(opts.LookupEnv, map[string]string{
		localControlPlaneAPIKeyEnv: localControlPlaneAPIKey,
	}))
	if err != nil {
		return nil, err
	}
	cfg.ControlPlane.BaseURL = controlPlaneURL
	cfg.ControlPlane.UnixSocketPath = controlPlaneUnixSocket
	cfg.ControlPlane.TunnelID = opts.TunnelID
	cfg.ControlPlane.APIKey = localControlPlaneAPIKey
	cfg.ControlPlane.PollBackoffMin = tunnelClientPollBackoffMin
	cfg.ControlPlane.PollBackoffMax = tunnelClientPollBackoffMax
	return cfg, nil
}

func prepareControlPlaneUnixSocket() (string, func()) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "tunnel-client-")
	if err != nil {
		return "", nil
	}
	socketPath := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil
	}
	_ = listener.Close()
	_ = os.Remove(socketPath)
	return socketPath, func() {
		_ = os.RemoveAll(dir)
	}
}

func overlayEnv(base func(string) (string, bool), overrides map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		if value, ok := overrides[key]; ok {
			return value, true
		}
		if base == nil {
			return "", false
		}
		return base(key)
	}
}

func waitForMCPProbe(ctx context.Context, timeout time.Duration, probeState *mcpclient.ProbeState) error {
	if probeState == nil {
		return errors.New("tunnel-client MCP probe state unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := probeState.WaitUntilDone(waitCtx); err != nil {
		return fmt.Errorf("wait for MCP probe: %w", err)
	}
	if _, err, ok := probeState.Wait(time.Millisecond); !ok {
		return errors.New("MCP probe did not publish status")
	} else if err != nil {
		return fmt.Errorf("MCP probe failed: %w", err)
	}
	return nil
}

func writeInfoFile(path string, info Info) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create URL file dir %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write URL file %s: %w", path, err)
	}
	return nil
}

type goBackendFactory struct{}

func (goBackendFactory) Name() BackendName {
	return BackendGo
}

func (goBackendFactory) StartBackend(_ context.Context, opts BackendOptions) (Backend, error) {
	server, err := startLocalServer(localServerOptions{
		ListenAddr:             opts.ListenAddr,
		ControlPlaneUnixSocket: opts.ControlPlaneUnixSocket,
		TunnelID:               opts.TunnelID,
		APIKey:                 opts.APIKey,
		ResponseTimeout:        opts.ResponseTimeout,
		ClientLastSeenTimeout:  opts.ClientLastSeenTimeout,
	})
	if err != nil {
		return nil, err
	}
	return &goBackend{
		server:                 server,
		controlPlaneUnixSocket: opts.ControlPlaneUnixSocket,
	}, nil
}

type goBackend struct {
	server                 *localServer
	controlPlaneUnixSocket string
}

func (b *goBackend) Name() BackendName {
	return BackendGo
}

func (b *goBackend) InfoBackend() string {
	return "go-in-memory"
}

func (b *goBackend) IngressBaseURL() *url.URL {
	if b == nil || b.server == nil {
		return nil
	}
	return b.server.IngressBaseURL()
}

func (b *goBackend) ControlPlaneBaseURL() *url.URL {
	if b == nil || b.server == nil {
		return nil
	}
	return b.server.ControlPlaneBaseURL()
}

func (b *goBackend) ControlPlaneUnixSocket() string {
	if b == nil {
		return ""
	}
	return b.controlPlaneUnixSocket
}

func (b *goBackend) WaitForTunnelClient(ctx context.Context, timeout time.Duration, _ types.TunnelID) error {
	if b == nil || b.server == nil {
		return errors.New("go local proxy backend is nil")
	}
	return b.server.WaitForPoll(ctx, timeout)
}

func (b *goBackend) Wait(ctx context.Context) error {
	if b == nil || b.server == nil {
		return nil
	}
	return b.server.Wait(ctx)
}

func (b *goBackend) Stop(ctx context.Context) error {
	if b == nil || b.server == nil {
		return nil
	}
	return b.server.Stop(ctx)
}

type localServerOptions struct {
	ListenAddr             string
	ControlPlaneUnixSocket string
	TunnelID               types.TunnelID
	APIKey                 string
	ResponseTimeout        time.Duration
	ClientLastSeenTimeout  time.Duration
}

type localServer struct {
	tunnelID              types.TunnelID
	apiKey                string
	responseTimeout       time.Duration
	clientLastSeenTimeout time.Duration
	ingressBaseURL        *url.URL
	controlPlaneBaseURL   *url.URL
	ingressServer         *http.Server
	controlPlaneServer    *http.Server
	ingressListener       net.Listener
	controlPlaneListener  net.Listener
	errCh                 chan error
	stopOnce              sync.Once

	mu       sync.Mutex
	stateCh  chan struct{}
	pending  []*localRequest
	inFlight map[string]*localRequest
	lastPoll time.Time
	nextID   atomic.Uint64
}

type localRequest struct {
	id         string
	channel    types.Channel
	command    json.RawMessage
	responseCh chan localResponse
}

type localResponse struct {
	payload wiretypes.TunnelResponsePayload
}

func startLocalServer(opts localServerOptions) (*localServer, error) {
	if opts.ListenAddr == "" {
		opts.ListenAddr = DefaultListenAddr
	}
	if opts.ResponseTimeout <= 0 {
		opts.ResponseTimeout = DefaultResponseTimeout
	}
	if opts.ClientLastSeenTimeout <= 0 {
		opts.ClientLastSeenTimeout = DefaultClientLastSeenTimeout
	}
	listener, err := net.Listen("tcp", opts.ListenAddr)
	if err != nil {
		return nil, err
	}
	ingressURL := &url.URL{
		Scheme: "http",
		Host:   listener.Addr().String(),
	}
	controlPlaneURL := ingressURL
	server := &localServer{
		tunnelID:              opts.TunnelID,
		apiKey:                opts.APIKey,
		responseTimeout:       opts.ResponseTimeout,
		clientLastSeenTimeout: opts.ClientLastSeenTimeout,
		ingressBaseURL:        ingressURL,
		controlPlaneBaseURL:   controlPlaneURL,
		ingressListener:       listener,
		errCh:                 make(chan error, 2),
		stateCh:               make(chan struct{}),
		inFlight:              make(map[string]*localRequest),
	}

	handler := server.handler()
	server.ingressServer = &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if opts.ControlPlaneUnixSocket != "" {
		controlPlaneListener, err := net.Listen("unix", opts.ControlPlaneUnixSocket)
		if err != nil {
			_ = listener.Close()
			return nil, err
		}
		server.controlPlaneListener = controlPlaneListener
		server.controlPlaneBaseURL = &url.URL{Scheme: "http", Host: "tunnel-client-local-proxy"}
		server.controlPlaneServer = &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
	}

	server.serve(server.ingressServer, listener)
	if server.controlPlaneServer != nil && server.controlPlaneListener != nil {
		server.serve(server.controlPlaneServer, server.controlPlaneListener)
	}
	return server, nil
}

func (s *localServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/tunnels/", s.handleTunnel)
	mux.HandleFunc("/v1/mcp/", s.handleMCP)
	return mux
}

func (s *localServer) serve(server *http.Server, listener net.Listener) {
	go func() {
		err := server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errCh <- err
			return
		}
		s.errCh <- nil
	}()
}

func (s *localServer) IngressBaseURL() *url.URL {
	if s == nil || s.ingressBaseURL == nil {
		return nil
	}
	copyURL := *s.ingressBaseURL
	return &copyURL
}

func (s *localServer) ControlPlaneBaseURL() *url.URL {
	if s == nil || s.controlPlaneBaseURL == nil {
		return nil
	}
	copyURL := *s.controlPlaneBaseURL
	return &copyURL
}

func (s *localServer) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var stopErr error
	s.stopOnce.Do(func() {
		if s.ingressServer != nil {
			if err := s.ingressServer.Shutdown(ctx); err != nil {
				stopErr = errors.Join(stopErr, err)
			}
		}
		if s.controlPlaneServer != nil {
			if err := s.controlPlaneServer.Shutdown(ctx); err != nil {
				stopErr = errors.Join(stopErr, err)
			}
		}
	})
	return stopErr
}

func (s *localServer) Wait(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case err := <-s.errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *localServer) WaitForPoll(ctx context.Context, timeout time.Duration) error {
	if s == nil {
		return errors.New("local control plane is nil")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		s.mu.Lock()
		ok := !s.lastPoll.IsZero()
		state := s.stateCh
		s.mu.Unlock()
		if ok {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("wait for tunnel-client poll: %w", waitCtx.Err())
		case <-state:
		case <-time.After(defaultPollProbeDelay):
		}
	}
}

func (s *localServer) handleTunnel(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(w, r) {
		return
	}
	tunnelID, suffix, ok := extractTunnelPath(r.URL.Path)
	if !ok || tunnelID != s.tunnelID.String() {
		http.NotFound(w, r)
		return
	}
	switch suffix {
	case "":
		s.handleMetadata(w, r)
	case "/poll":
		s.handlePoll(w, r)
	case "/response":
		s.handleResponse(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *localServer) handleMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]string{
		"id":          s.tunnelID.String(),
		"name":        "local tunnel-client dev proxy",
		"description": "Pure-Go in-memory control plane for local MCP tests",
	})
}

func (s *localServer) handlePoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 1)
	timeout := parseTimeout(r.URL.Query().Get("timeout_ms"))
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		s.mu.Lock()
		s.lastPoll = time.Now()
		s.signalStateChangeLocked()
		commands := s.dequeueLocked(limit)
		var state <-chan struct{}
		if len(commands) == 0 {
			state = s.stateCh
		}
		s.mu.Unlock()

		if len(commands) > 0 {
			w.Header().Set("X-Request-Id", "local-poll-"+strconv.FormatInt(time.Now().UnixNano(), 10))
			writeJSON(w, wiretypes.PolledCommandEnvelope{Commands: commands})
			return
		}

		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			w.Header().Set("X-Request-Id", "local-poll-empty")
			writeJSON(w, wiretypes.PolledCommandEnvelope{Commands: []json.RawMessage{}})
			return
		case <-state:
		}
	}
}

func (s *localServer) handleResponse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload wiretypes.TunnelResponsePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid response payload", http.StatusBadRequest)
		return
	}
	if payload.RequestID == "" {
		http.Error(w, "missing request_id", http.StatusBadRequest)
		return
	}
	if shardToken := r.Header.Get("X-Tunnel-Shard-Token"); shardToken != "" && shardToken != payload.RequestID {
		http.Error(w, "invalid shard token", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	request := s.inFlight[payload.RequestID]
	if request != nil {
		delete(s.inFlight, payload.RequestID)
	}
	s.signalStateChangeLocked()
	s.mu.Unlock()
	if request == nil {
		http.NotFound(w, r)
		return
	}

	select {
	case request.responseCh <- localResponse{payload: payload}:
	default:
	}
	w.Header().Set("X-Request-Id", "local-response-"+payload.RequestID)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *localServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	tunnelID, channel, ok := extractMCPPath(r.URL.Path)
	if !ok || tunnelID != s.tunnelID.String() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "tunnel-client local proxy only accepts POST streamable HTTP at this endpoint", http.StatusNotImplemented)
		return
	}
	payload, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read request body", http.StatusBadRequest)
		return
	}
	request, err := s.enqueueMCPRequest(channel, payload, r.Header)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer s.cancelRequest(request.id)

	timer := time.NewTimer(s.responseTimeout)
	defer timer.Stop()
	select {
	case response := <-request.responseCh:
		renderMCPResponse(w, response.payload)
	case <-r.Context().Done():
		return
	case <-timer.C:
		http.Error(w, "timed out waiting for tunnel-client response", http.StatusGatewayTimeout)
	}
}

func (s *localServer) enqueueMCPRequest(channel types.Channel, payload []byte, headers http.Header) (*localRequest, error) {
	if len(payload) == 0 {
		return nil, errors.New("jsonrpc request body is required")
	}
	requestID := "local-" + strconv.FormatUint(s.nextID.Add(1), 10)
	raw := wiretypes.RawJSONRPCPolledCommand{
		BaseRawPolledCommand: wiretypes.BaseRawPolledCommand{
			RequestID:   requestID,
			ShardToken:  requestID,
			CommandType: wiretypes.CommandTypeJSONRPC,
			Channel:     channel.String(),
			CreatedAt:   time.Now().UTC(),
			Headers:     sanitizeForwardableRequestHeaders(headers),
		},
		JSONRPC: json.RawMessage(append([]byte(nil), payload...)),
	}
	command, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	request := &localRequest{
		id:         requestID,
		channel:    channel,
		command:    command,
		responseCh: make(chan localResponse, 1),
	}
	s.mu.Lock()
	s.pending = append(s.pending, request)
	s.signalStateChangeLocked()
	s.mu.Unlock()
	return request, nil
}

func (s *localServer) dequeueLocked(limit int) []json.RawMessage {
	if limit <= 0 || len(s.pending) == 0 {
		return nil
	}
	if limit > len(s.pending) {
		limit = len(s.pending)
	}
	commands := make([]json.RawMessage, 0, limit)
	for i := 0; i < limit; i++ {
		request := s.pending[i]
		s.inFlight[request.id] = request
		commands = append(commands, append(json.RawMessage(nil), request.command...))
	}
	copy(s.pending, s.pending[limit:])
	for i := len(s.pending) - limit; i < len(s.pending); i++ {
		if i >= 0 {
			s.pending[i] = nil
		}
	}
	s.pending = s.pending[:len(s.pending)-limit]
	s.signalStateChangeLocked()
	return commands
}

func (s *localServer) cancelRequest(requestID string) {
	if requestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inFlight, requestID)
	for i, request := range s.pending {
		if request.id != requestID {
			continue
		}
		copy(s.pending[i:], s.pending[i+1:])
		s.pending[len(s.pending)-1] = nil
		s.pending = s.pending[:len(s.pending)-1]
		break
	}
	s.signalStateChangeLocked()
}

func (s *localServer) authorized(w http.ResponseWriter, r *http.Request) bool {
	want := "Bearer " + s.apiKey
	if r.Header.Get("Authorization") == want {
		return true
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (s *localServer) signalStateChangeLocked() {
	if s.stateCh == nil {
		s.stateCh = make(chan struct{})
		return
	}
	close(s.stateCh)
	s.stateCh = make(chan struct{})
}

func renderMCPResponse(w http.ResponseWriter, payload wiretypes.TunnelResponsePayload) {
	for name, values := range payload.ResponseHeaders {
		if shouldSkipResponseHeader(name) {
			continue
		}
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	statusCode := payload.ResponseCode
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	if len(payload.JSONResponse) > 0 && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(statusCode)
	if len(payload.JSONResponse) > 0 {
		_, _ = w.Write(payload.JSONResponse)
	}
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func extractTunnelPath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, "/v1/tunnels/")
	if rest == path || rest == "" {
		return "", "", false
	}
	for _, suffix := range []string{"/poll", "/response"} {
		if strings.HasSuffix(rest, suffix) {
			return strings.TrimSuffix(rest, suffix), suffix, true
		}
	}
	return rest, "", !strings.Contains(rest, "/")
}

func extractMCPPath(path string) (string, types.Channel, bool) {
	rest := strings.TrimPrefix(path, "/v1/mcp/")
	if rest == path || rest == "" {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || parts[0] == "" {
		return "", "", false
	}
	channelName := ""
	if len(parts) == 2 {
		channelName = parts[1]
	}
	channel, err := types.NormalizeChannel(channelName)
	if err != nil {
		return "", "", false
	}
	return parts[0], channel, true
}

func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func parseTimeout(raw string) time.Duration {
	ms, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || ms <= 0 {
		return DefaultControlPlanePollTimeout
	}
	timeout := time.Duration(ms) * time.Millisecond
	if timeout > 5*time.Second {
		return 5 * time.Second
	}
	return timeout
}

func sanitizeForwardableRequestHeaders(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	out := make(http.Header, len(headers))
	for name, values := range headers {
		if _, blocked := blockedRequestMCPHeaders[strings.ToLower(name)]; blocked {
			continue
		}
		for _, value := range values {
			if value != "" {
				out.Add(name, value)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shouldSkipResponseHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Content-Length", "Date", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	default:
		return false
	}
}
