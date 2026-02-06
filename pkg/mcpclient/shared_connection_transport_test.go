package mcpclient

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestNewSharedConnectionTransportReusesConnection(t *testing.T) {
	t.Parallel()

	base := &countingTransport{
		connectFn: func() mcp.Connection {
			return &fakeSharedConn{}
		},
	}

	shared := NewSharedConnectionTransport(base)
	require.NotNil(t, shared)

	connA, err := shared.Connect(context.Background())
	require.NoError(t, err)
	require.NotNil(t, connA)

	connB, err := shared.Connect(context.Background())
	require.NoError(t, err)
	require.NotNil(t, connB)

	require.Same(t, connA, connB)
	require.Equal(t, 1, base.connectCalls)
}

func TestNewSharedConnectionTransportNilBase(t *testing.T) {
	t.Parallel()

	require.Nil(t, NewSharedConnectionTransport(nil))
}

type countingTransport struct {
	connectCalls int
	connectFn    func() mcp.Connection
}

func (t *countingTransport) Connect(context.Context) (mcp.Connection, error) {
	t.connectCalls++
	return t.connectFn(), nil
}

type fakeSharedConn struct{}

func (fakeSharedConn) Read(context.Context) (jsonrpc.Message, error) { return nil, nil }
func (fakeSharedConn) Write(context.Context, jsonrpc.Message) error  { return nil }
func (fakeSharedConn) Close() error                                  { return nil }
func (fakeSharedConn) SessionID() string                             { return "" }
