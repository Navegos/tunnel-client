package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestDevMCPStubMetadataEndpoints(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(newDevMCPStubHandler("demo-stub", "0.1.0"))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL + "/.well-known/oauth-protected-resource/mcp")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var protectedResource map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&protectedResource))
	require.Equal(t, server.URL+"/mcp", protectedResource["resource"])
	require.Equal(t, []any{server.URL}, protectedResource["authorization_servers"])

	authResp, err := http.Get(server.URL + "/.well-known/oauth-authorization-server")
	require.NoError(t, err)
	t.Cleanup(func() { _ = authResp.Body.Close() })
	require.Equal(t, http.StatusOK, authResp.StatusCode)

	var authServer map[string]any
	require.NoError(t, json.NewDecoder(authResp.Body).Decode(&authServer))
	require.Equal(t, server.URL, authServer["issuer"])
	require.Equal(t, server.URL+"/jwks", authServer["jwks_uri"])
}

func TestDevMCPStubDemoToolsWorkOverStreamableHTTP(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(newDevMCPStubHandler("demo-stub", "0.1.0"))
	t.Cleanup(server.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	require.NoError(t, err)
	toolNames := map[string]bool{}
	for _, tool := range tools.Tools {
		toolNames[tool.Name] = true
	}
	require.True(t, toolNames["server_info"])
	require.True(t, toolNames["echo"])
	require.True(t, toolNames["uppercase"])

	echoResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"input": "hello from tunnel-client"},
	})
	require.NoError(t, err)
	require.False(t, echoResult.IsError)
	require.NotEmpty(t, echoResult.Content)
	echoText, ok := echoResult.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "hello from tunnel-client", echoText.Text)

	uppercaseResult, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "uppercase",
		Arguments: map[string]any{"input": "openai tunnel"},
	})
	require.NoError(t, err)
	require.False(t, uppercaseResult.IsError)
	require.NotEmpty(t, uppercaseResult.Content)
	uppercaseText, ok := uppercaseResult.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Equal(t, "OPENAI TUNNEL", uppercaseText.Text)
}
