package config

import (
	"fmt"
	"net/url"
	"time"
)

// DefaultLabLogDir is where the Incident Lab services write their log files
// when nothing says otherwise. It is a path inside the container, mounted from
// ./data/lab-logs, and it is the directory ops-mcp later mounts read-only.
const DefaultLabLogDir = "/var/log/lab"

// Checkout is checkout-service's configuration.
//
// The variable names come from the knowledge corpus rather than from this
// package's conventions: the runbooks tell a responder to check
// CHECKOUT_PAYMENT_TIMEOUT, and a lab that calls it something else would make
// the retrieved runbook wrong.
type Checkout struct {
	// PaymentURL is payment-service's base URL. checkout-service has exactly
	// one synchronous dependency, so there is one of these.
	PaymentURL string

	// PaymentTimeout bounds one call to payment-service. Exceeding it is the
	// correctness case the corpus is about: the authorization may already have
	// succeeded on the other side, so checkout answers 504 for an order that
	// may have been charged.
	//
	// The default is the value the May 2026 postmortem says was in place before
	// someone lowered it below payment-service's published 800ms p99.
	PaymentTimeout time.Duration

	// LogDir holds <service>.log. Empty disables file logging, which is what a
	// developer running the binary directly usually wants.
	LogDir string
}

// LoadCheckout reads checkout-service's configuration from the environment,
// reporting every problem it finds at once.
func LoadCheckout() (Checkout, error) {
	var e env

	c := Checkout{
		PaymentURL:     e.requiredString("CHECKOUT_PAYMENT_URL"),
		PaymentTimeout: e.optionalDuration("CHECKOUT_PAYMENT_TIMEOUT", 2*time.Second),
		LogDir:         e.optionalString("LAB_LOG_DIR", DefaultLabLogDir),
	}

	if c.PaymentURL != "" {
		if u, err := url.Parse(c.PaymentURL); err != nil || u.Scheme == "" || u.Host == "" {
			e.fail("CHECKOUT_PAYMENT_URL must be an absolute URL such as http://payment-service:8080, got %q", c.PaymentURL)
		}
	}
	if c.PaymentTimeout <= 0 {
		e.fail("CHECKOUT_PAYMENT_TIMEOUT must be greater than zero")
	}

	if err := e.err(); err != nil {
		return Checkout{}, err
	}
	return c, nil
}

// String renders the configuration for startup logging.
func (c Checkout) String() string {
	return fmt.Sprintf("payment_url=%s payment_timeout=%s log_dir=%s",
		c.PaymentURL, c.PaymentTimeout, c.LogDir)
}

// Payment is payment-service's configuration.
type Payment struct {
	// PoolSize is the number of concurrent connections to the simulated card
	// processor. It is the saturable resource the whole lab is built around.
	//
	// The default is the March 2026 postmortem's value, the one that was left
	// in place through a scale-out.
	PoolSize int

	// ProcessorLatency is how long one call to the simulated processor takes
	// before any injected fault is added. It is read from a millisecond count,
	// PAYMENT_PROCESSOR_LATENCY_MS, because that is the name the corpus uses.
	ProcessorLatency time.Duration

	// LogDir holds <service>.log. Empty disables file logging.
	LogDir string
}

// LoadPayment reads payment-service's configuration from the environment,
// reporting every problem it finds at once.
func LoadPayment() (Payment, error) {
	var e env

	p := Payment{
		PoolSize:         e.optionalInt("PAYMENT_POOL_SIZE", 20),
		ProcessorLatency: time.Duration(e.optionalInt("PAYMENT_PROCESSOR_LATENCY_MS", 50)) * time.Millisecond,
		LogDir:           e.optionalString("LAB_LOG_DIR", DefaultLabLogDir),
	}

	if p.PoolSize < 1 {
		e.fail("PAYMENT_POOL_SIZE must be at least 1, got %d", p.PoolSize)
	}
	if p.ProcessorLatency < 0 {
		e.fail("PAYMENT_PROCESSOR_LATENCY_MS must not be negative")
	}

	if err := e.err(); err != nil {
		return Payment{}, err
	}
	return p, nil
}

// String renders the configuration for startup logging.
func (p Payment) String() string {
	return fmt.Sprintf("pool_size=%d processor_latency=%s log_dir=%s",
		p.PoolSize, p.ProcessorLatency, p.LogDir)
}
