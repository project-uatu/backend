ALTER TABLE jobs
    ADD COLUMN onboarding_login TEXT NOT NULL DEFAULT '';

WITH pending_onboarding_logins AS (
    SELECT DISTINCT ON (j.id) j.id, ik.normalized_value
    FROM jobs j
    JOIN identity_claims ic
      ON ic.person_id = j.person_id AND ic.method = 'api_onboarding'
    JOIN identity_keys ik
      ON ik.id = ic.identity_key_id
     AND ik.forge_instance_id = j.forge_instance_id
     AND ik.kind = 'login'
    WHERE j.source_key = 'github_history'
      AND j.status IN ('queued', 'running')
      AND j.onboarding_login = ''
      AND NOT EXISTS (
          SELECT 1 FROM github_accounts ga
          WHERE ga.forge_instance_id = j.forge_instance_id
            AND ga.person_id = j.person_id
      )
    ORDER BY j.id, ic.asserted_at, ic.id
)
UPDATE jobs j
SET onboarding_login = pending.normalized_value
FROM pending_onboarding_logins pending
WHERE j.id = pending.id;

CREATE TABLE raw_event_actor_keys (
    raw_event_id UUID NOT NULL REFERENCES raw_events(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    forge_instance_id UUID NOT NULL,
    normalized_value TEXT NOT NULL,
    PRIMARY KEY (raw_event_id, kind, forge_instance_id, normalized_value)
);

CREATE INDEX raw_event_actor_keys_lookup_idx
    ON raw_event_actor_keys (kind, forge_instance_id, normalized_value, raw_event_id);

INSERT INTO raw_event_actor_keys (raw_event_id, kind, forge_instance_id, normalized_value)
SELECT DISTINCT re.id,
       actor->'key'->'identity_key'->>'kind',
       COALESCE(NULLIF(actor->'key'->>'forge_instance_id', '')::uuid,
                '00000000-0000-0000-0000-000000000000'::uuid),
       CASE actor->'key'->'identity_key'->>'kind'
           WHEN 'email' THEN
               split_part(actor->'key'->'identity_key'->>'value', '@', 1) || '@' ||
               lower(split_part(actor->'key'->'identity_key'->>'value', '@', 2))
           WHEN 'login' THEN lower(actor->'key'->'identity_key'->>'value')
           ELSE actor->'key'->'identity_key'->>'value'
       END
FROM raw_events re
CROSS JOIN LATERAL jsonb_array_elements(re.actor_keys) AS actor
WHERE COALESCE(actor->'key'->'identity_key'->>'kind', '') <> ''
  AND COALESCE(actor->'key'->'identity_key'->>'value', '') <> '';
