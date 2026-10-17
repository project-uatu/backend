package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

// APIPerson contains profile fields safe for the authenticated API.
type APIPerson struct {
	ID          uuid.UUID
	DisplayName *string
	CreatedAt   time.Time
}

// APISourceCoverage summarizes the recorded coverage for one source.
type APISourceCoverage struct {
	Source     string
	Status     string
	From       *time.Time
	To         *time.Time
	GapCount   int64
	RecordedAt time.Time
}

// APIJob contains collection status without internal lease or error details.
type APIJob struct {
	ID          uuid.UUID
	PersonID    uuid.UUID
	Source      string
	Status      string
	Attempts    int
	MaxAttempts int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ActivityCursor identifies the last row in a deterministic timeline page.
type ActivityCursor struct {
	OccurredAt time.Time
	EventID    uuid.UUID
	Role       string
}

// ActivityFilter selects timeline rows.
type ActivityFilter struct {
	PersonID uuid.UUID
	From     time.Time
	To       time.Time
	Source   string
	Kind     string
	After    *ActivityCursor
	Limit    int
}

// ActivityPage contains a bounded timeline page and whether another page exists.
type ActivityPage struct {
	Entries []TimelineEntry
	HasMore bool
}

// PersonSummary is a count of normalized actions in one year, source and kind.
type PersonSummary struct {
	Year   int
	Source string
	Kind   string
	Count  int64
}

// ProjectSummary is a distinct project and its normalized action count.
type ProjectSummary struct {
	Project string
	Count   int64
}

// APIIdentity is a login or email submitted through the authenticated API.
type APIIdentity struct {
	Kind  identity.Kind
	Value string
}

// ErrOnboardingResolutionPending indicates the required login cannot be removed yet.
var ErrOnboardingResolutionPending = errors.New("GitHub account resolution is pending")

// GitHubForge returns the canonical GitHub.com forge instance.
func (s *Store) GitHubForge(ctx context.Context) (ForgeInstance, error) {
	forge, err := s.CreateForgeInstance(ctx, "github", "https://github.com")
	if err != nil {
		return ForgeInstance{}, fmt.Errorf("load GitHub forge: %w", err)
	}
	return forge, nil
}

// CreateGitHubPerson creates a person, assigns submitted identifiers, and
// queues account resolution and the initial history backfill atomically.
func (s *Store) CreateGitHubPerson(ctx context.Context, displayName, login string, emails []string) (person Person, job CollectionJob, createErr error) {
	if login == "" {
		return Person{}, CollectionJob{}, errors.New("GitHub login is required")
	}
	for _, email := range emails {
		if _, err := identity.Normalize("github", identity.KindEmail, email); err != nil {
			return Person{}, CollectionJob{}, fmt.Errorf("invalid email identity: %w", err)
		}
	}
	forge, err := s.GitHubForge(ctx)
	if err != nil {
		return Person{}, CollectionJob{}, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Person{}, CollectionJob{}, fmt.Errorf("begin person onboarding: %w", err)
	}
	defer rollbackTx(ctx, tx, "person onboarding", &createErr)
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return Person{}, CollectionJob{}, err
	}
	person = Person{ID: uuid.New()}
	if displayName != "" {
		person.DisplayName = &displayName
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO persons (id, display_name)
		VALUES ($1, $2)
		RETURNING created_at, updated_at`, person.ID, person.DisplayName).
		Scan(&person.CreatedAt, &person.UpdatedAt); err != nil {
		return Person{}, CollectionJob{}, fmt.Errorf("create API person: %w", err)
	}
	for _, submitted := range append([]struct {
		kind  identity.Kind
		value string
	}{{kind: identity.KindLogin, value: login}}, emailIdentifiers(emails)...) {
		if err := claimAPIIdentity(ctx, tx, forge.ID, person.ID, submitted.kind, submitted.value); err != nil {
			return Person{}, CollectionJob{}, err
		}
		if err := queueIdentityRenormalization(ctx, tx, forge.ID, APIIdentity{
			Kind: submitted.kind, Value: submitted.value,
		}); err != nil {
			return Person{}, CollectionJob{}, err
		}
	}
	job = CollectionJob{
		ID: uuid.New(), PersonID: person.ID, ForgeInstanceID: forge.ID,
		SourceKey: SourceGitHubHistory, ScanMode: "backfill",
		OnboardingLogin: strings.ToLower(login),
		Window:          event.Window{To: time.Now().UTC()}, MaxAttempts: 10,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs
			(id, person_id, forge_instance_id, source_key, status, due_at, max_attempts,
			 cursor, scan_mode, onboarding_login, window_start, window_end)
		VALUES ($1, $2, $3, $4, 'queued', now(), $5, '', $6, $7, NULL, $8)
		RETURNING due_at`,
		job.ID, job.PersonID, job.ForgeInstanceID, job.SourceKey,
		job.MaxAttempts, job.ScanMode, job.OnboardingLogin, job.Window.To).Scan(&job.DueAt)
	if err != nil {
		return Person{}, CollectionJob{}, fmt.Errorf("enqueue initial GitHub account resolution: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Person{}, CollectionJob{}, fmt.Errorf("commit person onboarding: %w", err)
	}
	return person, job, nil
}

func emailIdentifiers(emails []string) []struct {
	kind  identity.Kind
	value string
} {
	identifiers := make([]struct {
		kind  identity.Kind
		value string
	}, 0, len(emails))
	for _, email := range emails {
		identifiers = append(identifiers, struct {
			kind  identity.Kind
			value string
		}{kind: identity.KindEmail, value: email})
	}
	return identifiers
}

func claimAPIIdentity(ctx context.Context, tx pgx.Tx, forgeID, personID uuid.UUID, kind identity.Kind, value string) error {
	normalized, err := identity.Normalize("github", kind, value)
	if err != nil {
		return fmt.Errorf("normalize API identity: %w", err)
	}
	var forge any = forgeID
	if kind == identity.KindEmail {
		forge = nil
	}
	keyID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_keys (id, forge_instance_id, kind, value, normalized_value)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		keyID, forge, kind, value, normalized); err != nil {
		return fmt.Errorf("save API identity key: %w", err)
	}
	if kind == identity.KindEmail {
		err = tx.QueryRow(ctx, `
			SELECT id FROM identity_keys WHERE forge_instance_id IS NULL
			  AND kind = $1 AND normalized_value = $2 FOR UPDATE`,
			kind, normalized).Scan(&keyID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id FROM identity_keys WHERE forge_instance_id = $1
			  AND kind = $2 AND normalized_value = $3 FOR UPDATE`,
			forgeID, kind, normalized).Scan(&keyID)
	}
	if err != nil {
		return fmt.Errorf("load API identity key: %w", err)
	}
	var assignedPersonID uuid.UUID
	err = tx.QueryRow(ctx,
		"SELECT person_id FROM identity_assignments WHERE identity_key_id = $1", keyID).
		Scan(&assignedPersonID)
	switch {
	case err == nil:
		if assignedPersonID == personID {
			return nil
		}
		return fmt.Errorf("%w: identifier is already assigned", ErrIdentityConflict)
	case !errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("check API identity assignment: %w", err)
	}
	claimID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_claims
			(id, identity_key_id, person_id, method, asserted_by, evidence, disposition)
		VALUES ($1, $2, $3, 'api_onboarding', 'api', $4, 'accepted')`,
		claimID, keyID, personID, json.RawMessage(`{"source":"authenticated_api"}`)); err != nil {
		return fmt.Errorf("record API identity claim: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_assignments (identity_key_id, person_id, claim_id, claim_disposition)
		VALUES ($1, $2, $3, 'accepted')`, keyID, personID, claimID); err != nil {
		return fmt.Errorf("assign API identity: %w", err)
	}
	return nil
}

// UpdatePersonIdentifiers applies audited additions and removals and queues
// affected raw events for re-normalization in the same transaction.
func (s *Store) UpdatePersonIdentifiers(ctx context.Context, personID, forgeID uuid.UUID, add, remove []APIIdentity) (updateErr error) {
	if personID == uuid.Nil || forgeID == uuid.Nil {
		return errors.New("identifier update requires a person and forge")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin API identifier update: %w", err)
	}
	defer rollbackTx(ctx, tx, "API identifier update", &updateErr)
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return err
	}
	var found bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM persons WHERE id = $1)", personID).Scan(&found); err != nil {
		return fmt.Errorf("check API person: %w", err)
	}
	if !found {
		return fmt.Errorf("person %s: %w", personID, ErrNotFound)
	}
	for _, item := range add {
		if item.Kind != identity.KindLogin && item.Kind != identity.KindEmail {
			return errors.New("API identifiers may only be GitHub logins or emails")
		}
		if err := claimAPIIdentity(ctx, tx, forgeID, personID, item.Kind, item.Value); err != nil {
			return err
		}
		if err := queueIdentityRenormalization(ctx, tx, forgeID, item); err != nil {
			return err
		}
	}
	for _, item := range remove {
		if item.Kind == identity.KindLogin {
			var pendingJobID uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT j.id FROM jobs j
				WHERE j.person_id = $1 AND j.forge_instance_id = $2
				  AND j.source_key = $3 AND j.status IN ('queued', 'running')
				  AND j.onboarding_login = $4
				  AND NOT EXISTS (
				      SELECT 1 FROM github_accounts ga
				      WHERE ga.forge_instance_id = j.forge_instance_id
				        AND ga.person_id = j.person_id
				  )
				FOR UPDATE OF j`,
				personID, forgeID, SourceGitHubHistory, strings.ToLower(item.Value)).Scan(&pendingJobID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("check pending onboarding login: %w", err)
			}
			if err == nil {
				return ErrOnboardingResolutionPending
			}
		}
		keyID, claimID, assigned, err := lockAssignedAPIIdentity(ctx, tx, forgeID, personID, item)
		if err != nil {
			return err
		}
		if !assigned {
			continue
		}
		if _, err := tx.Exec(ctx, "DELETE FROM event_people WHERE identity_claim_id = $1", claimID); err != nil {
			return fmt.Errorf("remove event attribution for identity: %w", err)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM identity_assignments WHERE identity_key_id = $1", keyID); err != nil {
			return fmt.Errorf("unassign API identity: %w", err)
		}
		if _, err := tx.Exec(ctx, "UPDATE identity_claims SET disposition = 'superseded' WHERE id = $1", claimID); err != nil {
			return fmt.Errorf("supersede removed identity claim: %w", err)
		}
		evidence, err := json.Marshal(map[string]string{"action": "identifier_removed"})
		if err != nil {
			return fmt.Errorf("encode identity removal evidence: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity_claims
				(id, identity_key_id, person_id, method, asserted_by, evidence, disposition)
			VALUES ($1, $2, $3, 'api_removal', 'api', $4, 'rejected')`,
			uuid.New(), keyID, personID, evidence); err != nil {
			return fmt.Errorf("record identity removal: %w", err)
		}
		if err := queueIdentityRenormalization(ctx, tx, forgeID, item); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit API identifier update: %w", err)
	}
	return nil
}

func lockAssignedAPIIdentity(ctx context.Context, tx pgx.Tx, forgeID, personID uuid.UUID, item APIIdentity) (uuid.UUID, uuid.UUID, bool, error) {
	if item.Kind != identity.KindLogin && item.Kind != identity.KindEmail {
		return uuid.Nil, uuid.Nil, false, errors.New("API identifiers may only be GitHub logins or emails")
	}
	normalized, err := identity.Normalize("github", item.Kind, item.Value)
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("normalize API identity: %w", err)
	}
	var keyID, assignedPersonID, claimID uuid.UUID
	if item.Kind == identity.KindEmail {
		err = tx.QueryRow(ctx, `
			SELECT ik.id, ia.person_id, ia.claim_id FROM identity_keys ik
			JOIN identity_assignments ia ON ia.identity_key_id = ik.id
			WHERE ik.forge_instance_id IS NULL AND ik.kind = $1 AND ik.normalized_value = $2
			FOR UPDATE OF ik, ia`, item.Kind, normalized).Scan(&keyID, &assignedPersonID, &claimID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT ik.id, ia.person_id, ia.claim_id FROM identity_keys ik
			JOIN identity_assignments ia ON ia.identity_key_id = ik.id
			WHERE ik.forge_instance_id = $1 AND ik.kind = $2 AND ik.normalized_value = $3
			FOR UPDATE OF ik, ia`, forgeID, item.Kind, normalized).Scan(&keyID, &assignedPersonID, &claimID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("load assigned API identity: %w", err)
	}
	if assignedPersonID != personID {
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("%w: identifier belongs to another person", ErrIdentityConflict)
	}
	return keyID, claimID, true, nil
}

func queueIdentityRenormalization(ctx context.Context, tx pgx.Tx, forgeID uuid.UUID, item APIIdentity) error {
	normalized, err := identity.Normalize("github", item.Kind, item.Value)
	if err != nil {
		return fmt.Errorf("normalize API identity: %w", err)
	}
	scopeID := uuid.Nil
	if item.Kind != identity.KindEmail {
		scopeID = forgeID
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO normalization_jobs (raw_event_id, normalizer_version)
		SELECT re.id, COALESCE(state.active_version, 1)
		FROM raw_events re
		LEFT JOIN raw_event_normalization_state state ON state.raw_event_id = re.id
		JOIN raw_event_actor_keys actor ON actor.raw_event_id = re.id
		WHERE actor.kind = $1 AND actor.forge_instance_id = $3
		  AND actor.normalized_value = $2
		ON CONFLICT (raw_event_id, normalizer_version)
		DO UPDATE SET status = 'queued', due_at = now(), retry_at = NULL,
		              attempts = 0, lease_owner = '', lease_expires_at = NULL,
		              fencing_token = NULL, last_error = '', updated_at = now()`,
		item.Kind, normalized, scopeID)
	if err != nil {
		return fmt.Errorf("queue identity-driven event normalization: %w", err)
	}
	return nil
}

// ListAPIPersons returns people ordered by UUID for stable cursor pagination.
func (s *Store) ListAPIPersons(ctx context.Context, search string, after uuid.UUID, limit int) ([]APIPerson, bool, error) {
	if limit < 1 || limit > 100 {
		return nil, false, errors.New("person page limit must be between 1 and 100")
	}
	rows, err := s.db.Query(ctx, `
		SELECT p.id, p.display_name, p.created_at
		FROM persons p
		WHERE p.id > $1
		  AND ($2 = '' OR position(lower($2) in lower(COALESCE(p.display_name, ''))) > 0
		       OR EXISTS (
		           SELECT 1 FROM identity_assignments search_ia
		           JOIN identity_keys search_ik ON search_ik.id = search_ia.identity_key_id
		           WHERE search_ia.person_id = p.id AND search_ik.kind = 'login'
		             AND position(lower($2) in lower(search_ik.value)) > 0
		       ))
		GROUP BY p.id
		ORDER BY p.id
		LIMIT $3`, after, search, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("list API persons: %w", err)
	}
	defer rows.Close()
	persons := make([]APIPerson, 0, limit+1)
	for rows.Next() {
		var person APIPerson
		if err := rows.Scan(&person.ID, &person.DisplayName, &person.CreatedAt); err != nil {
			return nil, false, fmt.Errorf("scan API person: %w", err)
		}
		person.CreatedAt = person.CreatedAt.UTC()
		persons = append(persons, person)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate API persons: %w", err)
	}
	hasMore := len(persons) > limit
	if hasMore {
		persons = persons[:limit]
	}
	return persons, hasMore, nil
}

// APIPersonByID returns one profile without email addresses or raw identities.
func (s *Store) APIPersonByID(ctx context.Context, personID uuid.UUID) (APIPerson, error) {
	var person APIPerson
	err := s.db.QueryRow(ctx, `
		SELECT p.id, p.display_name, p.created_at
		FROM persons p
		WHERE p.id = $1`, personID).
		Scan(&person.ID, &person.DisplayName, &person.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIPerson{}, fmt.Errorf("person %s: %w", personID, ErrNotFound)
	}
	if err != nil {
		return APIPerson{}, fmt.Errorf("load API person: %w", err)
	}
	person.CreatedAt = person.CreatedAt.UTC()
	return person, nil
}

// PersonSourceCoverage returns bounded per-source coverage summaries.
func (s *Store) PersonSourceCoverage(ctx context.Context, personID uuid.UUID) ([]APISourceCoverage, error) {
	rows, err := s.db.Query(ctx, `
		SELECT source_key,
		       CASE WHEN count(*) FILTER (WHERE status = 'gap') > 0 THEN 'partial' ELSE 'complete' END,
		       min(range_start) FILTER (WHERE status = 'complete'),
		       max(range_end) FILTER (WHERE status = 'complete'),
		       count(*) FILTER (WHERE status = 'gap'),
		       max(recorded_at)
		FROM sync_coverage
		WHERE person_id = $1
		GROUP BY source_key
		ORDER BY source_key`, personID)
	if err != nil {
		return nil, fmt.Errorf("query API source coverage: %w", err)
	}
	defer rows.Close()
	coverage := make([]APISourceCoverage, 0)
	for rows.Next() {
		var item APISourceCoverage
		if err := rows.Scan(&item.Source, &item.Status, &item.From, &item.To, &item.GapCount, &item.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan API source coverage: %w", err)
		}
		if item.From != nil {
			*item.From = item.From.UTC()
		}
		if item.To != nil {
			*item.To = item.To.UTC()
		}
		item.RecordedAt = item.RecordedAt.UTC()
		coverage = append(coverage, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API source coverage: %w", err)
	}
	return coverage, nil
}

// APIJobByID returns public status fields for one durable collection job.
func (s *Store) APIJobByID(ctx context.Context, jobID uuid.UUID) (APIJob, error) {
	var job APIJob
	err := s.db.QueryRow(ctx, `
		SELECT id, person_id, source_key, status, attempts, max_attempts, created_at, updated_at
		FROM jobs WHERE id = $1`, jobID).
		Scan(&job.ID, &job.PersonID, &job.Source, &job.Status, &job.Attempts,
			&job.MaxAttempts, &job.CreatedAt, &job.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIJob{}, fmt.Errorf("job %s: %w", jobID, ErrNotFound)
	}
	if err != nil {
		return APIJob{}, fmt.Errorf("load API job: %w", err)
	}
	job.CreatedAt, job.UpdatedAt = job.CreatedAt.UTC(), job.UpdatedAt.UTC()
	return job, nil
}

// ActivityPage returns a keyset-paginated timeline page.
func (s *Store) ActivityPage(ctx context.Context, filter ActivityFilter) (ActivityPage, error) {
	if filter.PersonID == uuid.Nil || filter.From.IsZero() || filter.To.IsZero() ||
		!filter.From.Before(filter.To) || filter.Limit < 1 || filter.Limit > 500 {
		return ActivityPage{}, errors.New("activity query requires a person, range and limit from 1 to 500")
	}
	var afterTime any
	var afterID any
	var afterRole any
	if filter.After != nil {
		afterTime, afterID, afterRole = filter.After.OccurredAt, filter.After.EventID, filter.After.Role
	}
	rows, err := s.db.Query(ctx, `
		SELECT ne.id, ne.kind, ep.role, ep.occurred_at, ne.project, ne.title, ne.url
		FROM event_people ep
		JOIN normalized_events ne ON ne.id = ep.normalized_event_id
		JOIN identity_assignments ia
		  ON ia.claim_id = ep.identity_claim_id AND ia.person_id = ep.person_id
		JOIN raw_events re ON re.id = ne.raw_event_id
		WHERE ep.person_id = $1 AND ep.occurred_at >= $2 AND ep.occurred_at < $3
		  AND ($4 = '' OR re.source_key = $4)
		  AND ($5 = '' OR ne.kind = $5)
		  AND ($6::timestamptz IS NULL
		       OR ep.occurred_at < $6
		       OR (ep.occurred_at = $6 AND ne.id < $7)
		       OR (ep.occurred_at = $6 AND ne.id = $7 AND ep.role > $8))
		  AND ne.is_current
		ORDER BY ep.occurred_at DESC, ne.id DESC, ep.role
		LIMIT $9`, filter.PersonID, filter.From.UTC(), filter.To.UTC(),
		filter.Source, filter.Kind, afterTime, afterID, afterRole, filter.Limit+1)
	if err != nil {
		return ActivityPage{}, fmt.Errorf("query API activity: %w", err)
	}
	defer rows.Close()
	page := ActivityPage{Entries: make([]TimelineEntry, 0, filter.Limit+1)}
	for rows.Next() {
		var entry TimelineEntry
		if err := rows.Scan(&entry.EventID, &entry.Kind, &entry.Role, &entry.OccurredAt,
			&entry.Project, &entry.Title, &entry.URL); err != nil {
			return ActivityPage{}, fmt.Errorf("scan API activity: %w", err)
		}
		entry.OccurredAt = entry.OccurredAt.UTC()
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ActivityPage{}, fmt.Errorf("iterate API activity: %w", err)
	}
	page.HasMore = len(page.Entries) > filter.Limit
	if page.HasMore {
		page.Entries = page.Entries[:filter.Limit]
	}
	return page, nil
}

// PersonSummaryByYear returns event counts grouped by UTC year, source and kind.
func (s *Store) PersonSummaryByYear(ctx context.Context, personID uuid.UUID) ([]PersonSummary, error) {
	rows, err := s.db.Query(ctx, `
		SELECT extract(year FROM ep.occurred_at AT TIME ZONE 'UTC')::integer,
		       re.source_key, ne.kind, count(DISTINCT ne.id)
		FROM event_people ep
		JOIN normalized_events ne ON ne.id = ep.normalized_event_id
		JOIN identity_assignments ia
		  ON ia.claim_id = ep.identity_claim_id AND ia.person_id = ep.person_id
		JOIN raw_events re ON re.id = ne.raw_event_id
		WHERE ep.person_id = $1 AND ne.is_current
		GROUP BY 1, 2, 3
		ORDER BY 1 DESC, 2, 3`, personID)
	if err != nil {
		return nil, fmt.Errorf("query API person summary: %w", err)
	}
	defer rows.Close()
	summary := make([]PersonSummary, 0)
	for rows.Next() {
		var item PersonSummary
		if err := rows.Scan(&item.Year, &item.Source, &item.Kind, &item.Count); err != nil {
			return nil, fmt.Errorf("scan API person summary: %w", err)
		}
		summary = append(summary, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate API person summary: %w", err)
	}
	return summary, nil
}

// PersonProjects returns a keyset-paginated list of contributed projects.
func (s *Store) PersonProjects(ctx context.Context, personID uuid.UUID, from, to *time.Time, after string, limit int) ([]ProjectSummary, bool, error) {
	if personID == uuid.Nil || limit < 1 || limit > 500 ||
		(from != nil && to == nil) || (from == nil && to != nil) ||
		(from != nil && !from.Before(*to)) {
		return nil, false, errors.New("project query requires a person, a valid range and a limit from 1 to 500")
	}
	var rangeFrom, rangeTo any
	if from != nil {
		rangeFrom, rangeTo = from.UTC(), to.UTC()
	}
	rows, err := s.db.Query(ctx, `
		SELECT ne.project, count(DISTINCT ne.id)
		FROM event_people ep
		JOIN normalized_events ne ON ne.id = ep.normalized_event_id
		JOIN identity_assignments ia
		  ON ia.claim_id = ep.identity_claim_id AND ia.person_id = ep.person_id
		WHERE ep.person_id = $1 AND ne.is_current AND ne.project <> ''
		  AND ($2::timestamptz IS NULL OR ep.occurred_at >= $2)
		  AND ($3::timestamptz IS NULL OR ep.occurred_at < $3)
		  AND ne.project > $4
		GROUP BY ne.project
		ORDER BY ne.project
		LIMIT $5`, personID, rangeFrom, rangeTo, after, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("query API projects: %w", err)
	}
	defer rows.Close()
	projects := make([]ProjectSummary, 0, limit+1)
	for rows.Next() {
		var project ProjectSummary
		if err := rows.Scan(&project.Project, &project.Count); err != nil {
			return nil, false, fmt.Errorf("scan API project: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate API projects: %w", err)
	}
	hasMore := len(projects) > limit
	if hasMore {
		projects = projects[:limit]
	}
	return projects, hasMore, nil
}
