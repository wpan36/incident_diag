package config

import (
	"fmt"
	"time"
)

// HTTPServer is the timeout configuration for the HTTP server.
//
// Like Database it is loaded on its own rather than being folded into Config,
// because only cmd/api reads it. A worker has no HTTP server to configure.
type HTTPServer struct {
	// ReadHeaderTimeout bounds how long a client may take to send its request
	// headers. It is the defence against a connection that opens and then says
	// nothing, which is why it is much shorter than ReadTimeout.
	ReadHeaderTimeout time.Duration

	// ReadTimeout bounds reading the whole request, body included. The
	// document upload in M5 is the largest thing a client sends, so this has
	// to be generous enough for a slow connection to finish one.
	ReadTimeout time.Duration

	// WriteTimeout bounds writing the response.
	//
	// The SSE endpoint in M26 holds a response open for as long as an
	// investigation runs, which is longer than any value that makes sense
	// here. That endpoint will need its own handling rather than a server-wide
	// timeout raised until it stops mattering.
	WriteTimeout time.Duration

	// IdleTimeout bounds how long a kept-alive connection may sit unused.
	IdleTimeout time.Duration

	// ShutdownTimeout is the budget for draining in-flight requests and closing
	// resources after a termination signal.
	ShutdownTimeout time.Duration
}

// LoadHTTPServer reads the HTTP server configuration from the environment,
// reporting every problem it finds at once.
func LoadHTTPServer() (HTTPServer, error) {
	var e env

	s := HTTPServer{
		ReadHeaderTimeout: e.optionalDuration("HTTP_READ_HEADER_TIMEOUT", 5*time.Second),
		ReadTimeout:       e.optionalDuration("HTTP_READ_TIMEOUT", 30*time.Second),
		WriteTimeout:      e.optionalDuration("HTTP_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:       e.optionalDuration("HTTP_IDLE_TIMEOUT", 120*time.Second),
		ShutdownTimeout:   e.optionalDuration("HTTP_SHUTDOWN_TIMEOUT", 15*time.Second),
	}

	if err := e.err(); err != nil {
		return HTTPServer{}, err
	}
	return s, nil
}

// String renders the configuration for startup logging.
func (s HTTPServer) String() string {
	return fmt.Sprintf("read_header_timeout=%s read_timeout=%s write_timeout=%s idle_timeout=%s shutdown_timeout=%s",
		s.ReadHeaderTimeout, s.ReadTimeout, s.WriteTimeout, s.IdleTimeout, s.ShutdownTimeout)
}
