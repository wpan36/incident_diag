//go:build integration

// Integration tests for the messaging layer. They need the Kafka from
// deploy/docker-compose.yml:
//
//	make up && make test-integration
//
// Every test suffixes its topic and consumer group with a ULID, so repeated or
// parallel runs cannot see each other's messages and no test depends on a
// cleanly wiped broker.
package mq

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
)

func testConfig(t *testing.T) config.Kafka {
	t.Helper()
	brokers := os.Getenv("TEST_KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("TEST_KAFKA_BROKERS is not set; skipping the integration tests")
	}
	return config.Kafka{
		Brokers:        strings.Split(brokers, ","),
		ProduceTimeout: 10 * time.Second,
	}
}

// testTopic creates a topic nothing else will touch.
func testTopic(t *testing.T, cfg config.Kafka, partitions int32) string {
	t.Helper()
	name := "test." + strings.ToLower(id.New())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := EnsureTopics(ctx, cfg.Brokers, TopicSpec{Name: name, Partitions: partitions, Replication: 1}); err != nil {
		t.Fatalf("creating topic %s: %v", name, err)
	}
	return name
}

func testProducer(t *testing.T, cfg config.Kafka) *Client {
	t.Helper()
	p, err := NewProducer(cfg)
	if err != nil {
		t.Fatalf("opening the producer: %v", err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// consume runs a consumer in the background and hands back a channel of the
// document IDs its handler saw, plus a stop function.
func consume(t *testing.T, cfg config.Kafka, topic, group string, h Handler) (*Consumer, func()) {
	t.Helper()
	c, err := NewConsumer(cfg, group, []string{topic}, log.Discard())
	if err != nil {
		t.Fatalf("opening the consumer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.Run(ctx, h); err != nil {
			t.Errorf("consumer returned %v, want nil", err)
		}
	}()

	stop := func() {
		cancel()
		c.Close()
		wg.Wait()
	}
	t.Cleanup(stop)
	return c, stop
}

// waitFor polls cond until it holds or the deadline passes. Kafka is
// asynchronous enough that a bare sleep is either flaky or slow.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProduceAndConsume(t *testing.T) {
	cfg := testConfig(t)
	topic := testTopic(t, cfg, 1)
	producer := testProducer(t, cfg)

	var mu sync.Mutex
	var seen []DocumentMessage
	consume(t, cfg, topic, "test-"+strings.ToLower(id.New()), func(_ context.Context, rec Record) error {
		msg, err := Decode[DocumentMessage](rec.Value)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, msg)
		return nil
	})

	documentID := id.New()
	sent := NewDocumentMessage(documentID)
	if err := producer.Produce(context.Background(), topic, documentID, sent); err != nil {
		t.Fatalf("Produce: %v", err)
	}

	waitFor(t, "the message to arrive", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0
	})

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("handler saw %d messages, want 1", len(seen))
	}
	if seen[0].DocumentID != documentID || seen[0].MessageID != sent.MessageID {
		t.Errorf("received %+v, want %+v", seen[0], sent)
	}
}

func TestProducedRecordCarriesItsHeadersAndKey(t *testing.T) {
	cfg := testConfig(t)
	topic := testTopic(t, cfg, 1)
	producer := testProducer(t, cfg)

	var mu sync.Mutex
	var got Record
	consume(t, cfg, topic, "test-"+strings.ToLower(id.New()), func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		got = rec
		return nil
	})

	documentID := id.New()
	if err := producer.Produce(context.Background(), topic, documentID, NewDocumentMessage(documentID)); err != nil {
		t.Fatalf("Produce: %v", err)
	}
	waitFor(t, "the message to arrive", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got.Key != ""
	})

	mu.Lock()
	defer mu.Unlock()
	if got.Key != documentID {
		t.Errorf("key = %q, want the document id %q", got.Key, documentID)
	}
	if got.Headers[HeaderContentType] != contentTypeJSON {
		t.Errorf("content-type header = %q, want %q", got.Headers[HeaderContentType], contentTypeJSON)
	}
}

// TestEnsureTopicsIsIdempotent is what lets every worker call it at startup
// instead of anyone having to remember a setup step.
func TestEnsureTopicsIsIdempotent(t *testing.T) {
	cfg := testConfig(t)
	name := "test." + strings.ToLower(id.New())
	spec := TopicSpec{Name: name, Partitions: DefaultPartitions, Replication: 1}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 2; i++ {
		if err := EnsureTopics(ctx, cfg.Brokers, spec); err != nil {
			t.Fatalf("EnsureTopics call %d: %v", i+1, err)
		}
	}
}

// TestEnsureTopicsRejectsATopicWithTooFewPartitions is the case that makes the
// check worth having: a topic the broker auto-created has one partition, and
// consuming it would silently halve the parallelism this design rests on.
func TestEnsureTopicsRejectsATopicWithTooFewPartitions(t *testing.T) {
	cfg := testConfig(t)
	name := testTopic(t, cfg, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := EnsureTopics(ctx, cfg.Brokers, TopicSpec{Name: name, Partitions: 3, Replication: 1})
	if err == nil {
		t.Fatal("EnsureTopics accepted a topic with one partition where three were asked for")
	}
	if !strings.Contains(err.Error(), "partitions") {
		t.Errorf("error = %v, want it to explain the partition count", err)
	}
}

// TestTwoConsumersShareThePartitions is the reason the topics have three
// partitions at all: group assignment and parallel processing have to be real
// behaviour rather than a code path nothing takes.
func TestTwoConsumersShareThePartitions(t *testing.T) {
	cfg := testConfig(t)
	topic := testTopic(t, cfg, 3)
	producer := testProducer(t, cfg)
	group := "test-" + strings.ToLower(id.New())

	var mu sync.Mutex
	partitionsPerConsumer := map[string]map[int32]bool{"a": {}, "b": {}}
	seen := map[string]bool{}

	handler := func(name string) Handler {
		return func(_ context.Context, rec Record) error {
			mu.Lock()
			defer mu.Unlock()
			partitionsPerConsumer[name][rec.Partition] = true
			seen[rec.Key] = true
			return nil
		}
	}
	consume(t, cfg, topic, group, handler("a"))
	consume(t, cfg, topic, group, handler("b"))

	// Enough distinct keys that all three partitions get something.
	const n = 30
	ids := make([]string, n)
	for i := range ids {
		ids[i] = id.New()
		if err := producer.Produce(context.Background(), topic, ids[i], NewDocumentMessage(ids[i])); err != nil {
			t.Fatalf("Produce: %v", err)
		}
	}

	waitFor(t, "every message to be handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == n
	})

	mu.Lock()
	defer mu.Unlock()
	if len(partitionsPerConsumer["a"]) == 0 || len(partitionsPerConsumer["b"]) == 0 {
		t.Errorf("partitions were not shared: a=%v b=%v",
			partitionsPerConsumer["a"], partitionsPerConsumer["b"])
	}
}

// TestRestartingAConsumerReplaysOnlyTheRecordInFlight is what per-record
// commit buys. With a per-batch commit, everything already handled in the batch
// would come back too — and because ClaimDocument accepts FAILED, those replays
// would re-run documents that had just failed, burning attempts and skipping
// the reconciler's backoff.
func TestRestartingAConsumerReplaysOnlyTheRecordInFlight(t *testing.T) {
	cfg := testConfig(t)
	topic := testTopic(t, cfg, 1)
	producer := testProducer(t, cfg)
	group := "test-" + strings.ToLower(id.New())

	const n = 6
	ids := make([]string, n)
	for i := range ids {
		ids[i] = id.New()
		if err := producer.Produce(context.Background(), topic, ids[i], NewDocumentMessage(ids[i])); err != nil {
			t.Fatalf("Produce: %v", err)
		}
	}

	// The first consumer handles three records and then stops inside the
	// fourth. Blocking until the context is cancelled and then returning is
	// what a crash looks like from the outside: the work for that record is not
	// recorded, and the commit that follows the handler fails because the
	// context is gone. The fourth record is therefore still uncommitted.
	//
	// The two consumers run one after the other, not at once. With
	// BlockRebalanceOnPoll a member sitting inside a handler holds up the whole
	// group, so a second member could not join until the first let go — which
	// is the property that keeps two workers out of the same message, and it
	// means this test has to model a restart rather than an overlap.
	var mu sync.Mutex
	var firstRun []string
	blocked := make(chan struct{})

	consumerCtx, cancelFirst := context.WithCancel(context.Background())
	first, err := NewConsumer(cfg, group, []string{topic}, log.Discard())
	if err != nil {
		t.Fatalf("opening the first consumer: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- first.Run(consumerCtx, func(ctx context.Context, rec Record) error {
			mu.Lock()
			handled := len(firstRun)
			mu.Unlock()
			if handled == 3 {
				close(blocked)
				<-ctx.Done()
				return nil
			}
			mu.Lock()
			firstRun = append(firstRun, rec.Key)
			mu.Unlock()
			return nil
		})
	}()

	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("the first consumer never reached the fourth record")
	}

	cancelFirst()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("the first consumer returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the first consumer did not return after cancellation")
	}
	first.Close()

	var secondRun []string
	consume(t, cfg, topic, group, func(_ context.Context, rec Record) error {
		mu.Lock()
		defer mu.Unlock()
		secondRun = append(secondRun, rec.Key)
		return nil
	})

	waitFor(t, "the second consumer to catch up", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(secondRun) >= n-3
	})

	mu.Lock()
	defer mu.Unlock()
	if len(firstRun) != 3 {
		t.Fatalf("the first consumer handled %d records, want 3", len(firstRun))
	}
	// Nothing the first consumer finished comes back: those offsets were
	// committed one at a time as each handler returned.
	done := map[string]bool{}
	for _, k := range firstRun {
		done[k] = true
	}
	for _, k := range secondRun {
		if done[k] {
			t.Errorf("record %s was replayed although its handler had already returned", k)
		}
	}
	// And nothing is lost: the record in flight plus everything after it.
	if len(secondRun) < n-3 {
		t.Errorf("the second consumer saw %d records, want at least %d", len(secondRun), n-3)
	}
}
