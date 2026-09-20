package mq

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// metadataTimeout bounds the wait for a freshly created topic to become
// visible in the broker's metadata. Creation returns once the controller has
// accepted it, which is earlier than the point at which it can be described.
const (
	metadataTimeout  = 10 * time.Second
	metadataInterval = 200 * time.Millisecond
)

// EnsureTopics creates every topic in specs if it does not already exist, and
// verifies the geometry of the ones that do.
//
// It is idempotent, so every worker can call it at startup rather than anyone
// having to remember a setup step. An existing topic with too few partitions is
// an error rather than a warning: that is what a topic auto-created by a broker
// looks like, and silently consuming a one-partition topic would make the
// parallelism this design rests on disappear without a symptom.
func EnsureTopics(ctx context.Context, brokers []string, specs ...TopicSpec) error {
	if len(specs) == 0 {
		return nil
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return fmt.Errorf("mq: open admin client: %w", err)
	}
	defer cl.Close()
	adm := kadm.NewClient(cl)

	// Only the topics that already existed are verified below. A topic this
	// call just created has the geometry this call asked for, and asking the
	// broker about it immediately is a race: creation returns as soon as the
	// controller accepts it, before the metadata is servable, which surfaces as
	// UNKNOWN_TOPIC_OR_PARTITION.
	var existing []string

	for _, s := range specs {
		resps, err := adm.CreateTopics(ctx, s.Partitions, s.Replication, nil, s.Name)
		if err != nil {
			return fmt.Errorf("mq: create topic %s: %w", s.Name, err)
		}
		for _, r := range resps {
			switch {
			case r.Err == nil:
			// Created here, with the partitions and replication asked for.
			case errors.Is(r.Err, kerr.TopicAlreadyExists):
				// Someone got there first: a previous run, or another worker
				// starting at the same moment. Both are expected, and both mean
				// the geometry is worth checking.
				existing = append(existing, r.Topic)
			default:
				return fmt.Errorf("mq: create topic %s: %w", r.Topic, r.Err)
			}
		}
	}

	if len(existing) == 0 {
		return nil
	}
	details, err := describe(ctx, adm, existing)
	if err != nil {
		return err
	}
	for _, s := range specs {
		d, ok := details[s.Name]
		if !ok {
			continue // not one of the pre-existing topics
		}
		if d.Err != nil {
			return fmt.Errorf("mq: describe topic %s: %w", s.Name, d.Err)
		}
		if got := len(d.Partitions); got < int(s.Partitions) {
			return fmt.Errorf("mq: topic %s has %d partitions, want %d "+
				"(a topic the broker auto-created has one; delete it or add partitions)",
				s.Name, got, s.Partitions)
		}
	}
	return nil
}

// describe lists the given topics, waiting for any that the broker does not yet
// know about.
//
// The wait is not paranoia. A topic another process created moments ago — or
// one this process created on a previous call — is accepted by the controller
// before it is servable, so describing it immediately can report
// UNKNOWN_TOPIC_OR_PARTITION for a topic that definitely exists. Failing
// startup on that would make two workers starting together a coin flip.
//
// Once the wait is spent, whatever came back is returned and the caller reports
// the per-topic error, which says more than "timed out" would.
func describe(ctx context.Context, adm *kadm.Client, names []string) (kadm.TopicDetails, error) {
	deadline := time.Now().Add(metadataTimeout)

	for {
		details, err := adm.ListTopics(ctx, names...)
		if err != nil {
			return nil, fmt.Errorf("mq: list topics: %w", err)
		}
		if !anyUnknown(details, names) || time.Now().After(deadline) {
			return details, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("mq: list topics: %w", ctx.Err())
		case <-time.After(metadataInterval):
		}
	}
}

func anyUnknown(details kadm.TopicDetails, names []string) bool {
	for _, name := range names {
		d, ok := details[name]
		if !ok || errors.Is(d.Err, kerr.UnknownTopicOrPartition) {
			return true
		}
	}
	return false
}
