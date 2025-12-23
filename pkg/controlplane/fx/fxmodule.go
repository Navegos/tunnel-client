package fx

import (
	"context"
	"log/slog"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/fx"

	"go.openai.org/api/tunnel-client/pkg/config"
	"go.openai.org/api/tunnel-client/pkg/controlplane"
	"go.openai.org/api/tunnel-client/pkg/controlplane/internal"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
)

// Module wires control-plane polling into the Fx graph.
var Module = fx.Module(
	"controlplane",
	fx.Provide(newTunnelServiceClient, newPoller),
	fx.Invoke(runPoller),
)

type fetcherParams struct {
	fx.In

	Config        *config.ControlPlaneConfig
	Logging       *config.LoggingConfig
	Logger        *slog.Logger
	MeterProvider *sdkmetric.MeterProvider
}

type clientResult struct {
	fx.Out

	Fetcher   controlplane.Fetcher
	Responder controlplane.Responder
}

func newTunnelServiceClient(p fetcherParams) (clientResult, error) {
	logger := p.Logger.With(tclog.FieldComponent, tclog.ComponentControlPlane)
	client, err := internal.NewTunnelServiceClient(context.Background(), p.Config, logger, p.Logging, p.MeterProvider)
	if err != nil {
		return clientResult{}, err
	}

	return clientResult{
		Fetcher:   client,
		Responder: client,
	}, nil
}

type pollerParams struct {
	fx.In

	Config             *config.ControlPlaneConfig
	PolledCommandQueue controlplane.PolledCommandQueue
	Fetcher            controlplane.Fetcher
	Logger             *slog.Logger
	MeterProvider      *sdkmetric.MeterProvider
}

func newPoller(p pollerParams) (internal.Poller, error) {
	logger := p.Logger.With(tclog.FieldComponent, tclog.ComponentControlPlane)
	if p.PolledCommandQueue == nil {
		panic("controlplane poller: dispatcher queue is nil")
	}
	queue := &queueAdapter{
		queue:  p.PolledCommandQueue,
		logger: logger,
	}
	meter := p.MeterProvider.Meter("controlplane")
	return internal.NewPoller(queue, p.Fetcher, logger, meter, p.Config.PollTimeout, p.Config.PollBackoffMin, p.Config.PollBackoffMax)
}

type runnerParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Logger    *slog.Logger
	Poller    internal.Poller
}

func runPoller(p runnerParams) error {
	logger := p.Logger.With(tclog.FieldComponent, tclog.ComponentControlPlane)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	p.Lifecycle.Append(fx.Hook{
		OnStart: func(context.Context) error {
			logger.InfoContext(ctx, "starting control-plane poller")
			go func() {
				defer close(done)
				p.Poller.Run(ctx)
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			logger.InfoContext(ctx, "stopping control-plane poller")
			cancel()
			<-done
			return nil
		},
	})

	return nil
}

type queueAdapter struct {
	queue  controlplane.PolledCommandQueue
	logger *slog.Logger
}

func (q *queueAdapter) Capacity() int {
	return cap(q.queue)
}

func (q *queueAdapter) Length() int {
	return len(q.queue)
}

func (q *queueAdapter) Enqueue(ctx context.Context, cmd controlplane.PolledCommand) bool {
	select {
	case <-ctx.Done():
		return false
	case q.queue <- cmd:
		return true
	}
}
