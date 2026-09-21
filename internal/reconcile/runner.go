package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
)

// target is what differs between sweeping documents and sweeping runs.
//
// There is one Runner, not two: the ticker, the per-sweep deadline, Stats and
// the logging are the same job whichever table is being swept, and the five
// fields below are the whole of the difference.
type target struct {
	// noun names the entity in a log line ("document", "run").
	noun string

	// topic is where a re-enqueued message goes.
	topic string

	// list returns the rows the sweep should look at.
	list func(context.Context, store.ReconcilePolicy, int) ([]store.ReconcileCandidate, error)

	// due is the policy's decision for this target. It is per target because
	// runs use RUNNING where documents use PROCESSING.
	due func(Policy, store.ReconcileCandidate, time.Time) (store.ReconcileCategory, bool)

	// message builds what is produced for a candidate.
	message func(entityID string) any

	// categories are the ones this target can produce, and the only ones its
	// log line names: a zero retryable_failure on every run sweep would be
	// noise about a category that cannot happen.
	categories []store.ReconcileCategory
}

// Runner sweeps on an interval for as long as its context lives.
//
// It produces messages and changes nothing. That is the property the whole
// design rests on: there is exactly one place where a row's state moves — the
// claim — so the sweep cannot race it. The worst a redundant message can do is
// be refused.
//
// One Runner serves both tables; target says which.
type Runner struct {
	target   target
	producer mq.Producer
	policy   Policy
	logger   *slog.Logger

	// now is time.Now except in tests, which need to place rows in the past
	// without sleeping.
	now func() time.Time
}

// NewDocumentRunner builds the sweep over documents. The caller owns its
// lifetime through the context it passes to Run.
func NewDocumentRunner(st *store.Store, p mq.Producer, policy Policy, logger *slog.Logger) *Runner {
	return newRunner(target{
		noun:    "document",
		topic:   mq.TopicDocumentsIngest,
		list:    st.ListDocumentsToReconcile,
		due:     Policy.DueDocument,
		message: func(id string) any { return mq.NewDocumentMessage(id) },
		categories: []store.ReconcileCategory{
			store.ReconcileNeverEnqueued, store.ReconcileAbandoned, store.ReconcileRetryable,
		},
	}, p, policy, logger)
}

// NewRunRunner builds the sweep over agent runs.
//
// It takes the shared config.Reconcile plus RUN_LEASE and RUN_MAX_ATTEMPTS
// rather than a Policy, because those two are the only values a run's policy
// does not share with a document's and PolicyFrom cannot express that.
func NewRunRunner(st *store.Store, p mq.Producer, cfg config.Reconcile,
	lease time.Duration, maxAttempts int, logger *slog.Logger) *Runner {

	return newRunner(target{
		noun:    "run",
		topic:   mq.TopicAgentRuns,
		list:    st.ListRunsToReconcile,
		due:     Policy.DueRun,
		message: func(id string) any { return mq.NewRunMessage(id) },
		categories: []store.ReconcileCategory{
			store.ReconcileNeverEnqueued, store.ReconcileAbandoned,
		},
	}, p, RunPolicy(cfg, lease, maxAttempts), logger)
}

func newRunner(t target, p mq.Producer, policy Policy, logger *slog.Logger) *Runner {
	return &Runner{target: t, producer: p, policy: policy, logger: logger, now: time.Now}
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
		"target", r.target.noun,
		"interval", r.policy.Interval, "batch", r.policy.Batch,
		"pending_after", r.policy.PendingAfter, "lease", r.policy.Lease,
		"max_attempts", r.policy.MaxAttempts)

	ticker := time.NewTicker(r.policy.Interval)
	defer ticker.Stop()

	for {
		r.sweepAndLog(ctx)

		select {
		case <-ctx.Done():
			r.logger.InfoContext(ctx, "reconciler stopped", "target", r.target.noun)
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
		r.logger.DebugContext(ctx, "sweep found nothing",
			"target", r.target.noun, "candidates", stats.Candidates)
		return
	}

	attrs := []any{"target", r.target.noun, "candidates", stats.Candidates}
	for _, c := range r.target.categories {
		attrs = append(attrs, string(c), stats.Enqueued[c])
	}
	attrs = append(attrs, "produce_failed", stats.Failed)
	r.logger.InfoContext(ctx, "sweep re-enqueued rows", attrs...)
}

// Sweep looks once for stuck rows and produces a message for each.
func (r *Runner) Sweep(ctx context.Context) (Stats, error) {
	stats := Stats{Enqueued: map[store.ReconcileCategory]int{}}

	candidates, err := r.target.list(ctx, r.policy.storePolicy(), r.policy.Batch)
	if err != nil {
		return stats, err
	}
	stats.Candidates = len(candidates)

	idKey := r.target.noun + "_id"
	at := r.now()
	for _, c := range candidates {
		category, due := r.target.due(r.policy, c, at)
		if !due {
			continue
		}
		if err := r.producer.Produce(ctx, r.target.topic, c.ID, r.target.message(c.ID)); err != nil {
			// Nothing was written, so the row still matches next time. Keep
			// going: one unreachable partition should not stop the rest.
			stats.Failed++
			r.logger.ErrorContext(ctx, "re-enqueue failed",
				idKey, c.ID, "category", category, "error", err)
			continue
		}
		stats.Enqueued[category]++
		r.logger.DebugContext(ctx, "re-enqueued row",
			idKey, c.ID, "category", category, "attempts", c.Attempts)
	}
	return stats, nil
}
