package log

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/tunnelctx"
)

const (
	// FieldComponent is the structured logging key used to describe the emitting sub-component.
	FieldComponent = "component"

	// FieldRequestID is the structured logging key for the MCP request identifier. This id assigned by control plane.
	FieldRequestID = "request_id"

	// FieldRpcRequestID is a Request identifier, which is defined by the spec to be a string, integer, or null.
	// https://www.jsonrpc.org/specification#request_object
	FieldRpcRequestID = "rpc_request_id"

	// FieldSessionID is the structured logging key that records the MCP session identifier.
	FieldSessionID = "session_id"

	// FieldControlPlaneCommandRequestID captures the upstream X-Request-Id issued by
	// plugin-service/connectors while they communicate with tunnel-service.
	FieldControlPlaneCommandRequestID = "cmd_request_id"

	// FieldTunnelServiceRequestID captures the X-Request-Id returned when the tunnel-client
	// talks directly to tunnel-service (e.g. poll, post response).
	FieldTunnelServiceRequestID = "tunnel_request_id"

	ComponentHealth       = "health"
	ComponentDispatcher   = "dispatcher"
	ComponentControlPlane = "controlplane"
	ComponentMcpClient    = "mcpclient"
	ComponentProcess      = "process"
)

// NewLogger constructs a slog.Logger configured according to the provided config.
// It returns the logger along with an optional closer that must be closed by the caller.
func NewLogger(cfg *config.LoggingConfig, defaultWriter io.Writer) (*slog.Logger, io.Closer, error) {
	if cfg == nil {
		return nil, nil, fmt.Errorf("logging config is nil")
	}

	var logger *slog.Logger
	writer := defaultWriter
	var closer io.Closer

	if cfg.Format == config.LogFormatUnset {
		logger = slog.Default()
		if cfg.File != "" {
			return nil, nil, fmt.Errorf("invalid logging configuration: file is only supported for json or struct-text")
		}
	} else {

		if cfg.File != "" {
			f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, nil, fmt.Errorf("open log file: %w", err)
			}
			writer = f
			closer = f
		}

		if writer == nil {
			writer = os.Stdout
		}

		handlerOpts := buildHandlerOptions(cfg.Level)
		handler, err := buildHandler(cfg.Format, writer, handlerOpts)
		if err != nil {
			CloseIfNeeded(closer)
			return nil, nil, err
		}

		logger = slog.New(handler)
	}

	if cfg.HTTPRawUnsafe {
		logger.Warn("\u26a0\ufe0f  WARNING: Raw HTTP logging enabled \u2014 sensitive data may be exposed")
	}

	return logger, closer, nil
}

// CloseIfNeeded closes the provided closer if it is non-nil, ignoring already-closed errors.
func CloseIfNeeded(closer io.Closer) {
	if closer == nil {
		return
	}
	if err := closer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		slog.Error("close log file", slog.String("error", err.Error()))
	}
}

func buildHandlerOptions(level slog.Level) *slog.HandlerOptions {
	return &slog.HandlerOptions{Level: level}
}

func buildHandler(format config.LogFormat, writer io.Writer, opts *slog.HandlerOptions) (slog.Handler, error) {
	switch format {
	case config.LogFormatJSON:
		return slog.NewJSONHandler(writer, opts), nil
	case config.LogFormatStructText:
		return slog.NewTextHandler(writer, opts), nil
	default:
		return nil, fmt.Errorf("unsupported log format %q", format.String())
	}
}

// LoggerWithContextIdentifiers returns a logger annotated with any identifiers stored on ctx.
func LoggerWithContextIdentifiers(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return nil
	}
	if requestID, ok := tunnelctx.RequestIDFromContext(ctx); ok {
		logger = logger.With(slog.String(FieldRequestID, requestID))
	}
	if controlPlaneRequestID, ok := tunnelctx.ControlPlaneCommandRequestIDFromContext(ctx); ok {
		logger = logger.With(slog.String(FieldControlPlaneCommandRequestID, controlPlaneRequestID.String()))
	}
	if tunnelServiceRequestID, ok := tunnelctx.TunnelServiceRequestIDFromContext(ctx); ok {
		logger = logger.With(slog.String(FieldTunnelServiceRequestID, tunnelServiceRequestID.String()))
	}
	if rpcRequestID, ok := tunnelctx.RPCRequestIDFromContext(ctx); ok {
		logger = logger.With(rpcRequestIDAttr(rpcRequestID))
	}
	if sessionID, ok := tunnelctx.SessionIDFromContext(ctx); ok {
		logger = logger.With(slog.String(FieldSessionID, sessionID))
	}
	return logger
}

func rpcRequestIDAttr(id jsonrpc.ID) slog.Attr {
	switch v := id.Raw().(type) {
	case string:
		return slog.String(FieldRpcRequestID, v)
	case int64:
		return slog.Int64(FieldRpcRequestID, v)
	default:
		return slog.String(FieldRpcRequestID, fmt.Sprint(v))
	}
}
