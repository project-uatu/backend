CREATE TABLE job_idempotency_keys (
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (person_id, forge_instance_id, source_key, idempotency_key)
);

INSERT INTO job_idempotency_keys
    (person_id, forge_instance_id, source_key, idempotency_key, job_id)
SELECT person_id, forge_instance_id, source_key, idempotency_key, id
FROM jobs
WHERE idempotency_key IS NOT NULL;

WITH ranked_jobs AS (
    SELECT id, sync_run_id,
           row_number() OVER (
               PARTITION BY person_id, forge_instance_id, source_key
               ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at, id
           ) AS position
    FROM jobs
    WHERE status IN ('queued', 'running')
)
UPDATE sync_runs sr
SET status = 'canceled', finished_at = now()
FROM ranked_jobs r
WHERE r.position > 1 AND r.sync_run_id = sr.id
  AND sr.status IN ('queued', 'running') AND sr.finished_at IS NULL;

WITH ranked_jobs AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY person_id, forge_instance_id, source_key
               ORDER BY CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at, id
           ) AS position
    FROM jobs
    WHERE status IN ('queued', 'running')
)
UPDATE jobs j
SET status = 'canceled', lease_owner = '', lease_expires_at = NULL,
    fencing_token = NULL, retry_at = NULL, rate_limit_reset_at = NULL,
    last_error = 'superseded while consolidating active source work',
    updated_at = now()
FROM ranked_jobs r
WHERE r.position > 1 AND r.id = j.id;

WITH active_jobs AS (
    SELECT DISTINCT ON (person_id, forge_instance_id, source_key)
           id, person_id, forge_instance_id, source_key
    FROM jobs
    WHERE status IN ('queued', 'running')
    ORDER BY person_id, forge_instance_id, source_key,
             CASE WHEN status = 'running' THEN 0 ELSE 1 END, created_at, id
)
UPDATE job_idempotency_keys k
SET job_id = winner.id
FROM jobs loser
JOIN active_jobs winner
  ON winner.person_id = loser.person_id
 AND winner.forge_instance_id = loser.forge_instance_id
 AND winner.source_key = loser.source_key
WHERE k.job_id = loser.id
  AND loser.status = 'canceled'
  AND loser.last_error = 'superseded while consolidating active source work';

DROP INDEX jobs_active_sync_idx;
CREATE UNIQUE INDEX jobs_active_sync_idx
    ON jobs (person_id, forge_instance_id, source_key)
    WHERE status IN ('queued', 'running');

CREATE TABLE collection_scan_gaps (
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    range_start TIMESTAMPTZ NOT NULL,
    range_end TIMESTAMPTZ NOT NULL,
    gap_reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (job_id, range_start, range_end, gap_reason),
    CHECK (range_end > range_start)
);

CREATE TABLE raw_event_normalization_state (
    raw_event_id UUID PRIMARY KEY REFERENCES raw_events(id) ON DELETE CASCADE,
    active_version INTEGER NOT NULL CHECK (active_version > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO raw_event_normalization_state (raw_event_id, active_version)
SELECT raw_event_id, max(normalizer_version)
FROM normalized_events
WHERE is_current
GROUP BY raw_event_id;

CREATE TABLE rate_limit_reservations (
    id UUID PRIMARY KEY,
    bucket_key TEXT NOT NULL REFERENCES rate_limit_buckets(bucket_key) ON DELETE CASCADE,
    job_id UUID NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    fencing_token UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bucket_key, job_id, fencing_token)
);

CREATE INDEX rate_limit_reservations_expiry_idx
    ON rate_limit_reservations (bucket_key, expires_at);
