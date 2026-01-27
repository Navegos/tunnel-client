package adminui

import (
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/oauth"
)

func TestBuildOAuthStatusWithAttempts(t *testing.T) {
	t.Parallel()

	serverURL := mustParseURL(t, "https://example.com/public/mcp")
	state := oauth.NewDiscoveryState()
	state.Set(&oauth.DiscoveryResult{
		Attempts: []oauth.DiscoveryAttempt{{
			URL:    "https://example.com/.well-known/oauth-protected-resource/public/mcp",
			Source: oauth.DiscoverySourceWellKnownPath,
			Tried:  true,
		}},
	}, nil, nil, []string{"https://example.com/override"})

	out := buildOAuthStatus(routeParams{
		MCPConfig:  &config.MCPConfig{ServerURL: serverURL},
		OAuthState: state,
	})

	require.False(t, out.Pending)
	require.Empty(t, out.Error)
	require.NotNil(t, out.Metadata)
	require.Len(t, out.Metadata.Attempts, 1)
	require.Equal(t, "https://example.com/override", out.DiscoveryURLs[0])
}

func TestBuildOAuthStatusPendingUsesConfigURLs(t *testing.T) {
	t.Parallel()

	serverURL := mustParseURL(t, "https://example.com/public/mcp")
	state := oauth.NewDiscoveryState()

	out := buildOAuthStatus(routeParams{
		MCPConfig:  &config.MCPConfig{ServerURL: serverURL},
		OAuthState: state,
	})

	require.True(t, out.Pending)
	require.NotEmpty(t, out.DiscoveryURLs)
	require.Equal(t, expectedMetadataURLs(serverURL), out.DiscoveryURLs)
}

func TestBuildOAuthStatusErrorIncludesAttempts(t *testing.T) {
	t.Parallel()

	serverURL := mustParseURL(t, "https://example.com/public/mcp")
	state := oauth.NewDiscoveryState()
	state.Set(&oauth.DiscoveryResult{
		Attempts: []oauth.DiscoveryAttempt{{
			URL:    "https://example.com/.well-known/oauth-protected-resource/public/mcp",
			Source: oauth.DiscoverySourceWellKnownPath,
			Tried:  true,
		}},
	}, errors.New("boom"), nil, []string{"https://example.com/override"})

	out := buildOAuthStatus(routeParams{
		MCPConfig:  &config.MCPConfig{ServerURL: serverURL},
		OAuthState: state,
	})

	require.Equal(t, "boom", out.Error)
	require.NotNil(t, out.Metadata)
	require.Len(t, out.Metadata.Attempts, 1)
}

func expectedMetadataURLs(serverURL *url.URL) []string {
	urls := oauth.BuildResourceMetadataURLs(serverURL)
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		if u == nil {
			continue
		}
		out = append(out, u.String())
	}
	return out
}

func mustParseURL(tb testing.TB, raw string) *url.URL {
	tb.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(tb, err)
	return parsed
}
