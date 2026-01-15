package mcpclient

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/fx"
)

type inMemoryProviderParams struct {
	fx.In

	Transport *mcp.InMemoryTransport `optional:"true"`
}

func newInMemoryTransportProvider(p inMemoryProviderParams) TransportProvider {
	return inMemoryTransportProvider{transport: p.Transport}
}

type stdioProviderParams struct {
	fx.In

	Transport *mcp.IOTransport `optional:"true"`
}

func newStdioTransportProvider(p stdioProviderParams) TransportProvider {
	return stdioTransportProvider{transport: p.Transport}
}
