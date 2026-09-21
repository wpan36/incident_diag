# internal/eval

## Purpose

Scores retrieval against a fixed set of labelled queries, and holds the definition of what
the resulting number means.

## Contents

- `eval.go` — `Manifest`/`Document` (the corpus and the metadata an upload would carry),
  `Label`, `Query`, `Set`, `Retrieved`, `Score`, `Outcome`, `RecallAt`, `UnresolvedLabels`,
  `Report`, and the two loaders.
- `eval_test.go` — unit tests. Everything but the loaders is pure, so the scoring is tested
  with literals and needs no infrastructure.
- `retrieval_integration_test.go` — `//go:build integration`. Chunks the real corpus with the
  shipped chunker, embeds it with the real provider, indexes it, and runs every query.

The corpus itself is `testdata/knowledge/`: the documents, `manifest.json` and `eval.json`.
The baseline is `docs/rag-eval.md`.

## How it fits in

It depends on nothing in the project. The integration test composes `ingest`, `embed` and
`search`, which is the same composition `api` performs for `GET /api/search`, so the
evaluation exercises the shipped pipeline rather than a reimplementation of it.

M32's agent evaluation belongs here too when it lands; that is why the package is named for
evaluation rather than for retrieval.

## Gotchas

- **Recall@k here is a hit rate**, the fraction of queries whose top k contains at least one
  relevant chunk — not the proportion of all relevant chunks retrieved. Both are called
  recall, so the definition is repeated wherever the number is reported.
- **Nothing is labelled by chunk id.** An id is `<document_id>-<chunk_index>`, so it depends
  on the chunking strategy and on which upload produced it; labelling against one would
  invalidate the set the first time chunking changed. A label is a source file plus an
  optional phrase copied out of the corpus, which survives re-chunking.
- **`UnresolvedLabels` runs before scoring and is a hard failure.** A label that matches
  nothing makes its query unanswerable, which shows up as a lower score that reads like a
  retrieval regression rather than as a broken label.
- **`RecallAt` on no outcomes is 0, not 1.** A perfect score for an empty set would let a
  harness that failed to load its labels report success.
- **The report lists queries answered but not first, separately from outright misses.** With
  a corpus this small the larger cutoffs saturate, and then the rank of the first hit is the
  only signal that still moves.
- **The floor is asserted under Recall@1, not only under the largest cutoff.** With this
  corpus Recall@3 and Recall@5 are saturated at 1.00, so a floor under those alone catches a
  collapse and nothing else — Recall@1 could fall from 0.81 to zero with the build still
  green. `recallFloors` holds one floor per cutoff and the test fails if a cutoff it names is
  not among the ones reported.
- **The integration test builds its own index behind a unique alias** and never touches the
  production `chunks` alias, which accumulates chunks from manual smoke tests. An evaluation
  whose corpus is whatever happens to be lying around measures nothing.
- **Cleanup deletes the index over plain HTTP** rather than through `internal/search`.
  Nothing in the product deletes an index, and a destructive method on the production client
  that only a test calls is a permanent foot-gun for a temporary convenience.
- **The labels are not widened when the retriever returns a plausible neighbour.** Two of the
  three demotions in the current baseline are arguably correct answers the labels reject, and
  `docs/rag-eval.md` says so rather than relaxing them: tuning labels to the retriever is the
  failure this evaluation exists to avoid.
