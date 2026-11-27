package serverownerfga

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	defaultQueueCapacity = 256
	defaultFlushBatch    = 50
)

// Projector is the minimal interface used by EventDispatcher to reconcile
// committed server ownership updates with Administration.
type Projector interface {
	Project(ctx context.Context, servers []ServerOwnerInput) (ProjectionResponse, error)
}

// EventDispatcher drains accepted server-registry commits asynchronously and
// reconciles their `server#owner` tuples in Administration without blocking
// the request path. Failures are always logged and retried by the periodic
// full-aggregate resync loop.
type EventDispatcher struct {
	projector Projector
	logger    *slog.Logger
	queue     chan ServerOwnerInput
	onError   func(error)
}

type EventDispatcherConfig struct {
	Projector     Projector
	Logger        *slog.Logger
	QueueCapacity int
	OnError       func(error)
}

func NewEventDispatcher(cfg EventDispatcherConfig) (*EventDispatcher, error) {
	if cfg.Projector == nil {
		return nil, errors.New("serverownerfga: projector required")
	}
	capacity := cfg.QueueCapacity
	if capacity <= 0 {
		capacity = defaultQueueCapacity
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &EventDispatcher{
		projector: cfg.Projector,
		logger:    log,
		queue:     make(chan ServerOwnerInput, capacity),
		onError:   cfg.OnError,
	}, nil
}

// Enqueue schedules one accepted server-registry state for immediate
// reconciliation. It never blocks the caller; a full queue logs a warning so
// operators see backpressure while the periodic resync loop guarantees
// convergence.
func (d *EventDispatcher) Enqueue(input ServerOwnerInput) {
	if d == nil {
		return
	}
	if err := ValidateInput(input); err != nil {
		d.reportError(err, input)
		return
	}
	select {
	case d.queue <- input:
	default:
		d.logger.Warn("server_owner_fga_event_queue_full",
			"tenant_id", input.TenantID,
			"server_id", input.ServerID,
			"aggregate_revision", input.AggregateRevision)
	}
}

// Run drains queued updates until ctx is canceled.
func (d *EventDispatcher) Run(ctx context.Context) {
	if d == nil {
		return
	}
	batch := make([]ServerOwnerInput, 0, defaultFlushBatch)
	for {
		select {
		case <-ctx.Done():
			return
		case first := <-d.queue:
			batch = append(batch[:0], first)
			for len(batch) < defaultFlushBatch {
				select {
				case next := <-d.queue:
					batch = append(batch, next)
				default:
					goto flush
				}
			}
		flush:
			reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, err := d.projector.Project(reqCtx, batch)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				d.reportBatchError(err, batch)
			}
		}
	}
}

func (d *EventDispatcher) reportError(err error, input ServerOwnerInput) {
	if d.onError != nil {
		d.onError(err)
	}
	d.logger.Warn("server_owner_fga_event_invalid",
		"tenant_id", input.TenantID,
		"server_id", input.ServerID,
		"aggregate_revision", input.AggregateRevision,
		"error", err)
}

func (d *EventDispatcher) reportBatchError(err error, batch []ServerOwnerInput) {
	if d.onError != nil {
		d.onError(err)
	}
	retryable := false
	var projErr *ProjectionError
	if errors.As(err, &projErr) {
		retryable = projErr.IsRetryable()
	}
	d.logger.Warn("server_owner_fga_event_projection_failed",
		"count", len(batch),
		"retryable", retryable,
		"error", err)
}
