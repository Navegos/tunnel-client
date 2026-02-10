package mcpclient

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/types"
)

func TestChannelTransportFactoryAppliesProxy(t *testing.T) {
	t.Parallel()

	targetCalled := make(chan struct{}, 1)
	targetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalled <- struct{}{}
		http.Error(w, "unexpected direct request", http.StatusBadGateway)
	}))
	t.Cleanup(targetServer.Close)

	proxyCalled := make(chan struct{}, 1)
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalled <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(proxyServer.Close)

	proxyURL := mustParseURLFactoryTest(t, proxyServer.URL)
	binding := config.MCPChannelBinding{
		Channel:         types.DefaultChannel,
		TransportKind:   config.MCPTransportHTTPStreamable,
		ServerURL:       mustParseURLFactoryTest(t, targetServer.URL),
		HTTPProxy:       proxyURL,
		HTTPProxySource: config.ProxySource("mcp.server-url"),
	}
	cfg := &config.MCPConfig{
		ChannelBindings: []config.MCPChannelBinding{binding},
	}

	factory, err := newChannelTransportFactory(channelTransportFactoryParams{
		Config:        cfg,
		Logging:       &config.LoggingConfig{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MeterProvider: sdkmetric.NewMeterProvider(),
	})
	if err != nil {
		t.Fatalf("newChannelTransportFactory failed: %v", err)
	}

	client, err := factory.HTTPClientForBinding(binding)
	if err != nil {
		t.Fatalf("HTTPClientForBinding failed: %v", err)
	}
	resp, err := client.Get(targetServer.URL)
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case <-proxyCalled:
	default:
		t.Fatalf("expected proxy to receive request")
	}
	select {
	case <-targetCalled:
		t.Fatalf("expected target not to be called directly")
	default:
	}
}

type blockingTransportProvider struct {
	started chan struct{}
	release chan struct{}
	count   atomic.Int32
}

func (p *blockingTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportHTTPStreamable
}

func (p *blockingTransportProvider) Build(TransportBuildParams) (mcp.Transport, error) {
	p.count.Add(1)
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return &stubTransport{}, nil
}

func TestChannelTransportFactoryBuildSingleInstanceUnderConcurrency(t *testing.T) {
	t.Parallel()

	binding := config.MCPChannelBinding{
		Channel:       types.DefaultChannel,
		TransportKind: config.MCPTransportHTTPStreamable,
		ServerURL:     mustParseURLFactoryTest(t, "https://example.com"),
	}
	cfg := &config.MCPConfig{
		ChannelBindings: []config.MCPChannelBinding{binding},
	}
	provider := &blockingTransportProvider{
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}

	factory, err := newChannelTransportFactory(channelTransportFactoryParams{
		Config:             cfg,
		Logging:            &config.LoggingConfig{},
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		MeterProvider:      sdkmetric.NewMeterProvider(),
		TransportProviders: []TransportProvider{provider},
	})
	if err != nil {
		t.Fatalf("newChannelTransportFactory failed: %v", err)
	}

	const callers = 8
	results := make([]mcp.Transport, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		index := i
		go func() {
			defer wg.Done()
			transport, err := factory.Build(binding)
			if err != nil {
				t.Errorf("Build failed: %v", err)
				return
			}
			results[index] = transport
		}()
	}

	<-provider.started
	close(provider.release)
	wg.Wait()

	if got := provider.count.Load(); got != 1 {
		t.Fatalf("expected provider to build once, got %d", got)
	}
	if results[0] == nil {
		t.Fatal("expected transport result, got nil")
	}
	if _, ok := results[0].(*stubTransport); !ok {
		t.Fatalf("expected *stubTransport, got %T", results[0])
	}
	for i := 1; i < callers; i++ {
		if results[i] == nil {
			t.Fatalf("expected transport result at %d, got nil", i)
		}
		if results[i] != results[0] {
			t.Fatalf("expected shared transport instance, index %d differed", i)
		}
	}
}

func mustParseURLFactoryTest(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
