package opsmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
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
// otherwise present a single unterminated "line" the size of the disk.
const maxLogLineBytes = 1 << 20

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
// avoid. The ring below means memory stays bounded by limit however large the
// file grows.
func scanLog(ctx context.Context, r interface{ Read([]byte) (int, error) }, f logFilter) (kept []record, matched, unparseable int, err error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLineBytes)

	lines := 0
	for sc.Scan() {
		// Checked periodically rather than per line: a cancelled call should
		// stop, but a syscall-free check on every line of a large file is
		// measurable.
		if lines++; lines%1000 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}

		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		rec, ok := parseRecord(line)
		if !ok {
			unparseable++
			continue
		}
		if !f.matches(rec, line) {
			continue
		}
		matched++
		kept = append(kept, rec)
		if len(kept) > f.limit {
			// Keep the tail: debugging wants the most recent records, and this
			// bounds memory by limit rather than by file size.
			kept = kept[1:]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, err
	}
	return kept, matched, unparseable, nil
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

func levelNames() string {
	names := make([]string, 0, len(levels))
	for name := range levels {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return levels[names[i]] < levels[names[j]] })
	return strings.Join(names, ", ")
}
