CREATE TABLE github_known_repositories (
    person_id UUID NOT NULL REFERENCES persons(id) ON DELETE CASCADE,
    forge_instance_id UUID NOT NULL REFERENCES forge_instances(id) ON DELETE CASCADE,
    repository TEXT NOT NULL CHECK (repository <> ''),
    PRIMARY KEY (person_id, forge_instance_id, repository)
);
