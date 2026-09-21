// Package eval scores retrieval against a fixed set of labelled queries.
//
// Everything here except loading the files is pure, so the parts that decide
// what counts as a hit and what the number means can be tested without
// Elasticsearch, an embedding provider or a corpus.
//
// The metric is a hit rate and is named as one wherever it is reported.
// "Recall@k" is used for two different things in the retrieval literature — the
// fraction of queries whose top k contains something relevant, and the fraction
// of all relevant items retrieved — and reporting one under a name that also
// means the other is how a number gets compared against a number it is not
// comparable with.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Document is one corpus file and the metadata it would be uploaded with.
//
// The manifest exists because service and document_type are not properties of
// the file's text: the API takes them as form fields, and the evaluation has to
// index the corpus the same way an upload would.
type Document struct {
	Source       string  `json:"source"`
	Service      *string `json:"service"`
	DocumentType string  `json:"document_type"`
}

// Manifest is the corpus.
type Manifest struct {
	Documents []Document `json:"documents"`
}

// Label says which chunks answer a query.
//
// It names a source file and, optionally, a phrase that must appear in the
// chunk. It deliberately does not name a chunk id: an id is
// <document_id>-<chunk_index>, so it depends both on the chunking strategy and
// on which upload produced it, and labelling against one would invalidate the
// whole set the first time chunking changed.
//
// A phrase survives re-chunking as long as the text does, which is why the
// phrases are copied out of the corpus rather than paraphrased.
type Label struct {
	Source      string `json:"source"`
	MustContain string `json:"must_contain,omitempty"`
}

// Query is one labelled query. Note is for the reader of the set, explaining
// what a query is meant to discriminate between; nothing reads it.
type Query struct {
	Query    string  `json:"query"`
	Note     string  `json:"note,omitempty"`
	Relevant []Label `json:"relevant"`
}

// Set is the evaluation set.
type Set struct {
	Queries []Query `json:"queries"`
}

// Retrieved is the minimum a result has to expose to be scored. It is not
// search.Result, so this package stays independent of how retrieval is
// implemented and the scoring can be tested with literals.
type Retrieved struct {
	Source  string
	Content string
}

// Matches reports whether r is one of the chunks this label describes.
func (l Label) Matches(r Retrieved) bool {
	if r.Source != l.Source {
		return false
	}
	return l.MustContain == "" || strings.Contains(r.Content, l.MustContain)
}

// Outcome is what one query produced.
type Outcome struct {
	Query     Query
	Retrieved []Retrieved

	// HitRank is the 1-based position of the first relevant result, or 0 if
	// none of them was relevant. One number answers every k at once, which is
	// what keeps the report cheap to extend.
	HitRank int
}

// Score runs the labels over one query's results, most similar first.
func Score(q Query, results []Retrieved) Outcome {
	out := Outcome{Query: q, Retrieved: results}
	for i, r := range results {
		for _, l := range q.Relevant {
			if l.Matches(r) {
				out.HitRank = i + 1
				return out
			}
		}
	}
	return out
}

// RecallAt is the fraction of queries whose top k contained something
// relevant.
//
// With no queries it is 0 rather than 1: an empty set has not demonstrated
// anything, and returning a perfect score for it would let a harness that
// failed to load its labels report success.
func RecallAt(outcomes []Outcome, k int) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	hits := 0
	for _, o := range outcomes {
		if o.HitRank > 0 && o.HitRank <= k {
			hits++
		}
	}
	return float64(hits) / float64(len(outcomes))
}

// UnresolvedLabels returns the labels that match nothing in the indexed corpus.
//
// This is a guard rather than a diagnostic. A label with a typo, or one whose
// phrase was split across a chunk boundary by a change to the chunker, matches
// nothing and silently makes its query unanswerable — which shows up as a lower
// score that looks like a retrieval regression. Checking the labels against the
// corpus before scoring turns that into an error that names the label.
func UnresolvedLabels(set Set, corpus []Retrieved) []Label {
	var out []Label
	for _, q := range set.Queries {
		for _, l := range q.Relevant {
			matched := false
			for _, c := range corpus {
				if l.Matches(c) {
					matched = true
					break
				}
			}
			if !matched {
				out = append(out, l)
			}
		}
	}
	return out
}

// Report renders the scores, the queries that were answered but not first, and
// every outright miss, as markdown.
//
// The two lists are the point. A table of numbers says retrieval got worse; the
// list of what came back instead says why, and it is the only part of this
// output anyone reads twice. The middle list matters once the corpus is small
// enough that the larger cutoffs saturate, because then the rank of the first
// hit is the only signal left.
func Report(outcomes []Outcome, ks []int) string {
	var b strings.Builder

	sorted := append([]int(nil), ks...)
	sort.Ints(sorted)

	fmt.Fprintf(&b, "| Metric | Value |\n| --- | --- |\n")
	for _, k := range sorted {
		fmt.Fprintf(&b, "| Recall@%d | %.2f |\n", k, RecallAt(outcomes, k))
	}
	fmt.Fprintf(&b, "| Queries | %d |\n", len(outcomes))

	largest := 0
	if len(sorted) > 0 {
		largest = sorted[len(sorted)-1]
	}

	// Every query that was not answered first, before the misses. Once the
	// corpus is small enough that the larger cutoffs saturate, this is the
	// only part of the report that still moves, and listing only outright
	// misses would hide it exactly when it is all there is.
	var demoted []Outcome
	for _, o := range outcomes {
		if o.HitRank > 1 {
			demoted = append(demoted, o)
		}
	}
	if len(demoted) > 0 {
		b.WriteString("\n### Answered, but not first\n\n")
		b.WriteString("| Rank | Query | Outranked by |\n| --- | --- | --- |\n")
		for _, o := range demoted {
			outranked := "—"
			if len(o.Retrieved) > 0 {
				outranked = "`" + o.Retrieved[0].Source + "`"
			}
			fmt.Fprintf(&b, "| %d | %s | %s |\n", o.HitRank, o.Query.Query, outranked)
		}
	}

	var missed []Outcome
	for _, o := range outcomes {
		if o.HitRank == 0 || o.HitRank > largest {
			missed = append(missed, o)
		}
	}
	if len(missed) == 0 {
		fmt.Fprintf(&b, "\nNo query missed at k=%d.\n", largest)
		return b.String()
	}

	fmt.Fprintf(&b, "\n### Misses at k=%d\n", largest)
	for _, o := range missed {
		fmt.Fprintf(&b, "\n**%s**\n\n", o.Query.Query)
		fmt.Fprintf(&b, "Wanted: %s\n\n", describeLabels(o.Query.Relevant))
		if len(o.Retrieved) == 0 {
			b.WriteString("Got: nothing\n")
			continue
		}
		b.WriteString("Got instead:\n\n")
		for i, r := range o.Retrieved {
			if i >= largest {
				break
			}
			fmt.Fprintf(&b, "%d. `%s` — %s\n", i+1, r.Source, firstLine(r.Content))
		}
	}
	return b.String()
}

func describeLabels(labels []Label) string {
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.MustContain == "" {
			parts = append(parts, "`"+l.Source+"`")
			continue
		}
		parts = append(parts, fmt.Sprintf("`%s` containing %q", l.Source, l.MustContain))
	}
	return strings.Join(parts, " or ")
}

// firstLine is the chunk's heading path, or its opening line when it has none,
// bounded so a report stays readable in a terminal.
func firstLine(content string) string {
	line, _, _ := strings.Cut(content, "\n")
	line = strings.TrimSpace(line)
	if len(line) > 90 {
		return line[:87] + "..."
	}
	return line
}

// LoadManifest reads the corpus manifest from dir.
func LoadManifest(dir string) (Manifest, error) {
	var m Manifest
	if err := readJSON(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return Manifest{}, err
	}
	if len(m.Documents) == 0 {
		return Manifest{}, fmt.Errorf("eval: the manifest in %s lists no documents", dir)
	}
	return m, nil
}

// LoadSet reads the evaluation set from dir.
func LoadSet(dir string) (Set, error) {
	var s Set
	if err := readJSON(filepath.Join(dir, "eval.json"), &s); err != nil {
		return Set{}, err
	}
	if len(s.Queries) == 0 {
		return Set{}, fmt.Errorf("eval: the set in %s has no queries", dir)
	}
	for i, q := range s.Queries {
		if strings.TrimSpace(q.Query) == "" {
			return Set{}, fmt.Errorf("eval: query %d has no text", i)
		}
		if len(q.Relevant) == 0 {
			return Set{}, fmt.Errorf("eval: query %q has no labels, so it can never be a hit", q.Query)
		}
	}
	return s, nil
}

func readJSON(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("eval: read %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("eval: parse %s: %w", path, err)
	}
	return nil
}
