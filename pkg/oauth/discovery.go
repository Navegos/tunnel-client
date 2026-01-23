package oauth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"go.openai.org/api/tunnel-client/pkg/types"
	"go.openai.org/api/tunnel-client/pkg/version"
)

// FetchOAuthMetadata attempts to retrieve OAuth protected resource metadata
// from the provided candidates (path-specific first, then root). It returns
// the first successful response with a non-empty body, falling back on 5xx/404
// responses and network errors until all options are exhausted.
func FetchOAuthMetadata(ctx context.Context, client *http.Client, metadataURLs []*url.URL, logger *slog.Logger) (*types.TunnelResponse, *url.URL, error) {
	if client == nil {
		return nil, nil, fmt.Errorf("oauth discovery: http client is nil")
	}
	if len(metadataURLs) == 0 {
		return nil, nil, fmt.Errorf("oauth discovery: no metadata URLs configured")
	}

	var lastErr error
	for i, u := range metadataURLs {
		if u == nil {
			continue
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", version.UserAgent)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("oauth discovery GET %s: %w", u.String(), err)
			if logger != nil {
				logger.WarnContext(ctx, "oauth discovery request failed", slog.String("url", u.String()), slog.String("error", err.Error()))
			}
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("oauth discovery read %s: %w", u.String(), readErr)
			if resp.StatusCode >= 500 && i+1 < len(metadataURLs) {
				if logger != nil {
					logger.DebugContext(ctx, "oauth discovery retrying after read failure", slog.String("url", u.String()), slog.Int("status", resp.StatusCode))
				}
				continue
			}
			return nil, nil, lastErr
		}

		if len(body) == 0 {
			lastErr = fmt.Errorf("oauth discovery empty body from %s (status %d)", u.String(), resp.StatusCode)
			if resp.StatusCode >= 500 && i+1 < len(metadataURLs) {
				if logger != nil {
					logger.DebugContext(ctx, "oauth discovery retrying after empty body", slog.String("url", u.String()), slog.Int("status", resp.StatusCode))
				}
				continue
			}
			return nil, nil, lastErr
		}

		// Retry on known fallback-friendly statuses.
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500) && i+1 < len(metadataURLs) {
			if logger != nil {
				logger.DebugContext(ctx, "oauth discovery received fallback-eligible status, trying next candidate", slog.String("url", u.String()), slog.Int("status", resp.StatusCode))
			}
			lastErr = fmt.Errorf("oauth discovery status %d from %s", resp.StatusCode, u.String())
			continue
		}

		return types.NewOAuthDiscoveryResponse(body, resp.StatusCode, resp.Header), u, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("oauth discovery: no responses")
	}
	return nil, nil, lastErr
}
