package codexplugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallExportsPluginAndUpdatesConfig(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	tunnelClientBin := filepath.Join(t.TempDir(), "tunnel-client")
	require.NoError(t, os.WriteFile(tunnelClientBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	detection, err := Install(codexHome, tunnelClientBin)
	require.NoError(t, err)

	require.True(t, detection.PluginInstalled)
	require.FileExists(t, filepath.Join(detection.PluginDir, ".codex-plugin", "plugin.json"))
	require.FileExists(t, filepath.Join(detection.PluginDir, "scripts", "install_plugin.py"))
	require.FileExists(t, filepath.Join(detection.PluginDir, "scripts", "tunnel_mcp"))
	require.FileExists(t, filepath.Join(detection.PluginDir, ".tunnel-client-bin"))

	configData, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(configData), `[plugins."tunnel-mcp@debug"]`)
	require.Contains(t, string(configData), "enabled = true")
}

func TestExportWritesBinaryHintWhenProvided(t *testing.T) {
	t.Parallel()

	exportDir := t.TempDir()
	tunnelClientBin := filepath.Join(t.TempDir(), "tunnel-client")
	require.NoError(t, os.WriteFile(tunnelClientBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	err := Export(exportDir, tunnelClientBin)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(exportDir, ".tunnel-client-bin"))
	require.NoError(t, err)
	require.Equal(t, tunnelClientBin+"\n", string(data))
}

func TestDetectReportsMissingPluginInstallHint(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("[features]\nplugins = true\n"), 0o644))

	detection := Detect(func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return codexHome, true
		}
		return "", false
	})

	require.True(t, detection.Detected)
	require.False(t, detection.PluginInstalled)
	require.Equal(t, "tunnel-client plugin codex install", detection.InstallHint)
}

func TestUninstallRemovesPluginBundleAndConfigSection(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	tunnelClientBin := filepath.Join(t.TempDir(), "tunnel-client")
	require.NoError(t, os.WriteFile(tunnelClientBin, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	detection, err := Install(codexHome, tunnelClientBin)
	require.NoError(t, err)

	result, err := Uninstall(codexHome)
	require.NoError(t, err)
	require.True(t, result.RemovedPluginDir)
	require.True(t, result.RemovedConfigSection)
	require.NoDirExists(t, detection.PluginDir)

	configData, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	require.NoError(t, err)
	require.NotContains(t, string(configData), `[plugins."tunnel-mcp@debug"]`)
}

func TestUninstallIsIdempotentWhenPluginMissing(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	result, err := Uninstall(codexHome)
	require.NoError(t, err)
	require.False(t, result.RemovedPluginDir)
	require.False(t, result.RemovedConfigSection)
}
