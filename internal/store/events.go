package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const eventIdentityLockID int64 = 0x554154554C494E4B

// lockEventIdentityChanges serializes identity assignment changes with
// event-person link writes and copies, so links are never written against,
// copied from or left on a stale assignment. The lock is global and held only
// for short database transactions that perform no upstream I/O.
func lockEventIdentityChanges(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", eventIdentityLockID); err != nil {
		return fmt.Errorf("lock event identity changes: %w", err)
	}
	return nil
}

// RawEventInput is a source event before normalization.
type RawEventInput struct {
	ForgeInstanceID      uuid.UUID
	ExternalID           string
	Kind                 string
	OccurredAt           *time.Time
	Payload              json.RawMessage
	PayloadSchemaVersion int
}

// NormalizedActionInput is one query-facing contribution action.
type NormalizedActionInput struct {
	ActionKey  string
	Kind       string
	OccurredAt time.Time
	Project    string
	Title      string
	URL        string
}

// EventPersonInput associates an event action with a person and role.
type EventPersonInput struct {
	PersonID        uuid.UUID
	Role            string
	IdentityClaimID *uuid.UUID
}

// TimelineEntry is a contribution action associated with a person.
type TimelineEntry struct {
	EventID    uuid.UUID
	Kind       string
	Role       string
	OccurredAt time.Time
	Project    string
	Title      string
	URL        string
}

// TimeBucket is a supported contribution timeline interval.
type TimeBucket string

const (
	TimeBucketDay   TimeBucket = "day"
	TimeBucketWeek  TimeBucket = "week"
	TimeBucketMonth TimeBucket = "month"
	TimeBucketYear  TimeBucket = "year"
)

// TimelineBucket contains a contribution-action count for one UTC time bucket
// and kind.
type TimelineBucket struct {
	StartsAt time.Time
	Kind     string
	Count    int64
}

// CoverageInterval describes which part of a requested source history was
// completed or known to have a gap.
type CoverageInterval struct {
	SourceKey  string
	StartsAt   time.Time
	EndsAt     time.Time
	Status     string
	GapReason  string
	RecordedAt time.Time
}

// SyncRun is one attempt to collect a source for a person.
type SyncRun struct {
	ID              uuid.UUID
	PersonID        uuid.UUID
	ForgeInstanceID uuid.UUID
	SourceKey       string
	StartedAt       time.Time
	FinishedAt      *time.Time
	Status          string
	FetchReport     json.RawMessage
}

// ContributionCountInput is a source-reported aggregate observation.
type ContributionCountInput struct {
	PersonID        uuid.UUID
	ForgeInstanceID uuid.UUID
	ActivityDate    time.Time
	Metric          string
	Kind            string
	Value           int64
	ObservedAt      time.Time
}

// ContributionCount is a source-reported aggregate observation.
type ContributionCount struct {
	PersonID        uuid.UUID
	ForgeInstanceID uuid.UUID
	ActivityDate    time.Time
	Metric          string
	Kind            string
	Value           int64
	ObservedAt      time.Time
}

// SaveRawEvent inserts or refreshes a source event, deduplicated by instance
// and external ID.
func (s *Store) SaveRawEvent(ctx context.Context, input RawEventInput) (uuid.UUID, error) {
	if input.ForgeInstanceID == uuid.Nil || input.ExternalID == "" || input.Kind == "" {
		return uuid.Nil, errors.New("forge instance, external ID, and event kind are required")
	}
	if !json.Valid(input.Payload) || input.PayloadSchemaVersion < 1 {
		return uuid.Nil, errors.New("raw event payload must be valid JSON with a positive schema version")
	}
	eventID := uuid.New()
	err := s.db.QueryRow(ctx, `
		INSERT INTO raw_events
			(id, forge_instance_id, external_id, kind, occurred_at, payload, payload_schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (forge_instance_id, external_id)
		DO UPDATE SET kind = EXCLUDED.kind,
		              occurred_at = COALESCE(EXCLUDED.occurred_at, raw_events.occurred_at),
		              payload = EXCLUDED.payload,
		              payload_schema_version = EXCLUDED.payload_schema_version,
		              observed_at = now()
		RETURNING id`, eventID, input.ForgeInstanceID, input.ExternalID, input.Kind,
		input.OccurredAt, input.Payload, input.PayloadSchemaVersion).Scan(&eventID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("save raw event %s: %w", input.ExternalID, err)
	}
	return eventID, nil
}

// ReplaceNormalizedActions marks the prior actions for a raw event as
// non-current and inserts or refreshes the actions for one normalizer version.
// Existing action timestamps for the same normalizer version are preserved.
func (s *Store) ReplaceNormalizedActions(ctx context.Context, rawEventID uuid.UUID, version int, actions []NormalizedActionInput) (actionErr error) {
	if version < 1 {
		return errors.New("normalizer version must be positive")
	}
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if action.ActionKey == "" || action.Kind == "" || action.OccurredAt.IsZero() {
			return errors.New("normalized actions require an action key, kind, and occurrence time")
		}
		if _, ok := seen[action.ActionKey]; ok {
			return fmt.Errorf("duplicate normalized action key %q", action.ActionKey)
		}
		seen[action.ActionKey] = struct{}{}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event normalization: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			actionErr = errors.Join(actionErr, fmt.Errorf("rollback event normalization: %w", err))
		}
	}()
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return err
	}
	var lockedEventID uuid.UUID
	if err := tx.QueryRow(ctx,
		"SELECT id FROM raw_events WHERE id = $1 FOR UPDATE", rawEventID).Scan(&lockedEventID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("raw event %s: %w", rawEventID, ErrNotFound)
		}
		return fmt.Errorf("lock raw event: %w", err)
	}
	var activeVersion int
	err = tx.QueryRow(ctx, `
		SELECT active_version FROM raw_event_normalization_state
		WHERE raw_event_id = $1 FOR UPDATE`, rawEventID).Scan(&activeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
			INSERT INTO raw_event_normalization_state (raw_event_id, active_version)
			VALUES ($1, $2) ON CONFLICT (raw_event_id) DO NOTHING`, rawEventID, version); err != nil {
			return fmt.Errorf("initialize active normalizer version: %w", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT active_version FROM raw_event_normalization_state
			WHERE raw_event_id = $1 FOR UPDATE`, rawEventID).Scan(&activeVersion); err != nil {
			return fmt.Errorf("load initialized normalizer version: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("load active normalizer version: %w", err)
	}
	activate := version >= activeVersion
	if activate && version > activeVersion {
		if _, err := tx.Exec(ctx, `
			UPDATE raw_event_normalization_state
			SET active_version = $2, updated_at = now() WHERE raw_event_id = $1`,
			rawEventID, version); err != nil {
			return fmt.Errorf("advance active normalizer version: %w", err)
		}
	}
	type personLink struct {
		personID        uuid.UUID
		role            string
		identityClaimID uuid.UUID
	}
	previousLinks := make(map[string][]personLink)
	rows, err := tx.Query(ctx, `
		SELECT ne.action_key, ep.person_id, ep.role, ep.identity_claim_id
		FROM normalized_events ne
		JOIN event_people ep ON ep.normalized_event_id = ne.id
		WHERE ne.raw_event_id = $1
		  AND (ne.is_current OR ne.normalizer_version = $2)
		ORDER BY ne.action_key, ep.person_id, ep.role
		FOR UPDATE OF ne`, rawEventID, version)
	if err != nil {
		return fmt.Errorf("load current event person links: %w", err)
	}
	for rows.Next() {
		var actionKey string
		var link personLink
		if err := rows.Scan(&actionKey, &link.personID, &link.role, &link.identityClaimID); err != nil {
			rows.Close()
			return fmt.Errorf("scan current event person link: %w", err)
		}
		previousLinks[actionKey] = append(previousLinks[actionKey], link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate current event person links: %w", err)
	}
	rows.Close()
	if activate {
		if _, err := tx.Exec(ctx,
			"UPDATE normalized_events SET is_current = false WHERE raw_event_id = $1 AND is_current",
			rawEventID); err != nil {
			return fmt.Errorf("mark prior normalized actions non-current: %w", err)
		}
	}
	for _, action := range actions {
		var normalizedEventID uuid.UUID
		var normalizedOccurredAt time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO normalized_events
				(id, raw_event_id, normalizer_version, action_key, kind, occurred_at, project, title, url, is_current)
			VALUES ($1, $2, $3, $4, $5,
				COALESCE((SELECT occurred_at FROM normalized_events
				          WHERE raw_event_id = $2 AND action_key = $4
				          ORDER BY created_at, normalizer_version LIMIT 1), $6),
				$7, $8, $9, $10)
			ON CONFLICT (raw_event_id, normalizer_version, action_key)
			DO UPDATE SET kind = EXCLUDED.kind,
			              project = EXCLUDED.project,
			              title = EXCLUDED.title,
			              url = EXCLUDED.url,
			              is_current = EXCLUDED.is_current
			RETURNING id, occurred_at`,
			uuid.New(), rawEventID, version, action.ActionKey, action.Kind, action.OccurredAt,
			action.Project, action.Title, action.URL, activate).Scan(&normalizedEventID, &normalizedOccurredAt)
		if err != nil {
			return fmt.Errorf("save normalized action %s: %w", action.ActionKey, err)
		}
		for _, link := range previousLinks[action.ActionKey] {
			_, err = tx.Exec(ctx, `
			INSERT INTO event_people
				(normalized_event_id, person_id, role, occurred_at, identity_claim_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (normalized_event_id, person_id, role)
			DO UPDATE SET occurred_at = EXCLUDED.occurred_at,
			              identity_claim_id = EXCLUDED.identity_claim_id`,
				normalizedEventID, link.personID, link.role, normalizedOccurredAt, link.identityClaimID)
			if err != nil {
				return fmt.Errorf("carry event person links for action %s: %w", action.ActionKey, err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event normalization: %w", err)
	}
	return nil
}

// ReplaceEventPeople replaces the person-role links for one current action.
func (s *Store) ReplaceEventPeople(ctx context.Context, eventID uuid.UUID, people []EventPersonInput) (peopleErr error) {
	seen := make(map[string]struct{}, len(people))
	for _, person := range people {
		if person.PersonID == uuid.Nil || person.Role == "" ||
			person.IdentityClaimID == nil || *person.IdentityClaimID == uuid.Nil {
			return errors.New("event person, role, and accepted identity claim are required")
		}
		key := person.PersonID.String() + "\x00" + person.Role
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate event person role %q", person.Role)
		}
		seen[key] = struct{}{}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin event person replacement: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			peopleErr = errors.Join(peopleErr, fmt.Errorf("rollback event person replacement: %w", err))
		}
	}()
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return err
	}
	var occurredAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT occurred_at FROM normalized_events
		WHERE id = $1 AND is_current FOR UPDATE`, eventID).Scan(&occurredAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("current normalized event %s: %w", eventID, ErrNotFound)
		}
		return fmt.Errorf("load current normalized event: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM event_people WHERE normalized_event_id = $1", eventID); err != nil {
		return fmt.Errorf("delete prior event person links: %w", err)
	}
	for _, person := range people {
		tag, err := tx.Exec(ctx, `
			INSERT INTO event_people
				(normalized_event_id, person_id, role, occurred_at, identity_claim_id)
			SELECT $1, $2, $3, $4, $5
			WHERE EXISTS (
				SELECT 1 FROM identity_assignments
				WHERE claim_id = $5 AND person_id = $2
			)
			ON CONFLICT (normalized_event_id, person_id, role)
			DO UPDATE SET occurred_at = EXCLUDED.occurred_at,
			              identity_claim_id = EXCLUDED.identity_claim_id`,
			eventID, person.PersonID, person.Role, occurredAt, *person.IdentityClaimID)
		if err != nil {
			return fmt.Errorf("insert event person link: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("event person link requires a current identity assignment")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit event person replacement: %w", err)
	}
	return nil
}

// AddContributionCount records an observation without overwriting prior
// observations for the same activity date.
func (s *Store) AddContributionCount(ctx context.Context, input ContributionCountInput) error {
	if input.PersonID == uuid.Nil || input.ForgeInstanceID == uuid.Nil ||
		input.ActivityDate.IsZero() || input.Metric == "" || input.Value < 0 {
		return errors.New("contribution count requires person, forge, date, metric, and non-negative value")
	}
	observedAt := input.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO contribution_counts
			(id, person_id, forge_instance_id, activity_date, metric, kind, value, observed_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		uuid.New(), input.PersonID, input.ForgeInstanceID, input.ActivityDate.UTC(),
		input.Metric, input.Kind, input.Value, observedAt.UTC())
	if err != nil {
		return fmt.Errorf("record contribution count: %w", err)
	}
	return nil
}

// Timeline returns current actions attributed to person in descending
// occurrence-time order. A nonzero limit between 1 and 500 is required.
func (s *Store) Timeline(ctx context.Context, personID uuid.UUID, from, to time.Time, limit int) ([]TimelineEntry, error) {
	if personID == uuid.Nil || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("person and a non-empty time range are required")
	}
	if limit < 1 || limit > 500 {
		return nil, errors.New("timeline limit must be between 1 and 500")
	}
	rows, err := s.db.Query(ctx, `
		SELECT ne.id, ne.kind, ep.role, ep.occurred_at, ne.project, ne.title, ne.url
		FROM event_people ep
		JOIN normalized_events ne ON ne.id = ep.normalized_event_id
		JOIN identity_assignments ia
		  ON ia.claim_id = ep.identity_claim_id AND ia.person_id = ep.person_id
		WHERE ep.person_id = $1 AND ep.occurred_at >= $2 AND ep.occurred_at < $3
		  AND ne.is_current
		ORDER BY ep.occurred_at DESC, ne.id DESC, ep.role
		LIMIT $4`, personID, from.UTC(), to.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("query person timeline: %w", err)
	}
	defer rows.Close()
	entries := make([]TimelineEntry, 0)
	for rows.Next() {
		var entry TimelineEntry
		if err := rows.Scan(&entry.EventID, &entry.Kind, &entry.Role, &entry.OccurredAt,
			&entry.Project, &entry.Title, &entry.URL); err != nil {
			return nil, fmt.Errorf("scan person timeline: %w", err)
		}
		entry.OccurredAt = entry.OccurredAt.UTC()
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person timeline: %w", err)
	}
	return entries, nil
}

// ContributionCounts returns all source aggregate observations in the
// requested activity-date range, including prior observations of corrections.
func (s *Store) ContributionCounts(ctx context.Context, personID uuid.UUID, from, to time.Time) ([]ContributionCount, error) {
	if personID == uuid.Nil || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("person and a non-empty time range are required")
	}
	rows, err := s.db.Query(ctx, `
		SELECT person_id, forge_instance_id, activity_date, metric, kind, value, observed_at
		FROM contribution_counts
		WHERE person_id = $1 AND activity_date >= $2::date AND activity_date < $3::date
		ORDER BY activity_date, observed_at, metric, kind`,
		personID, from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("query contribution count history: %w", err)
	}
	defer rows.Close()
	counts := make([]ContributionCount, 0)
	for rows.Next() {
		var count ContributionCount
		if err := rows.Scan(&count.PersonID, &count.ForgeInstanceID, &count.ActivityDate,
			&count.Metric, &count.Kind, &count.Value, &count.ObservedAt); err != nil {
			return nil, fmt.Errorf("scan contribution count: %w", err)
		}
		count.ActivityDate = count.ActivityDate.UTC()
		count.ObservedAt = count.ObservedAt.UTC()
		counts = append(counts, count)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contribution count history: %w", err)
	}
	return counts, nil
}

// TimelineCounts groups current contribution actions by a supported UTC time
// bucket and kind.
func (s *Store) TimelineCounts(ctx context.Context, personID uuid.UUID, from, to time.Time, bucket TimeBucket) ([]TimelineBucket, error) {
	if personID == uuid.Nil || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("person and a non-empty time range are required")
	}
	switch bucket {
	case TimeBucketDay, TimeBucketWeek, TimeBucketMonth, TimeBucketYear:
	default:
		return nil, fmt.Errorf("unsupported timeline bucket %q", bucket)
	}
	rows, err := s.db.Query(ctx, `
		SELECT date_trunc($4, ep.occurred_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',
		       ne.kind, count(DISTINCT ne.id)
		FROM event_people ep
		JOIN normalized_events ne ON ne.id = ep.normalized_event_id
		JOIN identity_assignments ia
		  ON ia.claim_id = ep.identity_claim_id AND ia.person_id = ep.person_id
		WHERE ep.person_id = $1 AND ep.occurred_at >= $2 AND ep.occurred_at < $3
		  AND ne.is_current
		GROUP BY 1, ne.kind
		ORDER BY 1, ne.kind`, personID, from.UTC(), to.UTC(), string(bucket))
	if err != nil {
		return nil, fmt.Errorf("query contribution timeline counts: %w", err)
	}
	defer rows.Close()
	buckets := make([]TimelineBucket, 0)
	for rows.Next() {
		var item TimelineBucket
		if err := rows.Scan(&item.StartsAt, &item.Kind, &item.Count); err != nil {
			return nil, fmt.Errorf("scan contribution timeline count: %w", err)
		}
		item.StartsAt = item.StartsAt.UTC()
		buckets = append(buckets, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate contribution timeline counts: %w", err)
	}
	return buckets, nil
}

// RecordCoverage records a completed range or a known gap for a source.
func (s *Store) RecordCoverage(ctx context.Context, personID, forgeInstanceID uuid.UUID, sourceKey string, startsAt, endsAt time.Time, status, gapReason string, syncRunID *uuid.UUID) error {
	if personID == uuid.Nil || forgeInstanceID == uuid.Nil || sourceKey == "" ||
		startsAt.IsZero() || endsAt.IsZero() || !startsAt.Before(endsAt) {
		return errors.New("coverage requires person, source, and a non-empty time range")
	}
	if status != "complete" && status != "gap" {
		return fmt.Errorf("invalid coverage status %q", status)
	}
	if status == "complete" && gapReason != "" {
		return errors.New("complete coverage cannot have a gap reason")
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO sync_coverage
			(id, person_id, forge_instance_id, source_key, range_start, range_end, status, gap_reason, sync_run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		uuid.New(), personID, forgeInstanceID, sourceKey, startsAt.UTC(), endsAt.UTC(),
		status, gapReason, syncRunID)
	if err != nil {
		return fmt.Errorf("record sync coverage: %w", err)
	}
	return nil
}

// StartSyncRun records the start of a source collection attempt.
func (s *Store) StartSyncRun(ctx context.Context, personID, forgeInstanceID uuid.UUID, sourceKey string) (SyncRun, error) {
	if personID == uuid.Nil || forgeInstanceID == uuid.Nil || sourceKey == "" {
		return SyncRun{}, errors.New("sync run requires person, forge, and source")
	}
	run := SyncRun{
		ID:              uuid.New(),
		PersonID:        personID,
		ForgeInstanceID: forgeInstanceID,
		SourceKey:       sourceKey,
		Status:          "running",
	}
	err := s.db.QueryRow(ctx, `
		INSERT INTO sync_runs (id, person_id, forge_instance_id, source_key, status)
		VALUES ($1, $2, $3, $4, 'running')
		RETURNING started_at`, run.ID, run.PersonID, run.ForgeInstanceID, run.SourceKey).
		Scan(&run.StartedAt)
	if err != nil {
		return SyncRun{}, fmt.Errorf("start sync run: %w", err)
	}
	run.StartedAt = run.StartedAt.UTC()
	return run, nil
}

// FinishSyncRun stores the final status and fetch report for a running attempt.
func (s *Store) FinishSyncRun(ctx context.Context, runID uuid.UUID, status string, fetchReport json.RawMessage) error {
	switch status {
	case "succeeded", "partial", "failed", "canceled":
	default:
		return fmt.Errorf("invalid final sync run status %q", status)
	}
	if runID == uuid.Nil || !json.Valid(fetchReport) && len(fetchReport) > 0 {
		return errors.New("sync run ID and valid JSON fetch report are required")
	}
	if len(fetchReport) == 0 {
		fetchReport = json.RawMessage(`{}`)
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE sync_runs
		SET status = $2, fetch_report = $3, finished_at = now()
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`,
		runID, status, fetchReport)
	if err != nil {
		return fmt.Errorf("finish sync run %s: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish sync run %s: %w", runID, ErrNotFound)
	}
	return nil
}

// Coverage returns source coverage intervals overlapping the requested range.
func (s *Store) Coverage(ctx context.Context, personID, forgeInstanceID uuid.UUID, sourceKey string, from, to time.Time) ([]CoverageInterval, error) {
	if personID == uuid.Nil || forgeInstanceID == uuid.Nil || sourceKey == "" ||
		from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, errors.New("coverage query requires person, source, and a non-empty time range")
	}
	rows, err := s.db.Query(ctx, `
		SELECT source_key, range_start, range_end, status, gap_reason, recorded_at
		FROM sync_coverage
		WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = $3
		  AND range_start < $5 AND range_end > $4
		ORDER BY range_start, range_end, recorded_at`,
		personID, forgeInstanceID, sourceKey, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("query sync coverage: %w", err)
	}
	defer rows.Close()
	intervals := make([]CoverageInterval, 0)
	for rows.Next() {
		var interval CoverageInterval
		if err := rows.Scan(&interval.SourceKey, &interval.StartsAt, &interval.EndsAt,
			&interval.Status, &interval.GapReason, &interval.RecordedAt); err != nil {
			return nil, fmt.Errorf("scan sync coverage: %w", err)
		}
		interval.StartsAt = interval.StartsAt.UTC()
		interval.EndsAt = interval.EndsAt.UTC()
		interval.RecordedAt = interval.RecordedAt.UTC()
		intervals = append(intervals, interval)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sync coverage: %w", err)
	}
	return intervals, nil
}
