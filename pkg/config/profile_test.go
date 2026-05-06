package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadUsesProfileFromExplicitDir(t *testing.T) {
	dir := t.TempDir()
	name := "sample_mcp_with_dcr"
	path := filepath.Join(dir, name+".yaml")
	headerFile := filepath.Join(dir, "discovery-secret.txt")
	if err := os.WriteFile(headerFile, []byte("profile-discovery-secret\n"), 0o600); err != nil {
		t.Fatalf("write header secret file: %v", err)
	}
	writeProfileFile(t, path, `
config_version: 1
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: env:PROFILE_CONTROL_PLANE_API_KEY
  extra_headers:
    X-Control-Profile: env:PROFILE_CONTROL_HEADER
mcp:
  extra_headers:
    X-Internal-Auth: env:PROFILE_MCP_HEADER
  discovery_extra_headers:
    X-Discovery-Auth: file:`+headerFile+`
  server_urls:
    - channel: main
      url: https://profile-mcp.example/mcp
`)

	cfg, err := Load([]string{"--profile", name, "--profile-dir", dir}, lookupEnvMap(map[string]string{
		"PROFILE_CONTROL_PLANE_API_KEY": "profile-key",
		"PROFILE_CONTROL_HEADER":        "profile-control-secret",
		"PROFILE_MCP_HEADER":            "profile-mcp-secret",
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Runtime.ConfigFile != path {
		t.Fatalf("expected config file %q, got %q", path, cfg.Runtime.ConfigFile)
	}
	if cfg.Runtime.ProfileName != name {
		t.Fatalf("expected profile name %q, got %q", name, cfg.Runtime.ProfileName)
	}
	if cfg.Runtime.ProfilePath != path {
		t.Fatalf("expected profile path %q, got %q", path, cfg.Runtime.ProfilePath)
	}
	if cfg.Runtime.ProfileDir != dir {
		t.Fatalf("expected profile dir %q, got %q", dir, cfg.Runtime.ProfileDir)
	}
	if cfg.ControlPlane.APIKey != "profile-key" {
		t.Fatalf("expected resolved profile API key, got %q", cfg.ControlPlane.APIKey)
	}
	if cfg.ControlPlane.ExtraHeaders["X-Control-Profile"] != "profile-control-secret" {
		t.Fatalf("unexpected resolved profile control-plane headers: %#v", cfg.ControlPlane.ExtraHeaders)
	}
	if cfg.MCP.ServerURL == nil || cfg.MCP.ServerURL.String() != "https://profile-mcp.example/mcp" {
		t.Fatalf("unexpected profile MCP server URL: %v", cfg.MCP.ServerURL)
	}
	if cfg.MCP.ExtraHeaders["X-Internal-Auth"] != "profile-mcp-secret" {
		t.Fatalf("unexpected resolved profile MCP headers: %#v", cfg.MCP.ExtraHeaders)
	}
	if cfg.MCP.DiscoveryExtraHeaders["X-Discovery-Auth"] != "profile-discovery-secret" {
		t.Fatalf("unexpected resolved profile discovery headers: %#v", cfg.MCP.DiscoveryExtraHeaders)
	}
}

func TestLoadUsesXDGProfileDefault(t *testing.T) {
	xdgHome := t.TempDir()
	name := "xdg_profile"
	path := filepath.Join(xdgHome, "tunnel-client", name+".yaml")
	writeProfileFile(t, path, `
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: profile-key
mcp:
  commands:
    - channel: main
      command: python server.py
`)

	cfg, err := Load([]string{"--profile", name}, lookupEnvMap(map[string]string{
		"XDG_CONFIG_HOME": xdgHome,
		"HOME":            filepath.Join(t.TempDir(), "home"),
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Runtime.ProfilePath != path {
		t.Fatalf("expected XDG profile path %q, got %q", path, cfg.Runtime.ProfilePath)
	}
	if cfg.MCP.Command != "python server.py" {
		t.Fatalf("expected MCP command from profile, got %q", cfg.MCP.Command)
	}
}

func TestLoadUsesProfileDirEnvOverride(t *testing.T) {
	dir := t.TempDir()
	name := "env_profile"
	path := filepath.Join(dir, name+".yaml")
	writeProfileFile(t, path, `
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: profile-key
mcp:
  server_urls:
    - channel: main
      url: https://env-profile.example/mcp
`)

	cfg, err := Load(nil, lookupEnvMap(map[string]string{
		ProfileEnvName:    name,
		ProfileDirEnvName: dir,
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if cfg.Runtime.ProfileName != name || cfg.Runtime.ProfilePath != path {
		t.Fatalf("unexpected runtime profile metadata: %#v", cfg.Runtime)
	}
}

func TestLoadRejectsMutuallyExclusiveProfileConfigSources(t *testing.T) {
	configPath := writeTempConfigFile(t, `
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: config-key
mcp:
  server_urls:
    - channel: main
      url: https://config.example/mcp
`)

	_, err := Load([]string{"--config", configPath, "--profile", "sample"}, lookupEnvMap(nil))
	if err == nil || !strings.Contains(err.Error(), "--config and --profile are mutually exclusive") {
		t.Fatalf("expected explicit source conflict, got %v", err)
	}

	_, err = Load(nil, lookupEnvMap(map[string]string{
		ConfigEnvName:  configPath,
		ProfileEnvName: "sample",
	}))
	if err == nil || !strings.Contains(err.Error(), "TUNNEL_CLIENT_CONFIG and TUNNEL_CLIENT_PROFILE are mutually exclusive") {
		t.Fatalf("expected env source conflict, got %v", err)
	}
}

func TestValidateProfileRejectsInvalidNamesAndSecretReferenceSyntax(t *testing.T) {
	if err := ValidateProfileName("sample_mcp_with_dcr"); err != nil {
		t.Fatalf("expected underscore profile name to be valid: %v", err)
	}
	if err := ValidateProfileName("../sample"); err == nil {
		t.Fatalf("expected path separator profile name to be invalid")
	}

	err := ValidateProfileBytes("bad.yaml", []byte(`
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: "env:"
mcp:
  server_urls:
    - channel: main
      url: https://mcp.example/mcp
`))
	if err == nil || !strings.Contains(err.Error(), "environment variable name is invalid") {
		t.Fatalf("expected invalid env reference, got %v", err)
	}

	err = ValidateProfileBytes("bad-header.yaml", []byte(`
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: env:PROFILE_CONTROL_PLANE_API_KEY
mcp:
  extra_headers:
    X-Internal-Auth: env:BAD-NAME
  server_urls:
    - channel: main
      url: https://mcp.example/mcp
`))
	if err == nil || !strings.Contains(err.Error(), "environment variable name is invalid") {
		t.Fatalf("expected invalid header env reference, got %v", err)
	}
}

func TestValidateProfileDoesNotResolveSecrets(t *testing.T) {
	err := ValidateProfileBytes("profile.yaml", []byte(`
control_plane:
  tunnel_id: tunnel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  api_key: env:NOT_SET_IN_TEST
mcp:
  extra_headers:
    X-Internal-Auth: env:NOT_SET_IN_TEST
  discovery_extra_headers:
    X-Discovery-Auth: file:/path/not/read/during/validation
  server_urls:
    - channel: main
      url: https://mcp.example/mcp
`))
	if err != nil {
		t.Fatalf("expected validation without secret resolution to pass, got %v", err)
	}
}

func writeProfileFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir profile dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write profile file: %v", err)
	}
}
