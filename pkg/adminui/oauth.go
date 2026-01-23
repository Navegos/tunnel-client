package adminui

import (
	"time"

	"go.openai.org/api/tunnel-client/pkg/oauth"
)

type oauthStatusResponse struct {
	DiscoveryURLs []string               `json:"discovery_urls,omitempty"`
	Metadata      *oauth.DiscoveryResult `json:"metadata,omitempty"`
	Error         string                 `json:"error,omitempty"`
	Pending       bool                   `json:"pending,omitempty"`
}

func buildOAuthStatus(p routeParams) oauthStatusResponse {
	out := oauthStatusResponse{}

	if p.MCPConfig != nil && len(p.MCPConfig.OAuthResourceMetadataURLs) > 0 {
		out.DiscoveryURLs = make([]string, 0, len(p.MCPConfig.OAuthResourceMetadataURLs))
		for _, u := range p.MCPConfig.OAuthResourceMetadataURLs {
			if u == nil {
				continue
			}
			out.DiscoveryURLs = append(out.DiscoveryURLs, u.String())
		}
	}

	if p.OAuthState == nil {
		return out
	}

	if result, err, ok := p.OAuthState.Wait(10 * time.Millisecond); ok {
		if err != nil {
			out.Error = err.Error()
		} else {
			out.Metadata = result
		}
		return out
	}

	out.Pending = true
	return out
}
