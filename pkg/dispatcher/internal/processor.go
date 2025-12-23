package dispatcherinternal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/controlplane"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
	"go.openai.org/api/tunnel-client/pkg/mcpclient"
	"go.openai.org/api/tunnel-client/pkg/tunnelctx"
	"go.openai.org/api/tunnel-client/pkg/types"
)

// Processor forwards polled control plane commands to the downstream MCP server.
type Processor interface {
	Process(ctx context.Context, cmd controlplane.PolledCommand) error
}

type mcpProcessor struct {
	logger           *slog.Logger
	transport        mcpclient.ForwardingTransport
	tunnelResponder  controlplane.Responder
	connectionMaxTTL time.Duration
	metrics          *processorMetrics
	tunnelID         types.TunnelID
}

type forwardedNotification struct {
	Kind   string          `json:"kind"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// NewProcessor constructs a Processor that uses the provided transport.
func NewProcessor(logger *slog.Logger, transport mcpclient.ForwardingTransport, tunnelResponder controlplane.Responder, mcpConfig *config.MCPConfig, controlPlaneCfg *config.ControlPlaneConfig, meterProvider *sdkmetric.MeterProvider) (Processor, error) {
	if logger == nil {
		return nil, fmt.Errorf("dispatcher processor: nil logger")
	}
	if transport == nil {
		return nil, fmt.Errorf("dispatcher processor: nil transport")
	}
	if tunnelResponder == nil {
		return nil, fmt.Errorf("dispatcher processor: nil responder")
	}
	if mcpConfig == nil {
		return nil, fmt.Errorf("dispatcher processor: nil MCP config")
	}
	if mcpConfig.ConnectionMaxTTL <= 0 {
		return nil, fmt.Errorf("dispatcher processor: non-positive MCP connection TTL")
	}
	if controlPlaneCfg == nil {
		return nil, fmt.Errorf("dispatcher processor: nil control-plane config")
	}
	if meterProvider == nil {
		return nil, fmt.Errorf("dispatcher processor: nil meter provider")
	}
	baseLogger := logger.With(tclog.FieldComponent, tclog.ComponentDispatcher)

	meter := meterProvider.Meter("dispatcher")
	processorMetrics, err := newProcessorMetrics(meter)
	if err != nil {
		return nil, fmt.Errorf("dispatcher processor: %w", err)
	}

	return &mcpProcessor{
		logger:           baseLogger,
		transport:        transport,
		tunnelResponder:  tunnelResponder,
		connectionMaxTTL: mcpConfig.ConnectionMaxTTL,
		metrics:          processorMetrics,
		tunnelID:         controlPlaneCfg.TunnelID,
	}, nil
}

// Process delivers the command to the MCP server and logs the response.
func (p *mcpProcessor) Process(ctx context.Context, cmd controlplane.PolledCommand) error {
	requestID := cmd.RequestID()
	ctx = tunnelctx.ContextWithRequestID(ctx, requestID.String())
	if controlPlaneRequestID, ok := types.NewControlPlaneRequestIDFromHeader(cmd.Headers()); ok {
		ctx = tunnelctx.ContextWithControlPlaneCommandRequestID(ctx, controlPlaneRequestID)
	}
	shardToken := cmd.ShardToken()
	if shardToken == "" {
		return fmt.Errorf("dispatcher processor: missing shard token for request %s", requestID)
	}
	ctx = tunnelctx.ContextWithShardToken(ctx, shardToken)
	if sessionID, ok := cmd.SessionID(); ok {
		ctx = tunnelctx.ContextWithSessionID(ctx, sessionID)
	}
	logger := tclog.LoggerWithContextIdentifiers(ctx, p.logger)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	switch typedCmd := cmd.(type) {
	case controlplane.JsonRpcCommand:
		return p.processJsonRpcCommand(ctx, logger, typedCmd)
	case controlplane.OauthDiscoveryCommand:
		return p.processOauthDiscoveryCommand(ctx, logger, typedCmd)
	default:
		logger.ErrorContext(ctx, "polled command was not a JSON-RPC command")
		return fmt.Errorf("unexpected command type %T", cmd)
	}
}

func (p *mcpProcessor) processJsonRpcCommand(ctx context.Context, logger *slog.Logger, cmd controlplane.JsonRpcCommand) error {
	requestID := cmd.RequestID()
	req, ok := cmd.Message().(*jsonrpc.Request)
	if !ok {
		logger.ErrorContext(ctx, "polled command payload was not a JSON-RPC request", slog.String("type", fmt.Sprintf("%T", cmd.Message())))
		return fmt.Errorf("unexpected command type %T", cmd.Message())
	}

	// Establish MCP connection only for JSON-RPC commands.
	conn, err := p.transport.Connect(ctx)
	if err != nil {
		logger.ErrorContext(ctx, "failed to connect to MCP transport", slog.String("error", err.Error()))
		return fmt.Errorf("connect: %w", err)
	}

	isNotification := !req.ID.IsValid()
	if !isNotification {
		ctx = tunnelctx.ContextWithRPCRequestID(ctx, req.ID)
		logger = tclog.LoggerWithContextIdentifiers(ctx, p.logger)
	}

	requestKindAttrs := requestKindAttributes(req)
	latencyRecorded := &latencyFlags{}

	//TODO(denyska): upon receiving SessionTermination command, issue conn.Close() that will do DELETE

	statusCode, respHeader, err := conn.Write(ctx, cmd.Headers(), req)
	if err != nil {
		logger.ErrorContext(ctx, "failed to forward command to MCP server", slog.String("error", err.Error()))
		return fmt.Errorf("write: %w", err)
	}

	if _, ok := tunnelctx.SessionIDFromContext(ctx); !ok {
		if headerSession := mcpclient.SessionIDFromHeaders(respHeader); headerSession != nil {
			ctx = tunnelctx.ContextWithSessionID(ctx, *headerSession)
			logger = tclog.LoggerWithContextIdentifiers(ctx, p.logger)
		}
	}

	if isNotification {
		logger.DebugContext(ctx, "dispatcher forwarded notification to MCP server; acknowledging without waiting for response. conn.Write returned w/o error")

		if tsRequestID, err := p.tunnelResponder.PostResponse(ctx, requestID, types.NewNotificationAck(statusCode, respHeader)); err != nil {
			attrs := []any{slog.String("error", err.Error())}
			if tsRequestID != "" {
				attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tsRequestID.String()))
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				logger.WarnContext(ctx, "context canceled while acknowledging notification", attrs...)
			} else {
				logger.ErrorContext(ctx, "failed to acknowledge notification with control plane", attrs...)
			}
			return err
		}

		p.metrics.recordCommandLatencies(ctx, p.tunnelID, statusCode, requestKindAttrs, cmd.EnqueuedAt(), cmd.PolledAt(), latencyRecorded)
		logger.InfoContext(ctx, "dispatcher acknowledged notification with control plane")
		return nil
	}

	p.forwardResponses(ctx, conn, logger, cmd, statusCode, respHeader, requestKindAttrs, latencyRecorded)
	logger.InfoContext(ctx, "dispatcher forwarded command to MCP server")

	return nil
}

func (p *mcpProcessor) processOauthDiscoveryCommand(ctx context.Context, logger *slog.Logger, _ controlplane.OauthDiscoveryCommand) error {
	logger.WarnContext(ctx, "polled command was an OAuth discovery command; dispatcher does not support it yet")
	return fmt.Errorf("unsupported command type oauth_discovery")
}

// forwardResponses streams MCP responses for the request to the control plane
// while respecting the configured TTL window.
func (p *mcpProcessor) forwardResponses(ctx context.Context, conn mcpclient.ForwardingConnection, logger *slog.Logger, cmd controlplane.JsonRpcCommand, responseCode int, responseHeaders http.Header, metricAttrs []attribute.KeyValue, latencyRecorded *latencyFlags) {
	ttlCtx := ctx
	cancel := func() {}
	if p.connectionMaxTTL > 0 {
		ttlCtx, cancel = context.WithTimeout(ctx, p.connectionMaxTTL)
	}
	defer cancel()

	req := cmd.Message().(*jsonrpc.Request)

	for {
		msg, readErr := conn.Read(ttlCtx)
		if readErr != nil {
			switch {
			case errors.Is(readErr, mcp.ErrConnectionClosed) || errors.Is(readErr, io.EOF):
				logger.DebugContext(ctx, "MCP connection closed while reading response", slog.String("error", readErr.Error()))
			case errors.Is(readErr, context.DeadlineExceeded), errors.Is(readErr, context.Canceled):
				if errors.Is(ttlCtx.Err(), context.DeadlineExceeded) {
					logger.InfoContext(ctx, "MCP connection TTL reached; stopping response forwarding")
				} else {
					logger.DebugContext(ctx, "MCP connection context canceled while reading response", slog.String("error", readErr.Error()))
				}
			default:
				logger.ErrorContext(ctx, "failed to read response from MCP server", slog.String("error", readErr.Error()))
			}
			return
		}
		if msg == nil {
			continue
		}

		// TODO(denyska): Implement relaying of notifications back to the tunnel-service for long-running requests.
		// See specifications:
		// - JSON-RPC Notification: https://www.jsonrpc.org/specification#notification
		// - MCP Basic Spec: https://modelcontextprotocol.io/specification/2025-06-18/basic
		// Note: A notification is a request that does not include an ID.
		// Notifications are primarily used to provide updates on the progress or status
		// of long-running requests initiated by the client.
		// For simplicity, notification handling is currently not supported.
		if reqMsg, ok := msg.(*jsonrpc.Request); ok {
			if !reqMsg.ID.IsValid() {
				logger.DebugContext(
					ctx,
					"dispatcher received notification from MCP server. ignoring",
					attrsToArgs(messageSummaryAttrs(reqMsg))...,
				)
				continue
			}
		}

		response, ok := msg.(*jsonrpc.Response)
		if !ok {
			logger.ErrorContext(
				ctx,
				"received non-response message from MCP server",
				append(attrsToArgs(messageSummaryAttrs(msg)), slog.String("type", fmt.Sprintf("%T", msg)))...,
			)
			return
		}

		logger.DebugContext(ctx, "dispatcher received response from MCP server", attrsToArgs(messageSummaryAttrs(response))...)

		// per https://modelcontextprotocol.io/specification/2025-06-18/basic ,
		// Responses MUST include the same ID as the request they correspond to.
		// Notifications MUST NOT include an ID.
		// streamableClientConn.processStream has similar heuristics comparing req/resp IDs and breaking out
		finalResponse := response.ID.IsValid() && response.ID == req.ID
		if !finalResponse {
			logger.ErrorContext(ctx, "Received response without valid ID")
			return
		}

		// TODO(denyska): Implement handling of notifications from MCP server.
		// Until we support streaming notifications, we drop `text/event-stream` updates entirely, so propagating the upstream Content-Type would lie to the tunnel service.
		// Force any non-empty Content-Type header to `application/json` so the control plane only sees formats we truly deliver.
		if responseHeaders.Get("Content-Type") != "" {
			logger.DebugContext(ctx, "overriding Content-Type header", slog.String("original", responseHeaders.Get("Content-Type")), slog.String("new", "application/json"))
			responseHeaders = responseHeaders.Clone()
			responseHeaders.Set("Content-Type", "application/json")
		}

		tunnelResponse := types.NewTunnelResponse(response, responseCode, responseHeaders)
		if tunnelResponse.JSONRPC() == nil {
			logger.ErrorContext(ctx, "tunnel response missing JSON-RPC payload")
			return
		}

		if tsRequestID, err := p.tunnelResponder.PostResponse(ttlCtx, cmd.RequestID(), tunnelResponse); err != nil {
			attrs := []any{slog.String("error", err.Error())}
			if tsRequestID != "" {
				attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tsRequestID.String()))
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				if errors.Is(ttlCtx.Err(), context.DeadlineExceeded) {
					logger.InfoContext(ctx, "MCP connection TTL reached while delivering response", attrs...)
				} else {
					logger.DebugContext(ctx, "MCP connection context canceled while delivering response", attrs...)
				}
			} else {
				logger.ErrorContext(ctx, "failed to post response to control plane", attrs...)
			}
			return
		}

		p.metrics.recordCommandLatencies(ctx, p.tunnelID, responseCode, metricAttrs, cmd.EnqueuedAt(), cmd.PolledAt(), latencyRecorded)
		logger.DebugContext(ctx, "dispatcher delivered response to control plane", slog.Bool("finalResponse", finalResponse))
		return
	}
}

func messageSummaryAttrs(msg jsonrpc.Message) []slog.Attr {
	switch m := msg.(type) {
	case *jsonrpc.Request:
		attrs := []slog.Attr{
			slog.String("message_kind", "request"),
			slog.String("method", m.Method),
			slog.Bool("is_call", m.ID.IsValid()),
		}
		if m.ID.IsValid() {
			attrs = append(attrs, slog.String("id", fmt.Sprint(m.ID.Raw())))
		}
		return attrs
	case *jsonrpc.Response:
		attrs := []slog.Attr{
			slog.String("message_kind", "response"),
			slog.Bool("has_error", m.Error != nil),
		}
		if m.ID.IsValid() {
			attrs = append(attrs, slog.String("id", fmt.Sprint(m.ID.Raw())))
		}
		return attrs
	default:
		return []slog.Attr{
			slog.String("message_kind", fmt.Sprintf("unknown:%T", msg)),
		}
	}
}

func attrsToArgs(attrs []slog.Attr) []any {
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	return args
}

func requestKindAttributes(req *jsonrpc.Request) []attribute.KeyValue {
	if req == nil {
		return nil
	}
	kind := "call"
	if !req.ID.IsValid() {
		kind = "notification"
	}

	attrs := []attribute.KeyValue{
		attribute.String("request_kind", kind),
	}
	if req.Method != "" {
		attrs = append(attrs, attribute.String("request_method", req.Method))
	}
	return attrs
}
