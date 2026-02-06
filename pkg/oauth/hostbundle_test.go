package oauth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"go.openai.org/api/tunnel-client/pkg/harpoon/hostbus"
)

func TestBuildURLBundleFromPRMD(t *testing.T) {
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{
		Resource: "https://resource.internal/",
		AuthorizationServers: []string{
			"https://auth1.internal/",
			"https://auth2.internal/",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	bundle, _, err := buildURLBundleFromPRMDWithAuthServerMetadata(
		context.Background(),
		nil,
		payload,
		time.Unix(42, 0).UTC(),
		mustParseURL(t, "https://prmd.internal/.well-known/oauth-protected-resource"),
		nil,
	)
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	if len(bundle.URLs) != 4 {
		t.Fatalf("expected 4 urls, got %d", len(bundle.URLs))
	}

	if got := bundle.URLs[0].URL.String(); got != "https://resource.internal/" {
		t.Fatalf("unexpected resource url: %q", got)
	}
	if got := bundle.URLs[1].URL.String(); got != "https://auth1.internal/" {
		t.Fatalf("unexpected auth1 url: %q", got)
	}
	if got := bundle.URLs[2].URL.String(); got != "https://auth2.internal/" {
		t.Fatalf("unexpected auth2 url: %q", got)
	}
	if got := bundle.URLs[3].URL.String(); got != "https://prmd.internal/.well-known/oauth-protected-resource" {
		t.Fatalf("unexpected source url: %q", got)
	}

	if len(bundle.URLs[0].Tags) != 3 {
		t.Fatalf("expected tags for resource")
	}
}

func TestBuildURLBundleFromPRMDEmpty(t *testing.T) {
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if _, _, err := buildURLBundleFromPRMDWithAuthServerMetadata(context.Background(), nil, payload, time.Now(), nil, nil); err == nil {
		t.Fatalf("expected error for empty metadata")
	}
}

func TestBuildURLBundleFromPRMDWithAuthServerMetadata(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	issuer := server.URL + "/issuer-a"
	resource := server.URL + "/resource"
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{
		Resource:             resource,
		AuthorizationServers: []string{issuer},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	metaBody, err := json.Marshal(map[string]any{
		"issuer":                 issuer,
		"authorization_endpoint": issuer + "/authorize",
		"token_endpoint":         issuer + "/token",
		"jwks_uri":               issuer + "/jwks",
		"introspection_endpoint": issuer + "/introspect",
		"registration_endpoint":  issuer + "/register",
	})
	if err != nil {
		t.Fatalf("marshal metadata body: %v", err)
	}
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer-a", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(metaBody)
	})

	sourceURL := mustParseURL(t, server.URL+"/.well-known/oauth-protected-resource")
	bundle, _, err := buildURLBundleFromPRMDWithAuthServerMetadata(
		context.Background(),
		server.Client(),
		payload,
		time.Unix(42, 0).UTC(),
		sourceURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("build expanded bundle: %v", err)
	}

	if len(bundle.URLs) != 10 {
		t.Fatalf("expected 10 urls, got %d", len(bundle.URLs))
	}

	assertURLRecord(t, bundle.URLs[0], resource, "prmd-resource", "0")
	assertURLRecord(t, bundle.URLs[1], issuer, "prmd-auth-server", "0")
	assertURLRecord(t, bundle.URLs[2], sourceURL.String(), "prmd-source", "0")
	assertURLRecord(
		t,
		bundle.URLs[3],
		server.URL+"/.well-known/oauth-authorization-server/issuer-a",
		"auth-server-metadata",
		"0",
	)
	assertURLRecord(t, bundle.URLs[4], issuer, "issuer", "0")
	assertURLRecord(t, bundle.URLs[5], issuer+"/authorize", "authorization-endpoint", "0")
	assertURLRecord(t, bundle.URLs[6], issuer+"/token", "token-endpoint", "0")
	assertURLRecord(t, bundle.URLs[7], issuer+"/jwks", "jwks-uri", "0")
	assertURLRecord(t, bundle.URLs[8], issuer+"/introspect", "introspection-endpoint", "0")
	assertURLRecord(t, bundle.URLs[9], issuer+"/register", "registration-endpoint", "0")
}

func TestBuildURLBundleFromPRMDWithAuthServerMetadataPartialFailure(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	issuerA := server.URL + "/issuer-a"
	issuerB := server.URL + "/issuer-b"
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{
		Resource:             server.URL + "/resource",
		AuthorizationServers: []string{issuerA, issuerB},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	metaBody, err := json.Marshal(map[string]any{
		"issuer":         issuerA,
		"token_endpoint": issuerA + "/token",
	})
	if err != nil {
		t.Fatalf("marshal metadata body: %v", err)
	}
	var issuerARequests int
	var issuerBRequests int
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer-a", func(w http.ResponseWriter, _ *http.Request) {
		issuerARequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(metaBody)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer-b", func(w http.ResponseWriter, _ *http.Request) {
		issuerBRequests++
		http.Error(w, "upstream error", http.StatusBadGateway)
	})

	sourceURL := mustParseURL(t, server.URL+"/.well-known/oauth-protected-resource")
	bundle, _, err := buildURLBundleFromPRMDWithAuthServerMetadata(
		context.Background(),
		server.Client(),
		payload,
		time.Unix(42, 0).UTC(),
		sourceURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("build expanded bundle: %v", err)
	}

	// Base PRMD records (resource + 2 auth-servers + source) plus 3 metadata-derived records from issuer-a.
	if len(bundle.URLs) != 7 {
		t.Fatalf("expected 7 urls, got %d", len(bundle.URLs))
	}
	assertURLRecord(t, bundle.URLs[0], server.URL+"/resource", "prmd-resource", "0")
	assertURLRecord(t, bundle.URLs[1], issuerA, "prmd-auth-server", "0")
	assertURLRecord(t, bundle.URLs[2], issuerB, "prmd-auth-server", "1")
	assertURLRecord(t, bundle.URLs[3], sourceURL.String(), "prmd-source", "0")
	assertURLRecord(
		t,
		bundle.URLs[4],
		server.URL+"/.well-known/oauth-authorization-server/issuer-a",
		"auth-server-metadata",
		"0",
	)
	assertURLRecord(t, bundle.URLs[5], issuerA, "issuer", "0")
	assertURLRecord(t, bundle.URLs[6], issuerA+"/token", "token-endpoint", "0")
	if issuerARequests != 1 {
		t.Fatalf("expected exactly one metadata request for first auth server, got %d", issuerARequests)
	}
	if issuerBRequests != 0 {
		t.Fatalf("expected no metadata request for second auth server, got %d", issuerBRequests)
	}
}

func TestBuildURLBundleFromPRMDWithAuthServerMetadataUsesFirstAuthServerOnly(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	issuerA := server.URL + "/issuer-a"
	issuerB := server.URL + "/issuer-b"
	payload, err := json.Marshal(oauthex.ProtectedResourceMetadata{
		Resource:             server.URL + "/resource",
		AuthorizationServers: []string{issuerA, issuerB},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var issuerARequests int
	var issuerBRequests int
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer-a", func(w http.ResponseWriter, _ *http.Request) {
		issuerARequests++
		http.Error(w, "first issuer unavailable", http.StatusBadGateway)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server/issuer-b", func(w http.ResponseWriter, _ *http.Request) {
		issuerBRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"issuer":"` + issuerB + `","token_endpoint":"` + issuerB + `/token"}`))
	})

	sourceURL := mustParseURL(t, server.URL+"/.well-known/oauth-protected-resource")
	bundle, _, err := buildURLBundleFromPRMDWithAuthServerMetadata(
		context.Background(),
		server.Client(),
		payload,
		time.Unix(42, 0).UTC(),
		sourceURL,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("build expanded bundle: %v", err)
	}

	// Base PRMD records (resource + two auth-servers + source), no auth-metadata-derived records.
	if len(bundle.URLs) != 4 {
		t.Fatalf("expected 4 urls, got %d", len(bundle.URLs))
	}
	if issuerARequests != 1 {
		t.Fatalf("expected exactly one metadata request for first auth server, got %d", issuerARequests)
	}
	if issuerBRequests != 0 {
		t.Fatalf("expected no metadata request for second auth server, got %d", issuerBRequests)
	}
}

func assertURLRecord(t *testing.T, record hostbus.URLRecord, expectedURL string, expectedRole string, expectedIndex string) {
	t.Helper()
	if record.URL == nil {
		t.Fatalf("expected URL %q, got nil", expectedURL)
	}
	if got := record.URL.String(); got != expectedURL {
		t.Fatalf("unexpected url: got %q want %q", got, expectedURL)
	}
	if got := tagValueForTest(record.Tags, hostbus.TagKeyRole); got != expectedRole {
		t.Fatalf("unexpected role: got %q want %q", got, expectedRole)
	}
	if got := tagValueForTest(record.Tags, hostbus.TagKeyIndex); got != expectedIndex {
		t.Fatalf("unexpected index: got %q want %q", got, expectedIndex)
	}
}

func tagValueForTest(tags []hostbus.Tag, key hostbus.TagKey) string {
	for _, tag := range tags {
		if tag.Key == key {
			return tag.Value
		}
	}
	return ""
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
