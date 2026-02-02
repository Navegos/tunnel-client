package harpoon

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransformJSONBodyRewritesURLsExactMatch(t *testing.T) {
	rewriter := newURLRewriter([]Target{
		{Label: "root", BaseURL: mustParseURL(t, "https://example.com")},
		{Label: "api", BaseURL: mustParseURL(t, "https://example.com/api")},
	})

	body := []byte(`{"url":"https://example.com/api","other":"https://example.com","nested":["https://example.com/api/","api://kepler"],"n":1}`)
	updated, changed := transformJSONBody(body, rewriter)
	require.True(t, changed)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(updated, &payload))
	require.Equal(t, "harpoon://api", payload["url"])
	require.Equal(t, "harpoon://root", payload["other"])
	nested := payload["nested"].([]any)
	require.Equal(t, "https://example.com/api/", nested[0])
	require.Equal(t, "api://kepler", nested[1])
}

func TestTransformJSONBodySkipsInvalidJSON(t *testing.T) {
	rewriter := newURLRewriter([]Target{{Label: "root", BaseURL: mustParseURL(t, "https://example.com")}})
	body := []byte(`{"url":`)
	updated, changed := transformJSONBody(body, rewriter)
	require.False(t, changed)
	require.Equal(t, body, updated)
}

func TestTransformJSONBodyPreservesShapeWhenRewriting(t *testing.T) {
	rewriter := newURLRewriter([]Target{
		{Label: "page", BaseURL: mustParseURL(t, "https://example.com/page.html?x=1#frag")},
		{Label: "other", BaseURL: mustParseURL(t, "https://example.com/other")},
	})

	body := []byte(`{
  "meta" : { "note" : "keep spacing" },
  "url"  : "https://example.com/page.html?x=1#frag",
  "list" : [
    "https://example.com/other",
    { "inner_url" : "https://example.com/page.html?x=2#frag" }
  ],
  "https://example.com/page.html?x=1#frag" : "key must stay untouched"
}
`)

	updated, changed := transformJSONBody(body, rewriter)
	require.True(t, changed)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(updated, &payload))
	require.Equal(t, "harpoon://page", payload["url"])
	require.Equal(t, "key must stay untouched", payload["https://example.com/page.html?x=1#frag"])
	meta := payload["meta"].(map[string]any)
	require.Equal(t, "keep spacing", meta["note"])
	list := payload["list"].([]any)
	require.Equal(t, "harpoon://other", list[0])
	require.Equal(t, "https://example.com/page.html?x=2#frag", list[1].(map[string]any)["inner_url"])
}

func TestTransformJSONBodyNoMatchReturnsOriginalBytes(t *testing.T) {
	rewriter := newURLRewriter([]Target{{Label: "root", BaseURL: mustParseURL(t, "https://example.com")}})
	body := []byte(`{ "a": "https://another.example.com/x", "b": [1, 2, 3] }`)

	updated, changed := transformJSONBody(body, rewriter)
	require.False(t, changed)
	require.Equal(t, body, updated)
}

func TestTransformJSONBodyTopLevelString(t *testing.T) {
	rewriter := newURLRewriter([]Target{{Label: "api", BaseURL: mustParseURL(t, "https://example.com/api")}})

	updated, changed := transformJSONBody([]byte(`"https://example.com/api"`), rewriter)
	require.True(t, changed)
	require.Equal(t, `"harpoon://api"`, string(updated))
}

func TestTransformJSONBodyNormalizesFormatting(t *testing.T) {
	rewriter := newURLRewriter([]Target{{Label: "api", BaseURL: mustParseURL(t, "https://example.com/api")}})
	body := []byte("{\n  \"url\" : \"https://example.com/api\"\n}\n")

	updated, changed := transformJSONBody(body, rewriter)
	require.True(t, changed)
	require.NotEqual(t, string(body), string(updated))
	require.JSONEq(t, `{"url":"harpoon://api"}`, string(updated))
}

func TestTransformHeadersRewritesLocations(t *testing.T) {
	rewriter := newURLRewriter([]Target{
		{Label: "api", BaseURL: mustParseURL(t, "https://example.com/api")},
		{Label: "foo", BaseURL: mustParseURL(t, "https://example.com/foo#next")},
	})

	headers := http.Header{
		"Location": {"https://example.com/api"},
		"Link":     {`<https://example.com/foo#next>; rel="next"`},
		"X-Other":  {"https://example.com/api/v2"},
	}

	updated, changed := transformHeaders(headers, rewriter)
	require.True(t, changed)
	require.Equal(t, "harpoon://api", updated.Get("Location"))
	require.Equal(t, `<harpoon://foo>; rel="next"`, updated.Get("Link"))
	require.Equal(t, "https://example.com/api/v2", updated.Get("X-Other"))
}
