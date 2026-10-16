CREATE TABLE persons (
    id UUID PRIMARY KEY,
    display_name TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE forge_instances (
    id UUID PRIMARY KEY,
    provider TEXT NOT NULL,
    base_url TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, base_url)
);

CREATE TABLE identity_keys (
    id UUID PRIMARY KEY,
    forge_instance_id UUID REFERENCES forge_instances(id) ON DELETE CASCADE,
    kind TEXT NOT NULL CHECK (kind IN ('email', 'account_id', 'login')),
    value TEXT NOT NULL,
    normalized_value TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((kind = 'email' AND forge_instance_id IS NULL) OR
           (kind <> 'email' AND forge_instance_id IS NOT NULL))
);

CREATE UNIQUE INDEX identity_keys_global_unique
    ON identity_keys (kind, normalized_value)
    WHERE forge_instance_id IS NULL;

CREATE UNIQUE INDEX identity_keys_instance_unique
    ON identity_keys (forge_instance_id, kind, normalized_value)
    WHERE forge_instance_id IS NOT NULL;

CREATE TABLE identity_claims (
    id UUID PRIMARY KEY,
    identity_key_id UUID NOT NULL REFERENCES identity_keys(id) ON DELETE CASCADE,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    method TEXT NOT NULL,
    asserted_by TEXT NOT NULL DEFAULT 'system',
    reviewed_by TEXT,
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    disposition TEXT NOT NULL CHECK (disposition IN ('proposed', 'accepted', 'rejected', 'conflict', 'superseded')),
    asserted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    reviewed_at TIMESTAMPTZ,
    UNIQUE (id, person_id),
    UNIQUE (id, identity_key_id, person_id),
    UNIQUE (id, identity_key_id, person_id, disposition)
);

CREATE INDEX identity_claims_person_idx ON identity_claims (person_id, asserted_at);

CREATE TABLE identity_assignments (
    identity_key_id UUID PRIMARY KEY REFERENCES identity_keys(id) ON DELETE CASCADE,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    claim_id UUID NOT NULL UNIQUE,
    claim_disposition TEXT NOT NULL DEFAULT 'accepted' CHECK (claim_disposition = 'accepted'),
    assigned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (claim_id, identity_key_id, person_id, claim_disposition)
        REFERENCES identity_claims(id, identity_key_id, person_id, disposition) ON DELETE CASCADE
);

CREATE INDEX identity_assignments_person_idx ON identity_assignments (person_id, identity_key_id);

CREATE TABLE raw_events (
    id UUID PRIMARY KEY,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    external_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    occurred_at TIMESTAMPTZ,
    payload JSONB NOT NULL,
    payload_schema_version INTEGER NOT NULL CHECK (payload_schema_version > 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (forge_instance_id, external_id)
);

CREATE INDEX raw_events_occurred_idx ON raw_events (forge_instance_id, occurred_at);

CREATE TABLE normalized_events (
    id UUID PRIMARY KEY,
    raw_event_id UUID NOT NULL REFERENCES raw_events(id) ON DELETE CASCADE,
    normalizer_version INTEGER NOT NULL CHECK (normalizer_version > 0),
    action_key TEXT NOT NULL,
    kind TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    project TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    is_current BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (raw_event_id, normalizer_version, action_key)
);

CREATE UNIQUE INDEX normalized_events_current_action_idx
    ON normalized_events (raw_event_id, action_key)
    WHERE is_current;

CREATE INDEX normalized_events_timeline_idx
    ON normalized_events (occurred_at DESC, id DESC)
    WHERE is_current;

CREATE TABLE event_people (
    normalized_event_id UUID NOT NULL REFERENCES normalized_events(id) ON DELETE CASCADE,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    occurred_at TIMESTAMPTZ NOT NULL,
    identity_claim_id UUID NOT NULL,
    PRIMARY KEY (normalized_event_id, person_id, role),
    FOREIGN KEY (identity_claim_id, person_id)
        REFERENCES identity_claims(id, person_id) ON DELETE CASCADE
);

CREATE INDEX event_people_timeline_idx
    ON event_people (person_id, occurred_at DESC, normalized_event_id);

CREATE TABLE contribution_counts (
    id UUID PRIMARY KEY,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    activity_date DATE NOT NULL,
    metric TEXT NOT NULL,
    kind TEXT NOT NULL DEFAULT '',
    value BIGINT NOT NULL CHECK (value >= 0),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX contribution_counts_history_idx
    ON contribution_counts (person_id, activity_date, observed_at DESC);

CREATE TABLE sync_runs (
    id UUID PRIMARY KEY,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'partial', 'failed', 'canceled')),
    fetch_report JSONB NOT NULL DEFAULT '{}'::jsonb,
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);

CREATE INDEX sync_runs_person_source_idx
    ON sync_runs (person_id, forge_instance_id, source_key, started_at DESC);

CREATE TABLE sync_coverage (
    id UUID PRIMARY KEY,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    range_start TIMESTAMPTZ NOT NULL,
    range_end TIMESTAMPTZ NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('complete', 'gap')),
    gap_reason TEXT NOT NULL DEFAULT '',
    sync_run_id UUID REFERENCES sync_runs(id) ON DELETE SET NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (range_end > range_start),
    CHECK ((status = 'complete' AND gap_reason = '') OR status = 'gap')
);

CREATE INDEX sync_coverage_lookup_idx
    ON sync_coverage (person_id, forge_instance_id, source_key, range_start, range_end);

CREATE TABLE checkpoints (
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    cursor TEXT NOT NULL DEFAULT '',
    backfill_before TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (person_id, forge_instance_id, source_key)
);

CREATE TABLE jobs (
    id UUID PRIMARY KEY,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    due_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retry_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 10 CHECK (max_attempts > 0),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    fencing_token UUID,
    idempotency_key TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX jobs_idempotency_idx
    ON jobs (person_id, forge_instance_id, source_key, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE INDEX jobs_due_idx ON jobs (COALESCE(retry_at, due_at), created_at)
    WHERE status = 'queued';

CREATE INDEX jobs_expired_lease_idx ON jobs (lease_expires_at)
    WHERE status = 'running';

CREATE TABLE snapshots (
    source TEXT NOT NULL,
    username TEXT NOT NULL,
    data JSONB NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source, username)
);
