package mcpclient

import (
	"context"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type sharedConnectionTransport struct {
	base mcp.Transport
	mu   sync.Mutex
	conn mcp.Connection
	err  error
}

// NewSharedConnectionTransport returns a transport wrapper that reuses the
// same underlying connection across Connect calls.
func NewSharedConnectionTransport(base mcp.Transport) mcp.Transport {
	return newSharedConnectionTransport(base)
}

func newSharedConnectionTransport(base mcp.Transport) mcp.Transport {
	if base == nil {
		return nil
	}
	return &sharedConnectionTransport{base: base}
}

func (t *sharedConnectionTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	if t == nil || t.base == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != nil || t.err != nil {
		return t.conn, t.err
	}
	t.conn, t.err = t.base.Connect(ctx)
	return t.conn, t.err
}
