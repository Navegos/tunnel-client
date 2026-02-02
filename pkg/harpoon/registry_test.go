package harpoon

import (
	"io"
	"log/slog"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryRejectsInvalidLabel(t *testing.T) {
	registry, err := NewRegistry(discardLogger(), true, nil)
	require.NoError(t, err)

	parsed, err := url.Parse("https://example.com")
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "bad label", BaseURL: parsed})
	require.Error(t, err)
}

func TestRegistryRejectsDuplicateLabel(t *testing.T) {
	parsed, err := url.Parse("https://example.com")
	require.NoError(t, err)

	registry, err := NewRegistry(discardLogger(), true, []Target{{Label: "auth", BaseURL: parsed}})
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "auth", BaseURL: parsed})
	require.Error(t, err)
}

func TestRegistryRejectsPlaintextWhenDisallowed(t *testing.T) {
	parsed, err := url.Parse("http://example.com")
	require.NoError(t, err)

	_, err = NewRegistry(discardLogger(), false, []Target{{Label: "auth", BaseURL: parsed}})
	require.Error(t, err)
}

func TestRegistryNormalizesOnlySchemeAndHostCase(t *testing.T) {
	root, err := url.Parse("hTTps://EXAMPLE.com////")
	require.NoError(t, err)
	withQueryAndFragment, err := url.Parse("https://example.com/bla////?A=1&a=2&a=3#frag")
	require.NoError(t, err)

	registry, err := NewRegistry(discardLogger(), true, []Target{
		{Label: "root", Description: "Root", BaseURL: root},
		{Label: "bla", Description: "Path", BaseURL: withQueryAndFragment},
	})
	require.NoError(t, err)

	targets := registry.Targets()
	require.Len(t, targets, 2)
	require.Equal(t, "https://example.com////", targets[0].BaseURL.String())
	require.Equal(t, "https://example.com/bla////?A=1&a=2&a=3#frag", targets[1].BaseURL.String())
}

func TestRegistryResolveReturnsExactTargetURL(t *testing.T) {
	u, err := url.Parse("https://example.com/bla?x=1#frag")
	require.NoError(t, err)

	registry, err := NewRegistry(discardLogger(), true, []Target{{Label: "svc", BaseURL: u}})
	require.NoError(t, err)

	resolved, err := registry.Resolve("svc")
	require.NoError(t, err)
	require.Equal(t, "https://example.com/bla?x=1#frag", resolved.String())
}

func TestRegistryAllowsURLUsesExactMatchExceptSchemeHostCase(t *testing.T) {
	root, err := url.Parse("https://example.com")
	require.NoError(t, err)
	bla, err := url.Parse("https://example.com/bla")
	require.NoError(t, err)

	registry, err := NewRegistry(discardLogger(), true, []Target{
		{Label: "root", BaseURL: root},
		{Label: "bla", BaseURL: bla},
	})
	require.NoError(t, err)

	require.True(t, registry.AllowsURL(mustURL(t, "hTTps://EXAMPLE.com")))
	require.True(t, registry.AllowsURL(mustURL(t, "https://example.com/bla")))
	require.False(t, registry.AllowsURL(mustURL(t, "https://example.com/")))
	require.False(t, registry.AllowsURL(mustURL(t, "https://example.com/bla/")))
}

func TestRegistryPreservesDistinctEncodedPathAndQueryForms(t *testing.T) {
	u1 := mustURL(t, "https://example.com/a%2Fb?x=a+b")
	u2 := mustURL(t, "https://example.com/a/b?x=a%20b")

	registry, err := NewRegistry(discardLogger(), true, []Target{
		{Label: "enc", BaseURL: u1},
		{Label: "plain", BaseURL: u2},
	})
	require.NoError(t, err)

	require.True(t, registry.AllowsURL(mustURL(t, "https://example.com/a%2Fb?x=a+b")))
	require.True(t, registry.AllowsURL(mustURL(t, "https://example.com/a/b?x=a%20b")))
	require.False(t, registry.AllowsURL(mustURL(t, "https://example.com/a/b?x=a+b")))
}

func TestSummarizeTargets(t *testing.T) {
	urlA, err := url.Parse("https://example.com/base/")
	require.NoError(t, err)
	urlB, err := url.Parse("https://example.org")
	require.NoError(t, err)

	registry, err := NewRegistry(discardLogger(), true, []Target{
		{Label: "auth", Description: "Auth server", BaseURL: urlA},
		{Label: "idp", Description: "Identity", BaseURL: urlB},
	})
	require.NoError(t, err)

	summary := registry.SummarizeTargets()

	require.Equal(t, []map[string]string{
		{"label": "auth", "url": "https://example.com/base/", "desc": "Auth server"},
		{"label": "idp", "url": "https://example.org", "desc": "Identity"},
	}, summary)
}

func TestRegistryRespectsLimit(t *testing.T) {
	parsed, err := url.Parse("https://example.com")
	require.NoError(t, err)

	registry, err := NewRegistryWithLimit(discardLogger(), true, []Target{{Label: "auth", BaseURL: parsed}}, 2)
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "metrics", BaseURL: parsed})
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "logs", BaseURL: parsed})
	require.Error(t, err)
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
