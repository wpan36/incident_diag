# internal/ingest

## Purpose

Turns an uploaded file into indexed chunks, and drives one document through the ingestion
state machine: claim, parse, chunk, embed, delete, index, terminal write.

## Contents

- `parse.go` — `normalize` (UTF-8 validation, CRLF, BOM), `parseMarkdown` over goldmark's
  AST, `parseText`, and the `section`/`unit` types everything downstream is expressed in.
- `chunk.go` — `ChunkDocument`, the merge/pack rules, `Chunk`, `ChunkOptions`, `ErrNoChunks`
  and `TooManyChunksError`.
- `tokens.go` — `EstimateTokens` and the CJK rune ranges.
- `handler.go` — `Handler`, `Deps`, and the `mq.Handler` that ties the above to the store,
  the file storage, the embedder and Elasticsearch.
- `chunk_test.go`, `tokens_test.go` — the bulk of the package's tests, all pure.
- `integration_test.go` — the state machine against real MySQL, Kafka and Elasticsearch,
  with `embed.Fake` standing in for the provider.

## How it fits in

`cmd/ingestion-worker` builds a `Handler` and hands `Handler.Handle` to `mq.Consumer.Run`.
The parser and chunker are pure functions with no I/O and no clock, which is why they carry
most of the tests; the handler is the only part that touches infrastructure. The spec is
`docs/plans/document-ingestion-pipeline.md`.

## Gotchas

- **The embedded text is the heading path, a blank line, then the body**, and `Chunk.Content`
  holds exactly that — so what retrieval returns is what was scored against. The token
  estimate is computed over the same string, prefix included; packing against the bare body
  would push chunks over the budget by construction.
- **Format dispatch is on the stored `documents.format` column, never the filename.** That
  column is what the API already derived and validated. An unrecognized value is an error
  rather than a fall back to markdown.
- **A single packing unit over the budget is never split** — paragraph, list item, table or
  fenced code block alike. One rule rather than one per node type; the target sits far below
  the model's 8192 ceiling, so an oversized chunk still embeds. The handler logs it.
- **Merging is among siblings, including at the top level.** Top-level siblings merge under
  an empty heading path, because a runbook of `## Symptom` / `## Check` / `## Fix` with no
  title above it is the shape the rule exists for. The merged body keeps every heading line,
  so an empty path costs the keyword field and nothing else. The preamble never merges: it
  has no heading of its own to write into the body.
- **`newChunk` trims newlines, not whitespace.** An indented code block's first line begins
  with the four spaces that make it code, and `strings.TrimSpace` would take exactly those —
  `parse.go` expands every block to whole lines specifically to recover them.
- **A heading whose text is empty contributes nothing to the path.** Otherwise every path
  below it starts with `" > "`, in both the keyword field and the embedded text. It is still
  pushed on the stack, because the stack is what makes popping by level correct.
- **`section.level` is the heading's own level, not `len(path)`.** A `#` followed directly by
  a `###` has a path of length two and a level of three. `section.title` is likewise held
  separately rather than read off the end of `path`, since an empty title is not in the path.
- **Order is embed → delete → index, and it is load-bearing.** Nothing is removed until every
  vector is in hand, so a document that is already `READY` keeps its chunks through a failed
  re-ingest. A failed index deletes everything again: a document indexed at sixty percent
  shows up only as a retrieval score that is quietly worse than it should be.
- **The document deadline is applied the moment the claim succeeds**, and the terminal writes
  run on a context detached from it. A handler that has run out of time must still be able to
  say so; a cancelled write would leave the row `PROCESSING` for the lease to rescue.
- **`Handle` returns an error only when something outside the message is wrong.** Every
  outcome about the message itself is recorded in MySQL and returns nil, because the offset
  is committed either way.

## Known limitations

Both are recorded in the spec and accepted. The CJK token coefficient has never been
measured, so a Chinese document may produce chunks well over the intended budget — they still
embed, but the granularity is not what was designed. And a document with no headings degrades
to arbitrary boundaries, because greedy paragraph packing cuts wherever the budget runs out.
