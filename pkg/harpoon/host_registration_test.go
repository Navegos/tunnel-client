package harpoon

import (
	"io"
	"log/slog"
	"net/url"
	"testing"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/harpoon/hostbus"
	"go.openai.org/api/tunnel-client/pkg/harpoon/internal/hostclassifier"
)

func TestRegisterHostBundleRespectsClassifier(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	registry, err := NewRegistry(logger, true, nil)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	classifier := hostclassifier.NewHostClassifier(config.HarpoonHostClassifierConfig{
		IncludeSuffix:  []string{"internal"},
		IncludePrivate: false,
	})

	bundle := hostbus.URLBundle{
		URLs: []hostbus.URLRecord{
			{URL: mustParseURLForHostRegistration(t, "https://api.internal/v1#frag"), Description: "internal"},
			{URL: mustParseURLForHostRegistration(t, "https://public.example.com/v1"), Description: "public"},
		},
	}

	if err := registerHostBundle(bundle, classifier, registry, logger); err != nil {
		t.Fatalf("register bundle: %v", err)
	}

	if target, ok := registry.Lookup("oauth-0"); !ok {
		t.Fatalf("expected auto-registered label oauth-0")
	} else if target.InclusionReason == "" {
		t.Fatalf("expected inclusion reason to be set")
	} else if target.BaseURL == nil || target.BaseURL.String() != "https://api.internal/v1#frag" {
		t.Fatalf("expected fragment to be preserved in target URL, got %v", target.BaseURL)
	}
	if _, ok := registry.Lookup("oauth-1"); ok {
		t.Fatalf("unexpected registration for public host")
	}
}

func TestBuildAutoLabelUsesRoleIndex(t *testing.T) {
	label := buildAutoLabel(hostbus.URLRecord{
		Tags: []hostbus.Tag{{Key: hostbus.TagKeyRole, Value: "registration-endpoint"}, {Key: hostbus.TagKeyIndex, Value: "2"}},
	}, 0)

	if label != "oauth-registration-endpoint-2" {
		t.Fatalf("unexpected label: %q", label)
	}
}

func mustParseURLForHostRegistration(t *testing.T, raw string) *url.URL {
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	return parsed
}
