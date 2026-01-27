package adminui

import (
	"time"

	"go.openai.org/api/tunnel-client/pkg/oauth"
)

type oauthStatusResponse struct {
	DiscoveryURLs        []string                          `json:"discovery_urls,omitempty"`
	Metadata             *oauth.DiscoveryResult            `json:"metadata,omitempty"`
	Error                string                            `json:"error,omitempty"`
	Pending              bool                              `json:"pending,omitempty"`
	WWWAuthenticateProbe *oauth.WWWAuthenticateProbeStatus `json:"www_authenticate_probe,omitempty"`
	MetadataSource       string                            `json:"metadata_source,omitempty"`
}

func buildOAuthStatus(p routeParams) oauthStatusResponse {
	out := oauthStatusResponse{}

	if p.MCPConfig != nil {
		urls := oauth.BuildResourceMetadataURLs(p.MCPConfig.ServerURL)
		out.DiscoveryURLs = make([]string, 0, len(urls))
		for _, u := range urls {
			if u == nil {
				continue
			}
			out.DiscoveryURLs = append(out.DiscoveryURLs, u.String())
		}
	}

	if p.OAuthState == nil {
		return out
	}

	if result, probe, urls, err, ok := p.OAuthState.Wait(10 * time.Millisecond); ok {
		if len(urls) > 0 {
			out.DiscoveryURLs = urls
		}
		out.WWWAuthenticateProbe = probe
		if err != nil {
			out.Error = err.Error()
		}
		if result != nil {
			out.Metadata = result
		}
		out.MetadataSource = deriveMetadataSource(result, probe)
		return out
	}

	out.Pending = true
	return out
}

func deriveMetadataSource(
	result *oauth.DiscoveryResult,
	probe *oauth.WWWAuthenticateProbeStatus,
) string {
	if result == nil || result.URL == "" {
		return ""
	}
	if probe != nil && probe.URL != "" && result.URL == probe.URL {
		return "www_authenticate"
	}
	return "well_known"
}
