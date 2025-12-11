package internal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jpillora/backoff"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	tclog "go.openai.org/api/tunnel-client/pkg/log"
	"go.openai.org/api/tunnel-client/pkg/types"
)

const (
	defaultQueueFullDelay = 100 * time.Millisecond
	defaultBackoffMin     = 200 * time.Millisecond
	defaultBackoffMax     = 10 * time.Second
	defaultPollerTimeout  = 30 * time.Second
)

// PolledCommand mirrors the controlplane.PolledCommand interface. It lives in
// this package to avoid an import cycle between controlplane and its internal
// implementation details.
type PolledCommand interface {
	RequestID() types.RequestID
	Message() jsonrpc.Message
	EnqueuedAt() time.Time
	PolledAt() time.Time
	Headers() http.Header
	ShardToken() string
	SessionID() (string, bool)
}

// Queue exposes the minimal methods the poller needs from the dispatcher queue.
type Queue interface {
	Capacity() int
	Length() int
	Enqueue(ctx context.Context, cmd PolledCommand) bool
}

// Fetcher abstracts the control-plane poll endpoint. Implementations should honor
// the provided limit and return at most that many commands so the poller can respect
// downstream backpressure. TunnelServiceRequestID is returned so callers can log/trace
// the control-plane request identifier associated with the poll.
type Fetcher interface {
	Poll(ctx context.Context, limit int) ([]PolledCommand, types.TunnelServiceRequestID, error)
}

// Poller coordinates polling the control plane and publishing work items to the
// dispatcher queue. It manages basic retry/backoff behavior and ensures it does
// not enqueue more work than the queue can hold.
type Poller struct {
	queue          Queue
	fetcher        Fetcher
	logger         *slog.Logger
	backoff        *backoff.Backoff
	queueFullDelay time.Duration
	pollTimeout    time.Duration
	metrics        *pollerMetrics
}

// NewPoller builds a Poller with sensible defaults for retry and queue
// backpressure handling. A nil logger defaults to slog.Default(). backoffMin /
// backoffMax override the default retry window when non-zero; zero values
// preserve defaults.
func NewPoller(queue Queue, fetcher Fetcher, logger *slog.Logger, meter metric.Meter, pollTimeout time.Duration, backoffMin, backoffMax time.Duration) (*Poller, error) {
	if queue == nil {
		return nil, fmt.Errorf("controlplane internal poller: queue cannot be nil")
	}
	if fetcher == nil {
		return nil, fmt.Errorf("controlplane internal poller: fetcher cannot be nil")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if pollTimeout <= 0 {
		pollTimeout = defaultPollerTimeout
	}

	minBackoff := defaultBackoffMin
	if backoffMin > 0 {
		minBackoff = backoffMin
	}
	maxBackoff := defaultBackoffMax
	if backoffMax > 0 {
		maxBackoff = backoffMax
	}

	p := &Poller{
		queue:   queue,
		fetcher: fetcher,
		logger:  logger,
		backoff: &backoff.Backoff{
			Min:    minBackoff,
			Max:    maxBackoff,
			Factor: 2,
			Jitter: true,
		},
		queueFullDelay: defaultQueueFullDelay,
		pollTimeout:    pollTimeout,
	}
	if m, err := newPollerMetrics(meter, queue); err != nil {
		return nil, err
	} else {
		p.metrics = m
		return p, nil
	}
}

// Run starts the polling loop and blocks until the context is cancelled.
func (p *Poller) Run(ctx context.Context) {
	p.logger.InfoContext(ctx, "poller started")
	defer func() {
		if err := ctx.Err(); err != nil {
			p.logger.InfoContext(ctx, "poller stopped", slog.String("reason", err.Error()))
			return
		}
		p.logger.InfoContext(ctx, "poller stopped")
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		available := p.availableSlots()
		if available <= 0 {
			if !p.waitForQueue(ctx) {
				return
			}
			continue
		}

		p.logger.DebugContext(ctx, "poll cycle started", slog.Int("limit", available))
		p.metrics.totalCyclesStarted.Add(ctx, 1)

		pollStart := time.Now()
		pollCtx, cancel := context.WithTimeout(ctx, p.pollTimeout)
		commands, tunnelServiceRequestID, err := p.fetcher.Poll(pollCtx, available)
		cancel()
		p.metrics.pollLatency.Record(ctx, time.Since(pollStart).Seconds(), metric.WithAttributes(attribute.Bool("error", err != nil)))
		if err != nil {
			p.metrics.pollErrors.Add(ctx, 1, metric.WithAttributes(attribute.String(attributeKeyErrorKind, pollErrorKind(err))))
			delay := p.backoff.Duration()
			attrs := []any{
				slog.String("error", err.Error()),
				slog.Int64("retry_in_ms", delay.Milliseconds()),
			}
			if tunnelServiceRequestID != "" {
				attrs = append(attrs, slog.String(tclog.FieldTunnelServiceRequestID, tunnelServiceRequestID.String()))
			}
			if errors.Is(err, context.DeadlineExceeded) {
				attrs = append(attrs, slog.Duration("poll_timeout", p.pollTimeout))
				p.logger.WarnContext(ctx, "poll timed out; backing off", attrs...)
			} else {
				p.logger.WarnContext(ctx, "poll failed; backing off", attrs...)
			}
			if !p.sleep(ctx, delay) {
				return
			}
			continue
		}

		p.backoff.Reset()
		p.metrics.lastSuccessUnixSeconds.Store(time.Now().Unix())

		pulled := len(commands)
		if pulled == 0 {
			p.logger.DebugContext(ctx, "poll cycle complete", slog.Int("commands_polled", 0), slog.Int("commands_enqueued", 0))
			continue
		}

		if pulled > available {
			p.logger.ErrorContext(ctx, "more commands polled than available slots. "+
				"tunnel-service is not respecting limit request and overflowing client", slog.Int("polled", pulled), slog.Int("available", available))
		}

		enqueued := 0
		for _, cmd := range commands {
			p.recordCommandAge(ctx, cmd)
			if !p.enqueue(ctx, cmd) {
				p.logger.ErrorContext(ctx, "Internal queue is full")
				//TODO(denyska): add sleep and try to enqueue later
				break
			}
			enqueued++
		}

		p.metrics.totalCommandsPolled.Add(ctx, int64(pulled))
		p.metrics.totalCommandsEnqueued.Add(ctx, int64(enqueued))
		if enqueued < pulled {
			dropped := pulled - enqueued
			p.metrics.queueDrops.Add(ctx, int64(dropped), metric.WithAttributes(attribute.String(attributeKeyDropReason, queueDropReason(ctx))))
		}

		p.logger.DebugContext(ctx, "poll cycle complete",
			slog.Int("commands_polled", pulled),
			slog.Int("commands_enqueued", enqueued),
		)
	}
}

func (p *Poller) enqueue(ctx context.Context, cmd PolledCommand) bool {
	return p.queue.Enqueue(ctx, cmd)
}

func (p *Poller) availableSlots() int {
	capacity := p.queue.Capacity()
	if capacity == 0 {
		// Treat unbuffered channels as having a single available slot to avoid zero limits.
		return 1
	}
	available := capacity - p.queue.Length()
	if available < 0 {
		return 0
	}
	return available
}

func (p *Poller) waitForQueue(ctx context.Context) bool {
	timer := time.NewTimer(p.queueFullDelay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (p *Poller) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = defaultBackoffMin
	}
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func pollErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errorKindTimeout
	case errors.Is(err, context.Canceled):
		return errorKindContextCanceled
	default:
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return errorKindTimeout
		}
		return errorKindOther
	}
}

func queueDropReason(ctx context.Context) string {
	if ctx == nil {
		return dropReasonQueueFull
	}

	switch ctx.Err() {
	case context.Canceled, context.DeadlineExceeded:
		return dropReasonContextClosed
	default:
		return dropReasonQueueFull
	}
}

func (p *Poller) recordCommandAge(ctx context.Context, cmd PolledCommand) {
	enqueuedAt := cmd.EnqueuedAt()
	polledAt := cmd.PolledAt()
	if enqueuedAt.IsZero() || polledAt.IsZero() {
		return
	}

	age := polledAt.Sub(enqueuedAt).Seconds()
	if age < 0 {
		return
	}

	p.metrics.commandAge.Record(ctx, age)
}
