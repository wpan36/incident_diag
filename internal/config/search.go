package config

import "fmt"

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
}

// LoadSearch reads the Elasticsearch configuration from the environment,
// reporting every problem it finds at once.
func LoadSearch() (Search, error) {
	var e env

	s := Search{
		URL:        e.requiredString("ELASTICSEARCH_URL"),
		IndexAlias: e.optionalString("ES_INDEX_ALIAS", DefaultIndexAlias),
	}

	if err := e.err(); err != nil {
		return Search{}, err
	}
	return s, nil
}

// String renders the configuration for startup logging.
func (s Search) String() string {
	return fmt.Sprintf("url=%s index_alias=%s", s.URL, s.IndexAlias)
}
