# internal/search

## Purpose

Owns the Elasticsearch chunk index: its mapping, its alias, and the two writes ingestion
performs.

## Contents

- `search.go` — `Client`, `New`, `EnsureIndex` and the alias rules, `mapping`, `IndexChunks`
  with its bulk retry, `DeleteByDocument`; the `Chunk` document, `ChunkID`, `BulkItemError`.
- `search_test.go` — unit tests against a fake cluster, for the two replies a real one almost
  never produces.
- `integration_test.go` — the alias state table, kNN, and delete-by-document against real
  Elasticsearch.

## How it fits in

`cmd/ingestion-worker` calls `EnsureIndex` at startup, mirroring `mq.EnsureTopics`, and hands
the client to `ingest.Deps.Search`. The mapping's vector length comes from `embed.Dimensions`,
so the two cannot drift apart. Retrieval (M11) reads through the same alias.

## Gotchas

- **Elasticsearch holds retrievable knowledge here, never authoritative state.** A document's
  chunks can always be rebuilt from the original file plus the MySQL metadata, which is what
  lets ingestion delete and rewrite them freely.
- **Everything goes through the alias `chunks`, which points at `chunks_v1`.** When the
  embedding model changes, `chunks_v2` is built alongside and the alias switched atomically,
  so retrieval never goes down for a reindex. Building v2 and switching is a manual operation;
  there is no tooling for it, deliberately.
- **`EnsureIndex` accepts any single index behind the alias, whatever it is named.** Insisting
  on `chunks_v1` would mean the v2 switch bricks the next worker restart. What it does refuse
  is an alias resolving to several indices, and a name that is secretly a concrete index —
  both make "the alias" ambiguous and would send writes somewhere the operator did not intend
  while startup still looked healthy.
- **A bulk request answers 200 with its per-item failures inside the body**, so the client's
  own `RetryOnStatus` never sees them. Item statuses are classified here: 429 and the 50x
  family are backpressure and the batch is re-sent, bounded; a 400 is a mapping conflict and
  is not. Re-sending the whole batch is safe because the `_id` is the `chunk_id`, so an item
  that did succeed is overwritten with itself.
- **A 200 from delete-by-query is not proof the chunks are gone.** A failed shard is reported
  in the body, and `conflicts=proceed` skips a document that changed under the query rather
  than aborting. Both leave chunks behind while the status line says success, so the body is
  checked and either one is an error.
- **Bulk writes use `refresh=wait_for`**, which is not a nicety. The cleanup path deletes by
  query, and delete-by-query only sees what is searchable — a delete issued milliseconds after
  an unrefreshed bulk would match nothing and leave behind exactly the half-indexed document
  it was meant to remove.
- **`index: true` on the vector is explicit rather than left to a version default**, because
  the kNN query retrieval is built on requires it. `cosine` rather than `dot_product`: the
  faster option requires strictly unit-length vectors and rejects the write otherwise.
- **`number_of_replicas` is 0.** On a single node a replica can never be allocated, and the
  default of 1 leaves the index permanently yellow — turning cluster health into a signal
  nobody can read.
- **The client's major version must match the server's**, so the `go-elasticsearch/v8`
  requirement in `go.mod` and the image tag in `deploy/docker-compose.yml` are a pair.
- **`content` is analyzed `text` although nothing queries that inverted index.** ADR 0007
  removed BM25, so the analysis is a cost, not an oversight: it keeps the field matchable and
  highlightable for debugging, and means the mapping need not change if that is revisited.
- **The fake cluster in `search_test.go` must send `X-Elastic-Product: Elasticsearch`**, or
  the v8 client refuses to talk to it at all.
