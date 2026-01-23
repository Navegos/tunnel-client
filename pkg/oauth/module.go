package oauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/config"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
)

// Module wires OAuth discovery state and fetcher.
var Module = fx.Module(
	"oauth",
	fx.Provide(NewDiscoveryState),
	fx.Invoke(startOAuthDiscovery),
)

type discoveryParams struct {
	fx.In

	Lifecycle  fx.Lifecycle
	Logger     *slog.Logger
	MCPConfig  *config.MCPConfig
	HTTPClient *http.Client `name:"mcp_client"`
	State      *DiscoveryState
}

func startOAuthDiscovery(p discoveryParams) error {
	if p.Lifecycle == nil {
		return fmt.Errorf("oauth discovery: lifecycle is required")
	}
	if p.MCPConfig == nil {
		return fmt.Errorf("oauth discovery: mcp config is required")
	}
	if p.State == nil {
		return fmt.Errorf("oauth discovery: state is required")
	}
	if p.HTTPClient == nil {
		return fmt.Errorf("oauth discovery: http client is required")
	}

	logger := p.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With(tclog.FieldComponent, "oauth")

	transportKind := p.MCPConfig.TransportKind
	if transportKind == "" {
		transportKind = config.MCPTransportHTTPStreamable
	}
	urls := p.MCPConfig.OAuthResourceMetadataURLs

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if transportKind != config.MCPTransportHTTPStreamable || len(urls) == 0 {
				reason := fmt.Sprintf("oauth discovery disabled for transport %q", transportKind)
				if len(urls) == 0 {
					reason = "oauth discovery URLs are not configured"
				}
				p.State.Set(nil, errors.New(reason))
				logger.DebugContext(ctx, reason)
				return nil
			}

			go func() {
				fetchCtx, cancel := context.WithTimeout(context.Background(), DefaultDiscoveryTimeout)
				defer cancel()

				start := time.Now()
				resp, sourceURL, err := FetchOAuthMetadata(fetchCtx, p.HTTPClient, urls, logger)
				if err != nil {
					p.State.Set(nil, err)
					logger.WarnContext(fetchCtx, "oauth discovery failed", slog.String("error", err.Error()))
					return
				}
				result := BuildDiscoveryResult(resp, sourceURL, start)
				p.State.Set(result, nil)
				logger.InfoContext(fetchCtx, "oauth discovery metadata fetched",
					slog.Int("status_code", resp.ResponseCode()),
					slog.Duration("latency", time.Since(start)),
				)
			}()

			return nil
		},
	})

	return nil
}
