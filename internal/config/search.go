package config

import (
	"fmt"
	"time"
)

// DefaultIndexAlias is the alias every read and write goes through.
const DefaultIndexAlias = "chunks"

// Search is how to reach Elasticsearch.
//
// The alias is configurable but the concrete index name behind it is not: the
// alias is what makes an embedding-model change survivable, since a second
// index can be built alongside the first and the alias switched atomically.
// EnsureIndex accepts whatever single index the alias resolves to, so a switch
// to chunks_v2 does not have to be accompanied by a configuration change that
// someone could forget.
type Search struct {
	URL        string
	IndexAlias string

	// Timeout bounds one kNN query, and only that: the bulk index and the
	// delete-by-document belong to ingestion and are already bounded by
	// INGEST_DOCUMENT_TIMEOUT, which a ten-second cap would break.
	//
	// It exists because internal/search is the one external client with no
	// deadline of its own — embed has EMBED_TIMEOUT, llm has LLM_TIMEOUT,
	// mcpclient takes one at Connect — and the agent's run context has no
	// deadline on purpose. Without it a hung Elasticsearch bounds a run at
	// nothing at all, and LoadAgentWorker's worst case would not be honest.
	Timeout time.Duration
}

// LoadSearch reads the Elasticsearch configuration from the environment,
// reporting every problem it finds at once.
func LoadSearch() (Search, error) {
	var e env

	s := Search{
		URL:        e.requiredString("ELASTICSEARCH_URL"),
		IndexAlias: e.optionalString("ES_INDEX_ALIAS", DefaultIndexAlias),
		Timeout:    e.optionalDuration("SEARCH_TIMEOUT", 10*time.Second),
	}

	if s.Timeout <= 0 {
		e.fail("SEARCH_TIMEOUT must be greater than zero")
	}

	if err := e.err(); err != nil {
		return Search{}, err
	}
	return s, nil
}

// String renders the configuration for startup logging.
func (s Search) String() string {
	return fmt.Sprintf("url=%s index_alias=%s timeout=%s", s.URL, s.IndexAlias, s.Timeout)
}
