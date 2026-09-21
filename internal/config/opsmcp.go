package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// OpsMCP configures the tool server.
//
// Everything the agent can reach is in here. The tools take service names and
// resolve them against these maps, so a name that is not a key cannot become a
// request — which is what replaces path and URL validation rather than
// implementing it.
type OpsMCP struct {
	// PrometheusURL is the one endpoint prometheus_query talks to. It is not a
	// tool parameter, so the agent cannot point the query elsewhere.
	PrometheusURL string

	// ProbeTargets maps a service name to its base URL. http_probe joins its
	// path onto one of these and re-checks the resulting host.
	ProbeTargets map[string]string

	// LogRoot is the directory holding <service>.log.
	LogRoot string

	// LogServices lists which services may be read. Listed explicitly rather
	// than discovered by scanning LogRoot, because scanning is the same
	// file-discovery problem the no-paths decision exists to avoid.
	LogServices []string

	// Limits. Violations are refused rather than clamped: silently widening a
	// step changes the numbers the agent reasons about without telling it.
	PrometheusTimeout time.Duration
	MaxRange          time.Duration
	MinStep           time.Duration
	MaxSeries         int
	ProbeTimeout      time.Duration
	ProbeBodyBytes    int
	LogTimeout        time.Duration
	MaxLogLines       int
}

// Defaults. Every one of them is a guess; the spec says so and says they are
// revisited once there is real tool output to look at.
const (
	defaultPrometheusTimeout = 10 * time.Second
	defaultMaxRange          = 6 * time.Hour
	defaultMinStep           = 15 * time.Second
	defaultMaxSeries         = 50
	defaultProbeTimeout      = 5 * time.Second
	defaultProbeBodyBytes    = 2 << 10
	defaultLogTimeout        = 10 * time.Second
	defaultMaxLogLines       = 1000
)

// LoadOpsMCP reads the tool server's configuration, reporting every problem it
// finds at once.
func LoadOpsMCP() (OpsMCP, error) {
	var e env

	c := OpsMCP{
		PrometheusURL:     e.requiredString("OPS_MCP_PROMETHEUS_URL"),
		LogRoot:           e.requiredString("OPS_MCP_LOG_ROOT"),
		LogServices:       splitList(e.optionalString("OPS_MCP_LOG_SERVICES", "")),
		PrometheusTimeout: e.optionalDuration("OPS_MCP_PROMETHEUS_TIMEOUT", defaultPrometheusTimeout),
		MaxRange:          e.optionalDuration("OPS_MCP_MAX_RANGE", defaultMaxRange),
		MinStep:           e.optionalDuration("OPS_MCP_MIN_STEP", defaultMinStep),
		MaxSeries:         e.optionalInt("OPS_MCP_MAX_SERIES", defaultMaxSeries),
		ProbeTimeout:      e.optionalDuration("OPS_MCP_PROBE_TIMEOUT", defaultProbeTimeout),
		ProbeBodyBytes:    e.optionalInt("OPS_MCP_PROBE_BODY_BYTES", defaultProbeBodyBytes),
		LogTimeout:        e.optionalDuration("OPS_MCP_LOG_TIMEOUT", defaultLogTimeout),
		MaxLogLines:       e.optionalInt("OPS_MCP_MAX_LOG_LINES", defaultMaxLogLines),
	}
	c.ProbeTargets = parseTargets(&e, "OPS_MCP_PROBE_TARGETS", e.requiredString("OPS_MCP_PROBE_TARGETS"))

	if c.PrometheusURL != "" {
		if err := checkAbsoluteHTTP(c.PrometheusURL); err != nil {
			e.fail("OPS_MCP_PROMETHEUS_URL %v", err)
		}
	}
	if len(c.LogServices) == 0 {
		if _, set := lookup("OPS_MCP_LOG_SERVICES"); set {
			e.fail("OPS_MCP_LOG_SERVICES must name at least one service")
		} else {
			e.fail("OPS_MCP_LOG_SERVICES is required but not set")
		}
	}
	for _, n := range []struct {
		key string
		v   int
	}{
		{"OPS_MCP_MAX_SERIES", c.MaxSeries},
		{"OPS_MCP_PROBE_BODY_BYTES", c.ProbeBodyBytes},
		{"OPS_MCP_MAX_LOG_LINES", c.MaxLogLines},
	} {
		if n.v <= 0 {
			e.fail("%s must be greater than zero", n.key)
		}
	}
	for _, d := range []struct {
		key string
		v   time.Duration
	}{
		{"OPS_MCP_PROMETHEUS_TIMEOUT", c.PrometheusTimeout},
		{"OPS_MCP_MAX_RANGE", c.MaxRange},
		{"OPS_MCP_MIN_STEP", c.MinStep},
		{"OPS_MCP_PROBE_TIMEOUT", c.ProbeTimeout},
		{"OPS_MCP_LOG_TIMEOUT", c.LogTimeout},
	} {
		if d.v <= 0 {
			e.fail("%s must be greater than zero", d.key)
		}
	}
	if c.MinStep > 0 && c.MaxRange > 0 && c.MinStep > c.MaxRange {
		e.fail("OPS_MCP_MIN_STEP must not exceed OPS_MCP_MAX_RANGE")
	}

	if err := e.err(); err != nil {
		return OpsMCP{}, err
	}
	return c, nil
}

// parseTargets reads a name=url list.
//
// A malformed entry is a startup failure rather than a skipped target: a probe
// target that quietly went missing would look to the agent like a service that
// does not exist, which is a much harder thing to notice than a server that
// refused to start.
func parseTargets(e *env, key, raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range splitList(raw) {
		name, target, ok := strings.Cut(entry, "=")
		name, target = strings.TrimSpace(name), strings.TrimSpace(target)
		switch {
		case !ok || name == "" || target == "":
			e.fail("%s entry %q must be name=url", key, entry)
			continue
		case out[name] != "":
			e.fail("%s names %q twice", key, name)
			continue
		}
		if err := checkAbsoluteHTTP(target); err != nil {
			e.fail("%s target %q %v", key, name, err)
			continue
		}
		out[name] = target
	}
	if len(out) == 0 {
		e.fail("%s must name at least one target", key)
	}
	return out
}

// checkAbsoluteHTTP rejects anything http_probe could not safely join onto.
func checkAbsoluteHTTP(raw string) error {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("is not a URL: %w", err)
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("must be http or https, got %q", u.Scheme)
	case u.Host == "":
		return fmt.Errorf("must name a host")
	}
	return nil
}

// String renders the configuration for startup logging. The target and service
// names are included because "which services can the agent reach" is the first
// question anyone asks of this process.
func (c OpsMCP) String() string {
	names := make([]string, 0, len(c.ProbeTargets))
	for name := range c.ProbeTargets {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("prometheus=%s probe_targets=%s log_root=%s log_services=%s "+
		"max_range=%s min_step=%s max_series=%d probe_timeout=%s log_timeout=%s "+
		"max_log_lines=%d",
		c.PrometheusURL, strings.Join(names, ","), c.LogRoot,
		strings.Join(c.LogServices, ","), c.MaxRange, c.MinStep, c.MaxSeries,
		c.ProbeTimeout, c.LogTimeout, c.MaxLogLines)
}
