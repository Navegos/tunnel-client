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

type injectableTransportProvider struct {
	transport mcp.Transport
}

func (p injectableTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportInMemory
}

func (p injectableTransportProvider) Build(TransportBuildParams) (mcp.Transport, error) {
	if p.transport == nil {
		return nil, errors.New("mcpclient: in-memory transport requires injected transport")
	}
	return newSharedConnectionTransport(p.transport), nil
}

type stdioTransportProvider struct {
	commandTransport *stdioCommandTransport
}

func (p stdioTransportProvider) Kind() config.MCPTransportKind {
	return config.MCPTransportStdio
}

func (p stdioTransportProvider) Build(params TransportBuildParams) (mcp.Transport, error) {
	if p.commandTransport == nil {
		return nil, errors.New("mcpclient: stdio transport requires mcp.command")
	}
	transport, err := p.commandTransport.Transport(params.Config)
	if err != nil {
		return nil, err
	}
	return newSharedConnectionTransport(transport), nil
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
