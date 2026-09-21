package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// isolate clears every variable any loader in this package reads, so a test is
// not affected by whatever the developer happens to have exported in their
// shell — a .env sourced for `make up` sets most of these. Setting a variable
// to the empty string is equivalent to unsetting it here, because lookup treats
// blank as absent.
func isolate(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "HTTP_ADDR", "LOG_LEVEL",
		"MYSQL_DSN", "DB_MAX_OPEN_CONNS", "DB_MAX_IDLE_CONNS", "DB_CONN_MAX_LIFETIME",
		"HTTP_READ_HEADER_TIMEOUT", "HTTP_READ_TIMEOUT", "HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT", "HTTP_SHUTDOWN_TIMEOUT",
		"DOCUMENT_STORAGE_ROOT", "DOCUMENT_MAX_UPLOAD_BYTES",
		"KAFKA_BROKERS", "KAFKA_PRODUCE_TIMEOUT",
		"RECONCILE_INTERVAL", "RECONCILE_PENDING_AFTER", "RECONCILE_BATCH",
		"INGEST_LEASE", "INGEST_MAX_ATTEMPTS", "INGEST_DOCUMENT_TIMEOUT",
		"CHUNK_TARGET_TOKENS", "CHUNK_MAX_PER_DOCUMENT",
		"EMBEDDING_BASE_URL", "EMBEDDING_API_KEY", "EMBEDDING_MODEL",
		"EMBED_BATCH_SIZE", "EMBED_TIMEOUT", "EMBED_MAX_RETRIES",
		"ELASTICSEARCH_URL", "ES_INDEX_ALIAS",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	isolate(t)
	// No variables set at all: every default applies and nothing fails.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with an empty environment: %v", err)
	}
	if cfg.Env != EnvDevelopment {
		t.Errorf("Env = %q, want %q", cfg.Env, EnvDevelopment)
	}
	if !cfg.IsDevelopment() {
		t.Error("IsDevelopment() = false, want true")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	// Development defaults to debug so that local runs are verbose without
	// anyone having to remember a flag.
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadProductionDefaultsToInfo(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "production")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.IsDevelopment() {
		t.Error("IsDevelopment() = true in production")
	}
}

func TestLoadExplicitValues(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "PRODUCTION") // case-insensitive, stored lowercase
	t.Setenv("HTTP_ADDR", "127.0.0.1:9000")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Env != EnvProduction {
		t.Errorf("Env = %q, want %q", cfg.Env, EnvProduction)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelWarn)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "staging")
	t.Setenv("LOG_LEVEL", "chatty")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with two invalid values")
	}
	// The whole point of the collecting loader: one restart, all the problems.
	for _, want := range []string{"APP_ENV", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadReturnsZeroConfigOnError(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "staging")

	cfg, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with an invalid APP_ENV")
	}
	// A partly valid Config is more dangerous than none: a caller that ignores
	// the error should not get something that looks usable.
	if cfg != (Config{}) {
		t.Errorf("Load() returned %+v alongside an error, want the zero value", cfg)
	}
}

func TestStringOmitsNothingKnownButIsExplicit(t *testing.T) {
	cfg := Config{Env: EnvProduction, LogLevel: slog.LevelInfo, HTTPAddr: ":8080"}
	got := cfg.String()
	for _, want := range []string{"env=production", "log_level=INFO", "http_addr=:8080"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestBlankValueIsTreatedAsUnset(t *testing.T) {
	isolate(t)
	// A key left empty in a .env file is a common mistake. Falling back to the
	// default beats failing with "HTTP_ADDR must be host:port, got \"\"".
	t.Setenv("HTTP_ADDR", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want the default", cfg.HTTPAddr)
	}
}

// The remaining helpers have no caller in Config yet; they are exercised
// directly so that the milestone adding their first field does not also have to
// debug them.

func TestRequiredString(t *testing.T) {
	t.Setenv("PRESENT", "value")

	var e env
	if got := e.requiredString("PRESENT"); got != "value" {
		t.Errorf("requiredString(PRESENT) = %q", got)
	}
	if got := e.requiredString("ABSENT"); got != "" {
		t.Errorf("requiredString(ABSENT) = %q, want empty", got)
	}
	err := e.err()
	if err == nil || !strings.Contains(err.Error(), "ABSENT is required") {
		t.Errorf("err() = %v, want a complaint about ABSENT", err)
	}
}

func TestOptionalScalars(t *testing.T) {
	t.Setenv("N", "42")
	t.Setenv("B", "true")
	t.Setenv("D", "1m30s")

	var e env
	if got := e.optionalInt("N", 7); got != 42 {
		t.Errorf("optionalInt = %d, want 42", got)
	}
	if got := e.optionalInt("MISSING", 7); got != 7 {
		t.Errorf("optionalInt default = %d, want 7", got)
	}
	if got := e.optionalBool("B", false); !got {
		t.Error("optionalBool = false, want true")
	}
	if got := e.optionalDuration("D", time.Second); got != 90*time.Second {
		t.Errorf("optionalDuration = %v, want 1m30s", got)
	}
	if err := e.err(); err != nil {
		t.Fatalf("err() = %v, want nil", err)
	}
}

func TestOptionalScalarsRejectMalformedValues(t *testing.T) {
	t.Setenv("N", "many")
	t.Setenv("B", "yes-please")
	t.Setenv("D", "30")    // no unit
	t.Setenv("NEG", "-5s") // negative

	var e env
	e.optionalInt("N", 0)
	e.optionalBool("B", false)
	e.optionalDuration("D", 0)
	e.optionalDuration("NEG", 0)

	err := e.err()
	if err == nil {
		t.Fatal("err() = nil, want four problems")
	}
	for _, want := range []string{"N must be an integer", "B must be a boolean", "D must be a duration", "NEG must not be negative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestMalformedValueStillReturnsTheDefault(t *testing.T) {
	t.Setenv("N", "many")

	var e env
	// The caller is expected to check err(), but returning the default rather
	// than a zero value keeps the failure path from cascading into confusing
	// secondary problems while the rest of the config is being collected.
	if got := e.optionalInt("N", 7); got != 7 {
		t.Errorf("optionalInt on a bad value = %d, want the default 7", got)
	}
}

func TestLoadDatabase(t *testing.T) {
	isolate(t)
	const dsn = "incident:incident@tcp(127.0.0.1:3306)/incident_diag?parseTime=true&loc=UTC"
	t.Setenv("MYSQL_DSN", dsn)

	db, err := LoadDatabase()
	if err != nil {
		t.Fatalf("LoadDatabase: %v", err)
	}
	if db.DSN != dsn {
		t.Errorf("DSN = %q", db.DSN)
	}
	if db.MaxOpenConns != 25 || db.MaxIdleConns != 25 || db.ConnMaxLifetime != 5*time.Minute {
		t.Errorf("defaults not applied: %+v", db)
	}
}

func TestLoadDatabaseRequiresTheDSN(t *testing.T) {
	isolate(t)
	if _, err := LoadDatabase(); err == nil {
		t.Fatal("LoadDatabase succeeded without MYSQL_DSN")
	} else if !strings.Contains(err.Error(), "MYSQL_DSN") {
		t.Errorf("error = %q, want it to name the missing variable", err)
	}
}

func TestLoadDatabaseReportsEveryProblemAtOnce(t *testing.T) {
	isolate(t)
	t.Setenv("DB_MAX_OPEN_CONNS", "0")
	t.Setenv("DB_CONN_MAX_LIFETIME", "forever")

	_, err := LoadDatabase()
	if err == nil {
		t.Fatal("LoadDatabase succeeded")
	}
	for _, want := range []string{"MYSQL_DSN", "DB_MAX_OPEN_CONNS", "DB_CONN_MAX_LIFETIME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %s too", err, want)
		}
	}
}

func TestDatabaseStringHidesTheDSN(t *testing.T) {
	d := Database{DSN: "incident:hunter2@tcp(127.0.0.1:3306)/incident_diag"}
	if strings.Contains(d.String(), "hunter2") {
		t.Fatalf("String() leaks the password: %s", d.String())
	}
}

func TestLoadHTTPServerDefaults(t *testing.T) {
	isolate(t)
	s, err := LoadHTTPServer()
	if err != nil {
		t.Fatalf("LoadHTTPServer: %v", err)
	}
	if s.ReadHeaderTimeout != 5*time.Second || s.ShutdownTimeout != 15*time.Second {
		t.Errorf("defaults not applied: %+v", s)
	}
}

func TestLoadHTTPServerRejectsAMalformedDuration(t *testing.T) {
	isolate(t)
	t.Setenv("HTTP_READ_TIMEOUT", "soon")
	if _, err := LoadHTTPServer(); err == nil {
		t.Fatal("LoadHTTPServer accepted a malformed duration")
	}
}

func TestLoadDocuments(t *testing.T) {
	isolate(t)
	d, err := LoadDocuments()
	if err != nil {
		t.Fatalf("LoadDocuments: %v", err)
	}
	if d.StorageRoot != DefaultDocumentStorageRoot || d.MaxUploadBytes != 10<<20 {
		t.Errorf("defaults not applied: %+v", d)
	}

	t.Setenv("DOCUMENT_STORAGE_ROOT", "/srv/documents")
	t.Setenv("DOCUMENT_MAX_UPLOAD_BYTES", "1048576")
	d, err = LoadDocuments()
	if err != nil {
		t.Fatalf("LoadDocuments: %v", err)
	}
	if d.StorageRoot != "/srv/documents" || d.MaxUploadBytes != 1<<20 {
		t.Errorf("overrides not applied: %+v", d)
	}
}

func TestLoadDocumentsReadsTheChunkingBudget(t *testing.T) {
	isolate(t)
	d, err := LoadDocuments()
	if err != nil {
		t.Fatalf("LoadDocuments: %v", err)
	}
	if d.ChunkTargetTokens != 400 || d.ChunkMaxPerDocument != 2000 {
		t.Errorf("chunking defaults not applied: %+v", d)
	}

	t.Setenv("CHUNK_TARGET_TOKENS", "0")
	if _, err := LoadDocuments(); err == nil {
		t.Fatal("LoadDocuments accepted a zero chunk target")
	}
}

func TestLoadDocumentsRejectsAnImpossibleLimit(t *testing.T) {
	isolate(t)
	t.Setenv("DOCUMENT_MAX_UPLOAD_BYTES", "0")
	if _, err := LoadDocuments(); err == nil {
		t.Fatal("LoadDocuments accepted a zero upload limit")
	}
}

// --- Kafka ------------------------------------------------------------------

func TestLoadEmbedding(t *testing.T) {
	isolate(t)
	// All three provider settings are required: there is no sensible default
	// for an endpoint, a key or a model.
	if _, err := LoadEmbedding(); err == nil {
		t.Fatal("LoadEmbedding accepted an empty environment")
	}

	t.Setenv("EMBEDDING_BASE_URL", "https://api.example.com/v1")
	t.Setenv("EMBEDDING_API_KEY", "secret")
	t.Setenv("EMBEDDING_MODEL", "BAAI/bge-m3")

	c, err := LoadEmbedding()
	if err != nil {
		t.Fatalf("LoadEmbedding: %v", err)
	}
	if c.BatchSize != 32 || c.Timeout != 30*time.Second || c.MaxRetries != 3 {
		t.Errorf("defaults not applied: %+v", c)
	}
	if strings.Contains(c.String(), "secret") {
		t.Errorf("String() leaks the API key: %s", c.String())
	}

	t.Setenv("EMBED_BATCH_SIZE", "0")
	if _, err := LoadEmbedding(); err == nil {
		t.Fatal("LoadEmbedding accepted a zero batch size")
	}
}

func TestLoadSearch(t *testing.T) {
	isolate(t)
	if _, err := LoadSearch(); err == nil {
		t.Fatal("LoadSearch accepted an empty environment")
	}

	t.Setenv("ELASTICSEARCH_URL", "http://127.0.0.1:9200")
	s, err := LoadSearch()
	if err != nil {
		t.Fatalf("LoadSearch: %v", err)
	}
	if s.IndexAlias != DefaultIndexAlias {
		t.Errorf("IndexAlias = %q, want %q", s.IndexAlias, DefaultIndexAlias)
	}

	t.Setenv("ES_INDEX_ALIAS", "chunks_test")
	if s, err = LoadSearch(); err != nil || s.IndexAlias != "chunks_test" {
		t.Errorf("override not applied: %+v (%v)", s, err)
	}
}

func TestLoadKafkaRequiresBrokers(t *testing.T) {
	isolate(t)

	// KAFKA_BROKERS has no default: a worker that silently produced to
	// localhost would look healthy while writing to nothing.
	if _, err := LoadKafka(); err == nil {
		t.Fatal("LoadKafka() with no KAFKA_BROKERS succeeded, want an error")
	} else if !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Errorf("error = %v, want it to name KAFKA_BROKERS", err)
	}
}

func TestLoadKafkaSplitsBrokers(t *testing.T) {
	isolate(t)
	t.Setenv("KAFKA_BROKERS", " kafka-1:9092 , kafka-2:9092 ,")

	k, err := LoadKafka()
	if err != nil {
		t.Fatalf("LoadKafka(): %v", err)
	}
	want := []string{"kafka-1:9092", "kafka-2:9092"}
	if len(k.Brokers) != len(want) {
		t.Fatalf("Brokers = %v, want %v", k.Brokers, want)
	}
	for i := range want {
		if k.Brokers[i] != want[i] {
			t.Errorf("Brokers[%d] = %q, want %q", i, k.Brokers[i], want[i])
		}
	}
	if k.ProduceTimeout != 10*time.Second {
		t.Errorf("ProduceTimeout = %s, want 10s", k.ProduceTimeout)
	}
}

func TestLoadKafkaRejectsAValueThatIsOnlySeparators(t *testing.T) {
	isolate(t)
	t.Setenv("KAFKA_BROKERS", ",,")

	if _, err := LoadKafka(); err == nil {
		t.Fatal("LoadKafka() with KAFKA_BROKERS=,, succeeded, want an error")
	}
}

func TestLoadKafkaRejectsAZeroTimeout(t *testing.T) {
	isolate(t)
	t.Setenv("KAFKA_BROKERS", "kafka:9092")
	t.Setenv("KAFKA_PRODUCE_TIMEOUT", "0s")

	if _, err := LoadKafka(); err == nil {
		t.Fatal("LoadKafka() with a zero produce timeout succeeded, want an error")
	}
}

// --- Reconcile --------------------------------------------------------------

func TestLoadReconcileDefaults(t *testing.T) {
	isolate(t)

	r, err := LoadReconcile()
	if err != nil {
		t.Fatalf("LoadReconcile(): %v", err)
	}
	if r.Interval != 30*time.Second {
		t.Errorf("Interval = %s, want 30s", r.Interval)
	}
	if r.PendingAfter != time.Minute {
		t.Errorf("PendingAfter = %s, want 1m", r.PendingAfter)
	}
	if r.Batch != 100 {
		t.Errorf("Batch = %d, want 100", r.Batch)
	}
	if r.Lease != 10*time.Minute {
		t.Errorf("Lease = %s, want 10m", r.Lease)
	}
	if r.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want 3", r.MaxAttempts)
	}
	if r.DocumentTimeout != 5*time.Minute {
		t.Errorf("DocumentTimeout = %s, want 5m", r.DocumentTimeout)
	}
}

// TestLoadReconcileRejectsATimeoutThatOutlastsTheLease is the constraint the
// whole ingestion state machine rests on: a handler still working after the
// lease has expired is a document another worker is free to claim.
func TestLoadReconcileRejectsATimeoutThatOutlastsTheLease(t *testing.T) {
	isolate(t)
	t.Setenv("INGEST_LEASE", "2m")
	t.Setenv("INGEST_DOCUMENT_TIMEOUT", "5m")

	_, err := LoadReconcile()
	if err == nil {
		t.Fatal("LoadReconcile accepted a document timeout longer than the lease")
	}
	if !strings.Contains(err.Error(), "INGEST_LEASE") {
		t.Errorf("err = %v, want it to name both variables", err)
	}
}

func TestLoadReconcileCollectsEveryProblem(t *testing.T) {
	isolate(t)
	t.Setenv("RECONCILE_BATCH", "0")
	t.Setenv("INGEST_MAX_ATTEMPTS", "0")
	t.Setenv("INGEST_LEASE", "0s")

	_, err := LoadReconcile()
	if err == nil {
		t.Fatal("LoadReconcile() with three bad values succeeded, want an error")
	}
	for _, want := range []string{"RECONCILE_BATCH", "INGEST_MAX_ATTEMPTS", "INGEST_LEASE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadReconcileReturnsTheZeroValueOnError(t *testing.T) {
	isolate(t)
	t.Setenv("RECONCILE_INTERVAL", "not-a-duration")

	r, err := LoadReconcile()
	if err == nil {
		t.Fatal("LoadReconcile() with a malformed interval succeeded, want an error")
	}
	if r != (Reconcile{}) {
		t.Errorf("Reconcile = %+v, want the zero value: a caller that ignores the error must not get something usable", r)
	}
}
