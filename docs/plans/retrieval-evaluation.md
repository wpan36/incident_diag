# Retrieval Evaluation

**Tier B spec (M12).** The corpus written here is reused by Phase C and by the agent
evaluation; it is authored once.

## Problem

Retrieval either works or it does not, and until there is a number nobody can tell which.
The number also has to be worth having: measured after M10, a full runbook produces about
four chunks and a short one produces a single chunk, so ten unrelated documents give roughly
forty chunks. If every query targets the only document on its topic, dense retrieval finds
it almost every time and Recall lands near 1.0 while distinguishing nothing.

## Technical Plan

### The corpus contains deliberate confusables

`testdata/knowledge/` holds documents that **share symptoms and metric names**, so a query
has to discriminate rather than match the only document on its subject:

- both `checkout-service` and `payment-service` have a latency runbook, and both describe a
  rising p99
- a third runbook covers connection pool exhaustion generally, which the payment runbook
  also describes
- two postmortems describe incidents whose symptoms overlap with those runbooks
- the remaining documents cover the other injectable faults, CPU saturation and 5xx rate,
  which Phase C needs anyway

Recall will be lower than it would be against ten unrelated documents. That is the point:
the lower number carries information and the higher one does not.

### Labelling is by file and heading path, not by chunk id

A chunk id is `<document_id>-<chunk_index>`, so it depends on the chunking strategy and on
which upload produced it. Labelling against it would invalidate the whole evaluation set the
first time chunking changes.

Each query instead names a source filename and an optional heading path prefix. A retrieved
chunk is relevant when it came from that file and its heading path has that prefix. Headings
are authored here and are stable, so the labels survive re-chunking.

```json
{
  "query": "checkout is returning 504s but payment latency looks normal",
  "relevant": [{ "source": "checkout-latency-runbook.md", "heading_prefix": "Downstream" }]
}
```

### What the number means

**Recall@k is the fraction of queries whose top k contains at least one relevant chunk.**
This is a hit rate, not the proportion of all relevant chunks retrieved. The two are both
called recall and they are not the same, so the definition is stated wherever the number is
reported.

### The harness

An integration test, skipped without `TEST_ELASTICSEARCH_URL` and an embedding key, that:

1. creates its **own index behind a unique alias**, never the production `chunks` alias —
   that one already accumulates chunks from manual smoke tests, and an evaluation whose
   corpus is whatever happens to be lying around measures nothing
2. parses and chunks every corpus file with the real chunker, so the evaluation exercises
   the shipped chunking rather than a fixture
3. embeds with the real provider, indexes, and runs every query
4. prints a markdown table of Recall@1/3/5 and, for each miss, the query and what came back
   instead
5. asserts a **floor** rather than an exact score, so a collapse fails the build while
   ordinary drift in a hosted model does not
6. deletes its index afterwards

The misses matter more than the score. A table of numbers says retrieval got worse; the
list of what was returned instead says why.

`docs/rag-eval.md` records the baseline, the corpus size, the definition above, and the
date. It is written from the test's output rather than by the test: a `go test` run that
rewrites a tracked file is a surprise, and the number is worth a human looking at it.

## Verification

- `make check` — the labelling predicate and the recall arithmetic are pure functions and
  are unit tested without infrastructure.
- `make test-integration` — the harness runs end to end and the floor holds.
- `docs/rag-eval.md` exists and its numbers match a fresh run.
