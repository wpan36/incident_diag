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

`github.com/yuin/goldmark`, walking the AST rather than rendering it. Three node types
matter:

- **Headings** become section boundaries and contribute to the heading path.
- **Fenced code blocks** are opaque and are never split.
- **Paragraphs**, list items and tables are the packing unit.

Everything else is flattened to its text content.

A `.txt` file does not go through goldmark. It is treated as a single untitled section
whose paragraphs are separated by blank lines.

The content must be valid UTF-8; anything else is a permanent failure. The file is read
through `internal/files`, which needs a read path it does not currently have — see
**Detailed Implementation**.

### Chunking

Heading-aware, bounded, with paragraph packing as the fallback:

1. Split the document into sections at headings, carrying the full heading path
   (`Payment Service > Latency > Connection pool`).
2. A section within the budget becomes one chunk.
3. A section over the budget is packed greedily at paragraph boundaries.
4. A fenced code block is never split. One that exceeds the budget alone becomes an
   oversized chunk, recorded as such.

**The embedded text is the heading path, a blank line, then the content** — not the bare
content. "Raise the pool size to 50" is nearly useless as a retrieval unit once separated
from the heading naming the service and the symptom it belongs to. The stored `content`
field holds exactly the text that was embedded, so what retrieval returns is what was
scored against.

**Chunks do not overlap.** Heading-aware splitting already cuts at semantic boundaries, so
overlap buys little here while inflating the index, handing the agent near-duplicate chunks
that consume its context budget, and flattering Recall@k by placing the same sentence in
several chunks. Revisit if M12 shows recall is the binding problem.

#### The token budget

There is no bge-m3 tokenizer for Go, so the budget is enforced against an **estimate**:
approximately one token per four ASCII characters and one per one and a half CJK
characters. The target is 400 tokens, which leaves the real ceiling far below the model's
8192 — an estimate wrong by a factor of two still fits comfortably.

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

- Batched, `EMBED_BATCH_SIZE` texts per request, default 32.
- A per-request timeout, and bounded retries with exponential backoff that honour
  `Retry-After` on 429.
- **The returned vector length is validated against the configured dimension on every
  response.** A mismatch is a permanent failure with an explicit message. This is the check
  that catches `EMBEDDING_MODEL` being changed without a reindex — a hazard ADR 0006 names
  and that nothing in the system currently detects.
- An `Embedder` interface with a single method, because ingestion tests need a
  deterministic fake. This satisfies the interface rule's "genuinely needs to be faked"
  rather than being speculative.

### Elasticsearch

A single node. The client's major version must match the server's, and both are pinned in
one place so they cannot drift apart.

**The alias `chunks` points at the index `chunks_v1`, and all reads and writes go through
the alias.** When the embedding model changes, `chunks_v2` is built alongside and the alias
switched atomically, so retrieval never goes down for a reindex.

`EnsureIndex` runs at worker startup and is idempotent, mirroring `mq.EnsureTopics`. It
**fails loudly when the alias exists but points at an unexpected index** rather than
accepting it — otherwise startup looks healthy while writes go somewhere else.

| Field | Type | Notes |
| --- | --- | --- |
| `document_id` | keyword | |
| `chunk_id` | keyword | `<document_id>-<chunk_index>`, deterministic |
| `chunk_index` | integer | |
| `service` | keyword | nullable |
| `document_type` | keyword | |
| `source` | keyword | the original filename |
| `heading_path` | keyword | for citation and display |
| `content` | text | exactly the text that was embedded |
| `embedding` | `dense_vector`, 1024 dims, `cosine` | |
| `indexed_at` | date | |

The Elasticsearch `_id` is the `chunk_id`, so re-indexing a document upserts rather than
duplicates.

`cosine` rather than `dot_product`: the faster option requires strictly unit-length vectors
and rejects the write otherwise, and the difference is irrelevant at this scale. It is
baked into the mapping, so it is a deliberate choice rather than a default.

Bulk writes do not force a refresh. Integration tests call `_refresh` explicitly before
asserting.

### Failure semantics

S2 already defines the handler's three outcomes. This spec only says which failure is
which.

**Permanent — mark `FAILED` with a reason:**

- the stored file is missing or unreadable
- the content is not valid UTF-8
- the document produces zero chunks, or more than the cap
- the embedding API returns 400, including input-too-long
- the embedding dimension does not match the mapping
- Elasticsearch reports a mapping conflict

**Transient — bounded retry inside the handler, then `FAILED`:**

- embedding API 429 or 5xx, or a network error
- Elasticsearch unavailable, or a bulk item rejected for backpressure

#### A document is fully indexed or not indexed at all

The order is **delete existing chunks by `document_id` → bulk index → `MarkDocumentReady`**,
chosen so that every crash point converges:

- crash between delete and index — the document has no chunks and is still `PROCESSING`, so
  the lease reclaims it and the work is redone
- crash between index and `MarkDocumentReady` — the same reclaim, and the leading delete
  clears whatever the previous attempt wrote

This is the behaviour S2 relies on when it says re-processing converges rather than
duplicating chunks.

If any bulk item fails permanently, **every chunk for that document is deleted again**
before the document is marked `FAILED`. A document indexed at 60% is the hardest kind of
bad state to notice, because it shows up only as a retrieval score that is quietly worse
than it should be.

`chunk_count` is written by `MarkDocumentReady`, which already accepts it.

## Alternatives

**Overlapping chunks.** Catches an answer straddling a boundary. Rejected for the reasons
under Chunking; revisit if M12 shows recall is the problem.

**A real tokenizer.** Accurate counting, at the cost of a dependency, a tokenizer file of
roughly 17MB to distribute, and a second artifact to change whenever the embedding model
changes. The conservative budget makes the estimate good enough.

**A hand-written markdown scanner.** Around 150 lines and no dependency, but uploads are
arbitrary markdown: setext headings and indented code blocks would be silently mis-split.
goldmark has no transitive dependencies, so the cost of taking it is small.

**A plain index with no alias.** One less concept, but changing the embedding model would
then mean deleting and rebuilding with retrieval unavailable in between — and ADR 0006 says
that day is coming.

**Per-chunk retry on partial bulk failure.** Finer-grained recovery, but it leaves the door
open to a partially indexed document, which is the state this design most wants to make
impossible.

**Storing the bare content and the heading path as separate fields, embedding only the
content.** Keeps the stored text clean, but then what was scored is not what is returned,
and a chunk read back by the agent loses the context that made it retrievable.

## Detailed Implementation

**M7 — `internal/ingest`**

Parser and chunker as pure functions: text in, chunks out. No I/O, no infrastructure, no
clock. This is where the unit tests concentrate.

**M8 — `internal/embed`**

`Client` over the OpenAI-compatible endpoint, the `Embedder` interface, a deterministic
fake for other packages' tests, batching, bounded retry, and the dimension check.

**M9 — `internal/search`**

`EnsureIndex` with the alias handling, the mapping above, bulk index, and
delete-by-document. Elasticsearch added to `deploy/docker-compose.yml` with a health check.

**M10 — wiring**

Replace the placeholder in `internal/ingest/handler.go` between the claim and the terminal
write. `internal/files` gains `Open`.

**`internal/files` needs a read path, and it is security-relevant.** The package has `Save`
and `Remove`; the worker must read back by `storage_path`. `Open` would be the second
function in that package that turns stored input into a filesystem path, so it must be
jailed to the root exactly as `checkName` jails writes — the write path already defends
against `..` and the Windows backslash spelling, and a read path that does not is a way
back in.

**Configuration added**, through the existing typed helpers in `internal/config/env.go`:
`ELASTICSEARCH_URL` (required), `ES_INDEX_ALIAS` (`chunks`), `EMBED_BATCH_SIZE` (32),
`EMBED_TIMEOUT` (30s), `EMBED_MAX_RETRIES` (3), `CHUNK_TARGET_TOKENS` (400),
`CHUNK_MAX_PER_DOCUMENT` (2000).

## Verification

- `make check` — the parser and chunker carry the weight, being pure: headings at every
  level, a section far over budget, a code block over budget on its own, a document with no
  headings at all, an empty file, CRLF line endings, invalid UTF-8, and the heading path
  actually appearing in the embedded text.
- `make test-integration` — `EnsureIndex` is idempotent and rejects a mis-pointed alias;
  bulk index then search returns the chunk; delete-by-document removes exactly one
  document's chunks and no others; re-ingesting the same document leaves the chunk count
  unchanged rather than doubling it.
- End to end: upload a runbook, watch it reach `READY` with a plausible `chunk_count`, and
  confirm its chunks are searchable. Re-enqueue the same document and confirm the count
  does not change.
- With `EMBEDDING_BASE_URL` pointed at a closed port: the document reaches `FAILED` with a
  reason, and the reconciler retries it while under the attempt limit.

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
immediately so, and a concurrent retrieval test could observe it.
