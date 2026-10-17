CREATE TABLE github_accounts (
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL CHECK (account_id ~ '^[1-9][0-9]*$'),
    node_id TEXT NOT NULL CHECK (node_id <> ''),
    login TEXT NOT NULL CHECK (login <> ''),
    account_created_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (forge_instance_id, account_id),
    UNIQUE (forge_instance_id, person_id)
);

DROP TABLE snapshots;

ALTER TABLE sync_coverage
    ADD COLUMN coverage_scope TEXT NOT NULL DEFAULT 'account_wide';
