package localproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.openai.org/api/tunnel-client/testsupport/mockmcpserver"
)

func TestStartFrontsHTTPMCPServerWithoutHealthListener(t *testing.T) {
	mcpServer := mockmcpserver.NewMockMCPServer(
		mockmcpserver.WithToolListChangedNotificationsDisabled(),
		mockmcpserver.WithCalls(mockmcpserver.Call{
			Tool: "echo",
			DynamicResult: func(arguments json.RawMessage) (json.RawMessage, error) {
				var payload struct {
					Name string `json:"name"`
				}
				require.NoError(t, json.Unmarshal(arguments, &payload))
				return json.RawMessage(fmt.Sprintf(`{"message":"hi, %s!"}`, payload.Name)), nil
			},
		}),
	)
	mcpServer.Start(t)

	var stderr bytes.Buffer
	proxy, err := Start(context.Background(), Options{
		MCPServerURLs: []string{mcpServer.BaseURL().String()},
		Stderr:        &stderr,
	})
	require.NoError(t, err, stderr.String())
	t.Cleanup(func() {
		require.NoError(t, proxy.Stop(context.Background()))
	})
	info := proxy.Info()
	require.Empty(t, info.HealthURL)
	require.Equal(t, "go-in-memory", info.Backend)
	if runtime.GOOS == "windows" {
		require.Equal(t, "tcp", info.ControlPlaneTransport)
		require.Empty(t, info.ControlPlaneUnixSocket)
	} else {
		require.Equal(t, "unix", info.ControlPlaneTransport)
		require.NotEmpty(t, info.ControlPlaneUnixSocket)
	}

	sessionID := postInitialize(t, info.MCPURL, nil)
	postInitializedNotification(t, info.MCPURL, sessionID)
	response := postToolCall(t, info.MCPURL, sessionID, nil)
	require.Equal(t, "hi, Ada!", response.Result.StructuredContent["message"])

	recorded := mcpServer.ReceivedRequests()
	require.Len(t, recorded, 1)
	require.Equal(t, "echo", recorded[0].Tool)
}

func TestStartFrontsStdioMCPServer(t *testing.T) {
	invocationLog := t.TempDir() + "/stdio-invocations.log"
	t.Setenv("MOCK_MCP_INVOCATION_LOG", invocationLog)

	proxy, err := Start(context.Background(), Options{
		MCPCommands: []string{strings.Join(mockmcpserver.StdioServerCommand(t), " ")},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, proxy.Stop(context.Background()))
	})

	sessionID := postInitializeOptionalSession(t, proxy.Info().MCPURL, nil)
	postInitializedNotification(t, proxy.Info().MCPURL, sessionID)
	response := postToolCall(t, proxy.Info().MCPURL, sessionID, nil)
	require.Equal(t, "hello Ada", response.Result.StructuredContent["message"])

	data, err := os.ReadFile(invocationLog)
	require.NoError(t, err)
	require.NotEmpty(t, data)
}

func TestStartSupportsChannelRouteAndHeaderFiltering(t *testing.T) {
	mcpServer := mockmcpserver.NewMockMCPServer(
		mockmcpserver.WithToolListChangedNotificationsDisabled(),
		mockmcpserver.WithCalls(mockmcpserver.Call{
			Tool:   "echo",
			Result: json.RawMessage(`{"message":"tools channel"}`),
		}),
	)
	mcpServer.Start(t)

	proxy, err := Start(context.Background(), Options{
		MCPServerURLs: []string{
			mcpServer.BaseURL().String(),
			"url=" + mcpServer.BaseURL().String() + ",channel=tools",
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, proxy.Stop(context.Background()))
	})

	sessionID := postInitialize(t, proxy.Info().MCPURL, nil)
	postInitializedNotification(t, proxy.Info().MCPURL, sessionID)

	headers := http.Header{
		"Mcp-Session-Id": {sessionID},
		"Authorization":  {"Bearer connector-user-token"},
		"Cookie":         {"session=blocked"},
	}
	channelURL := strings.TrimRight(proxy.Info().MCPURL, "/") + "/tools"
	response := postToolCall(t, channelURL, sessionID, headers)
	require.Equal(t, "tools channel", response.Result.StructuredContent["message"])

	recorded := mcpServer.ReceivedRequests()
	require.Len(t, recorded, 1)
	require.Equal(t, "Bearer connector-user-token", recorded[0].Headers.Get("Authorization"))
	require.Empty(t, recorded[0].Headers.Get("Cookie"))
}

func TestStartWritesProxyInfoFile(t *testing.T) {
	mcpServer := mockmcpserver.NewMockMCPServer(mockmcpserver.WithToolListChangedNotificationsDisabled())
	mcpServer.Start(t)
	path := t.TempDir() + "/proxy.json"

	proxy, err := Start(context.Background(), Options{
		MCPServerURLs: []string{mcpServer.BaseURL().String()},
		URLFile:       path,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, proxy.Stop(context.Background()))
	})

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), `"mcp_url"`)
	require.Contains(t, string(data), proxy.Info().MCPURL)
	require.Contains(t, string(data), `"backend": "go-in-memory"`)
}

func TestStartEnablesEphemeralHealthListenerWhenURLFileRequested(t *testing.T) {
	mcpServer := mockmcpserver.NewMockMCPServer(mockmcpserver.WithToolListChangedNotificationsDisabled())
	mcpServer.Start(t)
	path := t.TempDir() + "/health.url"

	proxy, err := Start(context.Background(), Options{
		MCPServerURLs: []string{mcpServer.BaseURL().String()},
		HealthURLFile: path,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, proxy.Stop(context.Background()))
	})

	require.NotEmpty(t, proxy.Info().HealthURL)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	healthBaseURL := strings.TrimRight(strings.TrimSpace(string(data)), "/")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthBaseURL+"/healthz", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStartRejectsMissingMCPConfiguration(t *testing.T) {
	_, err := Start(context.Background(), Options{})
	require.ErrorContains(t, err, "set --mcp-server-url, --mcp-command, --profile, or --profile-file")
}

func TestStartRejectsUnavailableRustBackend(t *testing.T) {
	_, err := Start(context.Background(), Options{
		MCPServerURLs: []string{"http://127.0.0.1:1/mcp"},
		Backend:       BackendRust,
	})
	require.ErrorContains(t, err, "rust local proxy backend is unavailable in this build")
}

func TestResolveBackendAutoFallsBackToGo(t *testing.T) {
	factory, err := resolveBackendFactory(BackendAuto, nil)
	require.NoError(t, err)
	require.Equal(t, BackendGo, factory.Name())
}

func TestResolveBackendAutoPrefersRegisteredRust(t *testing.T) {
	factory, err := resolveBackendFactory(BackendAuto, []BackendFactory{
		fakeBackendFactory{name: BackendRust},
	})
	require.NoError(t, err)
	require.Equal(t, BackendRust, factory.Name())
}

func TestResolveBackendRejectsUnavailableRust(t *testing.T) {
	_, err := resolveBackendFactory(BackendRust, nil)
	require.ErrorContains(t, err, "rust local proxy backend is unavailable in this build")
}

func TestResolveBackendRejectsUnknownBackend(t *testing.T) {
	_, err := resolveBackendFactory(BackendName("python"), nil)
	require.ErrorContains(t, err, `unknown local proxy backend "python"`)
}

type fakeBackendFactory struct {
	name BackendName
}

func (f fakeBackendFactory) Name() BackendName {
	return f.name
}

func (fakeBackendFactory) StartBackend(context.Context, BackendOptions) (Backend, error) {
	return nil, errors.New("not implemented")
}

type toolCallResponse struct {
	Result struct {
		StructuredContent map[string]string `json:"structuredContent"`
	} `json:"result"`
}

func postInitialize(t *testing.T, mcpURL string, extraHeaders http.Header) string {
	t.Helper()
	sessionID := postInitializeOptionalSession(t, mcpURL, extraHeaders)
	require.NotEmpty(t, sessionID, "initialize response missing Mcp-Session-Id")
	return sessionID
}

func postInitializeOptionalSession(t *testing.T, mcpURL string, extraHeaders http.Header) string {
	t.Helper()
	received := postJSONRPC(t, mcpURL, extraHeaders, `{
		"jsonrpc":"2.0",
		"id":"initialize-0",
		"method":"initialize",
		"params":{
			"protocolVersion":"2025-06-18",
			"capabilities":{"sampling":{},"roots":{"listChanged":true}},
			"clientInfo":{"name":"tunnel-client-local-proxy-test","version":"0.0.1"}
		}
	}`)
	return received.headers.Get("Mcp-Session-Id")
}

func postInitializedNotification(t *testing.T, mcpURL string, sessionID string) {
	t.Helper()
	_ = postJSONRPC(t, mcpURL, sessionHeaders(sessionID), `{
		"jsonrpc":"2.0",
		"method":"notifications/initialized",
		"params":{}
	}`)
}

func postToolCall(t *testing.T, mcpURL string, sessionID string, extraHeaders http.Header) toolCallResponse {
	t.Helper()
	headers := sessionHeaders(sessionID)
	for name, values := range extraHeaders {
		headers.Del(name)
		for _, value := range values {
			headers.Add(name, value)
		}
	}
	received := postJSONRPC(t, mcpURL, headers, `{
		"jsonrpc":"2.0",
		"id":"tool-1",
		"method":"tools/call",
		"params":{"name":"echo","arguments":{"name":"Ada"}}
	}`)
	var decoded toolCallResponse
	require.NoError(t, json.Unmarshal(received.body, &decoded), string(received.body))
	return decoded
}

func sessionHeaders(sessionID string) http.Header {
	headers := http.Header{}
	if sessionID != "" {
		headers.Set("Mcp-Session-Id", sessionID)
	}
	return headers
}

type jsonRPCIngressResponse struct {
	body    []byte
	headers http.Header
}

func postJSONRPC(t *testing.T, mcpURL string, extraHeaders http.Header, body string) jsonRPCIngressResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	for name, values := range extraHeaders {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() {
		_ = resp.Body.Close()
	}()
	responseBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.GreaterOrEqual(t, resp.StatusCode, 200, string(responseBody))
	require.Less(t, resp.StatusCode, 300, string(responseBody))
	return jsonRPCIngressResponse{body: responseBody, headers: resp.Header.Clone()}
}
