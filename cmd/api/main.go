// Command api serves the REST API.
//
// It is the only process that terminates a client connection: the workers
// publish events and write rows, and this binary reads them. That is what lets
// workers restart or crash without disturbing anyone's browser.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/api"
	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/shutdown"
	"github.com/wpan36/incident_diag/internal/store"
)

// connectTimeout bounds the first connection to MySQL at startup. Without it a
// database that accepts TCP but never answers would leave the process hanging
// with no output.
const connectTimeout = 10 * time.Second

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
	httpCfg, httpErr := config.LoadHTTPServer()
	docCfg, docErr := config.LoadDocuments()
	kafkaCfg, kafkaErr := config.LoadKafka()
	embedCfg, embedErr := config.LoadEmbedding()
	searchCfg, searchErr := config.LoadSearch()
	// The API runs no agent. It needs these two because
	// POST /api/incidents/{id}/runs records the budget and the model on the
	// row, so a run stays interpretable after the configuration changes.
	agentCfg, agentErr := config.LoadAgent()
	llmCfg, llmErr := config.LoadLLM()
	if err := errors.Join(cfgErr, dbErr, httpErr, docErr, kafkaErr, embedErr, searchErr,
		agentErr, llmErr); err != nil {
		return err
	}

	logger := log.New(os.Stdout, cfg.LogLevel, "api")
	logger.Info("starting api", "config", cfg.String(), "database", dbCfg.String(),
		"http", httpCfg.String(), "documents", docCfg.String(), "kafka", kafkaCfg.String(),
		"embedding", embedCfg.String(), "search", searchCfg.String(),
		"agent", agentCfg.String(), "llm", llmCfg.String())

	// Signals become a cancelled context before anything is opened, so a
	// Ctrl-C during startup is honoured rather than queued.
	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	var closers shutdown.Group

	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	st, err := store.Open(connectCtx, dbCfg)
	if err != nil {
		return err
	}
	closers.Add("mysql", func(context.Context) error { return st.Close() })

	// Creating the storage root at startup means an unwritable volume is a
	// startup failure rather than a 500 on the first upload.
	fs, err := files.New(docCfg.StorageRoot, docCfg.MaxUploadBytes)
	if err != nil {
		return err
	}

	// Opening the producer does not connect to anything: franz-go dials on the
	// first produce. A broker that is down is therefore not a startup failure
	// here, which is the same position the upload handler takes — the row is
	// written, the reconciler enqueues it, and the API keeps answering.
	//
	// Topics are not created here either. The ingestion worker owns that, so
	// two processes cannot race to create the same topic.
	producer, err := mq.NewProducer(kafkaCfg)
	if err != nil {
		return err
	}
	closers.Add("kafka producer", func(context.Context) error { return producer.Close() })

	// Both are needed by GET /api/search and by nothing else. Neither connects
	// here: the embedding client dials on its first request, and the search
	// client only opens on its first query. EnsureIndex is not called — the
	// ingestion worker owns the index, so two processes cannot race to create
	// it, which is the same division already made for Kafka topics.
	embedder := embed.New(embedCfg, logger)
	searcher, err := search.New(searchCfg, logger)
	if err != nil {
		return err
	}

	// gin's debug mode writes its own startup banner and per-route lines, which
	// would be the only unstructured output this process produces.
	gin.SetMode(gin.ReleaseMode)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.NewServer(api.Deps{
			Store:    st,
			Files:    fs,
			Producer: producer,
			Embedder: embedder,
			Search:   searcher,
			Agent:    agentCfg,
			LLMModel: llmCfg.Model,
			Logger:   logger,
		}).Router(),
		ReadHeaderTimeout: httpCfg.ReadHeaderTimeout,
		ReadTimeout:       httpCfg.ReadTimeout,
		WriteTimeout:      httpCfg.WriteTimeout,
		IdleTimeout:       httpCfg.IdleTimeout,
	}
	// Registered after the store, so it is closed before it: the server stops
	// accepting requests before the database those requests need goes away.
	closers.Add("http server", srv.Shutdown)

	// The listener owns this goroutine and ends it by returning; serveErr
	// carries that result back so a failure to bind is reported rather than
	// leaving the process alive and silent.
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			closers.Close(httpCfg.ShutdownTimeout)
			return fmt.Errorf("serving http: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	if err := closers.Close(httpCfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Info("stopped")
	return nil
}
