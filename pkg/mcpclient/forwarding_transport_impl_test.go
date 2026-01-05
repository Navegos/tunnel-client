package mcpclient

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.openai.org/api/tunnel-client/pkg/mcpclient/internal"
)

func TestForwardingConnectionPropagatesHeaders(t *testing.T) {
	respHeaders := http.Header{"X-Response": {"ok"}, "Another": {"value"}}
	const wantStatus = http.StatusAccepted
	sortStrings := cmpopts.SortSlices(func(a, b string) bool { return a < b })

	callID := mustMakeID(t, "call-1")

	fake := &fakeConnection{
		writeFunc: func(ctx context.Context, msg jsonrpc.Message) error {
			carrier := internal.CarrierFromContext(ctx)
			if carrier == nil {
				t.Fatalf("carrier missing in context")
			}
			carrier.StoreResponse(wantStatus, respHeaders)
			return nil
		},
		readFunc: func(ctx context.Context) (jsonrpc.Message, error) {
			return &jsonrpc.Response{
				ID: callID,
			}, nil
		},
	}

	conn := &forwardingConnection{
		base: fake,
	}

	req := &jsonrpc.Request{
		ID:     callID,
		Method: "testMethod",
	}

	requestHeaders := http.Header{"X-Forward": {"value"}}

	statusCode, gotWriteHeaders, err := conn.Write(context.Background(), requestHeaders, req)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if statusCode != wantStatus {
		t.Fatalf("unexpected status code: got %d, want %d", statusCode, wantStatus)
	}
	if diff := cmp.Diff(respHeaders, gotWriteHeaders, sortStrings); diff != "" {
		t.Fatalf("write headers mismatch (-want +got):\n%s", diff)
	}

	msg, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if _, ok := msg.(*jsonrpc.Response); !ok {
		t.Fatalf("expected jsonrpc.Response, got %T", msg)
	}

	if fake.lastForwardedHeader == nil {
		t.Fatalf("request headers were not forwarded to fake connection")
	}
	if diff := cmp.Diff(requestHeaders, fake.lastForwardedHeader, sortStrings); diff != "" {
		t.Fatalf("request headers mismatch (-want +got):\n%s", diff)
	}
}

func TestForwardingTransportConnectNilBaseReturnsNil(t *testing.T) {
	t.Parallel()

	transport := &forwardingTransport{}
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if conn != nil {
		t.Fatalf("expected nil connection, got %T", conn)
	}
}

func TestForwardingTransportConnectPropagatesBaseError(t *testing.T) {
	t.Parallel()

	transport := &forwardingTransport{base: &failingTransport{err: errors.New("connect failed")}}
	conn, err := transport.Connect(context.Background())
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if conn != nil {
		t.Fatalf("expected nil connection, got %T", conn)
	}
}

func TestForwardingConnectionCloseDelegates(t *testing.T) {
	t.Parallel()

	fake := &closeTrackingConnection{}
	conn := &forwardingConnection{base: fake}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
	if !fake.closed {
		t.Fatalf("expected base connection Close to be called")
	}
}

func TestForwardingConnectionCloseNilBaseReturnsNil(t *testing.T) {
	t.Parallel()

	conn := &forwardingConnection{base: nil}
	if err := conn.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}
}

func TestForwardingConnectionWriteNilBaseReturnsZeroes(t *testing.T) {
	t.Parallel()

	callID := mustMakeID(t, "call-nil-base")
	req := &jsonrpc.Request{ID: callID, Method: "noop"}

	conn := &forwardingConnection{base: nil}
	status, headers, err := conn.Write(context.Background(), http.Header{"X-Test": {"true"}}, req)
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if status != 0 {
		t.Fatalf("unexpected status code: got %d want 0", status)
	}
	if headers != nil {
		t.Fatalf("expected nil headers, got %v", headers)
	}
}

func TestForwardingConnectionReadNilBaseReturnsNils(t *testing.T) {
	t.Parallel()

	conn := &forwardingConnection{base: nil}
	msg, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if msg != nil {
		t.Fatalf("expected nil message, got %T", msg)
	}
}

func TestForwardingConnectionWriteNilContextReturnsError(t *testing.T) {
	t.Parallel()

	callID := mustMakeID(t, "call-nil-ctx")
	req := &jsonrpc.Request{ID: callID, Method: "noop"}

	conn := &forwardingConnection{base: &fakeConnection{}}
	//lint:ignore SA1012 exercising nil-context guard in ContextWithHeaders
	_, _, err := conn.Write(nil, nil, req)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

type fakeConnection struct {
	writeFunc           func(context.Context, jsonrpc.Message) error
	readFunc            func(context.Context) (jsonrpc.Message, error)
	lastForwardedHeader http.Header
}

func (f *fakeConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if f.readFunc != nil {
		return f.readFunc(ctx)
	}
	return nil, nil
}

func (f *fakeConnection) Write(ctx context.Context, msg jsonrpc.Message) error {
	if carrier := internal.CarrierFromContext(ctx); carrier != nil {
		f.lastForwardedHeader = carrier.RequestHeaders()
	}
	if f.writeFunc == nil {
		return nil
	}
	return f.writeFunc(ctx, msg)
}

func (f *fakeConnection) Close() error      { return nil }
func (f *fakeConnection) SessionID() string { return "" }

func mustMakeID(tb testing.TB, v any) jsonrpc.ID {
	tb.Helper()
	id, err := jsonrpc.MakeID(v)
	if err != nil {
		tb.Fatalf("jsonrpc.MakeID(%v): %v", v, err)
	}
	return id
}

type failingTransport struct {
	err error
}

func (t *failingTransport) Connect(context.Context) (mcp.Connection, error) {
	return nil, t.err
}

type closeTrackingConnection struct {
	closed bool
}

func (c *closeTrackingConnection) Read(context.Context) (jsonrpc.Message, error) { return nil, nil }
func (c *closeTrackingConnection) Write(context.Context, jsonrpc.Message) error  { return nil }
func (c *closeTrackingConnection) Close() error                                  { c.closed = true; return nil }
func (c *closeTrackingConnection) SessionID() string                             { return "" }
