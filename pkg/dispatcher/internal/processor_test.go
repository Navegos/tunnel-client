package dispatcherinternal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/mcpclient"
	"go.openai.org/api/tunnel-client/pkg/tunnelctx"
	"go.openai.org/api/tunnel-client/pkg/types"
)

func TestProcessorForwardResponses(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	serverLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, &mcp.ServerOptions{
		Logger: serverLogger,
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, serverTransport)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("server run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("server did not exit before timeout")
		}
	})

	responder := newRecordingResponder()
	processorLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	forwardingTransport := mcpclient.NewForwardingTransport(clientTransport)
	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(processorLogger, forwardingTransport, responder, &config.MCPConfig{
		ConnectionMaxTTL: 2 * time.Second,
	}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	params := mcp.InitializeParams{
		ClientInfo:      &mcp.Implementation{Name: "test-client", Version: "1.0.0"},
		ProtocolVersion: "2024-06-01",
	}
	paramsJSON, err := json.Marshal(&params)
	require.NoError(t, err)

	id, err := jsonrpc.MakeID("req-1")
	require.NoError(t, err)

	req := &jsonrpc.Request{
		ID:     id,
		Method: "initialize",
		Params: paramsJSON,
	}

	command := &fakePolledCommand{
		id:         types.RequestID("request-id"),
		message:    req,
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		headers: http.Header{
			"foo": []string{"bar"},
		},
		shardToken: "shard-request-id",
	}

	err = processor.Process(ctx, command)
	require.NoError(t, err)

	got := responder.waitForResponse(t)
	require.Equal(t, command.id, got.requestID)
	resp := got.response.JSONRPC()
	require.NotNil(t, resp, "expected JSON-RPC response payload")
	require.Nil(t, resp.Error)

	var result mcp.InitializeResult
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	require.NotNil(t, result.ServerInfo)
	require.Equal(t, "test-server", result.ServerInfo.Name)
	require.Equal(t, "1.0.0", result.ServerInfo.Version)
}

func TestProcessorStreamableNotificationsBeforeResponse(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)

	serverLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, &mcp.ServerOptions{
		Logger: serverLogger,
	})

	var notificationCount atomic.Int32
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				session, ok := req.GetSession().(*mcp.ServerSession)
				if !ok {
					return nil, fmt.Errorf("unexpected session type %T", req.GetSession())
				}
				for i := 1; i <= 3; i++ {
					if err := session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
						Message:  fmt.Sprintf("initializing %d/3", i),
						Progress: float64(i),
						Total:    3,
					}); err != nil {
						return nil, err
					}
					notificationCount.Add(1)
					time.Sleep(10 * time.Millisecond)
				}
			}
			return next(ctx, method, req)
		}
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, serverTransport)
	}()

	t.Cleanup(func() {
		cancel()

		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("server run returned error: %v", err)
			}
		case <-time.After(time.Second):
			t.Errorf("server did not exit before timeout")
		}
	})

	responder := newRecordingResponder()
	processorLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	forwardingTransport := mcpclient.NewForwardingTransport(clientTransport)
	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(processorLogger, forwardingTransport, responder, &config.MCPConfig{
		ConnectionMaxTTL: 5 * time.Second,
	}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	params := mcp.InitializeParams{
		ClientInfo:      &mcp.Implementation{Name: "test-client", Version: "1.0.0"},
		ProtocolVersion: "2025-03-26",
	}
	paramsJSON, err := json.Marshal(&params)
	require.NoError(t, err)

	id, err := jsonrpc.MakeID("streamable-req-1")
	require.NoError(t, err)

	req := &jsonrpc.Request{
		ID:     id,
		Method: "initialize",
		Params: paramsJSON,
	}

	command := &fakePolledCommand{
		id:         types.RequestID("streamable-request-id"),
		message:    req,
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		headers: http.Header{
			"scenario": []string{"streamable"},
		},
		shardToken: "shard-streamable",
	}

	err = processor.Process(ctx, command)
	require.NoError(t, err)

	got := responder.waitForResponse(t)
	require.Equal(t, command.id, got.requestID)
	resp := got.response.JSONRPC()
	require.NotNil(t, resp, "expected JSON-RPC response payload")
	require.Nil(t, resp.Error)

	var result mcp.InitializeResult
	require.NoError(t, json.Unmarshal(resp.Result, &result))
	require.NotNil(t, result.ServerInfo)
	require.Equal(t, "test-server", result.ServerInfo.Name)
	require.Equal(t, "1.0.0", result.ServerInfo.Version)
	require.EqualValues(t, 3, notificationCount.Load())
}

func TestProcessorAcknowledgesNotifications(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	serverLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "1.0.0"}, &mcp.ServerOptions{
		Logger: serverLogger,
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(ctx, serverTransport)
	}()
	t.Cleanup(func() {
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Errorf("server did not exit before timeout")
		}
	})

	responder := newRecordingResponder()
	processorLogger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	forwardingTransport := mcpclient.NewForwardingTransport(clientTransport)
	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(processorLogger, forwardingTransport, responder, &config.MCPConfig{
		ConnectionMaxTTL: 2 * time.Second,
	}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	req := &jsonrpc.Request{
		Method: "notifications/initialized",
	}

	command := &fakePolledCommand{
		id:         types.RequestID("notif-request-id"),
		message:    req,
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		shardToken: "shard-notification",
	}

	err = processor.Process(ctx, command)
	require.NoError(t, err)

	got := responder.waitForResponse(t)
	require.Equal(t, command.id, got.requestID)
	require.Nil(t, got.response.JSONRPC(), "notification acknowledgements must not carry JSON-RPC payloads")
	require.Equal(t, types.ResponseTypeNotificationAcknowledgment, got.response.Type(), "notification acknowledgements must set the ack response type")
}

func TestProcessorLogsIncludeRequestAndSessionID(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name           string
		commandSession *string
		headerSession  *string
		wantSession    string
	}{
		{
			name:        "from_command",
			wantSession: "cmd-session",
			commandSession: func() *string {
				s := "cmd-session"
				return &s
			}(),
		},
		{
			name:        "from_header",
			wantSession: "header-session",
			headerSession: func() *string {
				s := "header-session"
				return &s
			}(),
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			responder := newRecordingResponder()
			transport := &stubForwardingTransport{
				conn: &stubForwardingConnection{
					responseHeaders: func() http.Header {
						headers := make(http.Header)
						if tc.headerSession != nil {
							headers.Set(mcpclient.HeaderSessionID, *tc.headerSession)
						}
						return headers
					}(),
				},
			}
			meterProvider := newTestMeterProvider(t)
			processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, newTestControlPlaneConfig(t), meterProvider)
			require.NoError(t, err)

			id, err := jsonrpc.MakeID("session-log")
			require.NoError(t, err)

			cmd := &fakePolledCommand{
				id:         types.RequestID("session-log-request"),
				message:    &jsonrpc.Request{ID: id, Method: "ping"},
				enqueuedAt: time.Now(),
				polledAt:   time.Now(),
				sessionID:  tc.commandSession,
				shardToken: "shard-session-log",
			}

			transport.conn.(*stubForwardingConnection).response = &jsonrpc.Response{
				ID:     id,
				Result: json.RawMessage(`{"ok":true}`),
			}

			require.NoError(t, processor.Process(context.Background(), cmd))
			_ = responder.waitForResponse(t)

			require.Contains(t, buf.String(), "session_id="+tc.wantSession)
			require.Contains(t, buf.String(), "request_id=session-log-request")
		})
	}
}

func TestProcessorOverridesContentTypeHeader(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	responder := newRecordingResponder()

	id, err := jsonrpc.MakeID("content-type-override")
	require.NoError(t, err)

	transport := &stubForwardingTransport{
		conn: &stubForwardingConnection{
			responseHeaders: http.Header{
				http.CanonicalHeaderKey("Content-Type"): []string{"text/event-stream"},
			},
			response: &jsonrpc.Response{
				ID:     id,
				Result: json.RawMessage(`{"ok":true}`),
			},
		},
	}

	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	cmd := &fakePolledCommand{
		id:         types.RequestID("content-type-request"),
		message:    &jsonrpc.Request{ID: id, Method: "ping"},
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		shardToken: "shard-content-type",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	require.NoError(t, processor.Process(ctx, cmd))
	resp := responder.waitForResponse(t)
	require.Equal(t, cmd.id, resp.requestID)
	require.Equal(t, "application/json", resp.response.Headers().Get("Content-Type"))

	jsonResp := resp.response.JSONRPC()
	require.NotNil(t, jsonResp)
	require.Equal(t, id, jsonResp.ID)
}

func TestProcessorPropagatesControlPlaneCommandRequestID(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := newRecordingResponder()

	id, err := jsonrpc.MakeID("control-plane-context")
	require.NoError(t, err)

	transport := &stubForwardingTransport{conn: &stubForwardingConnection{
		response: &jsonrpc.Response{
			ID:     id,
			Result: json.RawMessage(`{"ok":true}`),
		},
	}}

	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := &fakePolledCommand{
		id:         types.RequestID("control-plane-forward"),
		message:    &jsonrpc.Request{ID: id, Method: "ping"},
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		headers: http.Header{
			"X-Request-Id": []string{"cp-command-id"},
		},
		shardToken: "shard-control-plane",
	}

	require.NoError(t, processor.Process(ctx, cmd))

	resp := responder.waitForResponse(t)
	require.Equal(t, "cp-command-id", resp.controlPlaneCommandRequestID)
	require.Equal(t, cmd.id, resp.requestID)
}

func TestProcessorRecordsEndToEndLatency(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	controlPlaneCfg := newTestControlPlaneConfig(t)
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	responder := newRecordingResponder()

	id, err := jsonrpc.MakeID("latency-request")
	require.NoError(t, err)

	transport := &stubForwardingTransport{
		conn: &stubForwardingConnection{
			response: &jsonrpc.Response{
				ID:     id,
				Result: json.RawMessage(`{"ok":true}`),
			},
		},
	}

	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, controlPlaneCfg, meterProvider)
	require.NoError(t, err)

	enqueuedAt := time.Now().Add(-750 * time.Millisecond)

	cmd := &fakePolledCommand{
		id:         types.RequestID("latency-request-id"),
		message:    &jsonrpc.Request{ID: id, Method: "ping"},
		enqueuedAt: enqueuedAt,
		polledAt:   enqueuedAt,
		shardToken: "shard-latency",
	}

	require.NoError(t, processor.Process(context.Background(), cmd))
	_ = responder.waitForResponse(t)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	histogram, ok := findHistogram(rm, metricNameCommandEndToEndLatency)
	require.True(t, ok, "command_end_to_end_latency_seconds metric not found")
	require.Len(t, histogram.DataPoints, 2)

	dpByType := dataPointsByLatencyType(t, histogram.DataPoints)

	enqueuedDP := dpByType["enqueue_to_response"]
	require.EqualValues(t, 1, enqueuedDP.Count)
	require.InDelta(t, float64(time.Since(enqueuedAt)/time.Millisecond), enqueuedDP.Sum, 250)

	pollDP := dpByType["poll_to_response"]
	require.EqualValues(t, 1, pollDP.Count)
	require.Greater(t, pollDP.Sum, 0.0)

	for _, dp := range []metricdata.HistogramDataPoint[float64]{enqueuedDP, pollDP} {
		requestKind, ok := dp.Attributes.Value(attribute.Key("request_kind"))
		require.True(t, ok)
		require.Equal(t, "call", requestKind.AsString())

		tunnelID, ok := dp.Attributes.Value(attribute.Key("tunnel_id"))
		require.True(t, ok)
		require.Equal(t, "test-tunnel", tunnelID.AsString())

		status, ok := dp.Attributes.Value(attribute.Key("tunnel_service_status"))
		require.True(t, ok)
		require.EqualValues(t, 0, status.AsInt64())
	}
}

func TestProcessorRecordsNotificationLatency(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	controlPlaneCfg := newTestControlPlaneConfig(t)
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
	responder := newRecordingResponder()

	transport := &stubForwardingTransport{
		conn: &stubForwardingConnection{
			responseHeaders: http.Header{},
		},
	}

	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, controlPlaneCfg, meterProvider)
	require.NoError(t, err)

	enqueuedAt := time.Now().Add(-500 * time.Millisecond)

	cmd := &fakePolledCommand{
		id:         types.RequestID("notification-latency"),
		message:    &jsonrpc.Request{Method: "notifications/initialized"},
		enqueuedAt: enqueuedAt,
		polledAt:   enqueuedAt,
		shardToken: "shard-notification-latency",
	}

	require.NoError(t, processor.Process(context.Background(), cmd))
	_ = responder.waitForResponse(t)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	histogram, ok := findHistogram(rm, metricNameCommandEndToEndLatency)
	require.True(t, ok, "command_end_to_end_latency_seconds metric not found")
	require.Len(t, histogram.DataPoints, 2)
	dpByType := dataPointsByLatencyType(t, histogram.DataPoints)

	enqueuedDP := dpByType["enqueue_to_response"]
	require.EqualValues(t, 1, enqueuedDP.Count)
	require.InDelta(t, float64(time.Since(enqueuedAt)/time.Millisecond), enqueuedDP.Sum, 250)

	pollDP := dpByType["poll_to_response"]
	require.EqualValues(t, 1, pollDP.Count)
	require.Greater(t, pollDP.Sum, 0.0)

	for _, dp := range []metricdata.HistogramDataPoint[float64]{enqueuedDP, pollDP} {
		requestKind, ok := dp.Attributes.Value(attribute.Key("request_kind"))
		require.True(t, ok)
		require.Equal(t, "notification", requestKind.AsString())

		tunnelID, ok := dp.Attributes.Value(attribute.Key("tunnel_id"))
		require.True(t, ok)
		require.Equal(t, "test-tunnel", tunnelID.AsString())

		status, ok := dp.Attributes.Value(attribute.Key("tunnel_service_status"))
		require.True(t, ok)
		require.EqualValues(t, 0, status.AsInt64())
	}
}

func TestProcessorConnectFailureDoesNotRecordLatency(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := newRecordingResponder()
	transport := &failingForwardingTransport{err: errors.New("connect failed")}

	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	cmd := &fakePolledCommand{
		id:         types.RequestID("connect-failure"),
		message:    &jsonrpc.Request{Method: "initialize"},
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
		shardToken: "shard-connect-failure",
	}

	err = processor.Process(context.Background(), cmd)
	require.Error(t, err)

	select {
	case resp := <-responder.responses:
		t.Fatalf("unexpected response posted: %+v", resp)
	default:
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	if histogram, ok := findHistogram(rm, metricNameCommandEndToEndLatency); ok {
		require.Len(t, histogram.DataPoints, 0, "latency metrics should not record on connect failure")
	}
}

func TestProcessorRequiresShardToken(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	responder := newRecordingResponder()
	transport := &stubForwardingTransport{conn: &stubForwardingConnection{}}

	meterProvider := newTestMeterProvider(t)
	processor, err := NewProcessor(logger, transport, responder, &config.MCPConfig{ConnectionMaxTTL: time.Second}, newTestControlPlaneConfig(t), meterProvider)
	require.NoError(t, err)

	cmd := &fakePolledCommand{
		id:         types.RequestID("missing-shard"),
		message:    &jsonrpc.Request{Method: "ping"},
		enqueuedAt: time.Now(),
		polledAt:   time.Now(),
	}

	err = processor.Process(context.Background(), cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing shard token")
}

type recordingResponder struct {
	responses chan tunnelResponse
}

type tunnelResponse struct {
	requestID                    types.RequestID
	response                     *types.TunnelResponse
	controlPlaneCommandRequestID string
}

func newRecordingResponder() *recordingResponder {
	return &recordingResponder{
		responses: make(chan tunnelResponse, 1),
	}
}

func (r *recordingResponder) PostResponse(ctx context.Context, requestID types.RequestID, response *types.TunnelResponse) (types.TunnelServiceRequestID, error) {
	var controlPlaneRequestID string
	if id, ok := tunnelctx.ControlPlaneCommandRequestIDFromContext(ctx); ok {
		controlPlaneRequestID = id.String()
	}

	select {
	case r.responses <- tunnelResponse{requestID: requestID, response: response, controlPlaneCommandRequestID: controlPlaneRequestID}:
		return "", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (r *recordingResponder) waitForResponse(t *testing.T) tunnelResponse {
	t.Helper()

	select {
	case resp := <-r.responses:
		return resp
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for response")
		return tunnelResponse{}
	}
}

func newTestMeterProvider(t *testing.T) *sdkmetric.MeterProvider {
	t.Helper()
	provider := sdkmetric.NewMeterProvider()
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	return provider
}

func newTestControlPlaneConfig(t *testing.T) *config.ControlPlaneConfig {
	t.Helper()
	return &config.ControlPlaneConfig{
		TunnelID: types.TunnelID("test-tunnel"),
	}
}

func findHistogram(rm metricdata.ResourceMetrics, name string) (metricdata.Histogram[float64], bool) {
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				return metricdata.Histogram[float64]{}, false
			}
			return histogram, true
		}
	}
	return metricdata.Histogram[float64]{}, false
}

func dataPointsByLatencyType(t *testing.T, dps []metricdata.HistogramDataPoint[float64]) map[string]metricdata.HistogramDataPoint[float64] {
	t.Helper()
	out := make(map[string]metricdata.HistogramDataPoint[float64])
	for _, dp := range dps {
		latencyType, ok := dp.Attributes.Value(attribute.Key("latency_type"))
		if !ok {
			continue
		}
		out[latencyType.AsString()] = dp
	}
	require.Contains(t, out, "enqueue_to_response")
	require.Contains(t, out, "poll_to_response")
	return out
}

type stubForwardingTransport struct {
	conn mcpclient.ForwardingConnection
}

func (s *stubForwardingTransport) Connect(context.Context) (mcpclient.ForwardingConnection, error) {
	return s.conn, nil
}

type failingForwardingTransport struct {
	err error
}

func (s *failingForwardingTransport) Connect(context.Context) (mcpclient.ForwardingConnection, error) {
	return nil, s.err
}

type stubForwardingConnection struct {
	responseHeaders http.Header
	response        jsonrpc.Message
}

func (c *stubForwardingConnection) Write(context.Context, http.Header, jsonrpc.Message) (int, http.Header, error) {
	return 0, c.responseHeaders, nil
}

func (c *stubForwardingConnection) Read(context.Context) (jsonrpc.Message, error) {
	if c.response == nil {
		return nil, io.EOF
	}
	msg := c.response
	c.response = nil
	return msg, nil
}

func (c *stubForwardingConnection) Close() error { return nil }

type fakePolledCommand struct {
	id         types.RequestID
	message    jsonrpc.Message
	enqueuedAt time.Time
	polledAt   time.Time
	headers    http.Header
	sessionID  *string
	shardToken string
}

func (f *fakePolledCommand) RequestID() types.RequestID {
	return f.id
}

func (f *fakePolledCommand) Message() jsonrpc.Message {
	return f.message
}

func (f *fakePolledCommand) EnqueuedAt() time.Time {
	return f.enqueuedAt
}

func (f *fakePolledCommand) PolledAt() time.Time {
	return f.polledAt
}

func (f *fakePolledCommand) Headers() http.Header {
	return f.headers
}

func (f *fakePolledCommand) ShardToken() string {
	return f.shardToken
}

func (f *fakePolledCommand) SessionID() (string, bool) {
	if f.sessionID == nil {
		return "", false
	}
	return *f.sessionID, true
}
