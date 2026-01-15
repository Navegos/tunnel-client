package mcpclient

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.openai.org/api/tunnel-client/pkg/config"
)

// TransportProvider constructs an MCP transport for a specific transport kind.
type TransportProvider interface {
	Kind() config.MCPTransportKind
	Build(TransportBuildParams) (mcp.Transport, error)
}

// TransportBuildParams carries shared dependencies for transport construction.
type TransportBuildParams struct {
	Config     *config.MCPConfig
	HTTPClient *http.Client
}

type streamableTransportProvider struct{}

func newStreamableTransportProvider() TransportProvider {
	return streamableTransportProvider{}
}

func (streamableTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportHTTPStreamable
}

func (streamableTransportProvider) Build(params TransportBuildParams) (mcp.Transport, error) {
	if params.Config == nil || params.Config.ServerURL == nil {
		return nil, errors.New("mcpclient: server URL is required for http-streamable transport")
	}
	return &mcp.StreamableClientTransport{
		Endpoint:   params.Config.ServerURL.String(),
		HTTPClient: params.HTTPClient,
	}, nil
}

type inMemoryTransportProvider struct {
	transport *mcp.InMemoryTransport
}

func (p inMemoryTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportInMemory
}

func (p inMemoryTransportProvider) Build(TransportBuildParams) (mcp.Transport, error) {
	if p.transport == nil {
		return nil, errors.New("mcpclient: in-memory transport requires injected transport")
	}
	return newSharedConnectionTransport(p.transport), nil
}

type stdioTransportProvider struct {
	transport *mcp.IOTransport
}

func (p stdioTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportStdio
}

func (p stdioTransportProvider) Build(TransportBuildParams) (mcp.Transport, error) {
	if p.transport == nil {
		return nil, errors.New("mcpclient: stdio transport requires injected transport")
	}
	return newSharedConnectionTransport(p.transport), nil
}

func selectTransportProvider(kind config.MCPTransportKind, providers []TransportProvider) (TransportProvider, error) {
	if kind == "" {
		kind = config.MCPTransportHTTPStreamable
	}
	byKind := make(map[config.MCPTransportKind]TransportProvider, len(providers))
	for _, provider := range providers {
		if provider == nil {
			continue
		}
		existing, ok := byKind[provider.Kind()]
		if ok && existing != nil {
			return nil, fmt.Errorf("mcpclient: multiple transport providers registered for %q", provider.Kind())
		}
		byKind[provider.Kind()] = provider
	}
	provider, ok := byKind[kind]
	if !ok || provider == nil {
		return nil, fmt.Errorf("mcpclient: no transport provider registered for %q", kind)
	}
	return provider, nil
}
