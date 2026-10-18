-- Generalize GitHub-named state so new forges need no core migration. The old
-- names remain as updatable views for N-1 binaries during rolling updates and
-- are dropped by a later contract migration.
ALTER TABLE github_accounts RENAME TO source_accounts;
ALTER TABLE source_accounts ALTER COLUMN node_id DROP NOT NULL;
ALTER TABLE source_accounts ALTER COLUMN account_created_at DROP NOT NULL;

CREATE VIEW github_accounts AS
SELECT forge_instance_id, person_id, account_id, node_id, login,
       account_created_at, created_at, updated_at
FROM source_accounts;

ALTER TABLE github_known_repositories RENAME TO known_repositories;

CREATE VIEW github_known_repositories AS
SELECT person_id, forge_instance_id, repository
FROM known_repositories;

-- A boundary gap is a permanent source limit, such as an activity feed that
-- does not reach the start of history. It stays visible as a coverage gap but
-- does not force every later sync to repeat the full backfill.
ALTER TABLE collection_scan_gaps ADD COLUMN boundary boolean NOT NULL DEFAULT false;
