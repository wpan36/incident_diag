// Package search owns the Elasticsearch chunk index: its mapping, its alias,
// and the two writes ingestion performs.
//
// Elasticsearch holds retrievable knowledge here, never authoritative state. A
// document's chunks can always be rebuilt from the original file plus the MySQL
// metadata, which is what lets ingestion delete and rewrite them freely.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
)

// bulkBatchSize is how many chunks go in one bulk request.
//
// A document's chunks go out as several bounded requests rather than as one
// carrying up to CHUNK_MAX_PER_DOCUMENT documents, because every request waits
// for a refresh and an unbounded one would also build an unbounded body in
// memory.
const bulkBatchSize = 100

// bulkMaxAttempts and bulkRetryDelay bound how many times one batch is
// re-sent after Elasticsearch rejected its items.
//
// This is a second retry loop on top of the client's own RetryOnStatus, and it
// is not redundant: a bulk request answers 200 with its per-item failures
// inside the body, so the transport-level retry never sees them.
const (
	bulkMaxAttempts = 3
	bulkRetryDelay  = 500 * time.Millisecond
)

// Chunk is one indexed chunk. The JSON tags are the mapping's field names, so
// the two are changed together or not at all.
type Chunk struct {
	DocumentID   string    `json:"document_id"`
	ChunkID      string    `json:"chunk_id"`
	ChunkIndex   int       `json:"chunk_index"`
	Service      *string   `json:"service"`
	DocumentType string    `json:"document_type"`
	Source       string    `json:"source"`
	HeadingPath  string    `json:"heading_path"`
	Content      string    `json:"content"`
	Embedding    []float32 `json:"embedding"`
	IndexedAt    time.Time `json:"indexed_at"`
}

// ChunkID is the Elasticsearch _id: deterministic, so re-indexing a document
// upserts its chunks rather than duplicating them.
func ChunkID(documentID string, chunkIndex int) string {
	return fmt.Sprintf("%s-%d", documentID, chunkIndex)
}

// BulkItemError is one item Elasticsearch refused inside an otherwise
// successful bulk request.
//
// It carries the status because that is what says whether another attempt
// could help: a mapping conflict is a 400 and will be a 400 every time, while
// es_rejected_execution_exception is a 429 and means only that the write queue
// was full a moment ago.
type BulkItemError struct {
	ChunkID string
	Status  int
	Type    string
	Reason  string
}

func (e *BulkItemError) Error() string {
	return fmt.Sprintf("search: bulk index: chunk %s rejected with %d: %s: %s",
		e.ChunkID, e.Status, e.Type, e.Reason)
}

// retryableStatus reports whether a per-item status is worth another attempt.
// They are the same statuses the client retries at the request level, which is
// the point: where the failure was reported should not change how it is
// treated.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// Client is the Elasticsearch client this project uses.
type Client struct {
	es     *elasticsearch.Client
	alias  string
	logger *slog.Logger
}

// New builds a client.
//
// Retrying transient statuses is the client's own business rather than the
// handler's: 429 is backpressure and the 50x family is a node that is briefly
// unwell, and both are worth a second attempt before a document is marked
// FAILED.
func New(cfg config.Search, logger *slog.Logger) (*Client, error) {
	es, err := elasticsearch.NewClient(elasticsearch.Config{
		Addresses:     []string{cfg.URL},
		RetryOnStatus: []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout},
		MaxRetries:    3,
		RetryBackoff:  func(attempt int) time.Duration { return time.Duration(attempt) * 500 * time.Millisecond },
	})
	if err != nil {
		return nil, fmt.Errorf("search: open client: %w", err)
	}
	return &Client{es: es, alias: cfg.IndexAlias, logger: logger}, nil
}

// Alias returns the alias every read and write goes through.
func (c *Client) Alias() string { return c.alias }

// EnsureIndex creates the index and its alias if they are not there, and
// verifies them if they are.
//
// It is idempotent and runs at worker startup, mirroring mq.EnsureTopics, so
// that nobody has to remember a setup step. What it accepts is deliberately
// loose in one direction and strict in the other: any single index behind the
// alias is fine, whatever it is called, because insisting on chunks_v1 would
// mean a switch to chunks_v2 bricks the next worker restart. An alias that
// fans out to several indices, or a name that is secretly a concrete index,
// both make "the alias" ambiguous and would send writes somewhere the operator
// did not intend while startup still looked healthy.
func (c *Client) EnsureIndex(ctx context.Context) error {
	target, err := c.resolveAlias(ctx)
	switch {
	case err != nil:
		return err
	case target != "":
		c.logger.InfoContext(ctx, "chunk index ready", "alias", c.alias, "index", target)
		return nil
	}

	index := c.alias + "_v1"
	if err := c.createIndex(ctx, index); err != nil {
		return err
	}
	c.logger.InfoContext(ctx, "created the chunk index", "alias", c.alias, "index", index)
	return nil
}

// resolveAlias returns the single index the alias points at, or "" if the alias
// does not exist.
func (c *Client) resolveAlias(ctx context.Context) (string, error) {
	res, err := c.es.Indices.GetAlias(
		c.es.Indices.GetAlias.WithContext(ctx),
		c.es.Indices.GetAlias.WithName(c.alias),
	)
	if err != nil {
		return "", fmt.Errorf("search: resolve alias %s: %w", c.alias, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusNotFound {
		// No alias by that name — but the name may be taken by a concrete
		// index, in which case Elasticsearch would refuse to create the alias
		// later with a message about alias names rather than about the actual
		// problem. Checking here is what turns that into an explanation.
		return "", c.checkNameIsFree(ctx)
	}
	if res.IsError() {
		return "", responseError(res, "resolve alias "+c.alias)
	}

	var found map[string]json.RawMessage
	if err := json.NewDecoder(res.Body).Decode(&found); err != nil {
		return "", fmt.Errorf("search: decode the alias response: %w", err)
	}

	switch len(found) {
	case 0:
		return "", c.checkNameIsFree(ctx)
	case 1:
		for index := range found {
			return index, nil
		}
	}

	names := make([]string, 0, len(found))
	for index := range found {
		names = append(names, index)
	}
	return "", fmt.Errorf("search: the alias %s resolves to %d indices (%s); "+
		"writes would fan out, so this has to be fixed by hand",
		c.alias, len(found), strings.Join(names, ", "))
}

// checkNameIsFree reports whether the alias name is taken by a concrete index.
//
// It is the second of the two ambiguities worth failing on: an index called
// "chunks" makes "the alias" mean something that cannot be switched atomically,
// so a reindex would require retrieval to go down — the thing the alias exists
// to avoid.
func (c *Client) checkNameIsFree(ctx context.Context) error {
	res, err := c.es.Indices.Exists([]string{c.alias}, c.es.Indices.Exists.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("search: check whether %s is an index: %w", c.alias, err)
	}
	defer res.Body.Close()

	if res.StatusCode == http.StatusOK {
		return fmt.Errorf("search: %q is a concrete index, not an alias: every read and write goes "+
			"through the alias, so this has to be fixed by hand", c.alias)
	}
	return nil
}

// createIndex creates the concrete index with the mapping and points the alias
// at it in the same request, so there is no window in which the index exists
// without its alias.
func (c *Client) createIndex(ctx context.Context, index string) error {
	res, err := c.es.Indices.Create(index,
		c.es.Indices.Create.WithContext(ctx),
		c.es.Indices.Create.WithBody(strings.NewReader(c.mapping())),
	)
	if err != nil {
		return fmt.Errorf("search: create index %s: %w", index, err)
	}
	defer res.Body.Close()

	if !res.IsError() {
		return nil
	}
	// Another worker starting at the same moment got there first, which is
	// expected rather than exceptional: both of them call this at startup.
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "resource_already_exists_exception") {
		target, err := c.resolveAlias(ctx)
		if err != nil {
			return err
		}
		if target == "" {
			return fmt.Errorf("search: index %s exists but the alias %s does not point at it", index, c.alias)
		}
		return nil
	}
	return fmt.Errorf("search: create index %s: %s: %s", index, res.Status(), strings.TrimSpace(string(body)))
}

// mapping is the index definition.
//
// index: true on the vector is explicit rather than left to a version default,
// because the kNN query retrieval is built on requires it and a dense_vector
// that is merely stored cannot be searched without rewriting the query as a
// script score.
//
// cosine rather than dot_product: the faster option requires strictly
// unit-length vectors and rejects the write otherwise, and the difference is
// irrelevant at this scale.
//
// number_of_replicas is 0 because on a single node a replica can never be
// allocated, and the default of 1 leaves the index permanently yellow — turning
// cluster health into a signal nobody can read.
//
// content is analyzed text although nothing queries that inverted index today.
// The analysis is a cost, not an oversight: it keeps the field matchable and
// highlightable for debugging, and means the mapping does not have to change if
// lexical retrieval is ever revisited.
func (c *Client) mapping() string {
	return fmt.Sprintf(`{
  "settings": { "number_of_replicas": 0 },
  "aliases": { %q: {} },
  "mappings": {
    "properties": {
      "document_id":   { "type": "keyword" },
      "chunk_id":      { "type": "keyword" },
      "chunk_index":   { "type": "integer" },
      "service":       { "type": "keyword" },
      "document_type": { "type": "keyword" },
      "source":        { "type": "keyword" },
      "heading_path":  { "type": "keyword" },
      "content":       { "type": "text" },
      "embedding":     { "type": "dense_vector", "dims": %d, "index": true, "similarity": "cosine" },
      "indexed_at":    { "type": "date" }
    }
  }
}`, c.alias, embed.Dimensions)
}

// IndexChunks writes chunks through the alias, in bounded batches.
//
// refresh=wait_for costs at most one refresh interval per request, which is
// nothing against the tens of seconds the embedding calls take — and it is what
// makes the cleanup path work at all, since delete-by-query only sees what is
// searchable.
func (c *Client) IndexChunks(ctx context.Context, chunks []Chunk) error {
	for start := 0; start < len(chunks); start += bulkBatchSize {
		end := min(start+bulkBatchSize, len(chunks))
		if err := c.bulkWithRetry(ctx, chunks[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// bulkWithRetry re-sends a batch whose items Elasticsearch rejected for
// backpressure.
//
// Without it, a full write queue costs the document one of its
// INGEST_MAX_ATTEMPTS and a failure_reason that reads like a bug — the
// transient failure the spec classifies it as, handled as a permanent one.
// Re-sending the whole batch rather than only the failed items is safe because
// the document _id is the chunk id, so an item that did succeed is overwritten
// with itself.
func (c *Client) bulkWithRetry(ctx context.Context, chunks []Chunk) error {
	var lastErr error
	for attempt := 1; attempt <= bulkMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, time.Duration(attempt-1)*bulkRetryDelay); err != nil {
				return errors.Join(lastErr, err)
			}
			c.logger.WarnContext(ctx, "Elasticsearch rejected a bulk batch, retrying",
				"attempt", attempt, "max_attempts", bulkMaxAttempts, "error", lastErr)
		}

		err := c.bulk(ctx, chunks)
		var itemErr *BulkItemError
		switch {
		case err == nil:
			return nil
		case !errors.As(err, &itemErr) || !retryableStatus(itemErr.Status):
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("after %d attempts: %w", bulkMaxAttempts, lastErr)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// bulkResponse is the part of a bulk reply that matters: whether anything
// failed, and why the first failure did.
type bulkResponse struct {
	Errors bool `json:"errors"`
	Items  []map[string]struct {
		Status int `json:"status"`
		Error  *struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	} `json:"items"`
}

func (c *Client) bulk(ctx context.Context, chunks []Chunk) error {
	var body bytes.Buffer
	for _, chunk := range chunks {
		action := fmt.Sprintf(`{"index":{"_id":%q}}`, chunk.ChunkID)
		body.WriteString(action)
		body.WriteByte('\n')
		if err := json.NewEncoder(&body).Encode(chunk); err != nil {
			return fmt.Errorf("search: encode chunk %s: %w", chunk.ChunkID, err)
		}
	}

	res, err := c.es.Bulk(bytes.NewReader(body.Bytes()),
		c.es.Bulk.WithContext(ctx),
		c.es.Bulk.WithIndex(c.alias),
		c.es.Bulk.WithRefresh("wait_for"),
	)
	if err != nil {
		return fmt.Errorf("search: bulk index: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return responseError(res, "bulk index")
	}

	var decoded bulkResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("search: decode the bulk response: %w", err)
	}
	if !decoded.Errors {
		return nil
	}
	// A bulk request answers 200 with per-item failures inside it, so the body
	// is where a mapping conflict actually shows up. Items come back in
	// request order, so the index names the chunk that was refused.
	for i, item := range decoded.Items {
		for _, result := range item {
			if result.Error == nil {
				continue
			}
			failed := &BulkItemError{Status: result.Status, Type: result.Error.Type, Reason: result.Error.Reason}
			if i < len(chunks) {
				failed.ChunkID = chunks[i].ChunkID
			}
			return failed
		}
	}
	return errors.New("search: bulk index reported errors but named none")
}

// DeleteByDocument removes every chunk of one document.
//
// It runs before indexing, so that re-processing a document converges rather
// than accumulating stale chunks, and again after a failed index, so that a
// document is never left indexed at sixty percent — the hardest kind of bad
// state to notice, because it shows up only as a retrieval score that is
// quietly worse than it should be.
func (c *Client) DeleteByDocument(ctx context.Context, documentID string) error {
	query := fmt.Sprintf(`{"query":{"term":{"document_id":%q}}}`, documentID)

	res, err := c.es.DeleteByQuery([]string{c.alias}, strings.NewReader(query),
		c.es.DeleteByQuery.WithContext(ctx),
		c.es.DeleteByQuery.WithRefresh(true),
		// Another worker holding the same document is waste rather than
		// corruption, and a version conflict here should not fail the delete.
		c.es.DeleteByQuery.WithConflicts("proceed"),
	)
	if err != nil {
		return fmt.Errorf("search: delete chunks of %s: %w", documentID, err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return responseError(res, "delete chunks of "+documentID)
	}

	// A 200 is not proof the chunks are gone. Delete-by-query reports a failed
	// shard in the body, and conflicts=proceed means a document that changed
	// under the query is skipped rather than aborting the request — so both
	// leave chunks behind while the status line says success. Both matter
	// here: this call runs before every index so that re-processing converges,
	// and again after a failed one so that a document is never left indexed at
	// sixty percent. Failing loudly gets the document marked FAILED and the
	// delete redone on the next attempt, which is the recoverable outcome.
	var decoded deleteByQueryResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("search: decode the delete response for %s: %w", documentID, err)
	}
	switch {
	case len(decoded.Failures) > 0:
		return fmt.Errorf("search: delete chunks of %s: %s: %s", documentID,
			decoded.Failures[0].Cause.Type, decoded.Failures[0].Cause.Reason)
	case decoded.VersionConflicts > 0:
		return fmt.Errorf("search: delete chunks of %s: %d chunks changed while they were being "+
			"deleted and were left behind", documentID, decoded.VersionConflicts)
	}
	return nil
}

// deleteByQueryResponse is the part of a delete-by-query reply that says
// whether the delete actually finished.
type deleteByQueryResponse struct {
	Deleted          int `json:"deleted"`
	VersionConflicts int `json:"version_conflicts"`
	Failures         []struct {
		Cause struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"cause"`
	} `json:"failures"`
}

// responseError turns an error response into one that names the status and the
// body, bounded so that a page of HTML cannot end up in a failure_reason
// column.
func responseError(res *esapi.Response, what string) error {
	body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return fmt.Errorf("search: %s: %s: %s", what, res.Status(), strings.TrimSpace(string(body)))
}
