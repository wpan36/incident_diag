# internal/embed

## Purpose

An OpenAI-compatible embeddings client: batching, timeouts, bounded retry, and the two
checks that make a wrong answer impossible to ignore.

## Contents

- `embed.go` — `Embedder`, `Client`, `New`, `Embed`; `Dimensions`; the retry schedule and
  `Retry-After` handling; `pair`; `APIError`, `DimensionError`, `ErrBadResponse`.
- `fake.go` — `Fake`, a deterministic `Embedder`, and `FakeVector`.
- `embed_test.go` — unit tests against `httptest`, including the out-of-order response.

## How it fits in

`cmd/ingestion-worker` builds a `Client` from `config.Embedding` and hands it to
`ingest.Deps.Embedder`. `internal/search` builds its `dense_vector` mapping from
`embed.Dimensions`, so this package is where the vector length is decided. ADR 0006 chose
hosted `BAAI/bge-m3`; the chat LLM is a separate endpoint and a separate key, because
DeepSeek has no embeddings endpoint at all.

## Gotchas

- **It is not an abstraction over embedding providers.** The `Embedder` interface exists
  because the ingestion handler's tests need a deterministic fake, not because a second
  implementation is expected.
- **The response is re-ordered by each item's `index` field**, and a count that does not
  match the request is an error. The schema carries that field precisely because order is not
  promised, and pairing vector *i* with chunk *j* is wrong in a way nothing downstream can
  detect: every write succeeds, every search returns something, and only the recall number is
  quietly poor.
- **`Dimensions` is a constant, not an environment variable.** A wrong environment value
  would silently build a wrong Elasticsearch mapping, which is the exact failure the
  dimension check exists to catch — and changing the model already means reindexing every
  document. The check is the only thing in this system that notices `EMBEDDING_MODEL` having
  been changed without one.
- **Permanent vs transient is a property of the document, not of a status code.** 400, 413
  and 422 all mean the input is too long and are not retried — SiliconFlow answers 400, a
  self-hosted TEI or vLLM behind the same URL answers 413 or 422. 401, 403 and 404 look
  permanent and are retried deliberately: they are a misconfiguration of the deployment, so
  the backlog should heal itself once an operator fixes the key rather than being re-uploaded
  by hand.
- **The HTTP client has no timeout of its own.** `EMBED_TIMEOUT` is applied per attempt
  through the context, so a retry gets a fresh one. What bounds the batches of one document
  together is `INGEST_DOCUMENT_TIMEOUT`.
- **Only the delta-seconds form of `Retry-After` is honoured**, and it is capped, so a
  provider asking for an hour cannot park the handler past its own deadline.
- **`Fake` ships in the package, not in a `_test.go` file**, because the code that most needs
  testing without a provider is in `internal/ingest`. It follows `mq.FakeProducer` in that.
  `FakeVector` is unit length, so a cosine search over fake vectors behaves like one over
  real ones.
