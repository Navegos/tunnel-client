package pluginsbundle

import (
	assistantkb "go.openai.org/api/tunnel-client/docs"
	"runtime"
	"strings"
)

const (
	tunnelMCPPromptMatchLimit   = 2
	tunnelMCPPromptExcerptChars = 700
)

var tunnelMCPReferenceFiles = []string{
	"tunnel-mcp/skills/tunnel-mcp/references/binary.md",
	"tunnel-mcp/skills/tunnel-mcp/references/setup-and-install.md",
	"tunnel-mcp/skills/tunnel-mcp/references/profiles-state-and-keys.md",
	"tunnel-mcp/skills/tunnel-mcp/references/runtime-flows.md",
	"tunnel-mcp/skills/tunnel-mcp/references/troubleshooting.md",
}

func BuildTunnelMCPPromptContext(prompt string) string {
	if isBinaryAcquisitionPrompt(prompt) {
		binaryExcerpt := buildBundledBinaryGuidanceExcerpt(runtime.GOOS)
		setupExcerpt := buildBundledSetupInstallExcerpt(runtime.GOOS)
		return assistantkb.FormatPromptContext([]string{
			"Curated tunnel-mcp plugin references injected from the binary.",
			"These snippets cover binary acquisition, plugin setup, runtime flows, profiles, state dirs, key split, and troubleshooting.",
			"Use them before guessing how the Codex plugin should create, connect, inspect, or debug a tunnel runtime.",
		}, "plugin_knowledge.match", []assistantkb.Match{
			{
				Path:    "plugins/tunnel-mcp/skills/tunnel-mcp/references/binary.md",
				Heading: "Obtaining a tunnel-client binary",
				Excerpt: binaryExcerpt,
				Score:   100,
			},
			{
				Path:    "plugins/tunnel-mcp/skills/tunnel-mcp/references/setup-and-install.md",
				Heading: "Setup and install",
				Excerpt: setupExcerpt,
				Score:   90,
			},
		})
	}
	matches := assistantkb.SearchFS(
		prompt,
		embeddedPluginFiles,
		tunnelMCPReferenceFiles,
		"plugins/",
		tunnelMCPPromptMatchLimit,
		tunnelMCPPromptExcerptChars,
	)
	return assistantkb.FormatPromptContext([]string{
		"Curated tunnel-mcp plugin references injected from the binary.",
		"These snippets cover binary acquisition, plugin setup, runtime flows, profiles, state dirs, key split, and troubleshooting.",
		"Use them before guessing how the Codex plugin should create, connect, inspect, or debug a tunnel runtime.",
	}, "plugin_knowledge.match", matches)
}

func isBinaryAcquisitionPrompt(prompt string) bool {
	lower := strings.ToLower(strings.TrimSpace(prompt))
	if lower == "" {
		return false
	}
	if !containsPluginPrompt(lower, "tunnel-client", "tunnel client", "tunnel-mcp", "tunnel mcp") {
		return false
	}
	if !containsPluginPrompt(lower, "missing", "not found", "can't find", "cannot find", "could not find", "not installed", "download", "install", "get a binary") {
		return false
	}
	return containsPluginPrompt(lower, "binary", "executable", "plugin", "path", "on path", "command -v")
}

func containsPluginPrompt(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func buildBundledBinaryGuidanceExcerpt(goos string) string {
	buildCommand, wrapperCommand, binaryFlag := assistantkb.BinaryAcquisitionGuidanceForOS(goos)
	return strings.Join([]string{
		"# Obtaining a tunnel-client binary",
		"",
		"First try existing binary discovery:",
		"",
		"- " + binaryFlag,
		"- TUNNEL_CLIENT_BIN",
		"- the installed plugin bundle's .tunnel-client-bin hint",
		"- adjacent local build outputs",
		"- PATH",
		"",
		"If tunnel-client is still missing, use one of these public-safe setup paths:",
		"",
		"- latest releases: https://github.com/openai/tunnel-client/releases/latest",
		"- public repo: https://github.com/openai/tunnel-client",
		"",
		"Source build from the public repo:",
		"",
		"git clone https://github.com/openai/tunnel-client.git",
		"cd tunnel-client",
		buildCommand,
		"",
		"After you have a binary:",
		"",
		"- set TUNNEL_CLIENT_BIN to the full path to the binary",
		"- or rerun the plugin/install command with " + binaryFlag,
		"- or reinstall the plugin with " + binaryFlag,
		"",
		"Do not suggest internal-only installer or checkout-specific commands for generic missing-binary help.",
		"",
		"Do not auto-download, auto-clone, or auto-run remote binaries just because the plugin cannot find tunnel-client.",
		"",
		"From the exported bundle root on this OS, the wrapper-first fallback command is:",
		"",
		"- " + wrapperCommand,
	}, "\n")
}

func buildBundledSetupInstallExcerpt(goos string) string {
	_, wrapperCommand, _ := assistantkb.BinaryAcquisitionGuidanceForOS(goos)
	return strings.Join([]string{
		"# Setup and install",
		"",
		"Use the binary-owned install path when a tunnel-client binary is available:",
		"",
		"- tunnel-client codex plugin install",
		"- tunnel-client codex plugin uninstall",
		"- tunnel-client codex status",
		"",
		"Use the exported bundle only when the binary is not already installed or when you need to inspect the plugin contents first:",
		"",
		"- tunnel-client codex plugin export --dir /tmp/tunnel-mcp",
		"- From the exported bundle root on this OS, run: " + wrapperCommand,
		"",
		"After install, prefer the installed plugin router and persisted .tunnel-client-bin hint over an ambient tunnel-client found on PATH.",
	}, "\n")
}
