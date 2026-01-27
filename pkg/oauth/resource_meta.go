package oauth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"go.openai.org/api/tunnel-client/pkg/version"
)

const defaultProtectedResourceMetadataURI = "/.well-known/oauth-protected-resource"

var resourceMetadataParamPattern = regexp.MustCompile(
	`(?i)resource_metadata\s*=\s*(?:"([^"]*)"|([^,\s]+))`,
)

// DiscoverySource labels how a metadata candidate was discovered.
type DiscoverySource string

const (
	DiscoverySourceWWWAuthenticate DiscoverySource = "www_authenticate"
	DiscoverySourceWellKnownPath   DiscoverySource = "well_known_path"
	DiscoverySourceWellKnownRoot   DiscoverySource = "well_known_root"
)

// DiscoveryCandidate represents a URL plus its discovery source.
type DiscoveryCandidate struct {
	URL    *url.URL        `json:"-"`
	Source DiscoverySource `json:"source"`
}

// DiscoveryAttempt captures one discovery attempt for UI/reporting.
type DiscoveryAttempt struct {
	URL        string          `json:"url"`
	Source     DiscoverySource `json:"source"`
	Tried      bool            `json:"tried,omitempty"`
	StatusCode int             `json:"status_code,omitempty"`
	Error      string          `json:"error,omitempty"`
	Selected   bool            `json:"selected,omitempty"`
}

// WWWAuthenticateProbeStatus captures the outcome of a WWW-Authenticate probe.
type WWWAuthenticateProbeStatus struct {
	Attempted bool   `json:"attempted"`
	URL       string `json:"url,omitempty"`
	Error     string `json:"error,omitempty"`
}

type wwwAuthenticateProbeResult struct {
	Attempted bool
	URL       *url.URL
	Error     string
}

func (p wwwAuthenticateProbeResult) status() *WWWAuthenticateProbeStatus {
	if !p.Attempted && p.URL == nil && p.Error == "" {
		return nil
	}
	status := &WWWAuthenticateProbeStatus{Attempted: p.Attempted}
	if p.URL != nil {
		status.URL = p.URL.String()
	}
	if p.Error != "" {
		status.Error = p.Error
	}
	return status
}

// BuildResourceMetadataURLs constructs the ordered list of candidate OAuth
// ProtectedResourceMetaData endpoints derived from the MCP server URL. It
// follows RFC 9728 discovery rules by prioritizing the path-specific well-known
// URI, then the root well-known URI.
func BuildResourceMetadataURLs(serverURL *url.URL) []*url.URL {
	candidates := buildWellKnownCandidates(serverURL)
	urls := make([]*url.URL, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == nil {
			continue
		}
		urls = append(urls, candidate.URL)
	}
	return urls
}

func buildWellKnownCandidates(serverURL *url.URL) []DiscoveryCandidate {
	if serverURL == nil {
		return nil
	}

	base := &url.URL{
		Scheme: serverURL.Scheme,
		Host:   serverURL.Host,
		Path:   defaultProtectedResourceMetadataURI,
	}

	candidates := make([]DiscoveryCandidate, 0, 2)
	pathSuffix := strings.Trim(serverURL.EscapedPath(), "/")
	if pathSuffix != "" {
		withPath := *base
		withPath.Path = path.Join(base.Path, pathSuffix)
		candidates = append(candidates, DiscoveryCandidate{
			URL:    &withPath,
			Source: DiscoverySourceWellKnownPath,
		})
	}

	candidates = append(candidates, DiscoveryCandidate{
		URL:    base,
		Source: DiscoverySourceWellKnownRoot,
	})

	return candidates
}

// BuildOAuthDiscoveryCandidates returns the ordered list of OAuth discovery candidates
// plus probe metadata for UI/reporting. It attempts WWW-Authenticate first, then
// the RFC 9728 well-known URLs.
func BuildOAuthDiscoveryCandidates(
	ctx context.Context,
	client *http.Client,
	serverURL *url.URL,
	logger *slog.Logger,
) ([]DiscoveryCandidate, *WWWAuthenticateProbeStatus) {
	if serverURL == nil {
		return nil, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	probe := probeWWWAuthenticateResourceMetadata(probeCtx, client, serverURL, logger)
	candidates := make([]DiscoveryCandidate, 0, 3)
	if probe.URL != nil {
		candidates = append(candidates, DiscoveryCandidate{
			URL:    probe.URL,
			Source: DiscoverySourceWWWAuthenticate,
		})
	}
	candidates = append(candidates, buildWellKnownCandidates(serverURL)...)
	return dedupeCandidates(candidates), probe.status()
}

func probeWWWAuthenticateResourceMetadata(
	ctx context.Context,
	client *http.Client,
	serverURL *url.URL,
	logger *slog.Logger,
) wwwAuthenticateProbeResult {
	result := wwwAuthenticateProbeResult{Attempted: false}
	if client == nil {
		result.Error = "oauth discovery: http client is nil"
		return result
	}
	if serverURL == nil {
		result.Error = "oauth discovery: server URL is nil"
		return result
	}
	result.Attempted = true

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL.String(), http.NoBody)
	if err != nil {
		result.Error = fmt.Sprintf("oauth discovery: build WWW-Authenticate probe: %v", err)
		return result
	}
	req.Header.Set("User-Agent", version.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		result.Error = fmt.Sprintf("oauth discovery: WWW-Authenticate probe failed: %v", err)
		if logger != nil {
			logger.WarnContext(ctx, "oauth discovery WWW-Authenticate probe failed", slog.String("error", err.Error()))
		}
		return result
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		return result
	}

	header := resp.Header.Get("WWW-Authenticate")
	if header == "" {
		result.Error = "oauth discovery: WWW-Authenticate header missing"
		return result
	}

	parsed, err := parseResourceMetadataFromWWWAuthenticate(header)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	result.URL = parsed
	return result
}

func parseResourceMetadataFromWWWAuthenticate(header string) (*url.URL, error) {
	match := resourceMetadataParamPattern.FindStringSubmatch(header)
	if len(match) == 0 {
		return nil, fmt.Errorf("oauth discovery: resource_metadata missing in WWW-Authenticate")
	}

	value := match[1]
	if value == "" {
		value = match[2]
	}
	if value == "" {
		return nil, fmt.Errorf("oauth discovery: resource_metadata empty in WWW-Authenticate")
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("oauth discovery: parse resource_metadata URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("oauth discovery: resource_metadata must be absolute")
	}
	return parsed, nil
}

func dedupeCandidates(candidates []DiscoveryCandidate) []DiscoveryCandidate {
	if len(candidates) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(candidates))
	out := make([]DiscoveryCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == nil {
			continue
		}
		key := candidate.URL.String()
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, candidate)
	}
	return out
}

func candidatesToStrings(candidates []DiscoveryCandidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.URL == nil {
			continue
		}
		out = append(out, candidate.URL.String())
	}
	return out
}
