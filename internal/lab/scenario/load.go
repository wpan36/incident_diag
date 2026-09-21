package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Route labels in the report. They match the labels the services put on
// http_request_duration_seconds, so a report and a PromQL query can be read
// side by side.
const (
	routeCreateOrder = "POST /orders"
	routeGetOrder    = "GET /orders/:id"
)

// basket is the order every request sends. It is constant: the point of the
// generator is to vary nothing except the fault.
var basket = []byte(`{"customer_id":"lab-scenario","items":[{"sku":"sku-1","quantity":2,"unit_price_cents":625}]}`)

// generator sends orders at a target rate.
//
// The rate is the design. A closed-loop generator — N workers, each sending the
// next request as soon as the last returns — offers concurrency N whatever the
// system does, so the pool is either saturated at baseline or can never
// saturate, and the fault makes no difference to the shape. Here concurrency
// follows from latency:
//
//	rps × hold_time = concurrent demand
//
// which is the relationship the whole lab is built to demonstrate.
type generator struct {
	client         *http.Client
	checkoutURL    string
	rps            float64
	maxInFlight    int
	requestTimeout time.Duration
}

// sample is one completed request.
type sample struct {
	route   string
	status  int
	latency time.Duration
}

// phaseStats is what one phase measured.
type phaseStats struct {
	name      string
	elapsed   time.Duration
	routes    map[string]*routeStats
	skipped   int // dispatches refused because the in-flight bound was reached
	sent      int
	poolInUse []float64
	before    snapshot
	after     snapshot
}

type routeStats struct {
	latencies []time.Duration
	statuses  map[int]int
}

func newPhaseStats(name string) *phaseStats {
	return &phaseStats{name: name, routes: map[string]*routeStats{}}
}

func (p *phaseStats) add(s sample) {
	r, ok := p.routes[s.route]
	if !ok {
		r = &routeStats{statuses: map[int]int{}}
		p.routes[s.route] = r
	}
	r.latencies = append(r.latencies, s.latency)
	r.statuses[s.status]++
}

// run sends requests for d, returning once every dispatched request has
// finished.
func (g *generator) run(ctx context.Context, name string, d time.Duration) *phaseStats {
	stats := newPhaseStats(name)

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		inFlight int
	)

	interval := time.Duration(float64(time.Second) / g.rps)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	deadline := time.After(d)
	start := time.Now()

loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-deadline:
			break loop
		case <-ticker.C:
		}

		mu.Lock()
		if inFlight >= g.maxInFlight {
			// Reported rather than silently dropped: reaching this bound means
			// the numbers under it understate the real demand, which changes
			// how the whole phase should be read.
			stats.skipped++
			mu.Unlock()
			continue
		}
		inFlight++
		stats.sent++
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			samples := g.order(ctx)
			mu.Lock()
			for _, s := range samples {
				stats.add(s)
			}
			inFlight--
			mu.Unlock()
		}()
	}

	wg.Wait()
	stats.elapsed = time.Since(start)
	return stats
}

// order sends one POST /orders and, if it was created, one GET /orders/{id}.
//
// The second call is the control: it touches nothing downstream, so a slow
// payment-service leaves it alone while CPU saturation slows it with everything
// else. That difference is what the runbooks tell a responder to look for.
func (g *generator) order(ctx context.Context) []sample {
	reqCtx, cancel := context.WithTimeout(ctx, g.requestTimeout)
	defer cancel()

	start := time.Now()
	status, body, err := g.do(reqCtx, http.MethodPost, g.checkoutURL+"/orders", basket)
	created := sample{route: routeCreateOrder, status: status, latency: time.Since(start)}
	if err != nil {
		// 0 means the request never got an answer: the generator's own timeout,
		// or a connection failure. It is reported as its own status so it is
		// never confused with a 504 the service chose to send.
		created.status = 0
	}
	samples := []sample{created}
	if created.status != http.StatusCreated {
		return samples
	}

	var order struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &order); err != nil || order.ID == "" {
		return samples
	}

	start = time.Now()
	status, _, err = g.do(reqCtx, http.MethodGet, g.checkoutURL+"/orders/"+order.ID, nil)
	fetched := sample{route: routeGetOrder, status: status, latency: time.Since(start)}
	if err != nil {
		fetched.status = 0
	}
	return append(samples, fetched)
}

func (g *generator) do(ctx context.Context, method, url string, body []byte) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, raw, err
}

// percentile returns the pth percentile of ds, using nearest-rank. The samples
// are sorted in place.
func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	rank := int(float64(len(ds))*p/100 + 0.999999)
	if rank < 1 {
		rank = 1
	}
	if rank > len(ds) {
		rank = len(ds)
	}
	return ds[rank-1]
}

// statusSummary renders the status counts of one route as "201×210 504×12",
// ordered by status so two phases line up.
func statusSummary(r *routeStats) string {
	codes := make([]int, 0, len(r.statuses))
	for code := range r.statuses {
		codes = append(codes, code)
	}
	sort.Ints(codes)

	var out []byte
	for i, code := range codes {
		if i > 0 {
			out = append(out, ' ')
		}
		label := strconv.Itoa(code)
		if code == 0 {
			// Not a status: the generator never got an answer.
			label = "none"
		}
		out = append(out, fmt.Sprintf("%s×%d", label, r.statuses[code])...)
	}
	return string(out)
}
