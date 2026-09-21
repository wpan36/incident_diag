package ingest

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// ErrInvalidUTF8 is a permanent failure. Nothing downstream behaves sensibly on
// arbitrary bytes: not the markdown parser, not the token estimate over runes,
// and not a failure reason written into a utf8mb4 column.
var ErrInvalidUTF8 = errors.New("ingest: content is not valid UTF-8")

// section is a run of the document under one heading path.
//
// level is the heading's own level rather than len(path), because the two
// differ whenever a document skips a level — a "#" followed directly by a "###"
// has a path of length two and a level of three — and the level is what gets
// written back out when sections are merged.
//
// title is held separately rather than read back off the end of path, because
// a heading whose text is empty contributes nothing to the path and there
// would be nothing there to read.
type section struct {
	level int
	title string
	path  []string
	units []unit
}

// unit is one packing unit: a paragraph, a list item, a table or a fenced code
// block. It is the smallest thing chunking will separate, and the largest thing
// it will never split.
type unit struct {
	text string

	// tight marks a list item. Consecutive items are joined with a single
	// newline rather than a blank line, so a list that survives into one chunk
	// is still a list rather than double-spaced bullets.
	tight bool
}

// joinUnits renders units back into the text that will be embedded.
func joinUnits(us []unit) string {
	var b strings.Builder
	for i, u := range us {
		if i > 0 {
			if u.tight && us[i-1].tight {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(u.text)
	}
	return b.String()
}

// headingRef is one entry on the stack of open headings.
type headingRef struct {
	level int
	title string
}

// normalize prepares raw bytes for everything that follows.
//
// CRLF becomes LF so that a file authored on Windows produces the same chunks
// as the same file authored on Linux; without it every paragraph would carry a
// trailing carriage return into the embedded text. The BOM is stripped because
// it is invisible in an editor and would otherwise sit in front of the first
// "#", leaving the parser with no heading at all and the whole document
// degraded to a single untitled section.
func normalize(raw []byte) (string, error) {
	if !utf8.Valid(raw) {
		return "", ErrInvalidUTF8
	}
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	return strings.TrimPrefix(s, "\ufeff"), nil
}

// parseMarkdown splits a document into sections at every heading, at every
// level.
//
// The AST is walked rather than rendered, and each block node contributes its
// original source text rather than its flattened inline content: what is
// embedded should be what the author wrote, bullets, pipes and backticks
// included. A heading that has sub-headings still produces a section of its
// own, holding the text between it and its first child.
func parseMarkdown(src string) []section {
	source := []byte(src)
	doc := goldmark.New().Parser().Parse(text.NewReader(source))

	// The preamble: content before the first heading is a section with an empty
	// heading path, the same case a .txt file is.
	cur := section{}
	var sections []section
	var stack []headingRef

	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		h, ok := n.(*ast.Heading)
		if !ok {
			cur.units = append(cur.units, units(source, n)...)
			continue
		}

		sections = append(sections, cur)
		// Pop every heading at or below this one's level: a "##" closes the
		// previous "##" and everything nested under it.
		for len(stack) > 0 && stack[len(stack)-1].level >= h.Level {
			stack = stack[:len(stack)-1]
		}
		title := headingTitle(source, h)
		// Pushed even when the title is empty, because the stack is what makes
		// popping by level correct; it is the path that leaves it out.
		stack = append(stack, headingRef{level: h.Level, title: title})

		path := make([]string, 0, len(stack))
		for _, ref := range stack {
			// A heading whose text is empty — "#" followed by nothing but a
			// space, which is a typo — names nothing, so it contributes
			// nothing. Letting it through would put a leading " > " in front
			// of every path below it, and that path is both a keyword field
			// and the first line of the embedded text.
			if ref.title != "" {
				path = append(path, ref.title)
			}
		}
		cur = section{level: h.Level, title: title, path: path}
	}
	return append(sections, cur)
}

// parseText treats a .txt file as one section with an empty heading path, whose
// paragraphs are separated by blank lines.
//
// Dispatch happens on the stored format column rather than on the filename,
// because that column is what the API already derived and validated.
func parseText(src string) []section {
	var s section
	var para []string
	flush := func() {
		if len(para) > 0 {
			s.units = append(s.units, unit{text: strings.Join(para, "\n")})
			para = nil
		}
	}
	for _, line := range strings.Split(src, "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		para = append(para, strings.TrimRight(line, " \t"))
	}
	flush()
	return []section{s}
}

// units returns the packing units a block node contributes.
//
// A list contributes one unit per item, so a long list can be split between
// chunks at item boundaries. Everything else is one unit, which is what makes a
// fenced code block opaque — and what keeps a GFM table together, since without
// the table extension goldmark sees one as a single paragraph whose lines are
// the rows.
func units(source []byte, n ast.Node) []unit {
	if list, ok := n.(*ast.List); ok {
		var out []unit
		for item := list.FirstChild(); item != nil; item = item.NextSibling() {
			if text := rawText(source, item); text != "" {
				out = append(out, unit{text: text, tight: true})
			}
		}
		return out
	}
	if text := rawText(source, n); text != "" {
		return []unit{{text: text}}
	}
	return nil
}

// rawText returns a block node's original source.
//
// A node's own line segments cover only its content, so the span is expanded to
// whole lines: that is what recovers a list item's bullet and an indented code
// block's indentation. Fenced code blocks are the exception — their fences sit
// outside the content lines entirely — so they are reassembled explicitly, both
// because a retrieved chunk should still look like markdown and because the
// fence is what tells a reader the block was never a candidate for splitting.
func rawText(source []byte, n ast.Node) string {
	if fence, ok := n.(*ast.FencedCodeBlock); ok {
		var info string
		if fence.Info != nil {
			info = string(fence.Info.Segment.Value(source))
		}
		var body strings.Builder
		lines := fence.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			body.Write(seg.Value(source))
		}
		return "```" + info + "\n" + strings.TrimRight(body.String(), "\n") + "\n```"
	}

	start, stop, ok := blockSpan(n)
	if !ok {
		return ""
	}
	if i := bytes.LastIndexByte(source[:start], '\n'); i >= 0 {
		start = i + 1
	} else {
		start = 0
	}
	if i := bytes.IndexByte(source[stop:], '\n'); i >= 0 {
		stop += i
	} else {
		stop = len(source)
	}
	return strings.TrimRight(string(source[start:stop]), " \t\n")
}

// blockSpan returns the source offsets a node and its block descendants cover.
//
// Only block nodes are asked for their lines: goldmark's inline nodes panic on
// Lines rather than returning an empty set, so the type check is load-bearing
// rather than an optimization.
func blockSpan(n ast.Node) (start, stop int, ok bool) {
	start, stop = -1, -1
	_ = ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || node.Type() != ast.TypeBlock {
			return ast.WalkContinue, nil
		}
		lines := node.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			if start < 0 || seg.Start < start {
				start = seg.Start
			}
			if seg.Stop > stop {
				stop = seg.Stop
			}
		}
		return ast.WalkContinue, nil
	})
	return start, stop, start >= 0 && stop >= start
}

// headingTitle returns a heading's text without its markers.
func headingTitle(source []byte, h *ast.Heading) string {
	var b strings.Builder
	lines := h.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(source))
	}
	return strings.TrimSpace(b.String())
}
