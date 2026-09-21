//go:build integration

// The retrieval evaluation. It needs Elasticsearch and a real embedding
// provider:
//
//	make up && make test-integration
//
// It builds its own index behind a unique alias and deletes it afterwards. It
// never touches the production `chunks` alias: that one accumulates chunks from
// manual smoke tests, and an evaluation whose corpus is whatever happens to be
// lying around measures nothing.
package eval_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/eval"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/ingest"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// corpusDir is relative to this package.
const corpusDir = "../../testdata/knowledge"

// ks are the cutoffs reported. Recall@1 is what a single-shot retrieval would
// get; Recall@5 is closer to what the agent will actually be handed.
var ks = []int{1, 3, 5}

// recallFloor fails the build on a collapse without pinning an exact score.
//
// The embedding model is hosted and its behaviour can drift, so asserting a
// precise number would make this test fail for reasons that are not this
// project's. A floor still catches the failures worth catching: a broken query
// body, an empty index, a mapping change that silently stopped indexing the
// vector.
const recallFloor = 0.70

// chunkingForEval matches the production defaults. The evaluation exercises the
// shipped chunker rather than a fixture, so a chunking change shows up here as
// a score that moved.
var chunkingForEval = ingest.ChunkOptions{TargetTokens: 400, MaxPerDocument: 2000}

func TestRetrievalRecall(t *testing.T) {
	esURL := os.Getenv("TEST_ELASTICSEARCH_URL")
	if esURL == "" {
		t.Skip("TEST_ELASTICSEARCH_URL is not set; skipping the retrieval evaluation")
	}
	if os.Getenv("EMBEDDING_API_KEY") == "" {
		t.Skip("EMBEDDING_API_KEY is not set; skipping the retrieval evaluation")
	}

	embedCfg, err := config.LoadEmbedding()
	if err != nil {
		t.Fatalf("loading the embedding configuration: %v", err)
	}
	embedder := embed.New(embedCfg, log.Discard())

	alias := "eval_" + strings.ToLower(id.New())
	searcher, err := search.New(config.Search{URL: esURL, IndexAlias: alias}, log.Discard())
	if err != nil {
		t.Fatalf("opening the search client: %v", err)
	}

	ctx := context.Background()
	if err := searcher.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	t.Cleanup(func() { deleteIndices(t, esURL, alias) })

	manifest, err := eval.LoadManifest(corpusDir)
	if err != nil {
		t.Fatal(err)
	}
	set, err := eval.LoadSet(corpusDir)
	if err != nil {
		t.Fatal(err)
	}

	corpus := indexCorpus(ctx, t, embedder, searcher, manifest)
	t.Logf("indexed %d chunks from %d documents", len(corpus), len(manifest.Documents))

	// Before scoring anything. A label that matches nothing makes its query
	// unanswerable and depresses the score in a way that reads as a retrieval
	// regression, so it has to fail as what it is.
	if unresolved := eval.UnresolvedLabels(set, corpus); len(unresolved) > 0 {
		for _, l := range unresolved {
			t.Errorf("label matches no chunk: source=%s must_contain=%q", l.Source, l.MustContain)
		}
		t.Fatal("the evaluation set does not match the corpus")
	}

	outcomes := make([]eval.Outcome, 0, len(set.Queries))
	for _, q := range set.Queries {
		vectors, err := embedder.Embed(ctx, []string{q.Query})
		if err != nil {
			t.Fatalf("embedding %q: %v", q.Query, err)
		}
		results, err := searcher.Search(ctx, vectors[0], search.Query{K: maxK(ks)})
		if err != nil {
			t.Fatalf("searching %q: %v", q.Query, err)
		}
		outcomes = append(outcomes, eval.Score(q, retrieved(results)))
	}

	report := eval.Report(outcomes, ks)
	t.Logf("\n%s", report)

	for _, k := range ks {
		if got := eval.RecallAt(outcomes, k); k == maxK(ks) && got < recallFloor {
			t.Errorf("Recall@%d = %.2f, below the floor of %.2f", k, got, recallFloor)
		}
	}
}

// indexCorpus chunks and indexes every document in the manifest, returning what
// was indexed so the labels can be checked against it.
func indexCorpus(ctx context.Context, t *testing.T, embedder embed.Embedder,
	searcher *search.Client, manifest eval.Manifest) []eval.Retrieved {
	t.Helper()

	var corpus []eval.Retrieved
	for _, doc := range manifest.Documents {
		raw, err := os.ReadFile(filepath.Join(corpusDir, doc.Source))
		if err != nil {
			t.Fatalf("reading %s: %v", doc.Source, err)
		}

		chunks, err := ingest.ChunkDocument(store.FormatMarkdown, raw, chunkingForEval)
		if err != nil {
			t.Fatalf("chunking %s: %v", doc.Source, err)
		}

		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Content
		}
		vectors, err := embedder.Embed(ctx, texts)
		if err != nil {
			t.Fatalf("embedding %s: %v", doc.Source, err)
		}

		documentID := id.New()
		indexed := make([]search.Chunk, len(chunks))
		for i, c := range chunks {
			indexed[i] = search.Chunk{
				DocumentID:   documentID,
				ChunkID:      search.ChunkID(documentID, c.Index),
				ChunkIndex:   c.Index,
				Service:      doc.Service,
				DocumentType: doc.DocumentType,
				Source:       doc.Source,
				HeadingPath:  c.HeadingPath,
				Content:      c.Content,
				Embedding:    vectors[i],
				IndexedAt:    time.Now().UTC(),
			}
			corpus = append(corpus, eval.Retrieved{Source: doc.Source, Content: c.Content})
		}
		if err := searcher.IndexChunks(ctx, indexed); err != nil {
			t.Fatalf("indexing %s: %v", doc.Source, err)
		}
	}
	return corpus
}

func retrieved(results []search.Result) []eval.Retrieved {
	out := make([]eval.Retrieved, len(results))
	for i, r := range results {
		out[i] = eval.Retrieved{Source: r.Source, Content: r.Content}
	}
	return out
}

func maxK(ks []int) int {
	max := 0
	for _, k := range ks {
		if k > max {
			max = k
		}
	}
	return max
}

// deleteIndices removes what the run created, over plain HTTP.
//
// internal/search has no delete-index method and is not given one for this:
// nothing in the product deletes an index, and a destructive method on the
// production client that only a test calls is a permanent foot-gun in exchange
// for a temporary convenience. Cleanup that belongs to a test lives in the
// test.
//
// The indices are named rather than matched with a wildcard, because
// Elasticsearch refuses wildcard deletes by default and a cleanup that silently
// does nothing leaves a cluster full of evaluation indices.
func deleteIndices(t *testing.T, esURL, alias string) {
	t.Helper()
	target := strings.TrimSuffix(esURL, "/") + "/" + alias + "," + alias + "_v1" +
		"?ignore_unavailable=true"
	req, err := http.NewRequest(http.MethodDelete, target, nil)
	if err != nil {
		t.Logf("cleanup: building the delete request: %v", err)
		return
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Logf("cleanup: deleting %s: %v", alias, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		t.Logf("cleanup: deleting %s returned %s", alias, res.Status)
	}
}
