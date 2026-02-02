package harpoon

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var headerRewriteKeys = map[string]struct{}{
	"Content-Location": {},
	"Link":             {},
	"Location":         {},
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>"']+`)

// urlRewriter rewrites absolute http/https URLs to Harpoon URLs using known
// targets. Matching is exact after URL normalization (host/scheme case only).
type urlRewriter struct {
	labelsByURL map[string]string
}

func newURLRewriter(targets []Target) *urlRewriter {
	if len(targets) == 0 {
		return &urlRewriter{}
	}
	labelsByURL := make(map[string]string, len(targets))
	for _, target := range targets {
		if target.BaseURL == nil {
			continue
		}
		scheme := strings.ToLower(target.BaseURL.Scheme)
		if scheme != "http" && scheme != "https" {
			continue
		}
		key, err := normalizedURLKey(target.BaseURL)
		if err != nil {
			continue
		}
		if _, exists := labelsByURL[key]; exists {
			continue
		}
		labelsByURL[key] = target.Label
	}
	return &urlRewriter{labelsByURL: labelsByURL}
}

func (r *urlRewriter) RewriteURLString(raw string) (string, bool) {
	if r == nil || raw == "" {
		return raw, false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return raw, false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return raw, false
	}
	key, err := normalizedURLKey(parsed)
	if err != nil {
		return raw, false
	}
	label, ok := r.labelsByURL[key]
	if !ok {
		return raw, false
	}
	return "harpoon://" + label, true
}

func transformJSONBody(body []byte, rewriter *urlRewriter) ([]byte, bool) {
	if len(body) == 0 || rewriter == nil || !json.Valid(body) {
		return body, false
	}
	// Intentionally rely on json unmarshal/marshal to avoid maintaining a custom
	// JSON tokenizer/parser in Harpoon.
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, false
	}
	updated, changed := rewriteJSONValue(payload, rewriter)
	if !changed {
		return body, false
	}
	encoded, err := json.Marshal(updated)
	if err != nil {
		return body, false
	}
	return encoded, true
}

func rewriteJSONValue(value any, rewriter *urlRewriter) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for key, val := range typed {
			updated, ok := rewriteJSONValue(val, rewriter)
			if ok {
				typed[key] = updated
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for idx, val := range typed {
			updated, ok := rewriteJSONValue(val, rewriter)
			if ok {
				typed[idx] = updated
				changed = true
			}
		}
		return typed, changed
	case string:
		if rewriter == nil {
			return typed, false
		}
		if rewritten, ok := rewriter.RewriteURLString(typed); ok {
			return rewritten, true
		}
		return typed, false
	default:
		return typed, false
	}
}

func transformHeaders(headers http.Header, rewriter *urlRewriter) (http.Header, bool) {
	if headers == nil {
		return nil, false
	}
	changed := false
	out := make(http.Header, len(headers))
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}
		copied := make([]string, len(values))
		if shouldRewriteHeader(key) {
			for i, val := range values {
				newVal, updated := rewriteHeaderValue(val, rewriter)
				if updated {
					changed = true
				}
				copied[i] = newVal
			}
		} else {
			copy(copied, values)
		}
		out[key] = copied
	}
	return out, changed
}

func shouldRewriteHeader(key string) bool {
	if key == "" {
		return false
	}
	_, ok := headerRewriteKeys[http.CanonicalHeaderKey(key)]
	return ok
}

func rewriteHeaderValue(value string, rewriter *urlRewriter) (string, bool) {
	if value == "" || rewriter == nil {
		return value, false
	}
	changed := false
	out := urlPattern.ReplaceAllStringFunc(value, func(match string) string {
		replaced, ok := rewriter.RewriteURLString(match)
		if ok {
			changed = true
			return replaced
		}
		return match
	})
	return out, changed
}
