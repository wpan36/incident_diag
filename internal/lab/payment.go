package lab

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
)

// slowWaitThreshold is when queueing for a connection stops being normal and
// starts being worth a WARN. read_service_logs' min_level filter is only useful
// if the levels vary, and this is the line that varies first.
const slowWaitThreshold = time.Second

// PaymentOptions configure payment-service.
type PaymentOptions struct {
	// PoolSize is the number of concurrent connections to the card processor.
	PoolSize int

	// ProcessorLatency is one processor call before any injected delay.
	ProcessorLatency time.Duration
}

// Payment is payment-service: POST /charge and POST /refund, both through the
// processor connection pool.
type Payment struct {
	opts    PaymentOptions
	pool    *Pool
	metrics *paymentMetrics
	faults  *Controller
	logger  *slog.Logger
}

// NewPayment registers payment-service's metrics and routes on app.
func NewPayment(app *App, opts PaymentOptions) *Payment {
	m := newPaymentMetrics(app.Registry())
	p := &Payment{
		opts:    opts,
		pool:    NewPool(opts.PoolSize, m),
		metrics: m,
		faults:  app.Faults(),
		logger:  app.Logger(),
	}
	app.Handle(Route{Method: http.MethodPost, Path: "/charge", Kind: RouteBusiness, Handler: p.charge})
	app.Handle(Route{Method: http.MethodPost, Path: "/refund", Kind: RouteBusiness, Handler: p.refund})
	return p
}

type chargeRequest struct {
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

type chargeResponse struct {
	AuthorizationID    string `json:"authorization_id"`
	OrderID            string `json:"order_id"`
	Status             string `json:"status"`
	ProcessorLatencyMS int64  `json:"processor_latency_ms"`
	PoolWaitMS         int64  `json:"pool_wait_ms"`
}

func (p *Payment) charge(w http.ResponseWriter, r *http.Request) {
	var req chargeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.OrderID == "" {
		writeError(w, http.StatusBadRequest, "order_id is required")
		return
	}
	if req.AmountCents <= 0 {
		writeError(w, http.StatusBadRequest, "amount_cents must be greater than zero")
		return
	}

	spent, waited, ok := p.callProcessor(w, r, "charge")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, chargeResponse{
		AuthorizationID:    id.New(),
		OrderID:            req.OrderID,
		Status:             "authorized",
		ProcessorLatencyMS: spent.Milliseconds(),
		PoolWaitMS:         waited.Milliseconds(),
	})
}

type refundRequest struct {
	AuthorizationID string `json:"authorization_id"`
	AmountCents     int64  `json:"amount_cents"`
}

type refundResponse struct {
	RefundID           string `json:"refund_id"`
	Status             string `json:"status"`
	ProcessorLatencyMS int64  `json:"processor_latency_ms"`
}

func (p *Payment) refund(w http.ResponseWriter, r *http.Request) {
	var req refundRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.AuthorizationID == "" {
		writeError(w, http.StatusBadRequest, "authorization_id is required")
		return
	}

	// A refund is a processor call too, so it shares the pool. Nothing drives
	// this route today — no scenario sends refunds — so it is there because
	// the corpus says payment-service has it, not because the lab measures
	// anything through it.
	spent, _, ok := p.callProcessor(w, r, "refund")
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, refundResponse{
		RefundID:           id.New(),
		Status:             "refunded",
		ProcessorLatencyMS: spent.Milliseconds(),
	})
}

// callProcessor holds a pool connection for as long as the simulated processor
// takes. It answers 503 itself and reports false when no connection came free
// in time.
//
// The processor call is deliberately not cancellable. Once a connection is
// held the charge has been dispatched, so a caller that gives up does not stop
// it: the connection stays held for the full time, and the caller's timeout
// covers an authorization that actually succeeded. Both halves of that are in
// the corpus, and the second is what checkout_payment_client_timeouts_total
// exists to count. The pool wait, by contrast, is cancellable — a caller that
// leaves the queue frees it.
func (p *Payment) callProcessor(w http.ResponseWriter, r *http.Request, op string) (time.Duration, time.Duration, bool) {
	release, waited, err := p.pool.Acquire(r.Context())
	if err != nil {
		// 503, per the error rate runbook's reading: a pool refusing to hand
		// out a connection is the service protecting itself.
		p.logger.WarnContext(r.Context(), "no card processor connection came free",
			"op", op, "wait_ms", waited.Milliseconds(), "pool_size", p.pool.Size())
		writeError(w, http.StatusServiceUnavailable, "no card processor connection is available")
		return 0, waited, false
	}
	defer release()

	if waited > slowWaitThreshold {
		p.logger.WarnContext(r.Context(), "waited for a card processor connection",
			"op", op, "wait_ms", waited.Milliseconds(), "pool_size", p.pool.Size())
	}

	spent := p.opts.ProcessorLatency + p.faults.Delay()
	time.Sleep(spent)
	p.metrics.processorLatency.Observe(spent.Seconds())
	return spent, waited, true
}
