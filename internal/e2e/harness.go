package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/agent"
	"github.com/wpan36/incident_diag/internal/agentrun"
	"github.com/wpan36/incident_diag/internal/api"
	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/ingest"
	"github.com/wpan36/incident_diag/internal/lab/scenario"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// Timings. They are constants rather than configuration because every one of
// them is a property of this harness rather than of a deployment.
const (
	// warmUp is how long the fault runs before the incident is filed.
	//
	// The agent reasons over five-minute rate windows and looks back further
	// than that, and the evaluation wipes the metric history first — so this
	// is the whole of what it can see. Ninety seconds was enough when there
	// was history behind it; two and a half minutes is enough without.
	warmUp = 150 * time.Second

	// pollInterval is how often the terminal run is looked for.
	pollInterval = 2 * time.Second

	// runTimeout bounds one investigation from the API's point of view. It is
	// longer than AGENT_MAX_RUN_DURATION so that a run which hits its own
	// bound is reported as a bounded run rather than as a harness timeout.
	runTimeout = 10 * time.Minute

	// ingestTimeout bounds waiting for the corpus to become READY.
	ingestTimeout = 3 * time.Minute

	setupTimeout = 30 * time.Second
)

// The budget an end-to-end run gets, which is deliberately larger than the
// shipped default.
//
// With AGENT_MAX_TOOL_CALLS at its default of 6, both trial runs of the
// payment-latency scenario stopped at a bound rather than finishing, and one
// of the two named the wrong service — it had spent its calls before it could
// tell "payment-service is slow" from "checkout's timeout is too short". That
// is a finding for M32 to quantify, not something this test should be decided
// by, so the harness gives the agent room to converge and reports how much of
// it was used.
const (
	e2eMaxSteps        = 12
	e2eMaxToolCalls    = 10
	e2eMaxPromptTokens = 120000
)

// Config is what the harness needs to stand the system up.
//
// It is filled by ConfigFromEnv in the test. The fields are here rather than
// read inside so that M32 can point one run at a different provider or a
// different lab without a second loader.
type Config struct {
	MySQLDSN         string
	KafkaBrokers     []string
	ElasticsearchURL string
	RedisURL         string

	// ToolServerURL is ops-mcp, which runs in its container: the tools reach
	// Prometheus and the lab's log files, and reproducing that in process
	// would be reproducing the deployment rather than testing it.
	ToolServerURL string
	ToolTimeout   time.Duration

	CheckoutURL string
	PaymentURL  string

	// PrometheusURL is what the evaluation resets between scenarios, so that
	// the agent does not read the previous fault out of the history.
	PrometheusURL string

	LLM       config.LLM
	Embedding config.Embedding
	Agent     config.Agent

	// StorageRoot is where uploaded documents land. A temporary directory, so
	// a run leaves nothing behind.
	StorageRoot string

	// CorpusDir holds the knowledge documents to upload.
	CorpusDir string

	Logger *slog.Logger
}

// ConfigFromEnv reads the configuration an integration run needs.
//
// Infrastructure comes from the TEST_* variables the other integration tests
// use; the provider configuration comes from the ordinary ones, with the key
// replaced by TEST_LLM_API_KEY so that a billed completion is never an
// accident. A missing variable is an error naming it, which the caller turns
// into a skip.
func ConfigFromEnv(storageRoot, corpusDir string, logger *slog.Logger) (Config, error) {
	required := func(name string) (string, error) {
		if v := os.Getenv(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s is not set", name)
	}

	var cfg Config
	var err error
	for _, f := range []struct {
		name string
		dst  *string
	}{
		{"TEST_MYSQL_DSN", &cfg.MySQLDSN},
		{"TEST_ELASTICSEARCH_URL", &cfg.ElasticsearchURL},
		{"TEST_REDIS_URL", &cfg.RedisURL},
		{"TEST_LLM_API_KEY", new(string)},
	} {
		v, e := required(f.name)
		if e != nil {
			return Config{}, e
		}
		*f.dst = v
	}
	brokers, err := required("TEST_KAFKA_BROKERS")
	if err != nil {
		return Config{}, err
	}
	cfg.KafkaBrokers = strings.Split(brokers, ",")

	if cfg.LLM, err = config.LoadLLM(); err != nil {
		return Config{}, err
	}
	cfg.LLM.APIKey = os.Getenv("TEST_LLM_API_KEY")
	if cfg.Embedding, err = config.LoadEmbedding(); err != nil {
		return Config{}, err
	}
	if cfg.Agent, err = config.LoadAgent(); err != nil {
		return Config{}, err
	}
	cfg.Agent.MaxSteps = e2eMaxSteps
	cfg.Agent.MaxToolCalls = e2eMaxToolCalls
	cfg.Agent.MaxPromptTokens = e2eMaxPromptTokens

	worker, err := config.LoadAgentWorker(cfg.Agent, cfg.LLM, cfg.Embedding, config.Search{})
	if err != nil {
		return Config{}, err
	}
	cfg.ToolServerURL = worker.ToolServerURL
	cfg.ToolTimeout = worker.ToolTimeout

	cfg.CheckoutURL = envOr("CHECKOUT_URL", "http://127.0.0.1:8081")
	cfg.PaymentURL = envOr("PAYMENT_URL", "http://127.0.0.1:8082")
	cfg.PrometheusURL = envOr("PROMETHEUS_URL", "http://127.0.0.1:9090")
	cfg.StorageRoot = storageRoot
	cfg.CorpusDir = corpusDir
	cfg.Logger = logger
	return cfg, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// Harness is the whole system, assembled in one process.
//
// The API is an httptest server over internal/api, and the two workers are
// their consumers over the real Kafka. That is not the deployment — cmd/* wire
// the same pieces with configuration and shutdown around them — but it is the
// same code paths, and it means `make test-integration` runs an investigation
// rather than asking someone to start three binaries first.
type Harness struct {
	cfg    Config
	api    *httptest.Server
	driver *scenario.Driver
	client *http.Client

	store    *store.Store
	producer *mq.Client
	closers  []func()
}

// New stands the system up. The returned Harness must be closed.
func New(ctx context.Context, cfg Config) (*Harness, error) {
	h := &Harness{cfg: cfg, client: &http.Client{Timeout: 60 * time.Second}}

	// Anything that fails part way through still releases what opened before
	// it, so a broken environment does not leak a connection pool per attempt.
	ok := false
	defer func() {
		if !ok {
			h.Close()
		}
	}()

	connectCtx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()

	st, err := store.Open(connectCtx, config.Database{
		DSN: cfg.MySQLDSN, MaxOpenConns: 10, MaxIdleConns: 10, ConnMaxLifetime: 5 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("e2e: open mysql: %w", err)
	}
	h.store = st
	h.closers = append(h.closers, func() { st.Close() })

	if err := mq.EnsureTopics(connectCtx, cfg.KafkaBrokers, mq.DefaultTopics()...); err != nil {
		return nil, fmt.Errorf("e2e: ensure topics: %w", err)
	}

	kafkaCfg := config.Kafka{Brokers: cfg.KafkaBrokers, ProduceTimeout: 10 * time.Second}
	producer, err := mq.NewProducer(kafkaCfg)
	if err != nil {
		return nil, fmt.Errorf("e2e: open producer: %w", err)
	}
	h.producer = producer
	h.closers = append(h.closers, func() { producer.Close() })

	searchCfg := config.Search{URL: cfg.ElasticsearchURL, IndexAlias: config.DefaultIndexAlias, Timeout: 10 * time.Second}
	searcher, err := search.New(searchCfg, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("e2e: open elasticsearch: %w", err)
	}
	if err := searcher.EnsureIndex(connectCtx); err != nil {
		return nil, fmt.Errorf("e2e: ensure index: %w", err)
	}

	eventsCfg := config.Events{
		URL: cfg.RedisURL, PublishTimeout: 5 * time.Second,
		StreamMaxLen: 1000, StreamTTL: time.Hour,
		Heartbeat: 20 * time.Second, ReadBlock: 5 * time.Second,
	}
	publisher, err := events.New(eventsCfg, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("e2e: open redis: %w", err)
	}
	h.closers = append(h.closers, func() { publisher.Close() })

	tools, err := mcpclient.Connect(connectCtx, cfg.ToolServerURL, cfg.ToolTimeout, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("e2e: connect to ops-mcp at %s (is `make up-lab` running?): %w",
			cfg.ToolServerURL, err)
	}
	h.closers = append(h.closers, func() { tools.Close() })

	storage, err := files.New(cfg.StorageRoot, 10<<20)
	if err != nil {
		return nil, fmt.Errorf("e2e: open the document store: %w", err)
	}
	embedder := embed.New(cfg.Embedding, cfg.Logger)

	// The API. gin's release mode for the same reason cmd/api sets it: the
	// debug banner is the only unstructured output this would produce.
	gin.SetMode(gin.ReleaseMode)
	h.api = httptest.NewServer(api.NewServer(api.Deps{
		Store: st, Files: storage, Producer: producer,
		Embedder: embedder, Search: searcher, EventReader: publisher,
		Agent: cfg.Agent, Events: eventsCfg, LLMModel: cfg.LLM.Model,
		Logger: cfg.Logger, Service: "e2e-api",
	}).Router())
	h.closers = append(h.closers, h.api.Close)

	// The two workers. Their goroutines end when the consumer is closed, which
	// is what Close does, in the reverse of this order.
	docHandler := ingest.NewHandler(ingest.Deps{
		Store: st, Files: storage, Embedder: embedder, Search: searcher,
		Lease: 10 * time.Minute, DocumentTimeout: 5 * time.Minute,
		Chunking: ingest.ChunkOptions{TargetTokens: 400, MaxPerDocument: 2000},
		Logger:   cfg.Logger,
	})
	runHandler := agentrun.NewHandler(agentrun.Deps{
		Store: st, Events: publisher, LLM: llm.New(cfg.LLM, cfg.Logger),
		Knowledge: &agent.Knowledge{Embedder: embedder, Search: searcher},
		Tools:     tools, Lease: 15 * time.Minute, Logger: cfg.Logger,
	})
	if err := h.consume(kafkaCfg, mq.GroupIngestionWorker, mq.TopicDocumentsIngest, docHandler.Handle); err != nil {
		return nil, err
	}
	if err := h.consume(kafkaCfg, mq.GroupAgentWorker, mq.TopicAgentRuns, runHandler.Handle); err != nil {
		return nil, err
	}

	h.driver = scenario.NewDriver(scenario.Config{
		CheckoutURL: cfg.CheckoutURL, PaymentURL: cfg.PaymentURL,
		PrometheusURL: cfg.PrometheusURL,
		RPS:           4, RequestTimeout: 10 * time.Second, MaxInFlight: 32,
		Params: scenario.DefaultParams(), Out: io.Discard,
	})

	ok = true
	return h, nil
}

// consume starts one consumer group member and registers its shutdown.
func (h *Harness) consume(cfg config.Kafka, group, topic string, handle mq.Handler) error {
	consumer, err := mq.NewConsumer(cfg, group, []string{topic}, h.cfg.Logger,
		mq.WithRebalanceTimeout(6*time.Minute))
	if err != nil {
		return fmt.Errorf("e2e: open the %s consumer: %w", group, err)
	}

	// The goroutine is owned by this consumer: Run returns when the client is
	// closed, which Close does.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = consumer.Run(context.Background(), handle)
	}()
	h.closers = append(h.closers, func() {
		consumer.Close()
		<-done
	})
	return nil
}

// Close releases everything, in the reverse of the order it was opened, and
// clears any fault that is still set.
func (h *Harness) Close() {
	if h.driver != nil {
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		_ = h.driver.Stop(ctx)
		cancel()
	}
	for i := len(h.closers) - 1; i >= 0; i-- {
		h.closers[i]()
	}
	h.closers = nil
}

// APIURL is the base URL of the API this harness serves.
func (h *Harness) APIURL() string { return h.api.URL }

// Run does the whole sequence for one scenario and returns what happened.
//
// It asserts nothing: a run that failed, stopped at a bound or named the wrong
// service all come back as an Outcome. Only something that stopped the run
// from happening at all — an unreachable dependency, a rejected request — is
// an error.
func (h *Harness) Run(ctx context.Context, s Scenario) (Outcome, error) {
	outs, err := h.RunRepeated(ctx, s, 1)
	if err != nil {
		return Outcome{}, err
	}
	return outs[0], nil
}

// RunRepeated investigates the same fault n times.
//
// The fault is injected once and held across all n, so the warm-up is paid
// once rather than n times. That is also the more honest comparison: the runs
// see the same system in the same state, so a difference between them is the
// model's, not the lab's.
func (h *Harness) RunRepeated(ctx context.Context, s Scenario, n int) ([]Outcome, error) {
	if n < 1 {
		return nil, fmt.Errorf("e2e: %d runs asked for", n)
	}
	if err := h.EnsureCorpus(ctx); err != nil {
		return nil, err
	}

	fault := scenario.Scenario{Name: "baseline"}
	if s.Fault != "" {
		found, err := scenario.Find(s.Fault)
		if err != nil {
			return nil, fmt.Errorf("e2e: %w", err)
		}
		fault = found
	}

	// Stopped on every exit path, including a failure below: a fault left
	// enabled would make the next scenario measure the wrong incident.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), setupTimeout)
		defer cancel()
		if err := h.driver.Stop(stopCtx); err != nil {
			h.cfg.Logger.Error("the fault could not be cleared", "error", err)
		}
	}()
	// Before the warm-up, not after: the agent looks back over an hour, and an
	// hour ago the lab was broken for a different reason. Without this, the
	// scenarios contaminate each other and the accuracy measures how well the
	// agent tells this fault from the last one.
	if err := h.driver.ResetMetrics(ctx); err != nil {
		return nil, err
	}
	if err := h.driver.Start(ctx, fault, warmUp); err != nil {
		return nil, fmt.Errorf("e2e: start the %s scenario: %w", fault.Name, err)
	}

	outs := make([]Outcome, 0, n)
	for i := 0; i < n; i++ {
		out, err := h.investigate(ctx, s)
		if err != nil {
			return nil, err
		}
		outs = append(outs, out)
	}
	return outs, nil
}

// investigate files one incident and waits for its run.
func (h *Harness) investigate(ctx context.Context, s Scenario) (Outcome, error) {
	started := time.Now()
	incidentID, err := h.fileIncident(ctx, s)
	if err != nil {
		return Outcome{}, err
	}
	runID, err := h.startRun(ctx, incidentID)
	if err != nil {
		return Outcome{}, err
	}

	out, err := h.awaitRun(ctx, runID)
	if err != nil {
		return Outcome{}, err
	}
	out.Scenario = s.Name
	out.IncidentID = incidentID
	out.Elapsed = time.Since(started)
	return out, nil
}

// EnsureCorpus uploads testdata/knowledge unless it is already there.
//
// Uploading through the API rather than writing rows: the ingestion pipeline
// is part of what this test covers, and a corpus put there another way would
// be a corpus the product cannot produce.
func (h *Harness) EnsureCorpus(ctx context.Context) error {
	ready, err := h.readyDocuments(ctx)
	if err != nil {
		return err
	}

	// The manifest decides what each document is, rather than its filename.
	// A document it does not list is not uploaded: an unlisted file is one the
	// corpus does not claim, and guessing its service is how the CPU runbook
	// ended up attributed to payment-service.
	manifest, err := loadManifest(h.cfg.CorpusDir)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(h.cfg.CorpusDir)
	if err != nil {
		return fmt.Errorf("e2e: read the corpus: %w", err)
	}

	// Only READY counts as already there. A row left FAILED by an earlier run
	// — a worker that could not reach the file, a provider that was down — is
	// re-uploaded rather than treated as a permanent block, and only the
	// documents uploaded here are waited on.
	var uploaded []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || ready[name] {
			continue
		}
		entry, listed := manifest[name]
		if !listed {
			return fmt.Errorf("e2e: %s is in the corpus but not in its manifest", name)
		}
		id, err := h.uploadDocument(ctx, filepath.Join(h.cfg.CorpusDir, name), entry)
		if err != nil {
			return err
		}
		uploaded = append(uploaded, id)
	}

	deadline := time.Now().Add(ingestTimeout)
	for _, id := range uploaded {
		for {
			var doc struct {
				Filename      string  `json:"filename"`
				Status        string  `json:"status"`
				FailureReason *string `json:"failure_reason"`
			}
			if err := h.get(ctx, "/api/documents/"+id, &doc); err != nil {
				return err
			}
			if doc.Status == "READY" {
				break
			}
			if doc.Status == "FAILED" {
				reason := ""
				if doc.FailureReason != nil {
					reason = *doc.FailureReason
				}
				return fmt.Errorf("e2e: %s failed to ingest: %s", doc.Filename, reason)
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("e2e: %s was still %s after %s", doc.Filename, doc.Status, ingestTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
		}
	}
	return nil
}

// manifestEntry is one document as testdata/knowledge/manifest.json declares
// it. Service is a pointer because the manifest writes null for the runbooks
// that belong to no one service.
type manifestEntry struct {
	Source       string  `json:"source"`
	Service      *string `json:"service"`
	DocumentType string  `json:"document_type"`
}

// loadManifest reads the corpus's own description of itself.
//
// This used to be guessed from the filename, and the guess was wrong in a way
// that mattered: cpu-saturation-runbook.md has no "checkout" in its name, so it
// was uploaded as payment-service, when the manifest declares it unattributed
// because it applies to either. A search filtered to checkout-service could
// then not see the CPU runbook at all.
func loadManifest(dir string) (map[string]manifestEntry, error) {
	body, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("e2e: read the corpus manifest: %w", err)
	}
	var doc struct {
		Documents []manifestEntry `json:"documents"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("e2e: decode the corpus manifest: %w", err)
	}

	out := make(map[string]manifestEntry, len(doc.Documents))
	for _, d := range doc.Documents {
		out[d.Source] = d
	}
	return out, nil
}

func (h *Harness) uploadDocument(ctx context.Context, path string, entry manifestEntry) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("e2e: read %s: %w", path, err)
	}
	name := filepath.Base(path)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("file", name)
	if err != nil {
		return "", fmt.Errorf("e2e: build the upload: %w", err)
	}
	if _, err := part.Write(content); err != nil {
		return "", fmt.Errorf("e2e: build the upload: %w", err)
	}

	fields := map[string]string{
		"title":         strings.TrimSuffix(name, ".md"),
		"document_type": entry.DocumentType,
	}
	// Omitted rather than sent empty when the manifest says null: the document
	// belongs to no one service, and claiming one would hide it from a search
	// filtered to the other.
	if entry.Service != nil {
		fields["service"] = *entry.Service
	}
	for field, value := range fields {
		if err := w.WriteField(field, value); err != nil {
			return "", fmt.Errorf("e2e: build the upload: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("e2e: build the upload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.api.URL+"/api/documents", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	var created struct {
		ID string `json:"id"`
	}
	if err := h.do(req, http.StatusCreated, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// readyDocuments returns the filenames already ingested successfully.
func (h *Harness) readyDocuments(ctx context.Context) (map[string]bool, error) {
	var page struct {
		Items []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
		} `json:"items"`
	}
	if err := h.get(ctx, "/api/documents?limit=100", &page); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, d := range page.Items {
		if d.Status == "READY" {
			out[d.Filename] = true
		}
	}
	return out, nil
}

func (h *Harness) fileIncident(ctx context.Context, s Scenario) (string, error) {
	payload := map[string]any{
		"title": s.Title, "description": s.Description,
		"service": s.Service, "severity": "SEV2",
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := h.post(ctx, "/api/incidents", payload, http.StatusCreated, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

func (h *Harness) startRun(ctx context.Context, incidentID string) (string, error) {
	var created struct {
		ID string `json:"id"`
	}
	if err := h.post(ctx, "/api/incidents/"+incidentID+"/runs", map[string]any{},
		http.StatusCreated, &created); err != nil {
		return "", err
	}
	return created.ID, nil
}

// runResponse is GET /api/runs/{id}, in the fields this package reads.
type runResponse struct {
	Status           string          `json:"status"`
	StopReason       string          `json:"stop_reason"`
	Error            *string         `json:"error"`
	StepCount        int             `json:"step_count"`
	ToolCallCount    int             `json:"tool_call_count"`
	PromptTokens     int             `json:"prompt_tokens"`
	CompletionTokens int             `json:"completion_tokens"`
	FinalResult      json.RawMessage `json:"final_result"`
	Steps            []struct {
		ID         string `json:"id"`
		StepNumber int    `json:"step_number"`
		ActionType string `json:"action_type"`
		Status     string `json:"status"`
		Error      *string
		DurationMS int `json:"duration_ms"`
		Action     struct {
			Tool string `json:"tool"`
		} `json:"action"`
		ToolCall *struct {
			ToolName string `json:"tool_name"`
			Status   string `json:"status"`
		} `json:"tool_call"`
		Evidence []Evidence `json:"evidence"`
	} `json:"steps"`
}

func (h *Harness) awaitRun(ctx context.Context, runID string) (Outcome, error) {
	deadline := time.Now().Add(runTimeout)
	for {
		var r runResponse
		if err := h.get(ctx, "/api/runs/"+runID, &r); err != nil {
			return Outcome{}, err
		}
		if r.Status != "PENDING" && r.Status != "RUNNING" {
			return outcomeFrom(runID, r), nil
		}
		if time.Now().After(deadline) {
			return Outcome{}, fmt.Errorf("e2e: run %s was still %s after %s", runID, r.Status, runTimeout)
		}
		select {
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func outcomeFrom(runID string, r runResponse) Outcome {
	out := Outcome{
		RunID: runID, Status: r.Status, StopReason: r.StopReason,
		StepCount: r.StepCount, ToolCallCount: r.ToolCallCount,
		PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens,
		FinalResult: r.FinalResult,
	}
	if r.Error != nil {
		out.Error = *r.Error
	}
	for _, s := range r.Steps {
		step := Step{
			ID: s.ID, Number: s.StepNumber, ActionType: s.ActionType, Status: s.Status,
			Tool: s.Action.Tool, Duration: time.Duration(s.DurationMS) * time.Millisecond,
			Evidence: s.Evidence,
		}
		if s.Error != nil {
			step.Error = *s.Error
		}
		if s.ToolCall != nil {
			step.ToolStatus = s.ToolCall.Status
		}
		out.Steps = append(out.Steps, step)
	}

	var final struct {
		RootCause       string `json:"root_cause"`
		AffectedService string `json:"affected_service"`
	}
	// A run that failed before finishing has no final_result, and that is not
	// an error here: the Outcome says so through Status.
	if len(r.FinalResult) > 0 {
		_ = json.Unmarshal(r.FinalResult, &final)
	}
	out.RootCause = final.RootCause
	out.AffectedService = final.AffectedService
	return out
}

func (h *Harness) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.api.URL+path, nil)
	if err != nil {
		return err
	}
	return h.do(req, http.StatusOK, into)
}

func (h *Harness) post(ctx context.Context, path string, payload any, want int, into any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.api.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return h.do(req, want, into)
}

func (h *Harness) do(req *http.Request, want int, into any) error {
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("e2e: %s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("e2e: %s %s: reading the body: %w", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("e2e: %s %s returned %d, want %d: %s",
			req.Method, req.URL.Path, resp.StatusCode, want, truncate(string(body)))
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("e2e: %s %s returned a body that will not decode: %w",
			req.Method, req.URL.Path, err)
	}
	return nil
}

func truncate(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
