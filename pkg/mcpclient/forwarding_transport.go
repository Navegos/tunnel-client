package mcpclient

import (
	"context"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ForwardingTransport decorates an mcp.Transport so callers can attach
// per-request headers and capture the response headers returned by the MCP
// server.
type ForwardingTransport interface {
	Connect(ctx context.Context) (ForwardingConnection, error)
}

// SessionTerminatingTransport can explicitly close an MCP Streamable HTTP session and report
// the upstream HTTP response returned by the MCP server.
type SessionTerminatingTransport interface {
	TerminateSession(ctx context.Context, headers http.Header) (int, http.Header, error)
}

// ForwardingConnection extends mcp.Connection with helpers that return the
// response headers collected from the underlying HTTP transport.
type ForwardingConnection interface {
	// Write writes a new message to the connection.
	//
	// Write may be called concurrently, as calls or responses may occur
	// concurrently in user code.
	//
	// It returns the HTTP status code from the server response, the
	// response headers, and an error (if any) encountered while writing
	// or processing the response.
	Write(context.Context, http.Header, jsonrpc.Message) (int, http.Header, error)

	Read(ctx context.Context) (jsonrpc.Message, error)

	// Close closes the connection. It is implicitly called whenever a Read or
	// Write fails.
	//
	// Close may be called multiple times, potentially concurrently.
	Close() error
}

// NewForwardingTransport wraps the provided transport with header-forwarding
// capabilities.
func NewForwardingTransport(base mcp.Transport) ForwardingTransport {
	if base == nil {
		return nil
	}
	return &forwardingTransport{base: base}
}
