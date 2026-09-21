# Retrieval evaluation

Baseline for dense retrieval over the corpus in `testdata/knowledge/`. Reproduce with:

```sh
make up
make test-integration          # or: go test -count=1 -tags=integration -run TestRetrievalRecall -v ./internal/eval/
```

## What the number means

**Recall@k here is a hit rate: the fraction of queries whose top k results contain at least
one chunk labelled relevant.** It is not the proportion of all relevant chunks retrieved.
Both are called recall in the literature, and reporting one under a name that also means the
other is how a number ends up compared against a number it is not comparable with.

## Baseline — 2026-09-21

Dense retrieval only: kNN over `BAAI/bge-m3` embeddings, cosine similarity, no filters
applied. 10 documents, 24 chunks, 16 queries.

| Metric | Value |
| --- | --- |
| Recall@1 | 0.81 |
| Recall@3 | 1.00 |
| Recall@5 | 1.00 |
| Queries | 16 |

### Only Recall@1 carries information

Recall@3 and Recall@5 are saturated. With 24 chunks, asking for five of them and requiring
one to be relevant is not a demanding test, and a future change to retrieval will move
neither number. **Recall@1 is the number to compare against.**

The larger cutoffs are still reported because they are what will start to discriminate once
the corpus grows, and because a drop in them would mean something had broken badly.

### Queries answered, but not first

| Rank | Query | Outranked by |
| --- | --- | --- |
| 2 | checkout is returning 504s but payment-service looks healthy | `postmortem-2026-05-checkout-timeout.md` |
| 2 | how do I tell whether a connection pool is actually the bottleneck | `payment-latency-runbook.md` |
| 3 | who do I page when payment-service is broken | `payment-latency-runbook.md` |

**Two of these three are arguably correct answers that the labels do not accept**, so 0.81
is pessimistic by a known amount:

- The postmortem that outranked the checkout runbook is the recorded instance of exactly
  that scenario. A person asking during an incident probably wants the runbook, but the
  postmortem is not a wrong result.
- `payment-latency-runbook.md` contains a section that does answer how to tell whether the
  pool is the bottleneck, alongside the generic runbook the label names.

The labels were **not** widened after seeing this. Adjusting labels to accept whatever the
retriever returned would tune the measurement to the thing being measured, which is the
failure this evaluation exists to avoid. A metric that is stable and slightly pessimistic
serves regression detection; one that was relaxed until it looked good serves nothing.

The third row is the labelling working as intended: `payment-latency-runbook.md` produces
four chunks and only the escalation one names the rotation, so the two chunks above it are
other sections of the same file.

## The corpus contains deliberate confusables

Ten unrelated documents would make this measurement worthless: with roughly forty chunks and
each query targeting the only document on its subject, dense retrieval finds it almost every
time. The corpus is built so a query has to discriminate.

- `payment-latency-runbook.md` and `checkout-latency-runbook.md` both describe a rising p99
  and both mention the other service.
- `connection-pool-runbook.md`, `payment-latency-runbook.md` and
  `postmortem-2026-03-payment-pool.md` all describe pool exhaustion.
- `cpu-saturation-runbook.md`, `checkout-latency-runbook.md` and
  `postmortem-2026-07-cpu-regression.md` all describe CPU saturation.
- Two postmortems describe 504s and timeouts from opposite ends.

## Labelling

A query names a source file and, optionally, a phrase that must appear in the chunk.
Nothing is labelled by chunk id: an id is `<document_id>-<chunk_index>`, so it depends on
the chunking strategy and on which upload produced it, and the whole set would break the
first time chunking changed. A phrase copied out of the corpus survives re-chunking.

The harness checks every label against the indexed corpus before scoring anything. A label
matching no chunk — a typo, or a phrase split across a chunk boundary — makes its query
unanswerable and would otherwise show up as a lower score that reads like a retrieval
regression.

## Known limitations

**The corpus is synthetic.** It was written for this evaluation, by the same project that
wrote the retriever. It is modelled on real runbooks and postmortems, and the confusables
are deliberate, but no number produced here is evidence about anyone else's documents.

**Documents below the token budget are never split.** The chunker merges sibling sections
while they fit `CHUNK_TARGET_TOKENS`, so a document under roughly 400 tokens becomes a
single chunk and its `heading_path` degenerates to the document title. Two consequences: a
citation cannot point at a section of a short document, and a corpus of short documents
makes chunking a no-op. The first corpus written for this evaluation had exactly that
problem — ten documents, ten chunks — and was rewritten to realistic length rather than
changing the chunker, because tuning the chunker to make an evaluation look better is
backwards.

**The floor is 0.70 at k=5, not an exact score.** The embedding model is hosted and can
drift, so pinning a precise number would make the test fail for reasons that are not this
project's. The floor still catches what is worth catching: a broken query body, an empty
index, a mapping change that stopped indexing the vector.
