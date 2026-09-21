# Document Ingestion Pipeline

**Tier A spec (S3).** Gates M7 (parse and chunk), M8 (embedding client), M9 (Elasticsearch),
M10 (wiring them into the existing handler).

## Problem

M6 built the ingestion frame — decode the message, claim the document, write a terminal
status — and deliberately left the work itself unimplemented. `internal/ingest/handler.go`
marks every document `FAILED` with "ingestion is not implemented yet (M10)". This spec
fills that hole.

Everything the retrieval side depends on is decided here: how a document becomes chunks,
what a chunk looks like in Elasticsearch, and which failures are worth retrying. Two of
those are expensive to revisit. The chunking strategy determines what retrieval can find at
all, and the Elasticsearch mapping fixes the embedding dimensionality — ADR 0006 already
records that changing the embedding model means reindexing every document.

## Technical Plan

### Parsing

The file is read through `internal/files` and normalized before anything else looks at it:

- **Invalid UTF-8 is a permanent failure.** Nothing downstream — not goldmark, not the token
  estimate, not a `utf8mb4` failure reason — behaves sensibly on arbitrary bytes.
- **CRLF becomes LF**, so a file authored on Windows produces the same chunks as the same
  file authored on Linux. Without this, every paragraph carries a trailing `\r` into the
  embedded text.
- **A leading UTF-8 BOM is stripped.** It is invisible in an editor and would otherwise sit
  in front of the first `#`, so goldmark would not see a heading at all and the whole
  document would degrade to a single untitled section.

Markdown is parsed with `github.com/yuin/goldmark`, walking the AST rather than rendering
it. Three node types matter:

- **Headings** become section boundaries and contribute to the heading path. A heading
  whose text is empty — `#` followed by nothing but a space — is still a boundary but
  contributes nothing, because a path of `" > Sub"` would be both a keyword field and the
  first line of the embedded text.
- **Fenced code blocks** are opaque and are never split.
- **Paragraphs**, list items and tables are the packing unit.

Everything else is flattened to its text content.

A `.txt` file does not go through goldmark. Dispatch is on the stored `format` column
(`markdown` or `text`), not on the filename, because that column is what the API already
derived and validated. A text file is treated as a single section with an **empty heading
path**, whose paragraphs are separated by blank lines. Markdown content appearing before the
first heading is the same case: a section with an empty heading path.

### Chunking

Heading-aware, bounded, with paragraph packing as the fallback:

1. Split the document into sections at **every** heading, at every level. A heading that has
   sub-headings still produces a section of its own for the text between it and its first
   child.
2. Carry the full heading path, joined with `" > "`:
   `Payment Service > Latency > Connection pool`.
3. **Merge consecutive sibling sections while they fit the budget.** A runbook of a dozen
   two-line `###` headings would otherwise produce a dozen chunks of forty tokens each, one
   embedding call apiece, none of which carries enough context to be worth retrieving. A
   merged chunk's heading path is the shared parent path, and **each merged section keeps
   its own heading line in the body** — otherwise merging silently discards the names of the
   things it merged.

   **Top-level headings merge too, under an empty heading path.** A runbook of
   `## Symptom` / `## Check` / `## Fix` with no title above them is the most common shape
   this system ingests, and excluding it would leave the rule not firing in exactly the case
   that motivates it. The merged body carries every heading line, so an empty path costs the
   keyword field and nothing else — the same position a `.txt` file is already in. The
   preamble is the one section that never merges: it has no heading of its own to write into
   the body, so a merge would join it to a named section with nothing saying where it ended.
4. A section that is within the budget and was not merged becomes one chunk. Its heading is
   already in the path, so the body does not repeat it.
5. A section over the budget is packed greedily at packing-unit boundaries.
6. **Any single packing unit over the budget — a paragraph, a list item, a table or a fenced
   code block — becomes an oversized chunk and is never split.** One rule rather than one
   per node type: the target is far enough below the model's ceiling that an oversized chunk
   still embeds, and a sentence splitter accurate across English and Chinese punctuation is
   more machinery than the case is worth. An oversized chunk is logged at warning level with
   its `chunk_id` and its estimate.
7. **A section with a heading and no body is dropped.** It would otherwise produce a chunk
   whose entire content is its own heading path, which most embedding APIs reject as empty
   and which no retrieval result should ever return.

`chunk_index` is 0-based and follows document order.

**The embedded text is the heading path, a blank line, then the content** — not the bare
content. "Raise the pool size to 50" is nearly useless as a retrieval unit once separated
from the heading naming the service and the symptom it belongs to. The stored `content`
field holds exactly the text that was embedded, so what retrieval returns is what was
scored against. Every chunk packed out of one over-budget section repeats the prefix; a
chunk whose heading path is empty is the body alone, with no leading blank line.

**Chunks do not overlap.** Heading-aware splitting already cuts at semantic boundaries, so
overlap buys little here while inflating the index, handing the agent near-duplicate chunks
that consume its context budget, and flattering Recall@k by placing the same sentence in
several chunks. Revisit if M12 shows recall is the binding problem.

#### The token budget

There is no bge-m3 tokenizer for Go, so the budget is enforced against an **estimate** over
runes:

- a rune in the CJK ranges counts as `1 / 1.5` tokens
- **every other rune** counts as `1 / 4` tokens

The second rule is stated as a catch-all rather than as "ASCII", because Cyrillic, accented
Latin and emoji otherwise fall through a rule written for two alphabets, and this is a pure
function carrying most of M7's unit tests — it has to be total.

The estimate is computed over **the embedded text, including the heading path prefix**. A
deep heading path is sixty or eighty characters that are part of what gets sent, so packing
against the bare body would push chunks over the budget by construction.

The target is `CHUNK_TARGET_TOKENS`, 400, which leaves the real ceiling far below the
model's 8192 — an estimate wrong by a factor of two still fits comfortably.

**The CJK coefficient is a guess that has never been measured.** It is recorded as a known
limitation below and should be calibrated against the real corpus once M12 exists.

If the estimate is wrong enough that the embedding API rejects the input anyway, that is a
permanent failure naming the offending chunk, not something to retry.

#### Caps

A document producing **zero chunks** is a permanent failure. An empty or whitespace-only
upload must not become a `READY` document with nothing behind it, because retrieval
evaluation would later have to unpick that.

A document producing more than `CHUNK_MAX_PER_DOCUMENT` (default 2000) is also a permanent
failure, so one pathological upload cannot consume the whole embedding budget.

### Embedding

`internal/embed`, an OpenAI-compatible client against `EMBEDDING_BASE_URL`.

```go
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
}
```

An interface with a single method, because ingestion tests need a deterministic fake. This
satisfies the interface rule's "genuinely needs to be faked" rather than being speculative.

- Batched, `EMBED_BATCH_SIZE` texts per request, default 32.
- A per-request timeout (`EMBED_TIMEOUT`), and bounded retries (`EMBED_MAX_RETRIES`) with
  exponential backoff that honour `Retry-After` on 429.
- **The response is re-ordered by each embedding's `index` field, and a count that does not
  match the request is an error.** The OpenAI-compatible schema carries `index` precisely
  because order is not promised, and pairing vector *i* with chunk *j* produces an index
  that is wrong in a way nothing downstream can detect — every write succeeds, every search
  returns something, and only the recall number is quietly poor.
- **`embed.Dimensions = 1024` is a constant in this package.** `internal/search` builds its
  mapping from it and the client validates every returned vector against it, so the mapping
  and the check cannot drift apart. A mismatch is a permanent failure with an explicit
  message — this is the check that catches `EMBEDDING_MODEL` being changed without a
  reindex, a hazard ADR 0006 names and that nothing in the system currently detects. A
  constant rather than a variable because a wrong environment value would silently build a
  wrong mapping, which is the exact failure this check exists to catch, and because changing
  the model already requires reindexing everything.

### Elasticsearch

`github.com/elastic/go-elasticsearch/v8` against a single-node `docker.elastic.co/
elasticsearch/elasticsearch` 8.17 image, with security disabled. **The client's major
version must match the server's**, so the `go.mod` requirement and the Compose image tag are
a pair: neither moves without the other, and the Compose file says so in a comment.

**The alias `chunks` points at the index `chunks_v1`, and all reads and writes go through
the alias.** When the embedding model changes, `chunks_v2` is built alongside and the alias
switched atomically, so retrieval never goes down for a reindex. Building v2 and switching
the alias is a manual operation — there is no tooling for it in this project, and inventing
some before the first model change would be guessing at the shape of that day.

`EnsureIndex` runs at worker startup and is idempotent, mirroring `mq.EnsureTopics`:

| State found | Action |
| --- | --- |
| the alias does not exist | create `chunks_v1` with the mapping below, point the alias at it |
| the alias resolves to exactly one index | accept it, whatever it is named |
| the alias resolves to several indices | fail loudly |
| the alias name exists as a concrete index, not an alias | fail loudly |

Accepting any single target rather than insisting on `chunks_v1` is what keeps the v2 switch
from bricking the next worker restart. What is still caught is the case worth catching —
writes going somewhere the operator did not intend while startup looks healthy — because a
fan-out alias and a name that is secretly an index both make "the alias" ambiguous.

| Field | Type | Notes |
| --- | --- | --- |
| `document_id` | keyword | |
| `chunk_id` | keyword | `<document_id>-<chunk_index>`, deterministic |
| `chunk_index` | integer | 0-based, document order |
| `service` | keyword | nullable |
| `document_type` | keyword | |
| `source` | keyword | the original filename |
| `heading_path` | keyword | `A > B > C`, empty for content under no heading |
| `content` | text | exactly the text that was embedded |
| `embedding` | `dense_vector`, `embed.Dimensions` dims, `index: true`, `cosine` | |
| `indexed_at` | date | |

`index: true` is explicit rather than left to a version default, because the kNN query M11 is
built on requires it and a `dense_vector` that is merely stored cannot be searched without
rewriting the query as a script score.

`number_of_replicas` is 0. On a single node a replica can never be allocated, so the default
of 1 leaves the index permanently yellow and turns cluster health into a signal nobody can
read.

The Elasticsearch `_id` is the `chunk_id`, so re-indexing a document upserts rather than
duplicates.

`cosine` rather than `dot_product`: the faster option requires strictly unit-length vectors
and rejects the write otherwise, and the difference is irrelevant at this scale. It is
baked into the mapping, so it is a deliberate choice rather than a default.

**Bulk writes use `refresh=wait_for`.** A document's chunks go out as several bulk requests
of bounded size rather than one request carrying up to 2000 documents. `wait_for` costs at
most one refresh interval per request, which is nothing against the tens of seconds the
embedding calls take — and it is what makes the cleanup path below actually work, since
delete-by-query only sees what is searchable.

**A bulk request answers 200 with its per-item failures inside the body**, so the client's
own `RetryOnStatus` never sees them. Item statuses are therefore classified in this package
and a batch whose items were rejected for backpressure (429, or the 50x family) is re-sent,
bounded. Re-sending the whole batch rather than only the failed items is safe because the
`_id` is the `chunk_id`, so an item that did succeed is overwritten with itself. A mapping
conflict is a 400 and is not retried. Without this, a full write queue would be handled as
a permanent failure and would cost the document one of its `INGEST_MAX_ATTEMPTS`.

**A 200 from delete-by-query is not proof the chunks are gone.** A failed shard is reported
in the body, and `conflicts=proceed` skips a document that changed under the query rather
than aborting the request. Both leave chunks behind while the status line says success, so
the response body is checked and either one is an error — which marks the document `FAILED`
and has the delete redone on the next attempt, rather than leaving a half-removed document
nobody will look at again.

### Failure semantics

S2 already defines the handler's three outcomes. This spec only says which failure is
which.

**Permanent — mark `FAILED` with a reason:**

- the stored file is missing or unreadable
- the content is not valid UTF-8
- the document produces zero chunks, or more than the cap
- the embedding API returns 400, 413 or 422 — input-too-long, whichever status the
  provider spells it with
- the embedding dimension does not match `embed.Dimensions`
- Elasticsearch reports a mapping conflict
- `INGEST_DOCUMENT_TIMEOUT` is exceeded

**Transient — bounded retry inside the handler, then `FAILED`:**

- embedding API 429 or 5xx, or a network error
- embedding API **401, 403 or 404** — a missing or expired `EMBEDDING_API_KEY`, or an
  `EMBEDDING_MODEL` the provider does not serve
- Elasticsearch unavailable, or a bulk item rejected for backpressure

The permanent set is about the document rather than about a status code: SiliconFlow
answers an over-long input with 400, and a self-hosted TEI or vLLM behind the same
OpenAI-compatible URL answers 413 or 422. Retrying any of them spends the whole budget
before failing with the message it would have failed with immediately.

401 and 403 look permanent and are classified transient deliberately. They are a
misconfiguration of the deployment rather than a property of the document, so the useful
behaviour is for the backlog to heal itself once an operator fixes the key: the reconciler
retries at 1 minute and 5 minutes, which is enough for someone watching the logs. Treating
them as permanent would burn all three attempts of every document uploaded during the
outage, and S2 already records that a document which fails three times needs a human and
that there is no endpoint for that person to use. The failure reason names the status code
either way, so the logs still say what happened.

#### A document is fully indexed or not indexed at all

The order is **parse → chunk → embed → delete existing chunks by `document_id` → bulk index
→ `MarkDocumentReady`**.

Embedding comes before the delete, and that ordering is load-bearing: a document that is
already `READY` and healthy keeps its chunks through a failed re-ingest, because nothing is
removed until every vector is in hand. Deleting first would mean one provider 400 costs
retrieval a document it already had.

Every crash point converges:

- crash between delete and index — the document has no chunks and is still `PROCESSING`, so
  the lease reclaims it and the work is redone
- crash between index and `MarkDocumentReady` — the same reclaim, and the leading delete
  clears whatever the previous attempt wrote

This is the behaviour S2 relies on when it says re-processing converges rather than
duplicating chunks.

If any bulk item fails permanently, **every chunk for that document is deleted again** before
the document is marked `FAILED`. A document indexed at 60% is the hardest kind of bad state
to notice, because it shows up only as a retrieval score that is quietly worse than it should
be. This delete is why the bulk requests wait for a refresh: a delete-by-query issued
milliseconds after an unrefreshed bulk would match nothing and leave behind exactly the
half-indexed document it was meant to remove.

`chunk_count` is written by `MarkDocumentReady`, which already accepts it. A `false` return
from it is logged at debug and the record is committed — per the store's contract, `false`
means the work was already done by another delivery, not that anything failed.

#### The handler's time budget

`INGEST_DOCUMENT_TIMEOUT` (default 5m) is applied as a context deadline immediately after
the claim succeeds, and covers everything from reading the file to the terminal write.

Without it the worst case is not bounded at all: 2000 chunks is 63 embedding requests, and
at `EMBED_TIMEOUT` 30s with `EMBED_MAX_RETRIES` 3 plus backoff those alone can run for well
over an hour. Two things break when they do.

- **S2's lease invariant.** "The lease must exceed the longest legitimate processing time,
  or a slow document gets claimed twice while the first attempt is still working."
- **The consumer group.** A member that blocks through a rebalance is evicted, the message
  is redelivered, and the claim then refuses it because the row is `PROCESSING` — the
  long-handler problem S2 wrote down for the agent worker, which now applies here.

So two constraints, both against the document timeout:

```
INGEST_DOCUMENT_TIMEOUT  <  INGEST_LEASE                  (5m < 10m at the defaults)
INGEST_DOCUMENT_TIMEOUT  <  the group's rebalance timeout
```

They are separate bounds, not a chain: the rebalance timeout has no relationship to the
lease. The ingestion consumer sets its rebalance timeout to `INGEST_DOCUMENT_TIMEOUT` plus a
one-minute margin rather than reading a variable of its own, so the second constraint holds
by construction and cannot be broken by someone raising the timeout and forgetting. The
first is validated at worker startup, the way S2 has M25 validate its pair.

## Alternatives

**Overlapping chunks.** Catches an answer straddling a boundary. Rejected for the reasons
under Chunking; revisit if M12 shows recall is the problem.

**A real tokenizer.** Accurate counting, at the cost of a dependency, a tokenizer file of
roughly 17MB to distribute, and a second artifact to change whenever the embedding model
changes. The conservative budget makes the estimate good enough.

**A hand-written markdown scanner.** Around 150 lines and no dependency, but uploads are
arbitrary markdown: setext headings and indented code blocks would be silently mis-split.
goldmark has no transitive dependencies, so the cost of taking it is small.

**No section merging — one chunk per heading.** The simplest rule and the easiest to test.
Rejected because a heading-dense runbook, which is exactly the shape of document this system
ingests, would become dozens of forty-token chunks, each costing an embedding call and none
carrying enough context to be worth retrieving.

**Splitting an over-budget paragraph at sentence boundaries.** More even chunk sizes, at the
cost of a sentence splitter that has to be right across English and Chinese punctuation —
and a layer that is hard to test and easy to get subtly wrong, in exchange for evenness the
budget does not actually need.

**A plain index with no alias.** One less concept, but changing the embedding model would
then mean deleting and rebuilding with retrieval unavailable in between — and ADR 0006 says
that day is coming.

**`EnsureIndex` demanding a specific index name.** Catches more, via an `ES_INDEX_NAME`
variable the startup check compares against. Rejected because the alias switch is then a
two-step operation where forgetting the second step stops the worker, and because the
variable is one more thing that can be set wrong.

**An `EMBEDDING_DIMENSIONS` variable instead of a constant.** Changing the embedding model
would need no code change. Rejected: a wrong value silently produces a wrong mapping, which
is the failure the dimension check exists to catch, and the model cannot change without a
reindex anyway.

**Per-chunk retry on partial bulk failure.** Finer-grained recovery, but it leaves the door
open to a partially indexed document, which is the state this design most wants to make
impossible.

**Storing the bare content and the heading path as separate fields, embedding only the
content.** Keeps the stored text clean, but then what was scored is not what is returned,
and a chunk read back by the agent loses the context that made it retrievable.

**Treating 401 and 403 as permanent failures.** Semantically defensible — retrying will not
fix a wrong key. Rejected under Failure semantics: it converts an operator's mistake into a
backlog that has to be re-uploaded by hand.

## Detailed Implementation

**M7 — `internal/ingest`**

Parser and chunker as pure functions: text in, chunks out. No I/O, no infrastructure, no
clock. This is where the unit tests concentrate.

**M8 — `internal/embed`**

`Client` over the OpenAI-compatible endpoint, the `Embedder` interface, a deterministic fake
for other packages' tests, batching, bounded retry, response re-ordering, `Dimensions` and
the dimension check.

**M9 — `internal/search`**

`EnsureIndex` with the alias rules above, the mapping, bulk index with `refresh=wait_for`,
and delete-by-document.

Elasticsearch is added to `deploy/docker-compose.yml`: single node
(`discovery.type=single-node`), `xpack.security.enabled=false`, a bounded `ES_JAVA_OPTS`
heap so it does not take the whole machine, a health check against the cluster health
endpoint because `make up` uses `--wait`, and the host port published through `ES_PORT` for
the same reason `MYSQL_PORT` and `KAFKA_PORT` exist.

**M10 — wiring**

Replace the placeholder in `internal/ingest/handler.go` between the claim and the terminal
write, and apply the `INGEST_DOCUMENT_TIMEOUT` deadline there. `cmd/ingestion-worker` gains
the embedding client, the search client and an `EnsureIndex` call at startup, alongside the
existing `EnsureTopics`.

**`internal/files` gains `Open(documentID, filename string)`, and it is security-relevant.**
The package has `Save` and `Remove`; the worker must read the bytes back. This is the second
function in that package that turns stored input into a filesystem path, so it takes the two
components separately and puts each through the existing `checkName`, exactly mirroring
`Save` — the write path already defends against `..` and the Windows backslash spelling, and
a read path that does not is a way back in. The caller has the `documents` row, which carries
both `id` and `filename`, so `storage_path` is never parsed as a path by anything.

**Configuration added**, through the existing typed helpers in `internal/config/env.go` and
following the existing split by concern:

| Loader | Variables |
| --- | --- |
| `LoadEmbedding` | `EMBEDDING_BASE_URL` (required), `EMBEDDING_API_KEY` (required), `EMBEDDING_MODEL` (required), `EMBED_BATCH_SIZE` (32), `EMBED_TIMEOUT` (30s), `EMBED_MAX_RETRIES` (3) |
| `LoadSearch` | `ELASTICSEARCH_URL` (required), `ES_INDEX_ALIAS` (`chunks`) |
| `LoadDocuments` | `CHUNK_TARGET_TOKENS` (400), `CHUNK_MAX_PER_DOCUMENT` (2000) |
| `LoadReconcile` | `INGEST_DOCUMENT_TIMEOUT` (5m), alongside the existing `INGEST_` pair |

The three `EMBEDDING_*` variables are already in `.env.example` but no loader reads them yet;
M8 is where they start being loaded. `ES_PORT` (9200) and `TEST_ELASTICSEARCH_URL` join them
in `.env.example`.

**Every new variable is added to `config_test`'s `isolate` list**, or those tests inherit
whatever the developer has in their shell — the rule that package's `CLAUDE.md` already
states.

**`make test-integration` gains a `TEST_ELASTICSEARCH_URL` guard** next to the existing
`TEST_MYSQL_DSN` and `TEST_KAFKA_BROKERS` ones. Without it, every M9 integration test skips
and the run reports success, which is the failure mode S2 already refused to accept.

## Verification

- `make check` — the parser and chunker carry the weight, being pure: headings at every
  level, sibling sections merging up to the budget and stopping at it, top-level siblings
  merging under an empty path while the preamble does not, a parent section not merging with
  its children, a merged chunk keeping its children's heading lines, a heading with no body
  being dropped, a heading with no text contributing nothing to the path, a section far over
  budget, a single paragraph, a single code block and a single table each over budget on
  their own, an indented code block keeping its indentation, a document with no headings at
  all, an empty file, CRLF line endings, a leading BOM, invalid UTF-8, the heading path
  actually appearing in the embedded text, and the token estimate over ASCII, CJK and a rune
  that is neither. In `internal/embed`: a response returned out of order is re-paired
  correctly, a short response is an error, a wrong-length vector is a permanent failure, and
  an over-long input is not retried whichever of 400, 413 and 422 the provider answers with.
  In `internal/search`, against a fake cluster, the two replies a real one almost never
  produces: a bulk answering 200 with a 429 inside it is retried while one with a 400 inside
  it is not, and a delete-by-query answering 200 having left chunks behind is an error.
- `make test-integration` — `EnsureIndex` is idempotent, creates `chunks_v1` and the alias
  from nothing, accepts an alias already pointing at `chunks_v2`, and fails on an alias
  resolving to two indices; bulk index then search returns the chunk; delete-by-document
  removes exactly one document's chunks and no others; re-ingesting the same document leaves
  the chunk count unchanged rather than doubling it; a bulk whose items fail leaves the
  document with zero chunks and `FAILED`, not with a partial set; a document over
  `CHUNK_MAX_PER_DOCUMENT` fails without embedding anything.
- End to end: upload a runbook, watch it reach `READY` with a plausible `chunk_count`, and
  confirm its chunks are searchable. Re-enqueue the same document and confirm the count
  does not change.
- With `EMBEDDING_BASE_URL` pointed at a closed port: the document reaches `FAILED` with a
  reason, and the reconciler retries it while under the attempt limit.
- With `EMBED_TIMEOUT` and `INGEST_DOCUMENT_TIMEOUT` turned down far enough that a real
  document cannot finish: the document reaches `FAILED` naming the timeout, and does so
  before the lease would have reclaimed it.

## Known limitations, accepted

**The CJK token coefficient is unmeasured.** A Chinese document may produce chunks well over
the intended budget. The ceiling sits far below the model's limit so they will still embed,
but the granularity will not be what was designed. Calibrate against the real corpus once
M12 exists.

**Documents without headings degrade to arbitrary boundaries.** The no-overlap decision
rests on heading-aware splitting cutting at semantic boundaries. A long unstructured text
file has none, and greedy paragraph packing cuts wherever the budget happens to run out.

**A reclaimed document is briefly half-indexed.** Convergence is guaranteed, but between a
crash mid-bulk and the lease expiring — up to `INGEST_LEASE`, ten minutes by default —
retrieval can see part of a document. The system is eventually consistent here, not
immediately so, and a concurrent retrieval test could observe it. The document timeout does
not shorten this: it bounds a *working* handler, while this window is opened by one that
died and can only be closed by the lease.

**`content` is mapped as analyzed `text` although nothing queries that inverted index.**
ADR 0007 removed BM25, so the analysis is paid for and unused. It is kept rather than
disabled so the field can be highlighted and matched on for debugging, and so the mapping
does not have to change if that decision is ever revisited — but it is a cost, not an
oversight.

**Merging is only among siblings.** Two short sections under different parents stay
separate even when both are tiny, because merging across a parent boundary would produce a
chunk whose heading path is a lie. The pathological input is a document whose every heading
has exactly one short child: every section is an only child, so nothing ever merges and the
document becomes a long row of tiny chunks.
