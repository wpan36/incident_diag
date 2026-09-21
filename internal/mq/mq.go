// Package mq is a thin wrapper over franz-go: message types, a producer, a
// consumer group, and idempotent topic creation.
//
// It is deliberately not an abstraction over messaging. There is one broker
// technology in this project and there will not be a second, so the value here
// is in having one place that knows the envelope, the headers and the commit
// discipline — not in being able to swap Kafka out.
//
// The governing principle of everything below is stated in
// docs/plans/async-messaging-and-idempotency.md: Kafka offsets are not the
// record of progress, MySQL is. Offsets exist only to avoid redundant work, so
// every record is committed once its handler returns, whatever the handler
// concluded.
package mq

// Topic names. These are constants rather than configuration: they are the
// contract between a producer and a consumer in a different process, and making
// them settable is how two services end up disagreeing about where work goes.
const (
	TopicDocumentsIngest = "documents.ingest.v1"
	TopicAgentRuns       = "agent.runs.v1"
)

// Consumer group names, constants for the same reason as the topic names.
const (
	GroupIngestionWorker = "ingestion-worker"
	GroupAgentWorker     = "agent-worker"
)

// Topic geometry.
//
// Three partitions on a single broker is more than throughput needs. It is
// there so that consumer group assignment, rebalancing and parallel processing
// are real behaviour an integration test can exercise, rather than code paths
// nothing ever takes. Replication is 1 because ADR 0005 rules out running more
// than one broker.
const (
	DefaultPartitions  = 3
	DefaultReplication = 1
)

// Record headers. Transport concerns travel in headers rather than in the
// payload, so the message body stays the domain's business.
const (
	HeaderContentType = "content-type"

	// HeaderTraceParent carries the W3C trace context. M29 fills it in; see
	// traceHeaders.
	HeaderTraceParent = "traceparent"

	contentTypeJSON = "application/json"
)

// TopicSpec describes a topic EnsureTopics should create.
type TopicSpec struct {
	Name        string
	Partitions  int32
	Replication int16
}

// DefaultTopics are the topics this system uses.
//
// Both workers create both topics at startup. Creation is idempotent, so
// either process can be the first one up and neither has to wait for the
// other.
func DefaultTopics() []TopicSpec {
	return []TopicSpec{
		{Name: TopicDocumentsIngest, Partitions: DefaultPartitions, Replication: DefaultReplication},
		{Name: TopicAgentRuns, Partitions: DefaultPartitions, Replication: DefaultReplication},
	}
}
