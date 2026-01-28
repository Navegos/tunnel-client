package harpoon

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRegistryRejectsInvalidLabel(t *testing.T) {
	registry, err := NewRegistry(true, nil)
	require.NoError(t, err)

	parsed, err := url.Parse("https://example.com")
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "bad label", BaseURL: parsed})
	require.Error(t, err)
}

func TestRegistryRejectsDuplicateLabel(t *testing.T) {
	parsed, err := url.Parse("https://example.com")
	require.NoError(t, err)

	registry, err := NewRegistry(true, []Target{{Label: "auth", BaseURL: parsed}})
	require.NoError(t, err)

	err = registry.RegisterTarget(Target{Label: "auth", BaseURL: parsed})
	require.Error(t, err)
}

func TestRegistryRejectsPlaintextWhenDisallowed(t *testing.T) {
	parsed, err := url.Parse("http://example.com")
	require.NoError(t, err)

	_, err = NewRegistry(false, []Target{{Label: "auth", BaseURL: parsed}})
	require.Error(t, err)
}
