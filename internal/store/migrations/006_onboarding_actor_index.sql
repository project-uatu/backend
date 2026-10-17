ALTER TABLE jobs
    ADD COLUMN onboarding_login TEXT NOT NULL DEFAULT '';

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
