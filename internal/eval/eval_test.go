package eval

import (
	"strings"
	"testing"
)

func chunk(source, content string) Retrieved { return Retrieved{Source: source, Content: content} }

func TestLabelMatching(t *testing.T) {
	cases := []struct {
		name  string
		label Label
		r     Retrieved
		want  bool
	}{
		{"source only", Label{Source: "a.md"}, chunk("a.md", "anything"), true},
		{"wrong source", Label{Source: "a.md"}, chunk("b.md", "anything"), false},
		{"phrase present", Label{Source: "a.md", MustContain: "pool"}, chunk("a.md", "the pool is full"), true},
		{"phrase absent", Label{Source: "a.md", MustContain: "pool"}, chunk("a.md", "the cpu is busy"), false},
		// The source has to match even when the phrase does: two runbooks
		// describing the same symptom is the whole point of the corpus, and a
		// label that matched either would score the confusable as a hit.
		{"phrase present, wrong source", Label{Source: "a.md", MustContain: "pool"}, chunk("b.md", "the pool is full"), false},
	}
	for _, c := range cases {
		if got := c.label.Matches(c.r); got != c.want {
			t.Errorf("%s: Matches = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestScoreFindsTheFirstRelevantRank(t *testing.T) {
	q := Query{
		Query:    "pool",
		Relevant: []Label{{Source: "a.md"}, {Source: "b.md"}},
	}
	got := Score(q, []Retrieved{chunk("c.md", ""), chunk("b.md", ""), chunk("a.md", "")})
	// Rank 2, not 3: any label satisfies the query, and the first one that
	// does is what k is measured against.
	if got.HitRank != 2 {
		t.Errorf("HitRank = %d, want 2", got.HitRank)
	}
}

func TestScoreReportsAMiss(t *testing.T) {
	q := Query{Query: "pool", Relevant: []Label{{Source: "a.md"}}}
	got := Score(q, []Retrieved{chunk("b.md", ""), chunk("c.md", "")})
	if got.HitRank != 0 {
		t.Errorf("HitRank = %d, want 0 for a miss", got.HitRank)
	}
}

func TestRecallAt(t *testing.T) {
	outcomes := []Outcome{
		{HitRank: 1},
		{HitRank: 3},
		{HitRank: 7},
		{HitRank: 0}, // never found
	}
	cases := map[int]float64{1: 0.25, 3: 0.5, 5: 0.5, 10: 0.75}
	for k, want := range cases {
		if got := RecallAt(outcomes, k); got != want {
			t.Errorf("RecallAt(%d) = %v, want %v", k, got, want)
		}
	}
}

func TestRecallOnAnEmptySetIsZero(t *testing.T) {
	// Not 1. An empty set has demonstrated nothing, and a perfect score for it
	// would let a harness that failed to load its labels report success.
	if got := RecallAt(nil, 5); got != 0 {
		t.Errorf("RecallAt on no outcomes = %v, want 0", got)
	}
}

func TestUnresolvedLabelsCatchesATypo(t *testing.T) {
	set := Set{Queries: []Query{
		{Query: "a", Relevant: []Label{{Source: "a.md", MustContain: "present"}}},
		{Query: "b", Relevant: []Label{{Source: "a.md", MustContain: "typoo"}}},
		{Query: "c", Relevant: []Label{{Source: "missing.md"}}},
	}}
	corpus := []Retrieved{chunk("a.md", "a phrase that is present")}

	got := UnresolvedLabels(set, corpus)
	if len(got) != 2 {
		t.Fatalf("got %d unresolved labels, want 2: %+v", len(got), got)
	}
	// Both failure modes matter: a phrase that no chunk contains, and a source
	// that is not in the corpus at all. Either silently makes its query
	// unanswerable, which reads as a retrieval regression rather than as a
	// broken label.
	if got[0].MustContain != "typoo" || got[1].Source != "missing.md" {
		t.Errorf("unresolved = %+v", got)
	}
}

func TestReportListsMissesWithWhatCameBackInstead(t *testing.T) {
	outcomes := []Outcome{
		{Query: Query{Query: "found"}, HitRank: 1},
		{
			Query: Query{Query: "missed", Relevant: []Label{{Source: "want.md", MustContain: "phrase"}}},
			Retrieved: []Retrieved{
				chunk("other.md", "Other Runbook > Symptoms\n\nbody"),
				chunk("another.md", "Another Runbook\n\nbody"),
			},
		},
	}
	report := Report(outcomes, []int{1, 3})

	for _, want := range []string{
		"| Recall@1 | 0.50 |",
		"| Recall@3 | 0.50 |",
		"| Queries | 2 |",
		"Misses at k=3",
		"**missed**",
		`containing "phrase"`,
		"Other Runbook > Symptoms",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	// A query that hit must not be listed as a miss.
	if strings.Contains(report, "**found**") {
		t.Errorf("report lists a hit among the misses:\n%s", report)
	}
}

func TestReportSaysSoWhenNothingMissed(t *testing.T) {
	report := Report([]Outcome{{HitRank: 1}}, []int{1, 3})
	if !strings.Contains(report, "No query missed") {
		t.Errorf("report does not say the set passed:\n%s", report)
	}
}

func TestReportListsQueriesAnsweredButNotFirst(t *testing.T) {
	// The case that matters once the corpus is small enough for the larger
	// cutoffs to saturate: nothing missed, but the rank of the first hit still
	// moved, and a report that only listed outright misses would say nothing
	// at all.
	outcomes := []Outcome{
		{Query: Query{Query: "answered first"}, HitRank: 1},
		{
			Query:     Query{Query: "answered third"},
			HitRank:   3,
			Retrieved: []Retrieved{chunk("confusable.md", "Confusable Runbook\n\nbody")},
		},
	}
	report := Report(outcomes, []int{1, 3, 5})

	for _, want := range []string{
		"| Recall@1 | 0.50 |",
		"| Recall@5 | 1.00 |",
		"Answered, but not first",
		"| 3 | answered third | `confusable.md` |",
		"No query missed at k=5",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "| 1 | answered first") {
		t.Errorf("a query answered first is listed as demoted:\n%s", report)
	}
}
