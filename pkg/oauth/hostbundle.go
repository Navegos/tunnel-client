package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"go.openai.org/api/tunnel-client/pkg/harpoon/hostbus"
)

func buildURLBundleFromPRMDWithAuthServerMetadata(
	ctx context.Context,
	client *http.Client,
	payload []byte,
	fetchedAt time.Time,
	sourceURL *url.URL,
	logger *slog.Logger,
) (hostbus.URLBundle, *AuthServerMetadataFetchResult, error) {
	var metadata oauthex.ProtectedResourceMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return hostbus.URLBundle{}, nil, fmt.Errorf("decode protected resource metadata: %w", err)
	}

	records := make([]hostbus.URLRecord, 0, 2+len(metadata.AuthorizationServers)*8)
	records = append(records, urlRecordFromPRMDResource(metadata.Resource, 0))

	for i, server := range metadata.AuthorizationServers {
		records = append(records, urlRecordFromPRMDAuthServer(server, i))
	}
	if sourceURL != nil {
		records = append(records, urlRecordFromPRMDSource(sourceURL, 0))
	}

	var authServerMetadataFetch *AuthServerMetadataFetchResult
	if len(metadata.AuthorizationServers) > 0 {
		derivedRecords, fetchResult := buildAuthServerMetadataURLRecords(
			ctx,
			client,
			metadata.AuthorizationServers[0],
			0,
			logger,
		)
		authServerMetadataFetch = fetchResult
		records = append(
			records,
			derivedRecords...,
		)
	}

	bundle := hostbus.URLBundle{FetchedAt: fetchedAt}
	bundle.URLs = records
	bundle.URLs = filterURLRecords(bundle.URLs)
	if len(bundle.URLs) == 0 {
		return hostbus.URLBundle{}, authServerMetadataFetch, fmt.Errorf("no urls found in protected resource metadata")
	}
	return bundle, authServerMetadataFetch, nil
}

// BuildURLBundleFromPRMDWithAuthServerMetadata builds a Harpoon registration bundle
// from Protected Resource Metadata payload and enriches it with auth-server metadata
// endpoint records.
func BuildURLBundleFromPRMDWithAuthServerMetadata(
	ctx context.Context,
	client *http.Client,
	payload []byte,
	fetchedAt time.Time,
	sourceURL *url.URL,
	logger *slog.Logger,
) (hostbus.URLBundle, *AuthServerMetadataFetchResult, error) {
	return buildURLBundleFromPRMDWithAuthServerMetadata(
		ctx,
		client,
		payload,
		fetchedAt,
		sourceURL,
		logger,
	)
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

func buildAuthServerMetadataURLRecords(
	ctx context.Context,
	client *http.Client,
	authServerRaw string,
	authServerIndex int,
	logger *slog.Logger,
) ([]hostbus.URLRecord, *AuthServerMetadataFetchResult) {
	issuerURL := parseURL(authServerRaw)
	if issuerURL == nil {
		return nil, &AuthServerMetadataFetchResult{IssuerURL: authServerRaw}
	}
	if client == nil {
		return nil, &AuthServerMetadataFetchResult{IssuerURL: issuerURL.String()}
	}

	meta, fetchResult, err := FetchAuthServerMetadataWithResult(ctx, client, issuerURL.String())
	if fetchResult == nil {
		fetchResult = &AuthServerMetadataFetchResult{IssuerURL: issuerURL.String()}
	}
	if err != nil {
		if logger != nil {
			logger.WarnContext(ctx, "oauth auth-server metadata fetch failed",
				slog.String("issuer", issuerURL.String()),
				slog.Int("auth_server_index", authServerIndex),
				slog.String("error", err.Error()),
			)
		}
		return nil, fetchResult
	}

	records := make([]hostbus.URLRecord, 0, 6)
	records = appendAuthServerMetadataRecord(
		records,
		fetchResult.SelectedURL,
		"Auth server metadata URL",
		"auth-server-metadata",
		authServerIndex,
	)
	records = appendAuthServerMetadataRecord(records, meta.Issuer, "Auth server issuer", "issuer", authServerIndex)
	records = appendAuthServerMetadataRecord(records, meta.AuthorizationEndpoint, "Auth server authorization endpoint", "authorization-endpoint", authServerIndex)
	records = appendAuthServerMetadataRecord(records, meta.TokenEndpoint, "Auth server token endpoint", "token-endpoint", authServerIndex)
	records = appendAuthServerMetadataRecord(records, meta.JWKSURI, "Auth server JWKS URI", "jwks-uri", authServerIndex)
	records = appendAuthServerMetadataRecord(records, meta.IntrospectionEndpoint, "Auth server introspection endpoint", "introspection-endpoint", authServerIndex)
	records = appendAuthServerMetadataRecord(records, meta.RegistrationEndpoint, "Auth server registration endpoint", "registration-endpoint", authServerIndex)
	return records, fetchResult
}

func appendAuthServerMetadataRecord(
	records []hostbus.URLRecord,
	raw string,
	description string,
	role string,
	authServerIndex int,
) []hostbus.URLRecord {
	parsed := parseURL(raw)
	if parsed == nil {
		return records
	}
	return append(records, hostbus.URLRecord{
		URL:         parsed,
		Description: description,
		Tags:        defaultAuthServerMetadataTags(role, authServerIndex),
	})
}

func defaultAuthServerMetadataTags(role string, authServerIndex int) []hostbus.Tag {
	return []hostbus.Tag{
		{Key: hostbus.TagKeySource, Value: "oauth"},
		{Key: hostbus.TagKeyRole, Value: role},
		{Key: hostbus.TagKeyIndex, Value: fmt.Sprintf("%d", authServerIndex)},
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
