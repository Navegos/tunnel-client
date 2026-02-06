package mcpclient

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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

func mustParseURLFactoryTest(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
