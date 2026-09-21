package ingest

import (
	"errors"
	"strings"
	"testing"

	"github.com/wpan36/incident_diag/internal/store"
)

// defaults are the production numbers. Tests that are about a boundary set
// their own target instead, so that the fixture can stay a few lines long.
var defaults = ChunkOptions{TargetTokens: 400, MaxPerDocument: 2000}

func chunk(t *testing.T, format, src string, opts ChunkOptions) []Chunk {
	t.Helper()
	cs, err := ChunkDocument(format, []byte(src), opts)
	if err != nil {
		t.Fatalf("ChunkDocument: %v", err)
	}
	return cs
}

func paths(cs []Chunk) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.HeadingPath)
	}
	return out
}

func TestHeadingPathIsBuiltAtEveryLevel(t *testing.T) {
	// The "###" under a "#" with no "##" between them is the case that breaks a
	// chunker which assumes the path length equals the heading level.
	src := `# Payment Service

Owns card authorization.

## Latency

The p99 is the alert everyone sees first.

### Connection pool

Raise the pool size to 50.

# Checkout Service

Calls payment-service synchronously.
`
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	want := []string{
		"Payment Service",
		"Payment Service > Latency",
		"Payment Service > Latency > Connection pool",
		"Checkout Service",
	}
	got := paths(cs)
	if len(got) != len(want) {
		t.Fatalf("got %d chunks with paths %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("chunk %d path = %q, want %q", i, got[i], want[i])
		}
		if cs[i].Index != i {
			t.Errorf("chunk %d has Index %d: chunk_index is 0-based and follows document order", i, cs[i].Index)
		}
	}
}

func TestTheHeadingPathIsPartOfTheEmbeddedText(t *testing.T) {
	// "Raise the pool size to 50" is nearly useless as a retrieval unit once
	// separated from the heading naming the service and the symptom.
	src := "# Payment Service\n\n## Connection pool\n\nRaise the pool size to 50.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)

	last := cs[len(cs)-1]
	want := "Payment Service > Connection pool\n\nRaise the pool size to 50."
	if last.Content != want {
		t.Errorf("Content = %q, want %q", last.Content, want)
	}
	if last.Tokens != EstimateTokens(last.Content) {
		t.Errorf("Tokens = %d, want the estimate of the content, %d", last.Tokens, EstimateTokens(last.Content))
	}
}

func TestSiblingSectionsMergeUnderTheirParent(t *testing.T) {
	// A runbook of short "###" headings is the shape this rule exists for: one
	// chunk of forty tokens per heading would be one embedding call apiece,
	// none of them carrying enough context to be worth retrieving.
	src := `# Payment Service

Owns card authorization.

## Remediation

### Pool

Raise it to 50.

### Timeouts

Set them to 2s.

### Retries

Cap them at 3.
`
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	if len(cs) != 2 {
		t.Fatalf("got %d chunks with paths %q, want 2: the three siblings merge", len(cs), paths(cs))
	}

	merged := cs[1]
	if merged.HeadingPath != "Payment Service > Remediation" {
		t.Errorf("merged path = %q, want the shared parent path", merged.HeadingPath)
	}
	// Merging must not silently discard the names of the things it merged.
	for _, heading := range []string{"### Pool", "### Timeouts", "### Retries"} {
		if !strings.Contains(merged.Content, heading) {
			t.Errorf("merged content is missing %q:\n%s", heading, merged.Content)
		}
	}
}

func TestMergingStopsAtTheBudget(t *testing.T) {
	// Each section is about 18 estimated tokens; the parent path costs a few
	// more. With a target of 50 the first two fit together and the third does
	// not, so the run closes rather than overflowing.
	body := strings.Repeat("word ", 14)
	src := "# Service\n\n## Remediation\n\n### One\n\n" + body +
		"\n\n### Two\n\n" + body + "\n\n### Three\n\n" + body + "\n"

	cs := chunk(t, store.FormatMarkdown, src, ChunkOptions{TargetTokens: 50, MaxPerDocument: 2000})
	if len(cs) != 2 {
		t.Fatalf("got %d chunks, want 2", len(cs))
	}
	for _, c := range cs {
		if c.Tokens > 50 {
			t.Errorf("chunk %d is %d tokens, over the target of 50:\n%s", c.Index, c.Tokens, c.Content)
		}
	}
	if !strings.Contains(cs[0].Content, "### One") || !strings.Contains(cs[0].Content, "### Two") {
		t.Errorf("the first two sections should have merged:\n%s", cs[0].Content)
	}
	// The third is alone, so its own heading is in the path rather than in the
	// body.
	if cs[1].HeadingPath != "Service > Remediation > Three" {
		t.Errorf("path = %q, want the unmerged section's own path", cs[1].HeadingPath)
	}
	if strings.Contains(cs[1].Content, "### Three") {
		t.Errorf("an unmerged section must not repeat its heading in the body:\n%s", cs[1].Content)
	}
}

// TestTopLevelSiblingsMergeUnderAnEmptyPath is the shape the merge rule exists
// for and the one it used to miss: a runbook whose headings are all at the top
// level, with no title above them. Three chunks of six tokens each would be
// three embedding calls, none carrying enough context to be worth retrieving.
func TestTopLevelSiblingsMergeUnderAnEmptyPath(t *testing.T) {
	src := "## Symptom\n\nThe p99 spikes.\n\n## Check\n\nLook at the pool.\n\n## Fix\n\nRaise it to 50.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	if len(cs) != 1 {
		t.Fatalf("got %d chunks with paths %q, want the three siblings merged into 1", len(cs), paths(cs))
	}
	if cs[0].HeadingPath != "" {
		t.Errorf("path = %q, want empty: there is nothing above a top-level heading to name it", cs[0].HeadingPath)
	}
	// An empty path is only acceptable because the names survive in the body.
	for _, heading := range []string{"## Symptom", "## Check", "## Fix"} {
		if !strings.Contains(cs[0].Content, heading) {
			t.Errorf("merged content is missing %q:\n%s", heading, cs[0].Content)
		}
	}
	// No path means no prefix and no leading blank line.
	if !strings.HasPrefix(cs[0].Content, "## Symptom") {
		t.Errorf("content = %q, want the body alone", cs[0].Content)
	}
}

// TestAParentSectionDoesNotMergeWithItsChildren: the run stops at the first
// section of a different depth, so a document's introduction stays separate
// from the sections under it.
func TestAParentSectionDoesNotMergeWithItsChildren(t *testing.T) {
	src := "# Payment Service\n\nOwns card authorization.\n\n## Latency\n\nThe p99 spikes.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	want := []string{"Payment Service", "Payment Service > Latency"}
	if got := paths(cs); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

// TestThePreambleNeverMerges: content before the first heading has no heading
// line of its own, so joining it to the section after it would give one chunk
// in which nothing says where the preamble ended.
func TestThePreambleNeverMerges(t *testing.T) {
	src := "Intro with no heading.\n\n# Alpha\n\nShort.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	if got := paths(cs); len(got) != 2 || got[0] != "" || got[1] != "Alpha" {
		t.Fatalf("paths = %q, want [\"\" Alpha]", got)
	}
}

// TestAnEmptyHeadingContributesNothingToThePath: "#" followed by a space is a
// typo, and a path of " > Sub" would be both a keyword field and the first
// line of the embedded text.
func TestAnEmptyHeadingContributesNothingToThePath(t *testing.T) {
	cs := chunk(t, store.FormatMarkdown, "# \n\n## Sub\n\nBody here.\n", defaults)
	if len(cs) != 1 {
		t.Fatalf("got %d chunks with paths %q, want 1", len(cs), paths(cs))
	}
	if cs[0].HeadingPath != "Sub" {
		t.Errorf("path = %q, want %q with no leading separator", cs[0].HeadingPath, "Sub")
	}
	if !strings.HasPrefix(cs[0].Content, "Sub\n\n") {
		t.Errorf("content = %q, want it to start with the path", cs[0].Content)
	}
}

// TestAnIndentedCodeBlockKeepsItsIndentation: the four spaces are what make it
// code. A chunk that begins with one used to have its first line unindented,
// which silently breaks any snippet whose meaning is its layout.
func TestAnIndentedCodeBlockKeepsItsIndentation(t *testing.T) {
	src := "# Doc\n\n    first: value\n      nested: value\n\nAfter.\n"
	cs := chunk(t, store.FormatMarkdown, src, ChunkOptions{TargetTokens: 12, MaxPerDocument: 100})
	if len(cs) == 0 {
		t.Fatal("no chunks")
	}
	if !strings.Contains(cs[0].Content, "    first: value\n      nested: value") {
		t.Errorf("the code block lost its indentation:\n%q", cs[0].Content)
	}
}

// TestATableOverTheBudgetIsNeverSplit: a table is one packing unit, so half a
// table — a header with no rows, or rows with no header — can never be indexed
// as a chunk of its own.
func TestATableOverTheBudgetIsNeverSplit(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Doc\n\n## Limits\n\n| service | setting | value |\n| --- | --- | --- |\n")
	for i := 0; i < 30; i++ {
		b.WriteString("| payment-service | connection pool size | 50 |\n")
	}
	b.WriteString("\nAfter the table.\n")

	cs := chunk(t, store.FormatMarkdown, b.String(), ChunkOptions{TargetTokens: 40, MaxPerDocument: 100})
	if len(cs) != 2 {
		t.Fatalf("got %d chunks, want the table alone and the trailing paragraph", len(cs))
	}
	if cs[0].Tokens <= 40 {
		t.Errorf("the first chunk is %d tokens; this test is meant to produce an oversized one", cs[0].Tokens)
	}
	if !strings.Contains(cs[0].Content, "| --- | --- | --- |") ||
		strings.Count(cs[0].Content, "| payment-service |") != 30 {
		t.Errorf("the table was split:\n%s", cs[0].Content)
	}
}

func TestSectionsUnderDifferentParentsDoNotMerge(t *testing.T) {
	src := "# Doc\n\n## Alpha\n\n### One\n\nShort.\n\n## Beta\n\n### Two\n\nAlso short.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	want := []string{"Doc > Alpha > One", "Doc > Beta > Two"}
	if got := paths(cs); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("paths = %q, want %q: merging across a parent boundary would make the path a lie", got, want)
	}
}

func TestAHeadingWithNoBodyIsDropped(t *testing.T) {
	// Its chunk would be its own heading path and nothing else, which most
	// embedding APIs reject as empty.
	src := "# Service\n\n## Empty\n\n## Full\n\nSomething.\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	if got := paths(cs); len(got) != 1 || got[0] != "Service > Full" {
		t.Fatalf("paths = %q, want only [Service > Full]", got)
	}
}

func TestAnOverBudgetSectionIsPackedAtUnitBoundaries(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Service\n\n## Log\n\n")
	for i := 0; i < 6; i++ {
		b.WriteString(strings.Repeat("word ", 20) + "\n\n")
	}

	cs := chunk(t, store.FormatMarkdown, b.String(), ChunkOptions{TargetTokens: 60, MaxPerDocument: 2000})
	if len(cs) < 3 {
		t.Fatalf("got %d chunks, want the section split across several", len(cs))
	}
	for _, c := range cs {
		if c.HeadingPath != "Service > Log" {
			t.Errorf("chunk %d path = %q, want every packed chunk to keep the section's path", c.Index, c.HeadingPath)
		}
		// Every chunk repeats the prefix, which is why packing is measured
		// against the embedded text rather than the bare body.
		if !strings.HasPrefix(c.Content, "Service > Log\n\n") {
			t.Errorf("chunk %d does not carry the heading path:\n%s", c.Index, c.Content)
		}
		if c.Tokens > 60 {
			t.Errorf("chunk %d is %d tokens, over the target", c.Index, c.Tokens)
		}
	}
}

func TestASinglePackingUnitOverTheBudgetIsNeverSplit(t *testing.T) {
	paragraph := strings.Repeat("word ", 200)
	code := "```go\n" + strings.Repeat("// a line of code that goes on\n", 40) + "```"

	for name, body := range map[string]string{"paragraph": paragraph, "code block": code} {
		t.Run(name, func(t *testing.T) {
			src := "# Service\n\n## Detail\n\n" + body + "\n\nA short trailing paragraph.\n"
			cs := chunk(t, store.FormatMarkdown, src, ChunkOptions{TargetTokens: 60, MaxPerDocument: 2000})
			if len(cs) != 2 {
				t.Fatalf("got %d chunks, want the oversized unit alone and the short one after it", len(cs))
			}
			if cs[0].Tokens <= 60 {
				t.Errorf("the first chunk is %d tokens; this test is meant to produce an oversized one", cs[0].Tokens)
			}
			if !strings.Contains(cs[1].Content, "A short trailing paragraph.") {
				t.Errorf("the trailing paragraph should be its own chunk, got:\n%s", cs[1].Content)
			}
		})
	}
}

func TestADocumentWithNoHeadingsHasAnEmptyHeadingPath(t *testing.T) {
	src := "Just a paragraph.\n\nAnd another one.\n"
	for _, format := range []string{store.FormatMarkdown, store.FormatText} {
		cs := chunk(t, format, src, defaults)
		if len(cs) != 1 {
			t.Fatalf("%s: got %d chunks, want 1", format, len(cs))
		}
		if cs[0].HeadingPath != "" {
			t.Errorf("%s: path = %q, want empty", format, cs[0].HeadingPath)
		}
		// No path means no prefix and no leading blank line.
		if !strings.HasPrefix(cs[0].Content, "Just a paragraph.") {
			t.Errorf("%s: content = %q, want the body alone", format, cs[0].Content)
		}
	}
}

func TestTextParagraphsAreSeparatedByBlankLines(t *testing.T) {
	// A paragraph's own line breaks are kept; only blank lines separate units.
	src := "first line\nstill the first paragraph\n\n\nsecond paragraph\n"
	cs := chunk(t, store.FormatText, src, ChunkOptions{TargetTokens: 5, MaxPerDocument: 2000})
	if len(cs) != 2 {
		t.Fatalf("got %d chunks, want 2", len(cs))
	}
	if cs[0].Content != "first line\nstill the first paragraph" {
		t.Errorf("first chunk = %q", cs[0].Content)
	}
}

func TestCRLFProducesTheSameChunksAsLF(t *testing.T) {
	lf := "# Service\n\n## Latency\n\nRaise the pool size.\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	want := chunk(t, store.FormatMarkdown, lf, defaults)
	got := chunk(t, store.FormatMarkdown, crlf, defaults)
	if len(got) != len(want) {
		t.Fatalf("got %d chunks from CRLF, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Content != want[i].Content {
			t.Errorf("chunk %d = %q, want %q", i, got[i].Content, want[i].Content)
		}
	}
}

func TestALeadingBOMDoesNotHideTheFirstHeading(t *testing.T) {
	// Left in place, the BOM sits in front of the first "#" and the whole
	// document degrades to a single untitled section.
	cs := chunk(t, store.FormatMarkdown, "\ufeff# Service\n\nBody.\n", defaults)
	if len(cs) != 1 || cs[0].HeadingPath != "Service" {
		t.Fatalf("paths = %q, want [Service]", paths(cs))
	}
}

func TestInvalidUTF8IsAPermanentFailure(t *testing.T) {
	_, err := ChunkDocument(store.FormatMarkdown, []byte{'#', ' ', 0xff, 0xfe, '\n'}, defaults)
	if !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("err = %v, want ErrInvalidUTF8", err)
	}
}

func TestAnEmptyDocumentProducesNoChunks(t *testing.T) {
	for _, src := range []string{"", "   \n\n\t\n", "#\n"} {
		if _, err := ChunkDocument(store.FormatMarkdown, []byte(src), defaults); !errors.Is(err, ErrNoChunks) {
			t.Errorf("ChunkDocument(%q) err = %v, want ErrNoChunks", src, err)
		}
	}
}

func TestTheChunkCapIsAPermanentFailure(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString("paragraph " + strings.Repeat("x", 40) + "\n\n")
	}
	_, err := ChunkDocument(store.FormatText, []byte(b.String()), ChunkOptions{TargetTokens: 5, MaxPerDocument: 10})

	var tooMany *TooManyChunksError
	if !errors.As(err, &tooMany) {
		t.Fatalf("err = %v, want a TooManyChunksError", err)
	}
	if tooMany.Max != 10 || tooMany.Count <= 10 {
		t.Errorf("err = %v, want it to name a count above the limit of 10", tooMany)
	}
}

func TestAnUnknownFormatIsRejected(t *testing.T) {
	if _, err := ChunkDocument("pdf", []byte("body"), defaults); err == nil {
		t.Fatal("ChunkDocument accepted an unknown format")
	}
}

func TestListItemsAreSeparatePackingUnitsButStayALine(t *testing.T) {
	src := "# Service\n\n## Checklist\n\n- restart the pool\n- check the dashboard\n"
	cs := chunk(t, store.FormatMarkdown, src, defaults)
	if len(cs) != 1 {
		t.Fatalf("got %d chunks, want 1", len(cs))
	}
	if !strings.Contains(cs[0].Content, "- restart the pool\n- check the dashboard") {
		t.Errorf("a list that fits in one chunk should still read as a list:\n%s", cs[0].Content)
	}
}
