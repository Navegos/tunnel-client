package oauth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"go.openai.org/api/tunnel-client/pkg/harpoon/hostbus"
)

func buildURLBundleFromPRMD(payload []byte, fetchedAt time.Time, sourceURL *url.URL) (hostbus.URLBundle, error) {
	var metadata oauthex.ProtectedResourceMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return hostbus.URLBundle{}, fmt.Errorf("decode protected resource metadata: %w", err)
	}

	bundle := hostbus.URLBundle{FetchedAt: fetchedAt}
	bundle.URLs = append(bundle.URLs, urlRecordFromPRMDResource(metadata.Resource, 0))

	for i, server := range metadata.AuthorizationServers {
		bundle.URLs = append(bundle.URLs, urlRecordFromPRMDAuthServer(server, i))
	}
	if sourceURL != nil {
		bundle.URLs = append(bundle.URLs, urlRecordFromPRMDSource(sourceURL, len(bundle.URLs)))
	}

	bundle.URLs = filterURLRecords(bundle.URLs)
	if len(bundle.URLs) == 0 {
		return hostbus.URLBundle{}, fmt.Errorf("no urls found in protected resource metadata")
	}
	return bundle, nil
}

func urlRecordFromPRMDResource(raw string, index int) hostbus.URLRecord {
	return hostbus.URLRecord{
		URL:         parseURL(raw),
		Description: "PRMD resource",
		Tags:        defaultPRMDTags("prmd-resource", index),
	}
}

func urlRecordFromPRMDAuthServer(raw string, index int) hostbus.URLRecord {
	return hostbus.URLRecord{
		URL:         parseURL(raw),
		Description: "PRMD authorization server",
		Tags:        defaultPRMDTags("prmd-auth-server", index),
	}
}

func urlRecordFromPRMDSource(sourceURL *url.URL, index int) hostbus.URLRecord {
	if sourceURL == nil {
		return hostbus.URLRecord{}
	}
	return hostbus.URLRecord{
		URL:         sourceURL,
		Description: "PRMD source URL",
		Tags:        defaultPRMDTags("prmd-source", index),
	}
}

func defaultPRMDTags(role string, index int) []hostbus.Tag {
	return []hostbus.Tag{
		{Key: hostbus.TagKeySource, Value: "oauth"},
		{Key: hostbus.TagKeyRole, Value: role},
		{Key: hostbus.TagKeyIndex, Value: fmt.Sprintf("%d", index)},
	}
}

func parseURL(raw string) *url.URL {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil
	}
	return parsed
}

func filterURLRecords(records []hostbus.URLRecord) []hostbus.URLRecord {
	out := make([]hostbus.URLRecord, 0, len(records))
	for _, record := range records {
		if record.URL == nil {
			continue
		}
		out = append(out, record)
	}
	return out
}
