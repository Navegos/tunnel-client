package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.openai.org/api/tunnel-client/pkg/codexplugin"
)

func newPluginCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Manage tunnel-client plugin integrations",
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.AddCommand(newPluginCodexCommand(lookupEnv, stdout, stderr))
	return cmd
}

func newPluginCodexCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Install, uninstall, or export the embedded Codex plugin bundle",
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.AddCommand(newPluginCodexInstallCommand(lookupEnv, stdout, stderr))
	cmd.AddCommand(newPluginCodexUninstallCommand(lookupEnv, stdout, stderr))
	cmd.AddCommand(newPluginCodexExportCommand(stdout, stderr))
	return cmd
}

func newPluginCodexInstallCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	var codexHome string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install the embedded Tunnel MCP Codex plugin into CODEX_HOME",
		RunE: func(cmd *cobra.Command, args []string) error {
			home := codexHome
			if home == "" {
				home = codexplugin.ResolveCodexHome(lookupEnv)
			}
			detection, err := codexplugin.Install(home, currentExecutablePath())
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Installed %s into %s\n", detection.PluginName, detection.PluginDir)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated Codex config %s\n", detection.ConfigPath)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Next:")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  tunnel-client help plugin")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  test -f %s\n", detection.PluginDir+"/.codex-plugin/plugin.json")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  start a new Codex session if plugins were already loaded")
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().StringVar(&codexHome, "codex-home", "", "Override CODEX_HOME for plugin installation")
	return cmd
}

func newPluginCodexExportCommand(stdout io.Writer, stderr io.Writer) *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the embedded Tunnel MCP Codex plugin bundle to disk",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(dir) == "" {
				return fmt.Errorf("--dir is required")
			}
			if err := codexplugin.Export(dir, ""); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Exported embedded Codex plugin bundle to %s\n", dir)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Next:")
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  cd %s && python3 scripts/install_plugin.py --tunnel-client-bin /path/to/tunnel-client\n", dir)
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().StringVar(&dir, "dir", "", "Destination directory")
	return cmd
}

func newPluginCodexUninstallCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	var codexHome string
	cmd := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the embedded Tunnel MCP Codex plugin from CODEX_HOME",
		RunE: func(cmd *cobra.Command, args []string) error {
			home := codexHome
			if home == "" {
				home = codexplugin.ResolveCodexHome(lookupEnv)
			}
			result, err := codexplugin.Uninstall(home)
			if err != nil {
				return err
			}
			if !result.RemovedPluginDir && !result.RemovedConfigSection {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Tunnel MCP plugin is not installed in %s\n", home)
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s\n", result.PluginName, result.PluginDir)
			if result.RemovedConfigSection {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed Codex config section from %s\n", result.ConfigPath)
			} else {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "No matching Codex config section found in %s\n", result.ConfigPath)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Next:")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  tunnel-client plugin codex install")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  start a new Codex session if plugins were already loaded")
			return nil
		},
	}
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.Flags().StringVar(&codexHome, "codex-home", "", "Override CODEX_HOME for plugin removal")
	return cmd
}

func currentExecutablePath() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
