# Architecture decision records

Create an ADR before implementing a major change: persistent schema or identity
rules, collection contracts, API behavior or authentication, deployment topology,
new source strategies, or persistent cache/retention policy.

1. Copy [the template](template.md) to the next `NNNN-short-title.md`.
2. Describe the context, alternatives, decision, consequences and validation.
   Link the implementation issue and any prerequisites.
3. Start with `Proposed`. Obtain maintainer acceptance before implementation,
   then change the status to `Accepted`.
4. Implement in a separate change after acceptance. An issue or proposed ADR
   alone is not permission to start the major change.
5. Preserve accepted decision history. Supersede an ADR with a new one instead
   of silently changing its decision.

Routine fixes, including correcting this repository's module path, do not need
an ADR unless they change an architectural contract.

## Records

| Record | Status |
|---|---|
| [0001: PostgreSQL and backend-only scope](0001-postgresql-backend-scope.md) | Accepted |
| [0002: Kubernetes collector scheduling](0002-kubernetes-collector.md) | Accepted |
| [0003: Read-only scaffold API](0003-read-only-scaffold-api.md) | Accepted |
| [0004: Person, identity and event data model](0004-person-identity-event-model.md) | Accepted |
| [0005: Resumable bounded collection and normalization](0005-resumable-collection.md) | Accepted |
| [0006: GitHub event enumeration and coverage strategy](0006-github-event-enumeration.md) | Accepted |
| [0007: Versioned person API, authentication and privacy](0007-versioned-person-api.md) | Accepted |
| [0008: Kubernetes delivery contract and separate Helm chart repository](0008-kubernetes-delivery-contract.md) | Accepted |
| [0009: GitLab.com and Codeberg connectors](0009-gitlab-codeberg-connectors.md) | Accepted |
| [0010: Bounded seeded-repository commit history](0010-commit-history.md) | Accepted |
