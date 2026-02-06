package mcpclient

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/config"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
	"go.openai.org/api/tunnel-client/pkg/tlsconfig"
)

// ChannelTransportFactory builds MCP transports for configured channel bindings.
type ChannelTransportFactory struct {
	config        *config.MCPConfig
	logger        *slog.Logger
	logging       *config.LoggingConfig
	providers     []TransportProvider
	meterProvider *sdkmetric.MeterProvider
	tlsBundle     *tlsconfig.Bundle

	mu          sync.Mutex
	transports  map[string]mcp.Transport
	httpClients map[string]*http.Client
}

type channelTransportFactoryParams struct {
	fx.In

	Config             *config.MCPConfig
	Logging            *config.LoggingConfig
	Logger             *slog.Logger
	MeterProvider      *sdkmetric.MeterProvider
	TLSBundle          *tlsconfig.Bundle
	TransportProviders []TransportProvider `group:"mcp_transport_providers"`
}

func newChannelTransportFactory(p channelTransportFactoryParams) (*ChannelTransportFactory, error) {
	if p.Config == nil || p.Logging == nil || p.Logger == nil || p.MeterProvider == nil {
		return nil, fmt.Errorf("mcpclient: channel transport factory requires config, logging, logger, and meter provider")
	}
	factory := &ChannelTransportFactory{
		config:        p.Config,
		logger:        p.Logger,
		logging:       p.Logging,
		meterProvider: p.MeterProvider,
		tlsBundle:     p.TLSBundle,
		providers:     p.TransportProviders,
		transports:    make(map[string]mcp.Transport),
		httpClients:   make(map[string]*http.Client),
	}
	factory.logProxyConfig()
	return factory, nil
}

// HTTPClientForBinding returns the HTTP client used for streamable MCP transports for a binding.
func (f *ChannelTransportFactory) HTTPClientForBinding(binding config.MCPChannelBinding) (*http.Client, error) {
	if f == nil {
		return nil, fmt.Errorf("mcpclient: channel transport factory is nil")
	}
	transportKind := binding.TransportKind
	if transportKind == "" {
		transportKind = config.MCPTransportHTTPStreamable
	}
	if transportKind != config.MCPTransportHTTPStreamable {
		return f.httpClientForKey("default", nil)
	}
	channelName := binding.Channel.Canonical()
	if channelName == "" {
		return nil, fmt.Errorf("mcpclient: invalid channel name")
	}
	return f.httpClientForKey(channelName.String(), binding.HTTPProxy)
}

// Build returns a cached transport for the requested binding.
func (f *ChannelTransportFactory) Build(binding config.MCPChannelBinding) (mcp.Transport, error) {
	if f == nil {
		return nil, fmt.Errorf("mcpclient: channel transport factory is nil")
	}
	channelName := binding.Channel.Canonical()
	if channelName == "" {
		return nil, fmt.Errorf("mcpclient: invalid channel name")
	}

	f.mu.Lock()
	if transport, ok := f.transports[channelName.String()]; ok {
		f.mu.Unlock()
		return transport, nil
	}
	f.mu.Unlock()

	transportKind := binding.TransportKind
	if transportKind == "" {
		transportKind = config.MCPTransportHTTPStreamable
	}
	provider, err := selectTransportProvider(transportKind, f.providers)
	if err != nil {
		return nil, err
	}
	httpClient, err := f.HTTPClientForBinding(binding)
	if err != nil {
		return nil, err
	}
	transport, err := provider.Build(TransportBuildParams{
		Config:     f.config,
		Binding:    binding,
		HTTPClient: httpClient,
	})
	if err != nil {
		return nil, err
	}
	transport = f.decorateTransport(transport)

	f.mu.Lock()
	f.transports[channelName.String()] = transport
	f.mu.Unlock()

	return transport, nil
}

func (f *ChannelTransportFactory) decorateTransport(base mcp.Transport) mcp.Transport {
	if base == nil {
		return nil
	}
	if f.logging == nil || !f.logging.HTTPRawUnsafe || f.logging.Level > slog.LevelDebug {
		return base
	}
	logger := f.logger.With(tclog.FieldComponent, tclog.ComponentMcpClient, "transport", "raw_http")
	return &mcp.LoggingTransport{
		Transport: base,
		Writer:    slogWriter{logger: logger},
	}
}

func (f *ChannelTransportFactory) httpClientForKey(key string, proxyURL *url.URL) (*http.Client, error) {
	f.mu.Lock()
	if client, ok := f.httpClients[key]; ok {
		f.mu.Unlock()
		return client, nil
	}
	f.mu.Unlock()
	transport, err := buildMcpHTTPTransport(f.logger, f.logging, f.meterProvider, f.tlsBundle, proxyURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: transport}
	f.mu.Lock()
	f.httpClients[key] = client
	f.mu.Unlock()
	return client, nil
}

func (f *ChannelTransportFactory) logProxyConfig() {
	if f == nil || f.logger == nil || f.config == nil {
		return
	}
	logger := f.logger.With(tclog.FieldComponent, tclog.ComponentMcpClient)
	for _, binding := range f.config.ChannelBindings {
		channel := binding.Channel.Canonical()
		transportKind := binding.TransportKind
		if transportKind == "" {
			transportKind = config.MCPTransportHTTPStreamable
		}
		if transportKind != config.MCPTransportHTTPStreamable {
			logger.Info("mcp channel proxy ignored for transport",
				slog.String("channel", channel.String()),
				slog.String("transport", string(transportKind)),
				slog.String("proxy_source", binding.HTTPProxySource.String()),
			)
			continue
		}
		fields := []any{
			slog.String("channel", channel.String()),
			slog.String("transport", string(transportKind)),
		}
		fields = append(fields, config.ProxyLogFields(binding.HTTPProxy, binding.HTTPProxySource)...)
		logger.Info("mcp channel proxy configured", fields...)
		if binding.HTTPProxySource != config.ProxySourceEnvironment && config.EnvProxyConfigured(os.LookupEnv) {
			logger.Info("mcp channel proxy overrides environment proxy settings",
				slog.String("channel", channel.String()),
			)
		}
	}
}
