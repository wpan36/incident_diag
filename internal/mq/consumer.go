package mq

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/wpan36/incident_diag/internal/config"
)

// Record is one consumed message, in terms this project uses rather than
// franz-go's.
//
// It carries the raw value rather than a decoded message: this package does not
// know which type a topic carries, and a handler written against one topic
// does. The handler calls Decode.
type Record struct {
	Topic     string
	Key       string
	Value     []byte
	Headers   map[string]string
	Partition int32
	Offset    int64
}

// Handler processes one record.
//
// Returning an error does not stop the consumer and does not withhold the
// commit; see Consumer.handle for why. It means "this record was not dealt
// with", which is worth a log line and nothing else, because the row it names
// is still in a state the reconciler will pick up.
type Handler func(ctx context.Context, rec Record) error

// Consumer is one consumer group member.
type Consumer struct {
	cl     *kgo.Client
	group  string
	logger *slog.Logger
}

// NewConsumer joins group and subscribes to topics. The returned Consumer must
// be closed.
func NewConsumer(cfg config.Kafka, group string, topics []string, logger *slog.Logger) (*Consumer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(topics...),
		// Offsets are committed by hand, one record at a time, in handle.
		kgo.DisableAutoCommit(),
		// A group with no committed offset starts at the beginning of the
		// topic. This is franz-go's default and the opposite of the Java
		// client's; it is set explicitly because it is load-bearing — starting
		// at the end would silently drop everything produced before the worker
		// first came up.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// A rebalance cannot happen while a poll's records are being processed.
		// Without this, a partition could move to another member mid-batch and
		// two workers would be inside the same message at once — which the
		// conditional claim would survive, but only by doing the work twice.
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return nil, fmt.Errorf("mq: open consumer: %w", err)
	}
	return &Consumer{cl: cl, group: group, logger: logger}, nil
}

// Run polls and dispatches until ctx is cancelled or the client is closed.
//
// Records are handled one at a time in poll order. Parallelism comes from
// partitions and from running more worker instances, not from concurrency
// inside one worker, which keeps the failure model small enough to reason
// about: at any moment this process is inside exactly one message.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	c.logger.InfoContext(ctx, "consuming", "group", c.group)

	for {
		fetches := c.cl.PollFetches(ctx)
		stopping := fetches.IsClientClosed() || ctx.Err() != nil

		if !stopping {
			// Fetch errors are not record errors. They are transient broker or
			// network problems that franz-go retries on its own, so they are
			// worth seeing and not worth acting on.
			fetches.EachError(func(topic string, partition int32, err error) {
				c.logger.ErrorContext(ctx, "kafka fetch failed",
					"topic", topic, "partition", partition, "error", err)
			})

			fetches.EachRecord(func(r *kgo.Record) {
				if ctx.Err() != nil {
					return
				}
				c.handle(ctx, h, r)
			})
		}

		// Exactly one AllowRebalance per poll, on every path out of the loop
		// included. BlockRebalanceOnPoll makes a pending rebalance wait for
		// this call, so returning without it deadlocks the group manager — and
		// therefore Close, which is trying to leave the group.
		c.cl.AllowRebalance()

		if stopping {
			return nil
		}
	}
}

// handle runs one record's handler and then commits it, whatever the handler
// concluded.
//
// Committing on a handler error looks wrong and is not. A Kafka offset is a
// position, not a per-record acknowledgement: withholding this record's commit
// while committing the next one would commit this one too. The only way to make
// a withheld commit mean anything is to stop consuming, which converts one bad
// message into a stalled partition. So the outcome is recorded in MySQL — where
// it is either terminal or reconcilable — and the offset moves on.
func (c *Consumer) handle(ctx context.Context, h Handler, r *kgo.Record) {
	if err := h(ctx, toRecord(r)); err != nil {
		c.logger.ErrorContext(ctx, "handler failed",
			"topic", r.Topic, "partition", r.Partition, "offset", r.Offset,
			"key", string(r.Key), "error", err)
	}

	// Committed per record rather than per batch, so a crash replays only the
	// record that was in flight. That costs one round trip per message, which
	// at this project's volumes is not the constraint.
	if err := c.cl.CommitRecords(ctx, r); err != nil {
		c.logger.ErrorContext(ctx, "commit failed",
			"topic", r.Topic, "partition", r.Partition, "offset", r.Offset, "error", err)
	}
}

// Close leaves the group and shuts the client down.
func (c *Consumer) Close() error {
	c.cl.Close()
	return nil
}

func toRecord(r *kgo.Record) Record {
	var h map[string]string
	if len(r.Headers) > 0 {
		h = make(map[string]string, len(r.Headers))
		for _, rh := range r.Headers {
			h[rh.Key] = string(rh.Value)
		}
	}
	return Record{
		Topic:     r.Topic,
		Key:       string(r.Key),
		Value:     r.Value,
		Headers:   h,
		Partition: r.Partition,
		Offset:    r.Offset,
	}
}
