ALTER TABLE raw_events
    ADD COLUMN source_key TEXT NOT NULL DEFAULT '',
    ADD COLUMN actor_keys JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE checkpoints
    ADD COLUMN incremental_cursor TEXT NOT NULL DEFAULT '',
    ADD COLUMN completed_through TIMESTAMPTZ,
    ADD COLUMN backfill_complete BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE jobs
    ADD COLUMN scan_mode TEXT NOT NULL DEFAULT 'backfill'
        CHECK (scan_mode IN ('backfill', 'incremental')),
    ADD COLUMN window_start TIMESTAMPTZ,
    ADD COLUMN window_end TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN cursor TEXT NOT NULL DEFAULT '',
    ADD COLUMN sync_run_id UUID REFERENCES sync_runs(id) ON DELETE SET NULL,
    ADD COLUMN rate_limit_reset_at TIMESTAMPTZ;

CREATE UNIQUE INDEX jobs_active_sync_idx
    ON jobs (person_id, forge_instance_id, source_key)
    WHERE status = 'queued';

CREATE TABLE normalization_jobs (
    raw_event_id UUID NOT NULL REFERENCES raw_events(id) ON DELETE CASCADE,
    normalizer_version INTEGER NOT NULL CHECK (normalizer_version > 0),
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    due_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    retry_at TIMESTAMPTZ,
    attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    max_attempts INTEGER NOT NULL DEFAULT 10 CHECK (max_attempts > 0),
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at TIMESTAMPTZ,
    fencing_token UUID,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (raw_event_id, normalizer_version)
);

CREATE INDEX normalization_jobs_due_idx
    ON normalization_jobs (COALESCE(retry_at, due_at), created_at)
    WHERE status = 'queued';

CREATE INDEX normalization_jobs_expired_lease_idx
    ON normalization_jobs (lease_expires_at)
    WHERE status = 'running';

CREATE TABLE rate_limit_buckets (
    bucket_key TEXT PRIMARY KEY,
    remaining INTEGER CHECK (remaining IS NULL OR remaining >= 0),
    reset_at TIMESTAMPTZ,
    in_flight INTEGER NOT NULL DEFAULT 0 CHECK (in_flight >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
