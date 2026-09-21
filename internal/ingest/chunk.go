package ingest

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/wpan36/incident_diag/internal/store"
)

// HeadingSeparator joins the levels of a heading path:
// "Payment Service > Latency > Connection pool".
const HeadingSeparator = " > "

// ErrNoChunks is a permanent failure. An empty or whitespace-only upload must
// not become a READY document with nothing behind it, because the retrieval
// evaluation would later have to unpick that.
var ErrNoChunks = errors.New("ingest: the document produced no chunks")

// TooManyChunksError is the other cap: one pathological upload must not be able
// to consume the whole embedding budget.
type TooManyChunksError struct {
	Count int
	Max   int
}

func (e *TooManyChunksError) Error() string {
	return fmt.Sprintf("ingest: the document produced %d chunks, more than the limit of %d", e.Count, e.Max)
}

// Chunk is one unit of retrievable text.
type Chunk struct {
	// Index is 0-based and follows document order.
	Index int

	// HeadingPath is the path this chunk sits under, or "" for a chunk with
	// nothing above it: a .txt file, markdown before the first heading, or a
	// run of top-level headings merged together. The headings of a merged run
	// are in the body, so an empty path is a missing keyword field rather than
	// missing context.
	HeadingPath string

	// Content is exactly the text that will be embedded and exactly the text
	// that will be stored, so what retrieval returns is what was scored
	// against. It is the heading path, a blank line, then the body: "raise the
	// pool size to 50" is nearly useless as a retrieval unit once separated
	// from the heading naming the service and the symptom.
	Content string

	// Tokens is the estimate Content was packed against. The caller logs the
	// ones over the target rather than this package doing it, which is what
	// keeps chunking a pure function.
	Tokens int
}

// ChunkOptions bounds the chunker.
type ChunkOptions struct {
	// TargetTokens is the budget one chunk is packed against. It is a target
	// rather than a ceiling: a single packing unit larger than this becomes an
	// oversized chunk rather than being split.
	TargetTokens int

	// MaxPerDocument caps how many chunks one document may produce.
	MaxPerDocument int
}

// ChunkDocument turns an uploaded file into chunks. It is pure: bytes in,
// chunks out, no I/O and no clock.
//
// format is the stored documents.format column, not the filename, because that
// column is what the API already derived and validated. An unrecognized value
// is an error rather than a silent fall back to markdown, so that adding a
// third format cannot quietly produce nonsense.
func ChunkDocument(format string, raw []byte, opts ChunkOptions) ([]Chunk, error) {
	src, err := normalize(raw)
	if err != nil {
		return nil, err
	}

	var sections []section
	switch format {
	case store.FormatMarkdown:
		sections = parseMarkdown(src)
	case store.FormatText:
		sections = parseText(src)
	default:
		return nil, fmt.Errorf("ingest: unsupported document format %q", format)
	}

	chunks := buildChunks(sections, opts.TargetTokens)
	switch {
	case len(chunks) == 0:
		return nil, ErrNoChunks
	case len(chunks) > opts.MaxPerDocument:
		return nil, &TooManyChunksError{Count: len(chunks), Max: opts.MaxPerDocument}
	}
	return chunks, nil
}

// buildChunks applies the three rules in order: merge consecutive siblings that
// fit, emit a section that fits on its own, pack one that does not.
func buildChunks(sections []section, target int) []Chunk {
	// A section with a heading and no body is dropped. It would otherwise
	// produce a chunk whose entire content is its own heading path, which most
	// embedding APIs reject as empty and which no retrieval result should ever
	// return. Dropping happens first, so two sections separated only by an
	// empty one are still siblings that can merge.
	sections = slices.DeleteFunc(slices.Clone(sections), func(s section) bool {
		return len(s.units) == 0
	})

	var out []Chunk
	for i := 0; i < len(sections); {
		if j := mergeRun(sections, i, target); j > i+1 {
			parent := path(sections[i].path[:len(sections[i].path)-1])
			out = append(out, newChunk(len(out), parent, mergedBody(sections[i:j])))
			i = j
			continue
		}

		s := sections[i]
		p := path(s.path)
		// Its heading is already in the path, so the body does not repeat it.
		body := joinUnits(s.units)
		if EstimateTokens(embedded(p, body)) <= target {
			out = append(out, newChunk(len(out), p, body))
		} else {
			out = append(out, packUnits(len(out), p, s.units, target)...)
		}
		i++
	}
	return out
}

// mergeRun returns the exclusive end of the run of sections starting at i that
// merge into one chunk. A return of i+1 means no merge.
//
// A runbook of a dozen two-line "###" headings would otherwise produce a dozen
// chunks of forty tokens each, one embedding call apiece, none of which carries
// enough context to be worth retrieving.
//
// Merging is only among siblings: two short sections under different parents
// stay separate, because a merged chunk's heading path would otherwise be a
// lie. Top-level headings do merge, under an empty heading path — the shape
// this rule exists for is a runbook of short "## Symptom" / "## Check" /
// "## Fix" headings with no title above them, and the merged body keeps every
// heading line, so the only thing an empty path costs is the keyword field.
// The preamble is the one section that never merges: it has no heading of its
// own to carry into the body, so a merge would join it to a named section
// without saying where it ended.
func mergeRun(sections []section, i, target int) int {
	first := sections[i]
	if len(first.path) == 0 {
		return i + 1
	}
	parent := path(first.path[:len(first.path)-1])

	body := render(first)
	if EstimateTokens(embedded(parent, body)) > target {
		return i + 1
	}

	j := i + 1
	for ; j < len(sections); j++ {
		next := sections[j]
		if len(next.path) != len(first.path) ||
			!slices.Equal(next.path[:len(next.path)-1], first.path[:len(first.path)-1]) {
			break
		}
		trial := body + "\n\n" + render(next)
		if EstimateTokens(embedded(parent, trial)) > target {
			break
		}
		body = trial
	}
	return j
}

// mergedBody renders the sections of a merge run.
//
// Each one keeps its own heading line in the body. Without that, merging
// silently discards the names of the things it merged, which is most of what
// made them findable.
func mergedBody(sections []section) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		parts = append(parts, render(s))
	}
	return strings.Join(parts, "\n\n")
}

// render writes a section back out with its heading line.
//
// A section whose heading text is empty is written out as its body alone:
// there is no name to preserve, and "#" on a line of its own is noise in the
// text that gets embedded.
func render(s section) string {
	body := joinUnits(s.units)
	if s.title == "" {
		return body
	}
	return strings.Repeat("#", s.level) + " " + s.title + "\n\n" + body
}

// packUnits greedily fills chunks at packing-unit boundaries.
//
// Any single unit over the budget — a paragraph, a list item, a table or a
// fenced code block — becomes an oversized chunk and is never split. One rule
// rather than one per node type: the target is far enough below the model's
// ceiling that an oversized chunk still embeds, and a sentence splitter
// accurate across English and Chinese punctuation is more machinery than the
// case is worth.
func packUnits(startIndex int, p string, units []unit, target int) []Chunk {
	var out []Chunk
	var cur []unit

	flush := func() {
		if len(cur) > 0 {
			out = append(out, newChunk(startIndex+len(out), p, joinUnits(cur)))
			cur = nil
		}
	}

	for _, u := range units {
		if len(cur) > 0 {
			trial := joinUnits(append(slices.Clone(cur), u))
			if EstimateTokens(embedded(p, trial)) <= target {
				cur = append(cur, u)
				continue
			}
			flush()
		}
		cur = []unit{u}
		// A unit that does not fit on its own will not fit with company
		// either, so it is emitted immediately rather than dragging the next
		// unit over the budget with it.
		if EstimateTokens(embedded(p, u.text)) > target {
			flush()
		}
	}
	flush()
	return out
}

func newChunk(index int, p, body string) Chunk {
	// Blank lines are trimmed, whitespace is not. An indented code block's
	// first line begins with the four spaces that make it code, and those are
	// exactly what strings.TrimSpace would take: parse.go expands every block
	// to whole lines to recover that indentation, and trimming it here would
	// hand the agent a snippet whose first line no longer lines up. Every
	// packing unit is already right-trimmed, so there is nothing else to take.
	content := embedded(p, strings.Trim(body, "\n"))
	return Chunk{Index: index, HeadingPath: p, Content: content, Tokens: EstimateTokens(content)}
}

// embedded is the text that is both embedded and stored. A chunk whose heading
// path is empty is the body alone, with no leading blank line.
func embedded(p, body string) string {
	if p == "" {
		return body
	}
	return p + "\n\n" + body
}

func path(titles []string) string { return strings.Join(titles, HeadingSeparator) }
