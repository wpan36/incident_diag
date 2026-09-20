package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
)

// Runner sweeps on an interval for as long as its context lives.
//
// It produces messages and changes nothing. That is the property the whole
// design rests on: there is exactly one place where a document's state moves —
// the claim — so the sweep cannot race it. The worst a redundant message can do
// is be refused.
type Runner struct {
	store    *store.Store
	producer mq.Producer
	policy   Policy
	logger   *slog.Logger

	// now is time.Now except in tests, which need to place rows in the past
	// without sleeping.
	now func() time.Time
}

// NewRunner builds a runner. The caller owns its lifetime through the context
// it passes to Run.
func NewRunner(st *store.Store, p mq.Producer, policy Policy, logger *slog.Logger) *Runner {
	return &Runner{store: st, producer: p, policy: policy, logger: logger, now: time.Now}
}

// Stats is what one sweep did.
type Stats struct {
	// Candidates is how many rows the query returned, before the policy
	// filtered them.
	Candidates int

	// Enqueued counts the messages produced, by category.
	Enqueued map[store.ReconcileCategory]int

	// Failed counts candidates whose produce failed. They stay in the same
	// state, so the next sweep tries again.
	Failed int
}

// Total returns how many messages the sweep produced.
func (s Stats) Total() int {
	n := 0
	for _, v := range s.Enqueued {
		n += v
	}
	return n
}

// Run sweeps immediately, then every Interval, until ctx is cancelled.
//
// The first sweep is immediate because the most likely reason this process is
// starting is that the last one died, possibly holding claims. Waiting a full
// interval to notice would add thirty seconds to every restart.
//
// A sweep that fails is logged and not retried early: the next tick is the
// retry, and a database that is down will still be down in thirty seconds.
func (r *Runner) Run(ctx context.Context) error {
	r.logger.InfoContext(ctx, "reconciler started",
		"interval", r.policy.Interval, "batch", r.policy.Batch,
		"pending_after", r.policy.PendingAfter, "lease", r.policy.Lease,
		"max_attempts", r.policy.MaxAttempts)

	ticker := time.NewTicker(r.policy.Interval)
	defer ticker.Stop()

	for {
		r.sweepAndLog(ctx)

		select {
		case <-ctx.Done():
			r.logger.InfoContext(ctx, "reconciler stopped")
			return nil
		case <-ticker.C:
		}
	}
}

// sweepAndLog runs one sweep under its own deadline and reports what happened.
func (r *Runner) sweepAndLog(ctx context.Context) {
	// A sweep gets one interval. Overrunning that would stack sweeps on top of
	// each other, and a sweep that slow is a symptom worth seeing rather than
	// something to wait out.
	ctx, cancel := context.WithTimeout(ctx, r.policy.Interval)
	defer cancel()

	stats, err := r.Sweep(ctx)
	if err != nil {
		// Cancellation during shutdown is not a failure worth alarming about.
		if ctx.Err() != nil {
			return
		}
		r.logger.ErrorContext(ctx, "sweep failed", "error", err)
		return
	}
	if stats.Total() == 0 && stats.Failed == 0 {
		// The common case, and it must not fill the log.
		r.logger.DebugContext(ctx, "sweep found nothing", "candidates", stats.Candidates)
		return
	}
	r.logger.InfoContext(ctx, "sweep re-enqueued documents",
		"candidates", stats.Candidates,
		"never_enqueued", stats.Enqueued[store.ReconcileNeverEnqueued],
		"abandoned", stats.Enqueued[store.ReconcileAbandoned],
		"retryable_failure", stats.Enqueued[store.ReconcileRetryable],
		"produce_failed", stats.Failed)
}

// Sweep looks once for stuck documents and produces a message for each.
func (r *Runner) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{Enqueued: map[store.ReconcileCategory]int{}}

	candidates, err := r.store.ListDocumentsToReconcile(ctx, r.policy.storePolicy(), r.policy.Batch)
	if err != nil {
		return stats, err
	}
	stats.Candidates = len(candidates)

	at := r.now()
	for _, c := range candidates {
		category, due := r.policy.Due(c, at)
		if !due {
			continue
		}
		if err := r.producer.Produce(ctx, mq.TopicDocumentsIngest, c.ID, mq.NewDocumentMessage(c.ID)); err != nil {
			// Nothing was written, so the row still matches next time. Keep
			// going: one unreachable partition should not stop the rest.
			stats.Failed++
			r.logger.ErrorContext(ctx, "re-enqueue failed",
				"document_id", c.ID, "category", category, "error", err)
			continue
		}
		stats.Enqueued[category]++
		r.logger.DebugContext(ctx, "re-enqueued document",
			"document_id", c.ID, "category", category, "attempts", c.Attempts)
	}
	return stats, nil
}
