package opsmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultLogLimit is how many records come back when the caller does not say.
const defaultLogLimit = 200

// levels are ordered so min_level can be compared numerically.
var levels = map[string]int{"DEBUG": 0, "INFO": 1, "WARN": 2, "ERROR": 3}

// maxLogLineBytes bounds one line. A log file with a corrupt tail could
// otherwise present a single unterminated "line" the size of the disk. A line
// over the bound is counted as unparseable, not fatal — see readLine.
const maxLogLineBytes = 1 << 20

// readBufferBytes is the reader's window. Lines longer than this still read
// correctly, in several passes.
const readBufferBytes = 64 << 10

// ReadServiceLogsArgs is the tool's input.
type ReadServiceLogsArgs struct {
	Service  string `json:"service" jsonschema:"one of the services this server can read logs for"`
	Since    string `json:"since,omitempty" jsonschema:"earliest record to return, RFC 3339 UTC"`
	Until    string `json:"until,omitempty" jsonschema:"latest record to return, RFC 3339 UTC"`
	MinLevel string `json:"min_level,omitempty" jsonschema:"DEBUG, INFO, WARN or ERROR; defaults to INFO"`
	Contains string `json:"contains,omitempty" jsonschema:"case-insensitive substring the raw line must contain"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many records to return, newest kept; defaults to 200"`
}

func (s *Server) readServiceLogs(ctx context.Context, _ *mcp.CallToolRequest, in ReadServiceLogsArgs) (*mcp.CallToolResult, Meta, error) {
	if !slices.Contains(s.cfg.LogServices, in.Service) {
		res, meta := unknownName("service", in.Service, s.cfg.LogServices)
		return res, meta, nil
	}

	filter, refusal := s.buildFilter(in)
	if refusal != "" {
		res, meta := refused("%s", refusal)
		return res, meta, nil
	}

	// The other two tools bound their dependency; this one bounds its own work.
	// The file is read from the start every call and grows without rotation, so
	// the scan is what can run long here.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.LogTimeout)
	defer cancel()

	// filepath.Join of the root and a name that is a key of the configured
	// list. The name never came from the caller as a path, so there is nothing
	// here for a traversal to hide in.
	path := filepath.Join(s.cfg.LogRoot, in.Service+".log")
	f, err := os.Open(path)
	if err != nil {
		res, meta := failed(KindError, "could not read %s's log: %v", in.Service, err)
		return res, meta, nil
	}
	defer f.Close()

	records, matched, unparseable, err := scanLog(ctx, f, filter)
	if err != nil {
		kind := KindError
		if ctx.Err() != nil {
			kind = KindTimeout
		}
		res, meta := failed(kind, "reading %s's log: %v", in.Service, err)
		return res, meta, nil
	}

	text := renderRecords(records, in.Service, matched, unparseable)
	res, meta := ok(text, Meta{
		Matched:          matched,
		Returned:         len(records),
		UnparseableLines: unparseable,
	})
	return res, meta, nil
}

// logFilter is the validated form of the tool's arguments.
type logFilter struct {
	since, until time.Time
	minLevel     int
	contains     string
	limit        int
}

func (s *Server) buildFilter(in ReadServiceLogsArgs) (logFilter, string) {
	f := logFilter{minLevel: levels["INFO"], limit: defaultLogLimit}

	if in.Since != "" {
		t, err := parseTime("since", in.Since)
		if err != nil {
			return f, err.Error()
		}
		f.since = t
	}
	if in.Until != "" {
		t, err := parseTime("until", in.Until)
		if err != nil {
			return f, err.Error()
		}
		f.until = t
	}
	if !f.since.IsZero() && !f.until.IsZero() && !f.until.After(f.since) {
		return f, "until must be after since"
	}

	if in.MinLevel != "" {
		lvl, known := levels[strings.ToUpper(in.MinLevel)]
		if !known {
			return f, fmt.Sprintf("min_level must be one of %s, got %q", levelNames(), in.MinLevel)
		}
		f.minLevel = lvl
	}

	f.contains = strings.ToLower(in.Contains)

	if in.Limit != 0 {
		if in.Limit < 1 || in.Limit > s.cfg.MaxLogLines {
			// Refused rather than clamped, so a caller that asked for 5000 knows
			// it did not get 5000.
			return f, fmt.Sprintf("limit must be between 1 and %d, got %d", s.cfg.MaxLogLines, in.Limit)
		}
		f.limit = in.Limit
	}
	return f, ""
}

// record is one parsed log line.
type record struct {
	At    time.Time
	Level string
	Msg   string
	Attrs map[string]any
}

// scanLog reads the whole file and keeps the last limit matching records.
//
// The whole file, because there is no rotation and no index: this is a lab, and
// the alternative is the file-discovery problem the no-paths decision exists to
// avoid. The sliding window below means memory stays bounded by limit however
// large the file grows.
func scanLog(ctx context.Context, r io.Reader, f logFilter) (kept []record, matched, unparseable int, err error) {
	br := bufio.NewReaderSize(r, readBufferBytes)

	for lines := 0; ; lines++ {
		// Checked periodically rather than per line: a cancelled call should
		// stop, but a syscall-free check on every line of a large file is
		// measurable.
		if lines%1000 == 0 && lines > 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}

		line, tooLong, err := readLine(br)
		switch {
		case tooLong:
			unparseable++
		case len(line) == 0:
		default:
			rec, parsed := parseRecord(line)
			switch {
			case !parsed:
				unparseable++
			case f.matches(rec, line):
				matched++
				kept = append(kept, rec)
				if len(kept) > f.limit {
					// Keep the tail: debugging wants the most recent records,
					// and this bounds memory by limit rather than by file size.
					kept = kept[1:]
				}
			}
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				return kept, matched, unparseable, nil
			}
			return nil, 0, 0, err
		}
	}
}

// readLine returns the next line with its terminator removed, reporting a line
// longer than maxLogLineBytes instead of returning it.
//
// It is hand-rolled rather than a bufio.Scanner because a Scanner cannot
// continue past a line longer than its buffer: one corrupt line would cost
// every record in the file, and with no rotation that service's log would stay
// unreadable. An over-long line is discarded and counted like any other line
// that could not be parsed.
func readLine(br *bufio.Reader) (line []byte, tooLong bool, err error) {
	chunk, err := br.ReadSlice('\n')
	if !errors.Is(err, bufio.ErrBufferFull) {
		// The whole line fit, which is every line in a healthy file. No copy.
		return trimEOL(chunk), false, err
	}

	buf := append([]byte(nil), chunk...)
	for {
		chunk, err = br.ReadSlice('\n')
		if len(buf)+len(chunk) > maxLogLineBytes {
			tooLong = true
		} else {
			buf = append(buf, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, true, err
		}
		return trimEOL(buf), false, err
	}
}

func trimEOL(line []byte) []byte {
	return bytes.TrimRight(line, "\r\n")
}

func (f logFilter) matches(rec record, raw []byte) bool {
	switch {
	case levels[rec.Level] < f.minLevel:
		return false
	case !f.since.IsZero() && rec.At.Before(f.since):
		return false
	case !f.until.IsZero() && !rec.At.Before(f.until):
		return false
	}
	// Matched against the raw line, so an attribute the renderer does not print
	// is still searchable.
	return f.contains == "" || strings.Contains(strings.ToLower(string(raw)), f.contains)
}

// parseRecord reads one line of the format internal/log emits.
//
// A line missing time, level or msg is unparseable rather than partly used: a
// record with a zero timestamp would silently pass or fail the time filter, and
// a wrong answer about when something happened is worse than a counted line.
func parseRecord(line []byte) (record, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return record{}, false
	}

	at, ok := raw["time"].(string)
	if !ok {
		return record{}, false
	}
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return record{}, false
	}
	level, ok := raw["level"].(string)
	if !ok {
		return record{}, false
	}
	if _, known := levels[level]; !known {
		return record{}, false
	}
	msg, ok := raw["msg"].(string)
	if !ok {
		return record{}, false
	}

	delete(raw, "time")
	delete(raw, "level")
	delete(raw, "msg")
	return record{At: t.UTC(), Level: level, Msg: msg, Attrs: raw}, true
}

// renderRecords prints one line per record as "time level msg key=value", which
// is markedly cheaper in tokens than re-emitting the JSON.
func renderRecords(records []record, service string, matched, unparseable int) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s: %d matching records", service, matched)
	if len(records) < matched {
		fmt.Fprintf(&b, ", showing the last %d", len(records))
	}
	if unparseable > 0 {
		// Reported, never silently dropped: a sudden count is itself a signal.
		fmt.Fprintf(&b, "; %d unparseable lines skipped", unparseable)
	}
	b.WriteString("\n")

	if len(records) == 0 {
		b.WriteString("\n(no records matched)")
		return b.String()
	}

	for _, rec := range records {
		fmt.Fprintf(&b, "\n%s %s %s", rec.At.Format(timeLayout), rec.Level, rec.Msg)
		for _, k := range sortedKeys(rec.Attrs) {
			fmt.Fprintf(&b, " %s=%v", k, rec.Attrs[k])
		}
	}
	return b.String()
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// levelNamesOrdered lists the levels from least to most severe. Both the
// refusal message and the tool's schema enum are built from it, so the two
// cannot come to disagree about which levels exist.
func levelNamesOrdered() []string {
	names := make([]string, 0, len(levels))
	for name := range levels {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return levels[names[i]] < levels[names[j]] })
	return names
}

func levelNames() string { return strings.Join(levelNamesOrdered(), ", ") }
