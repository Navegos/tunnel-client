package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.openai.org/api/tunnel-client/pkg/codexappserver"
	"go.openai.org/api/tunnel-client/pkg/codexplugin"
)

const codexCLIDocsURL = "https://developers.openai.com/codex/cli"

type codexInstallMethod struct {
	Name             string `json:"name"`
	InstallCommand   string `json:"install_command"`
	UpgradeCommand   string `json:"upgrade_command"`
	UninstallCommand string `json:"uninstall_command"`
}

type codexStatusReport struct {
	State                       string                   `json:"state"`
	DocsURL                     string                   `json:"docs_url"`
	Detected                    bool                     `json:"detected"`
	Path                        string                   `json:"path,omitempty"`
	Version                     string                   `json:"version,omitempty"`
	AppServerSupported          bool                     `json:"app_server_supported"`
	AppServerSupportError       string                   `json:"app_server_support_error,omitempty"`
	BridgeError                 string                   `json:"bridge_error,omitempty"`
	PluginInstalled             bool                     `json:"plugin_installed"`
	PluginDir                   string                   `json:"plugin_dir,omitempty"`
	PluginBinaryHint            string                   `json:"plugin_binary_hint,omitempty"`
	CurrentTunnelClientPath     string                   `json:"current_tunnel_client_path,omitempty"`
	PluginMatchesCurrentBinary  *bool                    `json:"plugin_matches_current_binary,omitempty"`
	PluginReinstallCommand      string                   `json:"plugin_reinstall_command,omitempty"`
	PreferredInstallMethod      string                   `json:"preferred_install_method,omitempty"`
	RecommendedInstallCommand   string                   `json:"recommended_install_command,omitempty"`
	RecommendedUpgradeCommand   string                   `json:"recommended_upgrade_command,omitempty"`
	RecommendedUninstallCommand string                   `json:"recommended_uninstall_command,omitempty"`
	FallbackInstallCommands     []string                 `json:"fallback_install_commands,omitempty"`
	BridgeReady                 bool                     `json:"bridge_ready"`
	AssistantState              string                   `json:"assistant_state,omitempty"`
	AssistantError              string                   `json:"assistant_error,omitempty"`
	Snapshot                    *codexappserver.Snapshot `json:"snapshot,omitempty"`
}

var codexStatusAssistantProbeTimeout = 5 * time.Second

func newCodexCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Use the Codex assistant surface and inspect CLI/app-server/plugin wiring",
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.AddCommand(newCodexStatusCommand(lookupEnv, stdout, stderr))
	cmd.AddCommand(newCodexAssistantCommand(stdout, stderr))
	cmd.AddCommand(newCodexPluginCommand(lookupEnv, stdout, stderr))
	cmd.AddCommand(newCodexGuideCommand("install", "Show official Codex CLI install commands", func() string {
		return "Install Codex with one of the supported package managers below."
	}, stdout, stderr))
	cmd.AddCommand(newCodexGuideCommand("upgrade", "Show official Codex CLI upgrade commands", func() string {
		return "Upgrade Codex using the same package manager that installed it."
	}, stdout, stderr))
	cmd.AddCommand(newCodexGuideCommand("uninstall", "Show official Codex CLI uninstall commands", func() string {
		return "Remove Codex with the same package manager that installed it."
	}, stdout, stderr))
	return cmd
}

func newCodexStatusCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Report Codex discovery, app-server availability, login state, and plugin wiring",
		RunE: func(cmd *cobra.Command, args []string) error {
			report := inspectCodexStatus(lookupEnv)
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), report)
			}
			printCodexStatus(cmd.OutOrStdout(), report)
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return cmd
}

func newCodexGuideCommand(use string, short string, intro func() string, stdout io.Writer, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			methods := availableCodexInstallMethods()
			preferred := preferredCodexInstallMethod(methods)
			if jsonOutput {
				payload := map[string]any{
					"action":           use,
					"docs_url":         codexCLIDocsURL,
					"preferred_method": preferred.Name,
					"methods":          methods,
				}
				return writeJSON(cmd.OutOrStdout(), payload)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), intro())
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Preferred on this host: %s\n", preferred.Name)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", commandForAction(preferred, use))
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Fallbacks:")
			for _, method := range methods {
				if method.Name == preferred.Name {
					continue
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s\n", method.Name, commandForAction(method, use))
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Docs: %s\n", codexCLIDocsURL)
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit JSON output")
	return cmd
}

func inspectCodexStatus(lookupEnv func(string) (string, bool)) codexStatusReport {
	methods := availableCodexInstallMethods()
	preferred := preferredCodexInstallMethod(methods)
	report := codexStatusReport{
		State:                       "missing",
		DocsURL:                     codexCLIDocsURL,
		PluginInstalled:             false,
		AppServerSupported:          false,
		PreferredInstallMethod:      preferred.Name,
		RecommendedInstallCommand:   preferred.InstallCommand,
		RecommendedUpgradeCommand:   preferred.UpgradeCommand,
		RecommendedUninstallCommand: preferred.UninstallCommand,
		FallbackInstallCommands:     fallbackInstallCommands(methods, preferred.Name),
	}

	detection := codexplugin.Detect(lookupEnv)
	report.PluginInstalled = detection.PluginInstalled
	report.PluginDir = detection.PluginDir
	report.PluginBinaryHint = detection.PluginBinaryHint
	if current := currentExecutablePath(); current != "" {
		normalizedCurrent, err := codexplugin.NormalizeBinaryPath(current)
		if err == nil {
			report.CurrentTunnelClientPath = normalizedCurrent
			if detection.PluginBinaryHint != "" {
				matches := normalizedCurrent == detection.PluginBinaryHint
				report.PluginMatchesCurrentBinary = &matches
				if !matches {
					report.PluginReinstallCommand = "tunnel-client codex plugin install"
				}
			}
		}
	}

	path, err := exec.LookPath("codex")
	if err != nil {
		return report
	}
	report.Detected = true
	report.Path = path
	if versionText, versionErr := readCommandLineOutput(path, "--version"); versionErr == nil {
		report.Version = versionText
	}
	if _, helpErr := readCommandOutput(path, "app-server", "--help"); helpErr != nil {
		report.State = "unsupported"
		report.AppServerSupportError = helpErr.Error()
		return report
	}

	report.AppServerSupported = true
	bridge := codexappserver.NewBridge(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := bridge.EnsureStarted(ctx); err != nil {
		report.State = "error"
		report.BridgeError = err.Error()
		return report
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	defer func() { _ = bridge.Stop(stopCtx) }()

	snapshot := bridge.Snapshot()
	report.BridgeReady = snapshot.Ready
	report.Snapshot = &snapshot
	switch {
	case (snapshot.Account == nil || strings.TrimSpace(snapshot.Account.Type) == "") &&
		snapshot.RequiresOpenAIAuth != nil &&
		*snapshot.RequiresOpenAIAuth:
		report.State = "logged_out"
		report.AssistantState = "logged_out"
		return report
	default:
		if probeErr := probeCodexAssistantReady(bridge); probeErr != nil {
			report.State = "bridge_ready"
			report.AssistantState = "unavailable"
			report.AssistantError = probeErr.Error()
		} else {
			report.State = "ready"
			report.AssistantState = "ready"
		}
	}
	return report
}

func printCodexStatus(w io.Writer, report codexStatusReport) {
	printStatusSection(w, "Codex", []string{
		fmt.Sprintf("State: %s", report.State),
		optionalStatusLine("Path", report.Path),
		optionalStatusLine("Version", report.Version),
	})
	if !report.Detected {
		_, _ = fmt.Fprintln(w)
		printStatusSection(w, "Commands", []string{
			fmt.Sprintf("Install: %s", report.RecommendedInstallCommand),
			fmt.Sprintf("Docs: %s", report.DocsURL),
		})
		return
	}
	_, _ = fmt.Fprintln(w)
	pluginLines := []string{}
	if report.PluginInstalled {
		pluginLines = append(pluginLines,
			"Status: installed",
			optionalStatusLine("Dir", report.PluginDir),
			optionalStatusLine("Binary hint", report.PluginBinaryHint),
		)
		if report.CurrentTunnelClientPath != "" {
			pluginLines = append(pluginLines, fmt.Sprintf("Current tunnel-client: %s", report.CurrentTunnelClientPath))
		}
		if report.PluginMatchesCurrentBinary != nil {
			pluginLines = append(pluginLines, fmt.Sprintf("Matches current tunnel-client: %t", *report.PluginMatchesCurrentBinary))
			if !*report.PluginMatchesCurrentBinary && report.PluginReinstallCommand != "" {
				pluginLines = append(pluginLines, "Codex will keep routing tunnel-mcp through the binary hint above until you repoint the plugin.")
				pluginLines = append(pluginLines, fmt.Sprintf("Reinstall plugin to use this binary: %s", report.PluginReinstallCommand))
			}
		}
	} else {
		pluginLines = append(pluginLines, "Status: not installed")
		if report.PluginDir != "" {
			pluginLines = append(pluginLines, fmt.Sprintf("Expected dir: %s", report.PluginDir))
		}
	}
	printStatusSection(w, "Tunnel MCP plugin", pluginLines)
	_, _ = fmt.Fprintln(w)
	appLines := []string{}
	if report.AppServerSupported {
		appLines = append(appLines, "app-server: supported")
	} else {
		appLines = append(appLines, fmt.Sprintf("app-server: unavailable (%s)", valueOrDash(report.AppServerSupportError)))
	}
	if report.BridgeError != "" {
		appLines = append(appLines, fmt.Sprintf("Bridge error: %s", report.BridgeError))
	}
	if report.BridgeReady {
		appLines = append(appLines, "Bridge: ready")
	}
	if report.AssistantState != "" {
		appLines = append(appLines, fmt.Sprintf("Assistant readiness: %s", report.AssistantState))
	}
	if report.AssistantError != "" {
		appLines = append(appLines, fmt.Sprintf("Assistant error: %s", report.AssistantError))
	}
	if report.AppServerSupported {
		appLines = append(appLines, "Assistant: tunnel-client codex assistant")
	}
	if report.Snapshot != nil && report.Snapshot.Account != nil {
		account := report.Snapshot.Account
		label := valueOrDash(account.Email)
		if strings.TrimSpace(account.PlanType) != "" {
			label += " (" + account.PlanType + ")"
		}
		appLines = append(appLines, fmt.Sprintf("Account: %s", label))
	}
	if !report.PluginInstalled {
		appLines = append(appLines, "Note: Bridge and Assistant readiness reflect Codex itself, not plugin files on disk. If you just uninstalled tunnel-mcp, already-running Codex sessions may still have the previously loaded plugin until restart.")
	}
	printStatusSection(w, "Codex app / bridge", appLines)
	_, _ = fmt.Fprintln(w)
	printStatusSection(w, "Commands", []string{
		fmt.Sprintf("Install: %s", report.RecommendedInstallCommand),
		fmt.Sprintf("Upgrade: %s", report.RecommendedUpgradeCommand),
		fmt.Sprintf("Uninstall: %s", report.RecommendedUninstallCommand),
		fmt.Sprintf("Docs: %s", report.DocsURL),
	})
}

func printStatusSection(w io.Writer, heading string, lines []string) {
	_, _ = fmt.Fprintf(w, "%s:\n", heading)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s\n", line)
	}
}

func optionalStatusLine(label string, value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", label, value)
}

func probeCodexAssistantReady(bridge *codexappserver.Bridge) error {
	if bridge == nil {
		return nil
	}
	workingDir := assistantWorkingDirectory("")
	ctx, cancel := context.WithTimeout(context.Background(), codexStatusAssistantProbeTimeout)
	defer cancel()
	_, err := bridge.StartThread(ctx, codexappserver.ThreadStartParams{
		CWD:                   workingDir,
		ApprovalPolicy:        defaultCodexAssistantApprovalPolicy,
		SandboxType:           defaultCodexAssistantSandboxType,
		DeveloperInstructions: buildCodexCLIDeveloperInstructions(workingDir, ""),
	})
	return err
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func availableCodexInstallMethods() []codexInstallMethod {
	return []codexInstallMethod{
		{
			Name:             "homebrew",
			InstallCommand:   "brew install codex",
			UpgradeCommand:   "brew upgrade codex",
			UninstallCommand: "brew uninstall codex",
		},
		{
			Name:             "npm",
			InstallCommand:   "npm install -g @openai/codex",
			UpgradeCommand:   "npm i -g @openai/codex@latest",
			UninstallCommand: "npm uninstall -g @openai/codex",
		},
	}
}

func preferredCodexInstallMethod(methods []codexInstallMethod) codexInstallMethod {
	brewAvailable := commandAvailable("brew")
	npmAvailable := commandAvailable("npm")
	switch {
	case runtime.GOOS == "darwin" && brewAvailable:
		return methods[0]
	case npmAvailable:
		return methods[1]
	case brewAvailable:
		return methods[0]
	default:
		return methods[1]
	}
}

func fallbackInstallCommands(methods []codexInstallMethod, preferred string) []string {
	out := []string{}
	for _, method := range methods {
		if method.Name == preferred {
			continue
		}
		out = append(out, method.InstallCommand)
	}
	return out
}

func commandForAction(method codexInstallMethod, action string) string {
	switch action {
	case "upgrade":
		return method.UpgradeCommand
	case "uninstall":
		return method.UninstallCommand
	default:
		return method.InstallCommand
	}
}

func commandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func readCommandOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("%s", text)
	}
	return text, nil
}

func readCommandLineOutput(name string, args ...string) (string, error) {
	text, err := readCommandOutput(name, args...)
	if err != nil {
		return "", err
	}
	lines := strings.Split(text, "\n")
	for idx := len(lines) - 1; idx >= 0; idx-- {
		if line := strings.TrimSpace(lines[idx]); line != "" {
			return line, nil
		}
	}
	return "", nil
}
