package pluginsbundle

import "testing"

func TestValidatePluginSegment(t *testing.T) {
	t.Parallel()

	valid := []string{"tunnel-mcp", "tunnel_mcp", "tunnel.mcp", "TunnelMCP1"}
	for _, value := range valid {
		if err := validatePluginSegment(value, "plugin name"); err != nil {
			t.Fatalf("validatePluginSegment(%q) returned error: %v", value, err)
		}
	}

	invalid := []string{"", ".", "..", "../escape", "bad/name", `bad"name`, " space", "-leading-dash"}
	for _, value := range invalid {
		if err := validatePluginSegment(value, "plugin name"); err == nil {
			t.Fatalf("validatePluginSegment(%q) unexpectedly succeeded", value)
		}
	}
}
