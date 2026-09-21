package lab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// postTo sends one request through the mux, with ctx on the request.
func postTo(t *testing.T, h http.Handler, ctx context.Context, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChargeAuthorizes(t *testing.T) {
	app, _ := newPaymentForTest(t, PaymentOptions{PoolSize: 2, ProcessorLatency: 5 * time.Millisecond})

	rec := postTo(t, app.Handler(), t.Context(), "/charge", `{"order_id":"o-1","amount_cents":1250,"currency":"USD"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /charge = %d: %s", rec.Code, rec.Body)
	}
	var got chargeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if got.AuthorizationID == "" || got.Status != "authorized" {
		t.Errorf("response = %+v", got)
	}
	if n := histogramCount(t, app, MetricProcessorLatency); n != 1 {
		t.Errorf("%s has %d observations, want 1", MetricProcessorLatency, n)
	}
	// The connection has to be back in the pool once the handler returns, or
	// the pool leaks one connection per request and saturates on its own.
	if inUse := gaugeValue(t, app, MetricPoolInUse); inUse != 0 {
		t.Errorf("%s = %v after the request, want 0", MetricPoolInUse, inUse)
	}
}

func TestChargeValidation(t *testing.T) {
	app, _ := newPaymentForTest(t, PaymentOptions{})
	h := app.Handler()

	for _, tc := range []struct {
		name string
		body string
	}{
		{"no order id", `{"amount_cents":100}`},
		{"zero amount", `{"order_id":"o-1","amount_cents":0}`},
		{"negative amount", `{"order_id":"o-1","amount_cents":-1}`},
		{"unknown field", `{"order_id":"o-1","amount_cents":100,"tip":1}`},
		{"not json", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := postTo(t, h, t.Context(), "/charge", tc.body); rec.Code != http.StatusBadRequest {
				t.Errorf("POST /charge = %d, want 400", rec.Code)
			}
		})
	}
}

func TestChargeAnswers503WhenNoConnectionComesFree(t *testing.T) {
	app, payment := newPaymentForTest(t, PaymentOptions{PoolSize: 1, ProcessorLatency: time.Millisecond})

	// Hold the only connection, so the request has to queue and then give up.
	release, _, err := payment.pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("Acquire(): %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	rec := postTo(t, app.Handler(), ctx, "/charge", `{"order_id":"o-1","amount_cents":100}`)

	// 503, per the error rate runbook: a pool refusing to hand out a connection
	// is the service protecting itself, not an unhandled failure.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /charge with a full pool = %d, want 503", rec.Code)
	}
}

func TestLatencyFaultIsAddedInsideThePool(t *testing.T) {
	app, payment := newPaymentForTest(t, PaymentOptions{PoolSize: 1, ProcessorLatency: 0})
	if err := app.Faults().SetLatency(LatencyFault{Enabled: true, DelayMS: 150}); err != nil {
		t.Fatalf("SetLatency(): %v", err)
	}

	// While the faulted request is in flight the pool must show as in use. That
	// is the whole causal chain: a slower processor holds connections longer,
	// which is what fills the pool.
	done := make(chan struct{})
	go func() {
		defer close(done)
		postTo(t, app.Handler(), context.Background(), "/charge", `{"order_id":"o-1","amount_cents":100}`)
	}()

	time.Sleep(60 * time.Millisecond)
	if inUse := gaugeValue(t, app, MetricPoolInUse); inUse != 1 {
		t.Errorf("%s = %v while a delayed charge is in flight, want 1", MetricPoolInUse, inUse)
	}
	_ = payment

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the charge never finished")
	}
}

func TestTheProcessorCallIsNotCancellable(t *testing.T) {
	app, _ := newPaymentForTest(t, PaymentOptions{PoolSize: 1, ProcessorLatency: 200 * time.Millisecond})

	// The caller gives up almost immediately. The charge still completes,
	// because the authorization has been dispatched: this is the correctness
	// case checkout_payment_client_timeouts_total counts, and it is also why
	// the pool stays saturated instead of draining as callers time out.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	postTo(t, app.Handler(), ctx, "/charge", `{"order_id":"o-1","amount_cents":100}`)
	elapsed := time.Since(start)

	if elapsed < 150*time.Millisecond {
		t.Errorf("the handler returned after %v, want it to hold the connection for the full 200ms", elapsed)
	}
	if n := histogramCount(t, app, MetricProcessorLatency); n != 1 {
		t.Errorf("%s has %d observations, want the abandoned charge to still be measured", MetricProcessorLatency, n)
	}
}

func TestRefundUsesThePoolToo(t *testing.T) {
	app, _ := newPaymentForTest(t, PaymentOptions{PoolSize: 2, ProcessorLatency: time.Millisecond})

	rec := postTo(t, app.Handler(), t.Context(), "/refund", `{"authorization_id":"a-1","amount_cents":100}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /refund = %d: %s", rec.Code, rec.Body)
	}
	if n := histogramCount(t, app, MetricProcessorLatency); n != 1 {
		t.Errorf("%s has %d observations, want the refund to be a processor call", MetricProcessorLatency, n)
	}
	if rec := postTo(t, app.Handler(), t.Context(), "/refund", `{"amount_cents":100}`); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /refund without an authorization id = %d, want 400", rec.Code)
	}
}
