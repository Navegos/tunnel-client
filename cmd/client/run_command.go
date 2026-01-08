package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"go.openai.org/api/tunnel-client/pkg/app"
	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/version"
)

type tunnelEventLogger struct {
	*fxevent.SlogLogger
	logger *slog.Logger
	cfg    *config.ControlPlaneConfig
}

func newTunnelEventLogger(logger *slog.Logger, cfg *config.ControlPlaneConfig) fxevent.Logger {
	return &tunnelEventLogger{
		SlogLogger: &fxevent.SlogLogger{Logger: logger},
		logger:     logger,
		cfg:        cfg,
	}
}

func (l *tunnelEventLogger) LogEvent(event fxevent.Event) {
	if started, ok := event.(*fxevent.Started); ok && started.Err == nil {
		tunnelURL := l.cfg.BaseURL.JoinPath("v1", "tunnel", l.cfg.TunnelID.String()).String()
		l.logger.Info("🟢 tunnel-client started",
			slog.String("tunnel_id", l.cfg.TunnelID.String()),
			slog.String("tunnel_url", tunnelURL),
			slog.String("version", version.Version),
		)
	} else {
		l.SlogLogger.LogEvent(event)
	}
}

func newRootCommand(lookupEnv func(string) (string, bool), stdout io.Writer, stderr io.Writer) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:           "tunnel-client",
		Short:         "Tunnel client for the OpenAI MCP control plane",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTunnel(cmd, lookupEnv)
		},
	}
	rootCmd.SetOut(stdout)
	rootCmd.SetErr(stderr)

	config.RegisterFlags(rootCmd.PersistentFlags())

	writeUsage := func(cmd *cobra.Command) {
		config.WriteUsage(rootCmd.PersistentFlags(), cmd.OutOrStdout())
	}
	rootCmd.SetUsageFunc(func(cmd *cobra.Command) error {
		writeUsage(cmd)
		return nil
	})
	rootCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		writeUsage(cmd)
	})
	rootCmd.Version = tunnelClientVersion()
	rootCmd.SetVersionTemplate("{{.Version}}\n")

	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the tunnel client poller",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTunnel(cmd, lookupEnv)
		},
	}
	runCmd.SetUsageFunc(func(cmd *cobra.Command) error {
		writeUsage(cmd)
		return nil
	})
	runCmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		writeUsage(cmd)
	})
	rootCmd.AddCommand(runCmd)

	return rootCmd
}

func runTunnel(cmd *cobra.Command, lookupEnv func(string) (string, bool)) error {
	cfg, err := config.LoadFromFlagSet(cmd.Flags(), lookupEnv)
	if err != nil {
		return fmt.Errorf("configure tunnel-client: %w", err)
	}

	fxApp := app.New(cfg,
		fx.Provide(func() io.Writer { return cmd.OutOrStdout() }),
		fx.WithLogger(func(logger *slog.Logger, cfg *config.ControlPlaneConfig) fxevent.Logger {
			return newTunnelEventLogger(logger, cfg)
		}),
	)
	fxApp.Run()
	return nil
}

func tunnelClientVersion() string {
	if version.GitSHA != "" {
		return fmt.Sprintf("%s (git sha: %s)", version.Version, version.GitSHA)
	}
	return version.Version
}
