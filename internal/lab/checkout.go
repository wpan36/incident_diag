package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
)

const (
	// maxOrders bounds the in-memory order book. Orders are only kept so that
	// GET /orders/{id} has something to return, and a lab process that runs for
	// an afternoon should not grow without limit.
	maxOrders = 10_000

	// statusClientGone is nginx's convention for a client that disconnected
	// before the response. It is not an HTTP status anyone standardised, and it
	// is used here to keep a load generator giving up out of the 504 rate,
	// which is the signal the checkout runbook is about.
	statusClientGone = 499

	// idleConnsPerHost has to exceed the concurrency the lab runs at.
	// net/http's default is 2, and the connection churn that produces would
	// show up as latency this service invented rather than measured.
	idleConnsPerHost = 256
)

// CheckoutOptions configure checkout-service.
type CheckoutOptions struct {
	// PaymentURL is payment-service's base URL.
	PaymentURL string

	// PaymentTimeout bounds one call to payment-service.
	PaymentTimeout time.Duration
}

// Checkout is checkout-service: POST /orders, which calls payment-service, and
// GET /orders/{id}, which calls nothing.
type Checkout struct {
	opts    CheckoutOptions
	client  *http.Client
	metrics *checkoutMetrics
	logger  *slog.Logger
	orders  *orderBook
}

// NewCheckout registers checkout-service's metrics and routes on app.
func NewCheckout(app *App, opts CheckoutOptions) *Checkout {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = idleConnsPerHost
	transport.MaxIdleConnsPerHost = idleConnsPerHost

	c := &Checkout{
		opts: opts,
		// No Timeout on the client: the deadline is per request, so that the
		// time spent is the time this service chose to wait rather than
		// whatever was left of a shared budget.
		client:  &http.Client{Transport: transport},
		metrics: newCheckoutMetrics(app.Registry()),
		logger:  app.Logger(),
		orders:  newOrderBook(maxOrders),
	}
	app.Handle(Route{Method: http.MethodPost, Path: "/orders", Kind: RouteBusiness, Handler: c.createOrder})
	// The label is the corpus's spelling, not the mux pattern: an order id in a
	// metric label would be one time series per order.
	app.Handle(Route{Method: http.MethodGet, Path: "/orders/{id}", Label: "/orders/:id",
		Kind: RouteBusiness, Handler: c.getOrder})
	return c
}

type orderItem struct {
	SKU            string `json:"sku"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type orderRequest struct {
	CustomerID string      `json:"customer_id"`
	Items      []orderItem `json:"items"`
}

type order struct {
	ID              string      `json:"id"`
	CustomerID      string      `json:"customer_id"`
	Items           []orderItem `json:"items"`
	TotalCents      int64       `json:"total_cents"`
	AuthorizationID string      `json:"authorization_id"`
	CreatedAt       time.Time   `json:"created_at"`
}

func (c *Checkout) createOrder(w http.ResponseWriter, r *http.Request) {
	var req orderRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	total, err := validateBasket(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	orderID := id.New()
	auth, err := c.charge(r.Context(), orderID, total)
	if err != nil {
		c.failOrder(w, r, orderID, err)
		return
	}

	o := order{
		ID:              orderID,
		CustomerID:      req.CustomerID,
		Items:           req.Items,
		TotalCents:      total,
		AuthorizationID: auth,
		CreatedAt:       time.Now().UTC(),
	}
	c.orders.put(o)
	writeJSON(w, http.StatusCreated, o)
}

func (c *Checkout) getOrder(w http.ResponseWriter, r *http.Request) {
	// This route calls nothing downstream, which is the point of it. CPU
	// saturation slows it along with everything else; a slow payment-service
	// does not touch it at all, and that difference is what the runbooks tell a
	// responder to look for.
	o, ok := c.orders.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such order")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// errPaymentTimeout is a call this service abandoned rather than one that
// failed. The distinction is the whole of the checkout latency runbook.
var errPaymentTimeout = errors.New("payment-service did not answer within the client timeout")

// errClientGone is the caller disconnecting while the order was in flight.
var errClientGone = errors.New("the client went away")

// charge calls payment-service under the client timeout and returns the
// authorization id.
func (c *Checkout) charge(ctx context.Context, orderID string, totalCents int64) (string, error) {
	body, err := json.Marshal(chargeRequest{OrderID: orderID, AmountCents: totalCents, Currency: "USD"})
	if err != nil {
		return "", fmt.Errorf("encoding the charge request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, c.opts.PaymentTimeout)
	defer cancel()

	url := strings.TrimSuffix(c.opts.PaymentURL, "/") + "/charge"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building the charge request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.client.Do(req)
	c.metrics.clientLatency.Observe(time.Since(start).Seconds())

	if err != nil {
		if ctx.Err() != nil {
			return "", errClientGone
		}
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			// Counted here rather than at the 504: this is the number the
			// runbook compares against payment-service's request count to find
			// authorizations that succeeded on the other side.
			c.metrics.clientTimeouts.Inc()
			return "", errPaymentTimeout
		}
		return "", fmt.Errorf("calling payment-service: %w", err)
	}
	defer resp.Body.Close()

	// Bounded: payment-service is a dependency, and a dependency's response
	// size is not something this service should trust.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading payment-service's response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("payment-service answered %d", resp.StatusCode)
	}

	var charged chargeResponse
	if err := json.Unmarshal(raw, &charged); err != nil {
		return "", fmt.Errorf("payment-service's response is not valid JSON: %w", err)
	}
	return charged.AuthorizationID, nil
}

// failOrder maps a failed charge onto a status. The three cases are
// deliberately distinct, because they are three different incidents.
func (c *Checkout) failOrder(w http.ResponseWriter, r *http.Request, orderID string, err error) {
	switch {
	case errors.Is(err, errPaymentTimeout):
		// 504: this service gave up. The authorization may have succeeded, so
		// the customer may have been charged for an order that does not exist.
		c.logger.ErrorContext(r.Context(), "abandoned a charge at the client timeout",
			"order_id", orderID, "timeout", c.opts.PaymentTimeout.String())
		writeError(w, http.StatusGatewayTimeout, "payment-service did not answer in time")
	case errors.Is(err, errClientGone):
		c.logger.InfoContext(r.Context(), "the client went away before the order completed",
			"order_id", orderID)
		writeError(w, statusClientGone, "the client went away")
	default:
		c.logger.ErrorContext(r.Context(), "the charge failed", "order_id", orderID, "error", err.Error())
		writeError(w, http.StatusBadGateway, "payment-service could not authorize this order")
	}
}

// validateBasket returns the basket total, or the reason it is not a basket.
func validateBasket(req orderRequest) (int64, error) {
	if len(req.Items) == 0 {
		return 0, errors.New("items must contain at least one line")
	}
	var total int64
	for i, item := range req.Items {
		if item.SKU == "" {
			return 0, fmt.Errorf("items[%d].sku is required", i)
		}
		if item.Quantity < 1 {
			return 0, fmt.Errorf("items[%d].quantity must be at least 1", i)
		}
		if item.UnitPriceCents < 1 {
			return 0, fmt.Errorf("items[%d].unit_price_cents must be at least 1", i)
		}
		total += int64(item.Quantity) * item.UnitPriceCents
	}
	return total, nil
}

// orderBook is the in-memory order store: a map for lookup and a slice for
// eviction order.
type orderBook struct {
	mu    sync.Mutex
	byID  map[string]order
	order []string
	max   int
}

func newOrderBook(max int) *orderBook {
	return &orderBook{byID: make(map[string]order, max), max: max}
}

func (b *orderBook) put(o order) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.byID[o.ID] = o
	b.order = append(b.order, o.ID)
	for len(b.order) > b.max {
		delete(b.byID, b.order[0])
		b.order = b.order[1:]
	}
}

func (b *orderBook) get(orderID string) (order, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.byID[orderID]
	return o, ok
}
