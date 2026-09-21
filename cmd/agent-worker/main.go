// Command agent-worker consumes agent.runs.v1 and executes one bounded
// investigation per message.
//
// It also owns the run reconciler, on the same principle cmd/ingestion-worker
// follows: the process that consumes the work is the one that should look for
// work that never arrived. That matters more for runs than for documents — a
// run abandoned in RUNNING keeps uniq_active_run set, so its incident refuses
// every new run until something reclaims it.
//
// Everything but the infrastructure runs on the host, so AGENT_TOOL_SERVER_URL
// is a host URL. ops-mcp sits behind Compose's `lab` profile, which means this
// binary needs `make up-lab` rather than `make up`: it refuses to start
// without a tool server.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wpan36/incident_diag/internal/agent"
	"github.com/wpan36/incident_diag/internal/agentrun"
	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/reconcile"
	"github.com/wpan36/incident_diag/internal/search"
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

	// toolConnectTimeout bounds the session with ops-mcp, which lists its
	// tools once at connect.
	toolConnectTimeout = 30 * time.Second

	// shutdownTimeout is the budget for leaving the consumer group and closing
	// everything, matching cmd/ingestion-worker.
	shutdownTimeout = 15 * time.Second

	// goroutineTimeout bounds the wait for the consumer and reconciler
	// goroutines to notice cancellation. It is part of the shutdown budget,
	// not additional to it.
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
	embedCfg, embedErr := config.LoadEmbedding()
	searchCfg, searchErr := config.LoadSearch()
	llmCfg, llmErr := config.LoadLLM()
	agentCfg, agentErr := config.LoadAgent()
	recCfg, recErr := config.LoadReconcile()
	eventsCfg, eventsErr := config.LoadEvents()
	if err := errors.Join(cfgErr, dbErr, kafkaErr, embedErr, searchErr,
		llmErr, agentErr, recErr, eventsErr); err != nil {
		return err
	}
	// Last, and separately: its lease invariant is arithmetic over four of the
	// structs above, so it cannot be checked until they have all loaded.
	workerCfg, err := config.LoadAgentWorker(agentCfg, llmCfg, embedCfg, searchCfg)
	if err != nil {
		return err
	}

	logger := log.New(os.Stdout, cfg.LogLevel, "agent-worker")
	logger.Info("starting agent worker", "config", cfg.String(), "database", dbCfg.String(),
		"kafka", kafkaCfg.String(), "embedding", embedCfg.String(), "search", searchCfg.String(),
		"llm", llmCfg.String(), "agent", agentCfg.String(), "reconcile", recCfg.String(),
		"events", eventsCfg.String(), "worker", workerCfg.String())

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

	// Idempotent, and called here as well as in the ingestion worker so that
	// either process can be the first one started.
	topicCtx, cancelTopics := context.WithTimeout(ctx, topicTimeout)
	defer cancelTopics()
	if err := mq.EnsureTopics(topicCtx, kafkaCfg.Brokers, mq.DefaultTopics()...); err != nil {
		return err
	}

	// The index is the ingestion worker's to create, so this one only reads
	// through the alias — the same division already made for Kafka topics.
	searchClient, err := search.New(searchCfg, logger)
	if err != nil {
		return err
	}

	// Nothing is dialled: go-redis connects lazily. A Redis that is down costs
	// a live timeline, and refusing to start over it would cost the runs.
	publisher, err := events.New(eventsCfg, logger)
	if err != nil {
		return err
	}
	closers.Add("redis", func(context.Context) error { return publisher.Close() })

	// The one startup dependency that is refused rather than deferred.
	// mcpclient.Connect lists the tools once by design, and a worker that
	// started without them would claim runs it cannot investigate — the same
	// reasoning that makes EnsureTopics a startup step.
	toolCtx, cancelTools := context.WithTimeout(ctx, toolConnectTimeout)
	defer cancelTools()
	tools, err := mcpclient.Connect(toolCtx, workerCfg.ToolServerURL, workerCfg.ToolTimeout, logger)
	if err != nil {
		return fmt.Errorf("%w (is ops-mcp running? it needs `make up-lab`)", err)
	}
	closers.Add("tool server", func(context.Context) error { return tools.Close() })
	logger.Info("tool server ready", "url", workerCfg.ToolServerURL, "tools", len(tools.Tools()))

	producer, err := mq.NewProducer(kafkaCfg)
	if err != nil {
		return err
	}
	closers.Add("kafka producer", func(context.Context) error { return producer.Close() })

	consumer, err := mq.NewConsumer(kafkaCfg, mq.GroupAgentWorker,
		[]string{mq.TopicAgentRuns}, logger,
		// Derived, not configured: a handler that outlasts this is evicted
		// from the group and its message redelivered.
		mq.WithRebalanceTimeout(workerCfg.RebalanceTimeout))
	if err != nil {
		return err
	}
	closers.Add("kafka consumer", func(context.Context) error { return consumer.Close() })

	handler := agentrun.NewHandler(agentrun.Deps{
		Store:  st,
		Events: publisher,
		LLM:    llm.New(llmCfg, logger),
		Knowledge: &agent.Knowledge{
			Embedder: embed.New(embedCfg, logger),
			Search:   searchClient,
		},
		Tools:  tools,
		Lease:  workerCfg.RunLease,
		Logger: logger,
	})
	reconciler := reconcile.NewRunRunner(st, producer, recCfg,
		workerCfg.RunLease, workerCfg.RunMaxAttempts, logger)

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
	// closers deal with whatever is left. A run in flight is cancelled and
	// writes nothing terminal: it stays RUNNING for the lease to reclaim.
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
// only touch resources the closers are about to close, and the process is
// about to exit.
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
