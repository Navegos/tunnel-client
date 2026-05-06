package config

import (
	"testing"

	"github.com/spf13/pflag"
)

func TestLoadAcceptsCommonKebabCaseAliases(t *testing.T) {
	t.Parallel()

	cfg, err := Load([]string{
		"--control-plane-tunnel-id", "tunnel_0123456789abcdef0123456789abcdef",
		"--mcp-server-url", "channel=main,url=https://mcp.example/mcp",
		"--mcp-extra-headers", "X-Internal-Auth: alias-static",
		"--mcp-discovery-extra-headers", "X-Discovery-Auth: alias-discovery",
	}, lookupEnvMap(map[string]string{
		"CONTROL_PLANE_API_KEY": "control-key",
	}))
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.ControlPlane.TunnelID.String() != "tunnel_0123456789abcdef0123456789abcdef" {
		t.Fatalf("unexpected tunnel id: %s", cfg.ControlPlane.TunnelID)
	}
	if binding := cfg.MCP.MainChannelBinding(); binding == nil || binding.ServerURL == nil || binding.ServerURL.String() != "https://mcp.example/mcp" {
		t.Fatalf("unexpected main MCP binding: %#v", binding)
	}
	if cfg.MCP.ExtraHeaders["X-Internal-Auth"] != "alias-static" {
		t.Fatalf("unexpected mcp extra headers: %#v", cfg.MCP.ExtraHeaders)
	}
	if cfg.MCP.DiscoveryExtraHeaders["X-Discovery-Auth"] != "alias-discovery" {
		t.Fatalf("unexpected mcp discovery extra headers: %#v", cfg.MCP.DiscoveryExtraHeaders)
	}
}

func TestRegisterFlagsHidesAliasFlags(t *testing.T) {
	t.Parallel()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	for _, alias := range commonFlagAliases {
		flag := fs.Lookup(alias.Alias)
		if flag == nil {
			t.Fatalf("missing alias flag %s", alias.Alias)
		}
		if flag != nil && !flag.Hidden {
			t.Fatalf("expected alias flag %s to be hidden", alias.Alias)
		}
	}
}

func TestValidateTunnelID(t *testing.T) {
	t.Parallel()

	if err := ValidateTunnelID("tunnel_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("ValidateTunnelID returned error: %v", err)
	}
	if err := ValidateTunnelID("not-a-tunnel-id"); err == nil {
		t.Fatalf("expected invalid tunnel id error")
	}
}
