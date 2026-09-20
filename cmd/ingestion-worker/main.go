// Command ingestion-worker consumes documents.ingest.v1 and drives each
// document through the ingestion state machine.
//
// It also owns the reconciler, which is why this binary exists at M6 rather
// than at M10: the sweep that re-enqueues stuck rows has to run somewhere, and
// the process that consumes the work is the process that should look for work
// that never arrived.
//
// The handler itself is a placeholder until M10. Everything around it — topic
// creation, the consumer group, the claim, the terminal write, the sweep — is
// real, and is what M6 set out to build.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/ingest"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/reconcile"
	"github.com/wpan36/incident_diag/internal/shutdown"
	"github.com/wpan36/incident_diag/internal/store"
)

const (
	// connectTimeout bounds the first connection to MySQL, so a database that
	// accepts TCP but never answers is a startup failure rather than a hang.
	connectTimeout = 10 * time.Second

	// topicTimeout bounds topic creation. It is generous because a broker that
	// has just started may still be electing its controller.
	topicTimeout = 30 * time.Second

	// shutdownTimeout is the budget for leaving the consumer group and closing
	// everything. It matches the HTTP server's default, since both are sized by
	// the same thing: how long an orchestrator waits after SIGTERM.
	shutdownTimeout = 15 * time.Second

	// goroutineTimeout bounds the wait for the consumer and reconciler
	// goroutines to notice cancellation. It is part of the shutdown budget, not
	// additional to it.
	goroutineTimeout = 5 * time.Second
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Every loader is called before any error is returned, so an operator with
	// problems in two of them is told about both at once.
	cfg, cfgErr := config.Load()
	dbCfg, dbErr := config.LoadDatabase()
	kafkaCfg, kafkaErr := config.LoadKafka()
	recCfg, recErr := config.LoadReconcile()
	if err := errors.Join(cfgErr, dbErr, kafkaErr, recErr); err != nil {
		return err
	}

	logger := log.New(os.Stdout, cfg.LogLevel)
	logger.Info("starting ingestion worker", "config", cfg.String(), "database", dbCfg.String(),
		"kafka", kafkaCfg.String(), "reconcile", recCfg.String())

	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	var closers shutdown.Group

	connectCtx, cancelConnect := context.WithTimeout(ctx, connectTimeout)
	defer cancelConnect()
	st, err := store.Open(connectCtx, dbCfg)
	if err != nil {
		return err
	}
	closers.Add("mysql", func(context.Context) error { return st.Close() })

	// Both topics, including the one M25 will consume, are created here: one
	// place doing it means two processes cannot race, and a worker that starts
	// before the API means the API's first produce has somewhere to go.
	topicCtx, cancelTopics := context.WithTimeout(ctx, topicTimeout)
	defer cancelTopics()
	if err := mq.EnsureTopics(topicCtx, kafkaCfg.Brokers, mq.DefaultTopics()...); err != nil {
		return err
	}
	logger.Info("topics ready", "topics", mq.DefaultTopics())

	producer, err := mq.NewProducer(kafkaCfg)
	if err != nil {
		return err
	}
	closers.Add("kafka producer", func(context.Context) error { return producer.Close() })

	consumer, err := mq.NewConsumer(kafkaCfg, mq.GroupIngestionWorker,
		[]string{mq.TopicDocumentsIngest}, logger)
	if err != nil {
		return err
	}
	closers.Add("kafka consumer", func(context.Context) error { return consumer.Close() })

	handler := ingest.NewHandler(st, recCfg.Lease, logger)
	reconciler := reconcile.NewRunner(st, producer, reconcile.PolicyFrom(recCfg), logger)

	// One cancellation for both goroutines, so that either one failing brings
	// the other down rather than leaving half a worker running.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- consumer.Run(runCtx, handler.Handle)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		errs <- reconciler.Run(runCtx)
	}()

	var runErr error
	select {
	case err := <-errs:
		if err != nil {
			runErr = fmt.Errorf("worker stopped: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}
	cancelRun()

	// Closing the clients is what unblocks a poll that is mid-request, so the
	// goroutines are given a moment to exit on cancellation alone and then the
	// closers deal with whatever is left.
	waitFor(&wg, goroutineTimeout)

	if err := closers.Close(shutdownTimeout); err != nil {
		return errors.Join(runErr, fmt.Errorf("shutdown: %w", err))
	}
	if runErr != nil {
		return runErr
	}
	logger.Info("stopped")
	return nil
}

// waitFor waits for wg, giving up after d. Giving up is safe: the goroutines
// only touch resources the closers are about to close, and the process is about
// to exit.
func waitFor(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}
