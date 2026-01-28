package harpoon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.openai.org/api/tunnel-client/pkg/config"
)

func TestListTargetsDoesNotExposeURLs(t *testing.T) {
	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   config.DefaultHarpoonMaxResponseBytes,
		MaxRedirects:       config.DefaultHarpoonMaxRedirects,
		Targets: []config.HarpoonTarget{{
			Label:       "auth",
			Description: "Auth service",
			BaseURL:     mustParseURL(t, "http://example.com"),
		}},
	}
	server := newTestServer(t, cfg)

	resp := server.listTargets()
	require.Len(t, resp.Targets, 1)
	require.Equal(t, "auth", resp.Targets[0].Label)
	require.Equal(t, "Auth service", resp.Targets[0].Description)
	require.NotEmpty(t, resp.Targets[0].AllowedMethods)

	payload, err := json.Marshal(resp)
	require.NoError(t, err)
	require.NotContains(t, string(payload), "http://")
	require.NotContains(t, string(payload), "https://")
}

func TestCallTargetSupportsMethods(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Method))
	}))
	defer server.Close()

	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "svc",
			BaseURL: mustParseURL(t, server.URL),
		}},
	}
	client := newTestServer(t, cfg)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut} {
		resp, err := client.callTarget(context.Background(), callTargetRequest{
			Label:  "svc",
			Path:   "/",
			Method: method,
		})
		require.NoError(t, err)
		body := decodeBody(t, resp.BodyBase64)
		require.Equal(t, method, body)
	}
}

func TestCallTargetRejectsInvalidMethod(t *testing.T) {
	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "svc",
			BaseURL: mustParseURL(t, "http://example.com"),
		}},
	}
	client := newTestServer(t, cfg)

	_, err := client.callTarget(context.Background(), callTargetRequest{
		Label:  "svc",
		Path:   "/",
		Method: "DELETE",
	})
	require.Error(t, err)
}

func TestCallTargetValidatesTimeouts(t *testing.T) {
	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "svc",
			BaseURL: mustParseURL(t, "http://example.com"),
		}},
	}
	client := newTestServer(t, cfg)

	tooShort := 50
	_, err := client.callTarget(context.Background(), callTargetRequest{
		Label:     "svc",
		Path:      "/",
		Method:    http.MethodGet,
		TimeoutMS: &tooShort,
	})
	require.Error(t, err)

	tooLong := 130000
	_, err = client.callTarget(context.Background(), callTargetRequest{
		Label:     "svc",
		Path:      "/",
		Method:    http.MethodGet,
		TimeoutMS: &tooLong,
	})
	require.Error(t, err)
}

func TestCallTargetEnforcesSizeLimits(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(strings.Repeat("a", 20)))
	}))
	defer server.Close()

	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   10,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "svc",
			BaseURL: mustParseURL(t, server.URL),
		}},
	}
	client := newTestServer(t, cfg)

	_, err := client.callTarget(context.Background(), callTargetRequest{
		Label:  "svc",
		Path:   "/",
		Method: http.MethodGet,
	})
	require.Error(t, err)
	require.True(t, called)

	called = false
	body := strings.Repeat("b", 20)
	_, err = client.callTarget(context.Background(), callTargetRequest{
		Label:  "svc",
		Path:   "/",
		Method: http.MethodPost,
		Body:   body,
	})
	require.Error(t, err)
	require.False(t, called)
}

func TestCallTargetRedirectHandling(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer second.Close()

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/ok", http.StatusFound)
	}))
	defer primary.Close()

	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{
			{Label: "primary", BaseURL: mustParseURL(t, primary.URL)},
			{Label: "second", BaseURL: mustParseURL(t, second.URL)},
		},
	}
	client := newTestServer(t, cfg)

	resp, err := client.callTarget(context.Background(), callTargetRequest{
		Label:  "primary",
		Path:   "/start",
		Method: http.MethodGet,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", decodeBody(t, resp.BodyBase64))

	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("blocked"))
	}))
	defer blocked.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, blocked.URL+"/escape", http.StatusFound)
	}))
	defer redirector.Close()

	blockedCfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "primary",
			BaseURL: mustParseURL(t, redirector.URL),
		}},
	}
	client = newTestServer(t, blockedCfg)

	_, err = client.callTarget(context.Background(), callTargetRequest{
		Label:  "primary",
		Path:   "/start",
		Method: http.MethodGet,
	})
	require.Error(t, err)
	require.NotContains(t, err.Error(), blocked.URL)
}

func TestCallTargetRedirectLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
	}))
	defer server.Close()

	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   1024,
		MaxRedirects:       5,
		Targets:            []config.HarpoonTarget{{Label: "loop", BaseURL: mustParseURL(t, server.URL)}},
	}
	client := newTestServer(t, cfg)

	maxRedirects := 1
	_, err := client.callTarget(context.Background(), callTargetRequest{
		Label:        "loop",
		Path:         "/",
		Method:       http.MethodGet,
		MaxRedirects: &maxRedirects,
	})
	require.Error(t, err)
}

func TestIntegrationRedirectTruncationAndPathJoin(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/start":
			http.Redirect(w, r, "/api/large", http.StatusFound)
		case "/api/large":
			_, _ = w.Write([]byte(strings.Repeat("x", 50)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	cfg := &config.HarpoonConfig{
		AllowPlaintextHTTP: true,
		MaxResponseBytes:   10,
		MaxRedirects:       5,
		Targets: []config.HarpoonTarget{{
			Label:   "svc",
			BaseURL: mustParseURL(t, server.URL+"/api"),
		}},
	}
	client := newTestServer(t, cfg)

	_, err := client.callTarget(context.Background(), callTargetRequest{
		Label:  "svc",
		Path:   "start",
		Method: http.MethodGet,
	})
	require.Error(t, err)
	require.Contains(t, paths, "/api/start")

	_, err = client.callTarget(context.Background(), callTargetRequest{
		Label:  "svc",
		Path:   "../escape",
		Method: http.MethodGet,
	})
	require.Error(t, err)
}

func newTestServer(t *testing.T, cfg *config.HarpoonConfig) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := NewRegistry(cfg.AllowPlaintextHTTP, convertTargets(cfg.Targets))
	require.NoError(t, err)
	server, err := NewServer(cfg, registry, logger)
	require.NoError(t, err)
	return server
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}

func decodeBody(t *testing.T, encoded string) string {
	t.Helper()
	payload, err := base64.StdEncoding.DecodeString(encoded)
	require.NoError(t, err)
	return string(payload)
}
