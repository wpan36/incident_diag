-- Initial schema: the six tables described in docs/plans/data-model-and-api-surface.md.
--
-- Conventions that apply to every table below:
--   * InnoDB, utf8mb4_0900_ai_ci.
--   * Identifiers are application-generated ULIDs, CHAR(26).
--   * Timestamps are DATETIME(6) holding UTC, supplied by the application.
--     TIMESTAMP would convert according to the server's time zone, which makes
--     test results depend on where they run.
--   * Status columns are VARCHAR, not ENUM: the transition rules have to live
--     in Go regardless, and adding a value to an ENUM is a DDL change.
--   * Counters are NOT NULL DEFAULT 0 so aggregates need no COALESCE and the Go
--     structs need no pointer fields.

CREATE TABLE documents (
    id                    CHAR(26)     NOT NULL,
    filename              VARCHAR(255) NOT NULL,
    -- Relative to the configured storage root, so the same rows work in
    -- Compose and on a developer's machine.
    storage_path          VARCHAR(512) NOT NULL,
    format                VARCHAR(16)  NOT NULL,
    size_bytes            BIGINT       NOT NULL DEFAULT 0,
    -- Recorded so a client can confirm what arrived. Deliberately not used to
    -- deduplicate: the same file uploaded with different metadata is ambiguous.
    content_sha256        CHAR(64)     NOT NULL,
    service               VARCHAR(64)  NULL,
    document_type         VARCHAR(32)  NOT NULL,
    status                VARCHAR(16)  NOT NULL,
    failure_reason        TEXT         NULL,
    chunk_count           INT          NOT NULL DEFAULT 0,
    attempts              INT          NOT NULL DEFAULT 0,
    processing_started_at DATETIME(6)  NULL,
    created_at            DATETIME(6)  NOT NULL,
    updated_at            DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    -- Listing by status, and the lease reclaim S2 will add. The ingestion
    -- worker itself looks a document up by primary key: the Kafka message
    -- carries the id.
    KEY idx_documents_status_id (status, id),
    KEY idx_documents_service_type (service, document_type)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

-- No status column. Incidents exist to give a run something to investigate;
-- OPEN and CLOSED would model a workflow this product does not have.
CREATE TABLE incidents (
    id          CHAR(26)     NOT NULL,
    title       VARCHAR(255) NOT NULL,
    description TEXT         NOT NULL,
    service     VARCHAR(64)  NULL,
    created_at  DATETIME(6)  NOT NULL,
    updated_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

CREATE TABLE agent_runs (
    id                   CHAR(26)    NOT NULL,
    incident_id          CHAR(26)    NOT NULL,
    -- Lifecycle and outcome are separate columns: a run stopped by a bound is
    -- SUCCEEDED with a stop_reason, because a partial diagnosis is a normal
    -- outcome rather than a failure.
    status               VARCHAR(16) NOT NULL,
    stop_reason          VARCHAR(24) NULL,
    -- One active run per incident, enforced by the database rather than by a
    -- read-then-write check that two requests could interleave. MySQL has no
    -- partial unique index, so this generated column holds the incident id
    -- while the run is in flight and NULL once it is terminal; NULLs do not
    -- collide under a UNIQUE index, so finished runs stop competing.
    active_incident_id   CHAR(26) GENERATED ALWAYS AS (
        CASE WHEN status IN ('PENDING', 'RUNNING') THEN incident_id END
    ) STORED,
    -- The budget that actually applied, so a run stays interpretable after the
    -- configuration changes.
    max_steps            INT         NOT NULL DEFAULT 0,
    max_tool_calls       INT         NOT NULL DEFAULT 0,
    max_duration_seconds INT         NOT NULL DEFAULT 0,
    max_prompt_tokens    INT         NOT NULL DEFAULT 0,
    model                VARCHAR(128) NOT NULL,
    step_count           INT         NOT NULL DEFAULT 0,
    tool_call_count      INT         NOT NULL DEFAULT 0,
    prompt_tokens        INT         NOT NULL DEFAULT 0,
    completion_tokens    INT         NOT NULL DEFAULT 0,
    final_result         JSON        NULL,
    error                TEXT        NULL,
    attempts             INT         NOT NULL DEFAULT 0,
    started_at           DATETIME(6) NULL,
    finished_at          DATETIME(6) NULL,
    created_at           DATETIME(6) NOT NULL,
    updated_at           DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uniq_active_run (active_incident_id),
    KEY idx_agent_runs_incident_id (incident_id, id),
    KEY idx_agent_runs_status_id (status, id),
    -- No ON DELETE clause, although ownership would argue for CASCADE: MySQL
    -- refuses a cascading foreign key on a column that a STORED generated
    -- column is derived from, and active_incident_id is derived from this one.
    -- Deleting an incident that still has runs is therefore refused rather than
    -- taking its runs with it. Nothing deletes incidents, so this costs nothing
    -- today, and the generated column is worth more: it cannot drift from
    -- status the way a column the application maintains could.
    CONSTRAINT fk_agent_runs_incident FOREIGN KEY (incident_id)
        REFERENCES incidents (id)
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

CREATE TABLE agent_steps (
    id                  CHAR(26)    NOT NULL,
    run_id              CHAR(26)    NOT NULL,
    step_number         INT         NOT NULL,
    action_type         VARCHAR(16) NOT NULL,
    -- The structured decision the model returned, never the reasoning behind
    -- it. No chain-of-thought is persisted anywhere in this schema.
    action              JSON        NOT NULL,
    observation_summary MEDIUMTEXT  NULL,
    observation_bytes   INT         NOT NULL DEFAULT 0,
    truncated           BOOL        NOT NULL DEFAULT FALSE,
    status              VARCHAR(16) NOT NULL,
    error               TEXT        NULL,
    duration_ms         INT         NOT NULL DEFAULT 0,
    created_at          DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uniq_step_number (run_id, step_number),
    KEY idx_agent_steps_run_id (run_id, id),
    CONSTRAINT fk_agent_steps_run FOREIGN KEY (run_id)
        REFERENCES agent_runs (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

CREATE TABLE tool_calls (
    id             CHAR(26)    NOT NULL,
    run_id         CHAR(26)    NOT NULL,
    step_id        CHAR(26)    NOT NULL,
    tool_name      VARCHAR(64) NOT NULL,
    arguments      JSON        NOT NULL,
    status         VARCHAR(16) NOT NULL,
    result_summary MEDIUMTEXT  NULL,
    result_bytes   INT         NOT NULL DEFAULT 0,
    truncated      BOOL        NOT NULL DEFAULT FALSE,
    error          TEXT        NULL,
    duration_ms    INT         NOT NULL DEFAULT 0,
    created_at     DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY idx_tool_calls_run_id (run_id, id),
    KEY idx_tool_calls_step_id (step_id),
    CONSTRAINT fk_tool_calls_run FOREIGN KEY (run_id)
        REFERENCES agent_runs (id) ON DELETE CASCADE,
    CONSTRAINT fk_tool_calls_step FOREIGN KEY (step_id)
        REFERENCES agent_steps (id) ON DELETE CASCADE
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;

-- A table rather than JSON inside final_result, because "which documents
-- actually get cited across runs" is then a GROUP BY. That query is direct
-- feedback on retrieval quality, which M12 and M32 exist to measure.
CREATE TABLE evidence (
    id           CHAR(26)     NOT NULL,
    run_id       CHAR(26)     NOT NULL,
    step_id      CHAR(26)     NOT NULL,
    tool_call_id CHAR(26)     NULL,
    source_type  VARCHAR(16)  NOT NULL,
    source_ref   VARCHAR(255) NOT NULL,
    document_id  CHAR(26)     NULL,
    summary      TEXT         NOT NULL,
    note         TEXT         NULL,
    created_at   DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_evidence_run_id (run_id, id),
    KEY idx_evidence_document_id (document_id),
    KEY idx_evidence_step_id (step_id),
    KEY idx_evidence_tool_call_id (tool_call_id),
    CONSTRAINT fk_evidence_run FOREIGN KEY (run_id)
        REFERENCES agent_runs (id) ON DELETE CASCADE,
    CONSTRAINT fk_evidence_step FOREIGN KEY (step_id)
        REFERENCES agent_steps (id) ON DELETE CASCADE,
    CONSTRAINT fk_evidence_tool_call FOREIGN KEY (tool_call_id)
        REFERENCES tool_calls (id) ON DELETE CASCADE,
    -- The exception to ownership-shaped cascades: evidence is not owned by the
    -- document it cites. Deleting a document must not erase the record that an
    -- investigation relied on it, and source_ref keeps the citation readable
    -- afterwards.
    CONSTRAINT fk_evidence_document FOREIGN KEY (document_id)
        REFERENCES documents (id) ON DELETE SET NULL
) ENGINE = InnoDB DEFAULT CHARSET = utf8mb4 COLLATE = utf8mb4_0900_ai_ci;
