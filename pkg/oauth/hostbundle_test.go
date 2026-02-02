package oauth

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
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

	bundle, err := buildURLBundleFromPRMD(payload, time.Unix(42, 0).UTC(), mustParseURL(t, "https://prmd.internal/.well-known/oauth-protected-resource"))
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

	if _, err := buildURLBundleFromPRMD(payload, time.Now(), nil); err == nil {
		t.Fatalf("expected error for empty metadata")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
