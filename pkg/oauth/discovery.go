package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"go.openai.org/api/tunnel-client/pkg/types"
	"go.openai.org/api/tunnel-client/pkg/version"
)

// FetchOAuthMetadata attempts to retrieve OAuth ProtectedResourceMetaData
// from the provided candidates. It returns the first successful response
// with a non-empty body, falling back on 5xx/404 responses and network errors
// until all options are exhausted.
func FetchOAuthMetadata(
	ctx context.Context,
	client *http.Client,
	candidates []DiscoveryCandidate,
	logger *slog.Logger,
) (*types.TunnelResponse, *url.URL, []DiscoveryAttempt, error) {
	if client == nil {
		return nil, nil, nil, fmt.Errorf("oauth discovery: http client is nil")
	}
	filtered := make([]DiscoveryCandidate, 0, len(candidates))
	attempts := make([]DiscoveryAttempt, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == nil {
			continue
		}
		urlStr := candidate.URL.String()
		if urlStr == "" {
			continue
		}
		filtered = append(filtered, candidate)
		attempts = append(attempts, DiscoveryAttempt{
			URL:    urlStr,
			Source: candidate.Source,
		})
	}
	if len(filtered) == 0 {
		return nil, nil, attempts, fmt.Errorf("oauth discovery: no metadata URLs configured")
	}

	var lastErr error
	for i, candidate := range filtered {
		attempts[i].Tried = true
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.URL.String(), nil)
		if err != nil {
			lastErr = err
			attempts[i].Error = err.Error()
			continue
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", version.UserAgent)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("oauth discovery GET %s: %w", candidate.URL.String(), err)
			attempts[i].Error = lastErr.Error()
			if logger != nil {
				logger.WarnContext(ctx, "oauth discovery request failed", slog.String("url", candidate.URL.String()), slog.String("error", err.Error()))
			}
			continue
		}

		attempts[i].StatusCode = resp.StatusCode
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("oauth discovery read %s: %w", candidate.URL.String(), readErr)
			attempts[i].Error = lastErr.Error()
			if resp.StatusCode >= 500 && i+1 < len(filtered) {
				if logger != nil {
					logger.DebugContext(ctx, "oauth discovery retrying after read failure", slog.String("url", candidate.URL.String()), slog.Int("status", resp.StatusCode))
				}
				continue
			}
			return nil, nil, attempts, lastErr
		}

		if len(body) == 0 {
			lastErr = fmt.Errorf("oauth discovery empty body from %s (status %d)", candidate.URL.String(), resp.StatusCode)
			attempts[i].Error = lastErr.Error()
			if resp.StatusCode >= 500 && i+1 < len(filtered) {
				if logger != nil {
					logger.DebugContext(ctx, "oauth discovery retrying after empty body", slog.String("url", candidate.URL.String()), slog.Int("status", resp.StatusCode))
				}
				continue
			}
			return nil, nil, attempts, lastErr
		}

		// Retry on known fallback-friendly statuses.
		if (resp.StatusCode == http.StatusNotFound || resp.StatusCode >= 500) && i+1 < len(filtered) {
			if logger != nil {
				logger.DebugContext(ctx, "oauth discovery received fallback-eligible status, trying next candidate", slog.String("url", candidate.URL.String()), slog.Int("status", resp.StatusCode))
			}
			lastErr = fmt.Errorf("oauth discovery status %d from %s", resp.StatusCode, candidate.URL.String())
			attempts[i].Error = lastErr.Error()
			continue
		}

		if err := validateProtectedResourceMetadata(body); err != nil {
			lastErr = fmt.Errorf("oauth discovery invalid metadata from %s: %w", candidate.URL.String(), err)
			attempts[i].Error = lastErr.Error()
			if resp.StatusCode >= 500 && i+1 < len(filtered) {
				if logger != nil {
					logger.DebugContext(ctx, "oauth discovery retrying after invalid metadata",
						slog.String("url", candidate.URL.String()),
						slog.Int("status", resp.StatusCode),
					)
				}
				continue
			}
			return nil, nil, attempts, lastErr
		}

		attempts[i].Selected = true
		return types.NewOAuthDiscoveryResponse(body, resp.StatusCode, resp.Header), candidate.URL, attempts, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("oauth discovery: no responses")
	}
	return nil, nil, attempts, lastErr
}

func validateProtectedResourceMetadata(body []byte) error {
	var metadata oauthex.ProtectedResourceMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return fmt.Errorf("decode protected resource metadata: %w", err)
	}
	if metadata.Resource == "" {
		return fmt.Errorf("protected resource metadata missing resource")
	}
	for _, server := range metadata.AuthorizationServers {
		_, err := url.Parse(server)
		if err != nil {
			return fmt.Errorf("parse authorization server %q: %w", server, err)
		}
	}
	return nil
}
