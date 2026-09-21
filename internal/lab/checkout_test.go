package lab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const oneItemBasket = `{"customer_id":"c-1","items":[{"sku":"sku-1","quantity":2,"unit_price_cents":625}]}`

// fakePayment stands in for payment-service.
func fakePayment(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

func authorizing(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, chargeResponse{AuthorizationID: "auth-1", Status: "authorized"})
}

func TestCreateOrderCharges(t *testing.T) {
	var charged chargeRequest
	url := fakePayment(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&charged); err != nil {
			t.Errorf("decoding the charge request: %v", err)
		}
		authorizing(w, r)
	})
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: url})

	rec := postTo(t, app.Handler(), t.Context(), "/orders", oneItemBasket)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /orders = %d: %s", rec.Code, rec.Body)
	}
	var created order
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if created.TotalCents != 1250 {
		t.Errorf("total = %d, want 1250", created.TotalCents)
	}
	if charged.AmountCents != 1250 || charged.OrderID != created.ID {
		t.Errorf("payment-service was asked for %+v, want the order's id and total", charged)
	}
	if n := histogramCount(t, app, MetricClientLatency); n != 1 {
		t.Errorf("%s has %d observations, want 1", MetricClientLatency, n)
	}

	// The order is readable afterwards from a route that calls nothing
	// downstream, which is the control the CPU runbook needs.
	rec = get(t, app.Handler(), "/orders/"+created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /orders/%s = %d", created.ID, rec.Code)
	}
	if got := requestCount(t, app, "/orders/:id", "200"); got != 1 {
		t.Errorf("/orders/:id observations = %d, want 1 under the pattern label", got)
	}
}

func TestGetUnknownOrder(t *testing.T) {
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: "http://payment.invalid"})
	if rec := get(t, app.Handler(), "/orders/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("GET /orders/nope = %d, want 404", rec.Code)
	}
}

func TestBasketValidation(t *testing.T) {
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: "http://payment.invalid"})
	h := app.Handler()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no items", `{"customer_id":"c-1","items":[]}`},
		{"no sku", `{"items":[{"quantity":1,"unit_price_cents":100}]}`},
		{"zero quantity", `{"items":[{"sku":"s","quantity":0,"unit_price_cents":100}]}`},
		{"free item", `{"items":[{"sku":"s","quantity":1,"unit_price_cents":0}]}`},
		{"unknown field", `{"items":[{"sku":"s","quantity":1,"unit_price_cents":1}],"gift":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postTo(t, h, t.Context(), "/orders", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /orders = %d, want 400", rec.Code)
			}
		})
	}
	// A refused basket must not have reached payment-service, which is why the
	// validation happens before the call rather than after it.
	if n := histogramCount(t, app, MetricClientLatency); n != 0 {
		t.Errorf("%s has %d observations, want none for a rejected basket", MetricClientLatency, n)
	}
}

func TestSlowPaymentServiceBecomesA504(t *testing.T) {
	url := fakePayment(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		authorizing(w, r)
	})
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: url, PaymentTimeout: 50 * time.Millisecond})

	rec := postTo(t, app.Handler(), t.Context(), "/orders", oneItemBasket)

	// 504 rather than 502: checkout gave up on something, it did not receive an
	// error. The whole checkout latency runbook rests on that distinction.
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("POST /orders = %d, want 504", rec.Code)
	}
	// Each increment is an authorization that may have succeeded on the other
	// side of a call this service abandoned.
	if got := counterValue(t, app, MetricClientTimeouts); got != 1 {
		t.Errorf("%s = %v, want 1", MetricClientTimeouts, got)
	}
}

func TestACallerThatGivesUpBecomesA499(t *testing.T) {
	url := fakePayment(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		authorizing(w, r)
	})
	// The client timeout is long enough that it cannot be what ends this call:
	// the only thing that does is the caller going away.
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: url, PaymentTimeout: 5 * time.Second})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	rec := postTo(t, app.Handler(), ctx, "/orders", oneItemBasket)

	// 499, not 504: a load generator that ran out of patience is not this
	// service abandoning a charge, and counting it as one would inflate the
	// rate the checkout runbook is about.
	if rec.Code != statusClientGone {
		t.Fatalf("POST /orders = %d, want %d for a caller that went away", rec.Code, statusClientGone)
	}
	if got := counterValue(t, app, MetricClientTimeouts); got != 0 {
		t.Errorf("%s = %v for a caller that went away, want 0", MetricClientTimeouts, got)
	}
	if got := requestCount(t, app, "/orders", "499"); got != 1 {
		t.Errorf("/orders 499 observations = %d, want 1", got)
	}
}

func TestFailingPaymentServiceBecomesA502(t *testing.T) {
	url := fakePayment(t, func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "no card processor connection is available")
	})
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: url})

	rec := postTo(t, app.Handler(), t.Context(), "/orders", oneItemBasket)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("POST /orders = %d, want 502", rec.Code)
	}
	// A downstream error is not a timeout, and counting it as one would make
	// the runbook's comparison against payment-service's request count
	// meaningless.
	if got := counterValue(t, app, MetricClientTimeouts); got != 0 {
		t.Errorf("%s = %v after a 503 from payment-service, want 0", MetricClientTimeouts, got)
	}
}

func TestUnreachablePaymentServiceBecomesA502(t *testing.T) {
	// Port 0 on the loopback interface refuses immediately, so this is a
	// connection failure rather than a slow one.
	app := newCheckoutForTest(t, CheckoutOptions{PaymentURL: "http://127.0.0.1:0", PaymentTimeout: time.Second})

	rec := postTo(t, app.Handler(), t.Context(), "/orders", oneItemBasket)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("POST /orders = %d, want 502", rec.Code)
	}
	if got := counterValue(t, app, MetricClientTimeouts); got != 0 {
		t.Errorf("%s = %v for a refused connection, want 0", MetricClientTimeouts, got)
	}
}

func TestOrderBookEvictsTheOldest(t *testing.T) {
	b := newOrderBook(2)
	b.put(order{ID: "a"})
	b.put(order{ID: "b"})
	b.put(order{ID: "c"})

	if _, ok := b.get("a"); ok {
		t.Error("the oldest order was not evicted")
	}
	for _, id := range []string{"b", "c"} {
		if _, ok := b.get(id); !ok {
			t.Errorf("order %s is missing", id)
		}
	}
}
