package adminui

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/oauth"
	"go.openai.org/api/tunnel-client/pkg/types"
)

func TestBuildOAuthStatusWithAttempts(t *testing.T) {
	t.Parallel()

	serverURL := mustParseURL(t, "https://example.com/public/mcp")
	body := json.RawMessage(`{
		"resource":"https://resource.internal",
		"authorization_servers":["https://auth-1.internal","https://auth-2.internal"]
	}`)
	state := oauth.NewDiscoveryState()
	state.Set(&oauth.DiscoveryResult{
		Body: body,
		Attempts: []oauth.DiscoveryAttempt{{
			URL:    "https://example.com/.well-known/oauth-protected-resource/public/mcp",
			Source: oauth.DiscoverySourceWellKnownPath,
			Tried:  true,
		}},
	}, nil, nil, []string{"https://example.com/override"})

	out := buildOAuthStatus(routeParams{
		MCPConfig: &config.MCPConfig{
			ServerURL: serverURL,
			ChannelBindings: []config.MCPChannelBinding{
				{
					Channel:       types.DefaultChannel,
					TransportKind: config.MCPTransportHTTPStreamable,
					ServerURL:     serverURL,
				},
			},
		},
		OAuthState: state,
	})

	require.False(t, out.Pending)
	require.Empty(t, out.Error)
	require.NotNil(t, out.Metadata)
	require.Len(t, out.Metadata.Attempts, 1)
	require.Equal(t, "https://example.com/override", out.DiscoveryURLs[0])
	require.Equal(t, "first authorization server only", out.AuthServerMetaMode)
	require.Equal(t, 2, out.AuthServerCount)
	require.Equal(t, "https://auth-1.internal", out.SelectedAuthServer)
}

func TestBuildOAuthStatusPendingUsesConfigURLs(t *testing.T) {
	t.Parallel()

	serverURL := mustParseURL(t, "https://example.com/public/mcp")
	state := oauth.NewDiscoveryState()

	out := buildOAuthStatus(routeParams{
		MCPConfig: &config.MCPConfig{
			ServerURL: serverURL,
			ChannelBindings: []config.MCPChannelBinding{
				{
					Channel:       types.DefaultChannel,
					TransportKind: config.MCPTransportHTTPStreamable,
					ServerURL:     serverURL,
				},
			},
		},
		OAuthState: state,
	})

	require.True(t, out.Pending)
	require.NotEmpty(t, out.DiscoveryURLs)
	require.Equal(t, expectedMetadataURLs(serverURL), out.DiscoveryURLs)
	require.Equal(t, "first authorization server only", out.AuthServerMetaMode)
	require.Equal(t, 0, out.AuthServerCount)
	require.Empty(t, out.SelectedAuthServer)
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
		MCPConfig: &config.MCPConfig{
			ServerURL: serverURL,
			ChannelBindings: []config.MCPChannelBinding{
				{
					Channel:       types.DefaultChannel,
					TransportKind: config.MCPTransportHTTPStreamable,
					ServerURL:     serverURL,
				},
			},
		},
		OAuthState: state,
	})

	require.Equal(t, "boom", out.Error)
	require.NotNil(t, out.Metadata)
	require.Len(t, out.Metadata.Attempts, 1)
	require.Equal(t, "first authorization server only", out.AuthServerMetaMode)
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
