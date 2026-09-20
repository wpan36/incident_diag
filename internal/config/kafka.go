package config

import (
	"fmt"
	"strings"
	"time"
)

// Kafka is the broker configuration shared by producers and consumers.
//
// Topic and consumer group names are deliberately absent: they are constants in
// internal/mq, because they are a contract between two processes and a
// configurable contract is one two services can disagree about.
type Kafka struct {
	// Brokers is the seed broker list. franz-go discovers the rest of the
	// cluster from it, so it does not have to be complete.
	Brokers []string

	// ProduceTimeout bounds one produce, acknowledgement included.
	//
	// It is spent inside the HTTP request on the upload path: with the broker
	// down, an upload blocks before answering 201.
	//
	// It is not the upper bound on that block. franz-go's dial backoff and
	// metadata refresh sit around the produce rather than inside it, so this
	// value at 10s measured 13.5-16.7s against a stopped broker. Keep it at or
	// below half of HTTP_WRITE_TIMEOUT, not merely below it.
	ProduceTimeout time.Duration
}

// LoadKafka reads the broker configuration from the environment, reporting
// every problem it finds at once.
func LoadKafka() (Kafka, error) {
	var e env

	k := Kafka{
		Brokers:        splitList(e.requiredString("KAFKA_BROKERS")),
		ProduceTimeout: e.optionalDuration("KAFKA_PRODUCE_TIMEOUT", 10*time.Second),
	}

	// requiredString has already complained if the variable was missing; this
	// catches a value that is present but is only separators, such as ",,".
	if len(k.Brokers) == 0 {
		if _, set := lookup("KAFKA_BROKERS"); set {
			e.fail("KAFKA_BROKERS must list at least one host:port")
		}
	}
	if k.ProduceTimeout <= 0 {
		e.fail("KAFKA_PRODUCE_TIMEOUT must be greater than zero")
	}

	if err := e.err(); err != nil {
		return Kafka{}, err
	}
	return k, nil
}

// splitList parses a comma-separated value, dropping empty entries so that a
// trailing comma is not read as a broker called "".
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// String renders the configuration for startup logging.
func (k Kafka) String() string {
	return fmt.Sprintf("brokers=%s produce_timeout=%s", strings.Join(k.Brokers, ","), k.ProduceTimeout)
}
