package mq

import (
	"context"
	"fmt"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/wpan36/incident_diag/internal/config"
)

// Producer publishes one message and waits for the broker to acknowledge it.
//
// It is an interface because the reconciler's unit tests need to observe what
// would have been produced without a broker in the room. The API and the
// reconciler are its only callers.
type Producer interface {
	Produce(ctx context.Context, topic, key string, msg any) error
}

// Client is the franz-go backed Producer.
type Client struct {
	cl      *kgo.Client
	timeout time.Duration
}

// NewProducer opens a producer. The returned Client must be closed.
//
// Automatic topic creation is not enabled — it is off by default and stays off.
// A broker that invents a topic on first produce turns a misspelled name into a
// silent second queue nothing consumes, and it would create it with one
// partition rather than three.
func NewProducer(cfg config.Kafka) (*Client, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.Brokers...),
		// acks=all: an acknowledgement means every in-sync replica has the
		// record. On a single broker that is one replica, but the setting is
		// what makes the acknowledgement mean anything at all.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		// The idempotent producer is on by default and is deliberately turned
		// off, which is the opposite of the usual advice and was measured
		// rather than reasoned about.
		//
		// franz-go will not fail a record that has already been sent while
		// producing idempotently: it cannot know whether the broker processed
		// the request, and dropping the record would corrupt the sequence
		// numbers of everything after it. So neither the context deadline nor
		// RecordDeliveryTimeout applies. With the broker stopped mid-request,
		// an upload handler blocked for as long as the broker was down — 223
		// seconds in the smoke test — while the client had long since been cut
		// off by the HTTP write timeout. That is a worse failure than the one
		// idempotency prevents.
		//
		// And it prevents a failure this system does not have. A retried
		// produce can write the same record twice; a duplicate message costs
		// exactly one refused claim, because ClaimDocument is a conditional
		// UPDATE (ADR 0003). Delivery here is at-least-once by design, so
		// paying for exactly-once delivery with an unbounded handler buys
		// nothing.
		kgo.DisableIdempotentWrite(),
		// Now that records can be failed, this is what bounds one: a record
		// that cannot be delivered within the timeout is returned as an error
		// instead of waiting for a broker that may never come back.
		kgo.RecordDeliveryTimeout(cfg.ProduceTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("mq: open producer: %w", err)
	}
	return &Client{cl: cl, timeout: cfg.ProduceTimeout}, nil
}

// Produce sends msg to topic under key and waits for the acknowledgement.
//
// The key is the entity ID, which is what puts every message about one document
// on one partition: a redelivery and a reconciler's re-enqueue then cannot be
// processed by two consumers at the same time.
//
// An unacknowledged produce is an error the caller sees. What the caller does
// with it differs — the upload handler logs it and still answers 201, the
// reconciler logs it and tries again next sweep — but neither pretends it
// succeeded.
func (c *Client) Produce(ctx context.Context, topic, key string, msg any) error {
	value, err := Encode(msg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	rec := &kgo.Record{
		Topic:   topic,
		Key:     []byte(key),
		Value:   value,
		Headers: headers(ctx),
	}
	if err := c.cl.ProduceSync(ctx, rec).FirstErr(); err != nil {
		return fmt.Errorf("mq: produce to %s: %w", topic, err)
	}
	return nil
}

// Close flushes and shuts the client down.
func (c *Client) Close() error {
	c.cl.Close()
	return nil
}

// headers builds the record headers.
//
// Transport concerns travel here rather than in the payload. The W3C
// traceparent that joins a producer's span to its consumer's belongs in this
// list, and M29 adds it by filling in traceHeaders — writing a header today
// would mean inventing a trace ID that no span ever belonged to, which is worse
// than not having one.
func headers(ctx context.Context) []kgo.RecordHeader {
	h := []kgo.RecordHeader{{Key: HeaderContentType, Value: []byte(contentTypeJSON)}}
	return append(h, traceHeaders(ctx)...)
}

// traceHeaders returns the trace context headers for ctx. It is empty until
// M29 installs an OpenTelemetry propagator.
func traceHeaders(_ context.Context) []kgo.RecordHeader { return nil }
