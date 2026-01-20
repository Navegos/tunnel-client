package dispatcherinternal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/controlplane"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
	"go.openai.org/api/tunnel-client/pkg/mcpclient"
	"go.openai.org/api/tunnel-client/pkg/tunnelctx"
	"go.openai.org/api/tunnel-client/pkg/types"
)

const (
	defaultOAuthDiscoveryTimeout = 5 * time.Second
	defaultAcceptHeaderValue     = "application/json, text/event-stream"
)

// Processor forwards polled control plane commands to the downstream MCP server.
type Processor interface {
	Process(ctx context.Context, cmd controlplane.PolledCommand) error
}

type processorParams struct {
	fx.In

	Logger          *slog.Logger
	Transport       mcpclient.ForwardingTransport
	TunnelResponder controlplane.Responder
	MCPConfig       *config.MCPConfig
	OAuthHTTPClient *http.Client `name:"mcp_client"`
	ControlPlaneCfg *config.ControlPlaneConfig
	MeterProvider   *sdkmetric.MeterProvider
}

type mcpProcessor struct {
	logger            *slog.Logger
	transport         mcpclient.ForwardingTransport
	tunnelResponder   controlplane.Responder
	connectionMaxTTL  time.Duration
	metrics           *processorMetrics
	tunnelID          types.TunnelID
	oauthHTTPClient   *http.Client
	oauthMetadataURLs []*url.URL
}

// NewProcessor constructs a Processor that uses the provided transport.
func NewProcessor(p processorParams) (Processor, error) {
	if p.Logger == nil {
		return nil, fmt.Errorf("dispatcher processor: nil logger")
	}
	if p.Transport == nil {
		return nil, fmt.Errorf("dispatcher processor: nil transport")
	}
	if p.TunnelResponder == nil {
		return nil, fmt.Errorf("dispatcher processor: nil responder")
	}
	if p.MCPConfig == nil {
		return nil, fmt.Errorf("dispatcher processor: nil MCP config")
	}
	if p.MCPConfig.ConnectionMaxTTL <= 0 {
		return nil, fmt.Errorf("dispatcher processor: non-positive MCP connection TTL")
	}
	if p.ControlPlaneCfg == nil {
		return nil, fmt.Errorf("dispatcher processor: nil control-plane config")
	}
	if p.MeterProvider == nil {
		return nil, fmt.Errorf("dispatcher processor: nil meter provider")
	}
	if p.OAuthHTTPClient == nil {
		return nil, fmt.Errorf("dispatcher processor: nil oauth http client")
	}

	baseLogger := p.Logger.With(tclog.FieldComponent, tclog.ComponentDispatcher)

	meter := p.MeterProvider.Meter("dispatcher")
	processorMetrics, err := newProcessorMetrics(meter)
	if err != nil {
		return nil, fmt.Errorf("dispatcher processor: %w", err)
	}

	transportKind := p.MCPConfig.TransportKind
	if transportKind == "" {
		transportKind = config.MCPTransportHTTPStreamable
	}
	metadataURLs := p.MCPConfig.OAuthResourceMetadataURLs
	if len(metadataURLs) == 0 && transportKind == config.MCPTransportHTTPStreamable {
		return nil, fmt.Errorf("dispatcher processor: missing OAuth resource metadata URLs")
	}

	return &mcpProcessor{
		logger:            baseLogger,
		transport:         p.Transport,
		tunnelResponder:   p.TunnelResponder,
		connectionMaxTTL:  p.MCPConfig.ConnectionMaxTTL,
		metrics:           processorMetrics,
		tunnelID:          p.ControlPlaneCfg.TunnelID,
		oauthHTTPClient:   p.OAuthHTTPClient,
		oauthMetadataURLs: metadataURLs,
	}, nil
}

// Process delivers the command to the MCP server and logs the response.
func (p *mcpProcessor) Process(ctx context.Context, cmd controlplane.PolledCommand) error {
	if cmd == nil {
		return fmt.Errorf("dispatcher processor: nil command")
	}

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

	headers := ensureDefaultAcceptHeader(cmd.Headers())
	statusCode, respHeader, err := conn.Write(ctx, headers, req)
	statusCode = normalizeTransportStatusCode(statusCode, err)
	if err != nil || statusCode >= http.StatusBadRequest {
		status := statusCode
		encodedError, encodeErr := buildJSONRPCErrorResponse(req, status, err)
		if encodeErr != nil {
			logger.ErrorContext(ctx, "failed to encode MCP error response", slog.String("error", encodeErr.Error()))
			return fmt.Errorf("encode error response: %w", encodeErr)
		}

		if respHeader == nil {
			respHeader = http.Header{}
		}
		if respHeader.Get("Content-Type") == "" {
			respHeader = respHeader.Clone()
			respHeader.Set("Content-Type", "application/json")
		}

		tunnelResponse := types.NewTunnelResponse(encodedError, status, respHeader)
		if tsRequestID, postErr := p.tunnelResponder.PostResponse(ctx, requestID, tunnelResponse); postErr != nil {
			attrs := []any{slog.String("error", postErr.Error())}
			if tsRequestID != "" {
				attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tsRequestID.String()))
			}
			logger.ErrorContext(ctx, "failed to post error response to control plane", attrs...)
			return postErr
		}

		p.metrics.recordCommandLatencies(ctx, p.tunnelID, status, requestKindAttrs, cmd.EnqueuedAt(), cmd.PolledAt(), latencyRecorded)
		logger.WarnContext(ctx, "dispatcher received error from MCP server", slog.Int("status_code", status))
		return nil
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

func (p *mcpProcessor) processOauthDiscoveryCommand(ctx context.Context, logger *slog.Logger, cmd controlplane.OauthDiscoveryCommand) error {
	if len(p.oauthMetadataURLs) == 0 {
		return fmt.Errorf("dispatcher processor: missing oauth metadata URLs")
	}

	resp, err := fetchOAuthMetadata(ctx, p.oauthHTTPClient, p.oauthMetadataURLs, logger)
	if err != nil {
		logger.ErrorContext(ctx, "failed to fetch oauth discovery metadata", slog.String("error", err.Error()))
		return err
	}

	tsRequestID, postErr := p.tunnelResponder.PostResponse(ctx, cmd.RequestID(), resp)
	if postErr != nil {
		attrs := []any{slog.String("error", postErr.Error())}
		if tsRequestID != "" {
			attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tsRequestID.String()))
		}
		if errors.Is(postErr, context.DeadlineExceeded) || errors.Is(postErr, context.Canceled) {
			logger.WarnContext(ctx, "context canceled while posting oauth discovery response", attrs...)
		} else {
			logger.ErrorContext(ctx, "failed to post oauth discovery response to control plane", attrs...)
		}
		return postErr
	}

	latencyRecorded := &latencyFlags{}
	metricAttrs := []attribute.KeyValue{
		attribute.String("request_kind", "oauth_discovery"),
	}
	p.metrics.recordCommandLatencies(ctx, p.tunnelID, resp.ResponseCode(), metricAttrs, cmd.EnqueuedAt(), cmd.PolledAt(), latencyRecorded)

	logger.InfoContext(ctx, "dispatcher delivered oauth discovery response to control plane",
		slog.Int("status_code", resp.ResponseCode()))
	return nil
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
			// Defensive: a nil message without an error would otherwise spin forever.
			logger.ErrorContext(ctx, "received nil message from MCP server without error")
			return
		}

		if notifyMsg, ok := asNotification(msg); ok {
			if err := p.forwardNotification(ctx, logger, cmd, responseCode, responseHeaders, notifyMsg); err != nil {
				return
			}
			continue
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

		encodedResponse, err := jsonrpc.EncodeMessage(response)
		if err != nil || len(encodedResponse) == 0 {
			logger.ErrorContext(ctx, "failed to encode response from MCP server", slog.String("error", err.Error()))
			return
		}

		// per https://modelcontextprotocol.io/specification/2025-06-18/basic ,
		// Responses MUST include the same ID as the request they correspond to.
		// Notifications MUST NOT include an ID.
		// streamableClientConn.processStream has similar heuristics comparing req/resp IDs and breaking out
		finalResponse := response.ID.IsValid() && response.ID == req.ID
		if !finalResponse {
			logger.ErrorContext(ctx, "Received response without valid ID")
			return
		}

		// Ensure final JSON-RPC responses present as application/json to the control plane,
		// even if the upstream server labeled them differently, unless the upstream
		// response is already an SSE stream.
		contentType := ""
		if responseHeaders != nil {
			contentType = responseHeaders.Get("Content-Type")
		}
		if contentType == "" || !isSSEContentType(contentType) {
			if responseHeaders == nil {
				responseHeaders = http.Header{}
			} else {
				responseHeaders = responseHeaders.Clone()
			}
			originalValue := contentType
			if originalValue == "" {
				originalValue = "<empty>"
			}
			logger.DebugContext(ctx, "overriding Content-Type header", slog.String("original", originalValue), slog.String("new", "application/json"))
			responseHeaders.Set("Content-Type", "application/json")
		}

		tunnelResponse := types.NewTunnelResponse(encodedResponse, responseCode, responseHeaders)

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

func (p *mcpProcessor) forwardNotification(ctx context.Context, logger *slog.Logger, cmd controlplane.JsonRpcCommand, responseCode int, responseHeaders http.Header, notifyMsg *jsonrpc.Request) error {
	logger.DebugContext(
		ctx,
		"dispatcher received notification from MCP server; forwarding to control plane",
		attrsToArgs(messageSummaryAttrs(notifyMsg))...,
	)

	encodedNotification, err := jsonrpc.EncodeMessage(notifyMsg)
	if err != nil || len(encodedNotification) == 0 {
		logger.ErrorContext(ctx, "failed to encode notification from MCP server", slog.String("error", err.Error()))
		return err
	}

	notificationHeaders := responseHeaders
	if notificationHeaders == nil {
		notificationHeaders = http.Header{}
	} else {
		notificationHeaders = notificationHeaders.Clone()
	}
	if notificationHeaders.Get("Content-Type") == "" {
		notificationHeaders.Set("Content-Type", "text/event-stream")
	}

	tunnelNotification := types.NewJSONRPCNotification(encodedNotification, responseCode, notificationHeaders)
	if tsRequestID, err := p.tunnelResponder.PostResponse(ctx, cmd.RequestID(), tunnelNotification); err != nil {
		attrs := []any{slog.String("error", err.Error())}
		if tsRequestID != "" {
			attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tsRequestID.String()))
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			logger.WarnContext(ctx, "context canceled while forwarding notification to control plane", attrs...)
		} else {
			logger.ErrorContext(ctx, "failed to forward notification to control plane", attrs...)
		}
		return err
	}

	return nil
}

// asNotification returns the request when the message is a JSON-RPC notification (request without an ID).
func asNotification(msg jsonrpc.Message) (*jsonrpc.Request, bool) {
	req, ok := msg.(*jsonrpc.Request)
	if !ok || req == nil {
		return nil, false
	}
	if req.IsCall() {
		return nil, false
	}
	return req, true
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

func isSSEContentType(value string) bool {
	if value == "" {
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "text/event-stream")
}

func buildJSONRPCErrorResponse(req *jsonrpc.Request, statusCode int, cause error) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("nil request provided to build error response")
	}
	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}
	message := http.StatusText(statusCode)
	if message == "" {
		message = "mcp transport error"
	}
	if cause != nil {
		message = fmt.Sprintf("%s: %v", message, cause)
	}
	resp := &jsonrpc.Response{
		ID: req.ID,
		Error: &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: message,
		},
	}
	return jsonrpc.EncodeMessage(resp)
}

func ensureDefaultAcceptHeader(headers http.Header) http.Header {
	if headers == nil {
		headers = http.Header{}
	}
	if headers.Get("Accept") != "" {
		return headers
	}
	clone := headers.Clone()
	clone.Set("Accept", defaultAcceptHeaderValue)
	return clone
}

func normalizeTransportStatusCode(statusCode int, err error) int {
	if statusCode != 0 {
		return statusCode
	}
	if err != nil {
		return http.StatusBadGateway
	}
	return http.StatusOK
}

func requestKindAttributes(req *jsonrpc.Request) []attribute.KeyValue {
	if req == nil {
		return nil
	}
	kind := "call"
	if !req.IsCall() {
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
