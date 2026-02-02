package oauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/harpoon/hostbus"
)

type recordingBus struct {
	mu      sync.Mutex
	bundles []hostbus.URLBundle
}

func (b *recordingBus) Publish(ctx context.Context, bundle hostbus.URLBundle) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bundles = append(b.bundles, bundle)
	return nil
}

func (b *recordingBus) Close() error { return nil }

func TestOAuthDiscoveryPublishesPRMDBundle(t *testing.T) {
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{
		Resource: "https://resource.internal/",
		AuthorizationServers: []string{
			"https://auth.internal/",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	bus := &recordingBus{}
	app := fx.New(
		fx.Provide(
			func() *config.MCPConfig {
				return &config.MCPConfig{ServerURL: serverURL, TransportKind: config.MCPTransportHTTPStreamable}
			},
			fx.Annotate(
				func() *http.Client { return server.Client() },
				fx.ResultTags(`name:"mcp_client"`),
			),
			func() hostbus.HostRegistrationBus { return bus },
			func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) },
			NewDiscoveryState,
		),
		fx.Invoke(startOAuthDiscovery),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := app.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = app.Stop(context.Background())
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		bus.mu.Lock()
		count := len(bus.bundles)
		bus.mu.Unlock()
		if count > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.bundles) != 1 {
		t.Fatalf("expected 1 bundle, got %d", len(bus.bundles))
	}
	if len(bus.bundles[0].URLs) != 3 {
		t.Fatalf("expected 3 urls, got %d", len(bus.bundles[0].URLs))
	}
}

func TestOAuthDiscoveryRequiresBus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"https://resource.internal/"}`))
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	app := fx.New(
		fx.Provide(
			func() *config.MCPConfig {
				return &config.MCPConfig{ServerURL: serverURL, TransportKind: config.MCPTransportHTTPStreamable}
			},
			fx.Annotate(
				func() *http.Client { return server.Client() },
				fx.ResultTags(`name:"mcp_client"`),
			),
			func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) },
			NewDiscoveryState,
		),
		fx.Invoke(startOAuthDiscovery),
	)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := app.Start(ctx); err == nil {
		t.Fatalf("expected start error when host bus is missing")
	}
}
