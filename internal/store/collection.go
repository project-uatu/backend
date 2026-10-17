package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

// ErrLeaseLost indicates that a job was reclaimed or its lease expired.
var ErrLeaseLost = errors.New("work lease lost")

// ErrCollectionJobActive indicates a conflicting scan is already queued or running.
var ErrCollectionJobActive = errors.New("collection scan already active")

// CollectionJob describes durable work for one person and source.
type CollectionJob struct {
	ID              uuid.UUID
	PersonID        uuid.UUID
	ForgeInstanceID uuid.UUID
	SourceKey       string
	ScanMode        string
	Window          event.Window
	IdentityKeys    []event.ActorKey
	Cursor          string
	DueAt           time.Time
	Attempts        int
	MaxAttempts     int
}

// CollectionJobInput describes collection work to enqueue.
type CollectionJobInput struct {
	PersonID        uuid.UUID
	ForgeInstanceID uuid.UUID
	SourceKey       string
	ScanMode        string
	Window          event.Window
	DueAt           time.Time
	MaxAttempts     int
	IdempotencyKey  string
}

// JobClaim owns a collection job and the sync run for this attempt.
type JobClaim struct {
	CollectionJob
	Owner               string
	Token               uuid.UUID
	LeaseExpiry         time.Time
	SyncRunID           uuid.UUID
	AccountNodeID       string
	CurrentAccountLogin string
}

// NormalizationClaim owns one raw event normalization attempt.
type NormalizationClaim struct {
	RawEventID        uuid.UUID
	NormalizerVersion int
	Attempts          int
	MaxAttempts       int
	Owner             string
	Token             uuid.UUID
	LeaseExpiry       time.Time
	Event             event.RawEvent
}

// QuotaReservation describes a shared upstream request allowance.
type QuotaReservation struct {
	Granted bool
	ResetAt *time.Time
	ID      uuid.UUID
}

// EnqueueCollectionJob creates or coalesces pending work for a source.
func (s *Store) EnqueueCollectionJob(ctx context.Context, input CollectionJobInput) (job CollectionJob, enqueueErr error) {
	if input.PersonID == uuid.Nil || input.ForgeInstanceID == uuid.Nil || input.SourceKey == "" {
		return CollectionJob{}, errors.New("collection job requires person, forge instance and source")
	}
	if input.ScanMode == "" {
		input.ScanMode = "backfill"
	}
	if input.ScanMode != "backfill" && input.ScanMode != "incremental" {
		return CollectionJob{}, fmt.Errorf("invalid collection scan mode %q", input.ScanMode)
	}
	if input.Window.To.IsZero() {
		input.Window.To = time.Now().UTC()
	}
	if !input.Window.From.IsZero() && !input.Window.From.Before(input.Window.To) {
		return CollectionJob{}, errors.New("collection window start must be before its end")
	}
	if input.ScanMode == "incremental" && input.Window.From.IsZero() {
		return CollectionJob{}, errors.New("incremental collection requires a window start")
	}
	if input.DueAt.IsZero() {
		input.DueAt = time.Now().UTC()
	}
	if input.MaxAttempts == 0 {
		input.MaxAttempts = 10
	}
	if input.MaxAttempts < 1 {
		return CollectionJob{}, errors.New("collection max attempts must be positive")
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return CollectionJob{}, fmt.Errorf("begin collection enqueue: %w", err)
	}
	defer rollbackTx(ctx, tx, "collection enqueue", &enqueueErr)
	if input.IdempotencyKey != "" {
		lockKey := fmt.Sprintf("%s:%s:%d:%s:%d:%s",
			input.PersonID, input.ForgeInstanceID, len(input.SourceKey), input.SourceKey,
			len(input.IdempotencyKey), input.IdempotencyKey)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", lockKey); err != nil {
			return CollectionJob{}, fmt.Errorf("lock collection idempotency key: %w", err)
		}
		existing, found, err := collectionJobForIdempotencyKey(ctx, tx, input)
		if err != nil {
			return CollectionJob{}, err
		}
		if found {
			if err := tx.Commit(ctx); err != nil {
				return CollectionJob{}, fmt.Errorf("commit idempotent collection enqueue: %w", err)
			}
			return existing, nil
		}
	}
	job = CollectionJob{
		ID: uuid.New(), PersonID: input.PersonID, ForgeInstanceID: input.ForgeInstanceID,
		SourceKey: input.SourceKey, ScanMode: input.ScanMode, Window: input.Window,
		Cursor: "", DueAt: input.DueAt, MaxAttempts: input.MaxAttempts,
	}
	var windowStart *time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO jobs
			(id, person_id, forge_instance_id, source_key, status, due_at, max_attempts, cursor,
			 idempotency_key, scan_mode, window_start, window_end)
		VALUES ($1, $2, $3, $4, 'queued', $5, $6, '', NULLIF($7, ''), $8, $9, $10)
		ON CONFLICT (person_id, forge_instance_id, source_key)
			WHERE status IN ('queued', 'running')
		DO UPDATE SET due_at = LEAST(jobs.due_at, EXCLUDED.due_at),
		              idempotency_key = COALESCE(jobs.idempotency_key, EXCLUDED.idempotency_key),
		              updated_at = now()
		WHERE jobs.scan_mode = EXCLUDED.scan_mode
		  AND jobs.window_start IS NOT DISTINCT FROM EXCLUDED.window_start
		  AND jobs.window_end = EXCLUDED.window_end
		RETURNING id, scan_mode, window_start, window_end, cursor, due_at, attempts, max_attempts`,
		job.ID, input.PersonID, input.ForgeInstanceID, input.SourceKey, input.DueAt,
		input.MaxAttempts, input.IdempotencyKey, input.ScanMode, nullableTime(input.Window.From),
		input.Window.To.UTC()).Scan(&job.ID, &job.ScanMode, &windowStart, &job.Window.To,
		&job.Cursor, &job.DueAt, &job.Attempts, &job.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return CollectionJob{}, fmt.Errorf("%w: requested scan window or mode differs from active work", ErrCollectionJobActive)
	}
	if err != nil {
		return CollectionJob{}, fmt.Errorf("enqueue collection job: %w", err)
	}
	if input.IdempotencyKey != "" {
		_, err = tx.Exec(ctx, `
			INSERT INTO job_idempotency_keys
				(person_id, forge_instance_id, source_key, idempotency_key, job_id)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (person_id, forge_instance_id, source_key, idempotency_key)
			DO NOTHING`,
			input.PersonID, input.ForgeInstanceID, input.SourceKey, input.IdempotencyKey, job.ID)
		if err != nil {
			return CollectionJob{}, fmt.Errorf("associate collection idempotency key: %w", err)
		}
		var associatedJobID uuid.UUID
		err := tx.QueryRow(ctx, `
			SELECT job_id FROM job_idempotency_keys
			WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = $3
			  AND idempotency_key = $4`,
			input.PersonID, input.ForgeInstanceID, input.SourceKey, input.IdempotencyKey).
			Scan(&associatedJobID)
		if err != nil {
			return CollectionJob{}, fmt.Errorf("load collection idempotency association: %w", err)
		}
		if associatedJobID != job.ID {
			return CollectionJob{}, fmt.Errorf("%w: idempotency key is associated with another job", ErrCollectionJobActive)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CollectionJob{}, fmt.Errorf("commit collection enqueue: %w", err)
	}
	if windowStart != nil {
		job.Window.From = windowStart.UTC()
	}
	job.Window.To = job.Window.To.UTC()
	job.DueAt = job.DueAt.UTC()
	return job, nil
}

// EnqueueGitHubHistory chooses an initial backfill or a seven-day-overlap
// incremental scan from the completed checkpoint.
func (s *Store) EnqueueGitHubHistory(ctx context.Context, personID, forgeInstanceID uuid.UUID) (CollectionJob, error) {
	if personID == uuid.Nil || forgeInstanceID == uuid.Nil {
		return CollectionJob{}, errors.New("GitHub history enqueue requires person and forge")
	}
	var accountCreatedAt time.Time
	if err := s.db.QueryRow(ctx, `
		SELECT account_created_at FROM github_accounts
		WHERE forge_instance_id = $1 AND person_id = $2`,
		forgeInstanceID, personID).Scan(&accountCreatedAt); err != nil {
		return CollectionJob{}, fmt.Errorf("load GitHub account creation time: %w", err)
	}
	input := CollectionJobInput{
		PersonID: personID, ForgeInstanceID: forgeInstanceID,
		SourceKey: SourceGitHubHistory, ScanMode: "backfill",
		Window: event.Window{From: accountCreatedAt.UTC()},
	}
	var backfillComplete bool
	var completedThrough *time.Time
	err := s.db.QueryRow(ctx, `
		SELECT backfill_complete, completed_through
		FROM checkpoints
		WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = $3`,
		personID, forgeInstanceID, SourceGitHubHistory).Scan(&backfillComplete, &completedThrough)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return CollectionJob{}, fmt.Errorf("load GitHub history checkpoint: %w", err)
	case backfillComplete && completedThrough != nil:
		input.ScanMode = "incremental"
		input.Window.From = completedThrough.Add(-7 * 24 * time.Hour).UTC()
	default:
	}
	input.Window.To = time.Now().UTC()
	return s.EnqueueCollectionJob(ctx, input)
}

func collectionJobForIdempotencyKey(ctx context.Context, tx pgx.Tx, input CollectionJobInput) (CollectionJob, bool, error) {
	var job CollectionJob
	var windowStart *time.Time
	err := tx.QueryRow(ctx, `
		SELECT j.id, j.person_id, j.forge_instance_id, j.source_key, j.scan_mode,
		       j.window_start, j.window_end, j.cursor, j.due_at, j.attempts, j.max_attempts
		FROM job_idempotency_keys k
		JOIN jobs j ON j.id = k.job_id
		WHERE k.person_id = $1 AND k.forge_instance_id = $2
		  AND k.source_key = $3 AND k.idempotency_key = $4`,
		input.PersonID, input.ForgeInstanceID, input.SourceKey, input.IdempotencyKey).
		Scan(&job.ID, &job.PersonID, &job.ForgeInstanceID, &job.SourceKey,
			&job.ScanMode, &windowStart, &job.Window.To, &job.Cursor,
			&job.DueAt, &job.Attempts, &job.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return CollectionJob{}, false, nil
	}
	if err != nil {
		return CollectionJob{}, false, fmt.Errorf("find idempotent collection job: %w", err)
	}
	if windowStart != nil {
		job.Window.From = windowStart.UTC()
	}
	job.Window.To = job.Window.To.UTC()
	job.DueAt = job.DueAt.UTC()
	return job, true, nil
}

// ClaimCollectionJob claims one due job and starts a sync run.
func (s *Store) ClaimCollectionJob(ctx context.Context, owner string, lease time.Duration) (claim JobClaim, found bool, claimErr error) {
	if owner == "" || lease <= 0 {
		return JobClaim{}, false, errors.New("job claim requires owner and positive lease")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return JobClaim{}, false, fmt.Errorf("begin collection claim: %w", err)
	}
	defer rollbackTx(ctx, tx, "collection claim", &claimErr)
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs sr SET status = 'failed', finished_at = now()
		FROM jobs j
		WHERE j.sync_run_id = sr.id AND j.status = 'running'
		  AND j.lease_expires_at <= now() AND j.attempts >= j.max_attempts
		  AND sr.status = 'running' AND sr.finished_at IS NULL`); err != nil {
		return JobClaim{}, false, fmt.Errorf("finish exhausted expired sync runs: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status = 'failed', lease_owner = '', fencing_token = NULL,
		                last_error = 'maximum attempts exhausted after lease expiry',
		                updated_at = now()
		WHERE status = 'running' AND lease_expires_at <= now()
		  AND attempts >= max_attempts`); err != nil {
		return JobClaim{}, false, fmt.Errorf("fail exhausted expired collection jobs: %w", err)
	}
	var job CollectionJob
	var oldRunID *uuid.UUID
	var windowStart *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, person_id, forge_instance_id, source_key, scan_mode,
		       window_start, window_end, cursor, due_at, attempts, max_attempts, sync_run_id
		FROM jobs
		WHERE (status = 'queued' AND GREATEST(due_at, COALESCE(retry_at, due_at),
		                                      COALESCE(rate_limit_reset_at, due_at)) <= now())
		   OR (status = 'running' AND lease_expires_at <= now() AND attempts < max_attempts)
		ORDER BY GREATEST(due_at, COALESCE(retry_at, due_at),
		                  COALESCE(rate_limit_reset_at, due_at)), created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&job.ID, &job.PersonID, &job.ForgeInstanceID, &job.SourceKey,
		&job.ScanMode, &windowStart, &job.Window.To, &job.Cursor, &job.DueAt,
		&job.Attempts, &job.MaxAttempts, &oldRunID)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return JobClaim{}, false, fmt.Errorf("commit empty collection claim: %w", err)
		}
		return JobClaim{}, false, nil
	}
	if err != nil {
		return JobClaim{}, false, fmt.Errorf("select due collection job: %w", err)
	}
	if windowStart != nil {
		job.Window.From = windowStart.UTC()
	}
	keys, err := assignedActorKeys(ctx, tx, job.PersonID, job.ForgeInstanceID)
	if err != nil {
		return JobClaim{}, false, err
	}
	job.IdentityKeys = keys
	var accountNodeID, currentLogin string
	if job.SourceKey == SourceGitHubHistory {
		if err := tx.QueryRow(ctx, `
			SELECT node_id, login FROM github_accounts
			WHERE forge_instance_id = $1 AND person_id = $2`,
			job.ForgeInstanceID, job.PersonID).Scan(&accountNodeID, &currentLogin); err != nil {
			return JobClaim{}, false, fmt.Errorf("load GitHub account identifiers: %w", err)
		}
	}
	if oldRunID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE sync_runs SET status = 'partial', finished_at = now()
			WHERE id = $1 AND status = 'running' AND finished_at IS NULL`, *oldRunID); err != nil {
			return JobClaim{}, false, fmt.Errorf("finish expired sync run: %w", err)
		}
	}
	runID := uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sync_runs (id, person_id, forge_instance_id, source_key, status, fetch_report)
		VALUES ($1, $2, $3, $4, 'running', '{"pages":[],"gaps":[],"errors":[]}')`,
		runID, job.PersonID, job.ForgeInstanceID, job.SourceKey); err != nil {
		return JobClaim{}, false, fmt.Errorf("start claimed sync run: %w", err)
	}
	token := uuid.New()
	var leaseExpiry time.Time
	err = tx.QueryRow(ctx, `
		UPDATE jobs
		SET status = 'running', attempts = attempts + 1, lease_owner = $2,
		    lease_expires_at = now() + ($3 * interval '1 microsecond'), fencing_token = $4,
		    sync_run_id = $5, updated_at = now()
		WHERE id = $1
		RETURNING attempts, lease_expires_at`,
		job.ID, owner, lease.Microseconds(), token, runID).Scan(&job.Attempts, &leaseExpiry)
	if err != nil {
		return JobClaim{}, false, fmt.Errorf("assign collection job lease: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return JobClaim{}, false, fmt.Errorf("commit collection claim: %w", err)
	}
	job.Window.From = job.Window.From.UTC()
	job.Window.To = job.Window.To.UTC()
	return JobClaim{
		CollectionJob: job,
		Owner:         owner,
		Token:         token,
		LeaseExpiry:   leaseExpiry.UTC(),
		SyncRunID:     runID,
		AccountNodeID: accountNodeID, CurrentAccountLogin: currentLogin,
	}, true, nil
}

// RenewCollectionLease extends a live claim without allowing a stale worker to
// renew work that has already been reclaimed.
func (s *Store) RenewCollectionLease(ctx context.Context, claim JobClaim, lease time.Duration) (renewErr error) {
	if lease <= 0 {
		return errors.New("collection lease duration must be positive")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection lease renewal: %w", err)
	}
	defer rollbackTx(ctx, tx, "collection lease renewal", &renewErr)
	tag, err := tx.Exec(ctx, `
		UPDATE jobs SET lease_expires_at = now() + ($3 * interval '1 microsecond'), updated_at = now()
		WHERE id = $1 AND status = 'running' AND fencing_token = $2
		  AND lease_expires_at > now()`, claim.ID, claim.Token, lease.Microseconds())
	if err != nil {
		return fmt.Errorf("renew collection lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("renew collection lease: %w", ErrLeaseLost)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rate_limit_reservations
		SET expires_at = now() + ($3 * interval '1 microsecond')
		WHERE job_id = $1 AND fencing_token = $2`,
		claim.ID, claim.Token, lease.Microseconds()); err != nil {
		return fmt.Errorf("renew collection quota reservation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection lease renewal: %w", err)
	}
	return nil
}

// PersistCollectionPage stores a page and advances its cursor only when the
// connector completed the page successfully.
func (s *Store) PersistCollectionPage(ctx context.Context, claim JobClaim, page event.Page, report event.FetchReport, successful bool, normalizerVersion int, reservationID uuid.UUID) (done bool, persistErr error) {
	if normalizerVersion < 1 {
		return false, errors.New("normalizer version must be positive")
	}
	if reservationID != uuid.Nil && report.RateLimitBucket == "" {
		return false, errors.New("quota reservation requires a reported bucket")
	}
	if len(page.Events) > 500 {
		return false, errors.New("connector page exceeds maximum size")
	}
	if successful && !page.Done && (page.NextCursor == "" || page.NextCursor == claim.Cursor) {
		return false, errors.New("incomplete connector page must advance its cursor")
	}
	if report.Requests < 0 || report.Pages < 0 ||
		report.RateLimitLimit != nil && *report.RateLimitLimit < 0 ||
		report.RateLimitRemaining != nil && *report.RateLimitRemaining < 0 ||
		report.RateLimitLimit != nil && report.RateLimitRemaining != nil &&
			*report.RateLimitRemaining > *report.RateLimitLimit {
		return false, errors.New("fetch report contains invalid counters")
	}
	for _, gap := range report.Gaps {
		if gap.From.IsZero() || gap.To.IsZero() || !gap.From.Before(gap.To) || gap.Reason == "" {
			return false, errors.New("fetch report contains an invalid coverage gap")
		}
	}
	for _, count := range report.ContributionCounts {
		if count.Date.IsZero() || count.Metric == "" || count.Value < 0 {
			return false, errors.New("fetch report contains an invalid contribution count")
		}
	}
	reportJSON, err := json.Marshal(report)
	if err != nil {
		return false, fmt.Errorf("encode fetch report: %w", err)
	}
	gapsJSON, err := json.Marshal(report.Gaps)
	if err != nil {
		return false, fmt.Errorf("encode fetch report gaps: %w", err)
	}
	errorsJSON, err := json.Marshal(report.Errors)
	if err != nil {
		return false, fmt.Errorf("encode fetch report errors: %w", err)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin collection page persistence: %w", err)
	}
	defer rollbackTx(ctx, tx, "collection page persistence", &persistErr)
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return false, err
	}
	for _, raw := range page.Events {
		if err := raw.Validate(claim.ForgeInstanceID); err != nil {
			return false, fmt.Errorf("validate connector event: %w", err)
		}
		if string(raw.Source) != claim.SourceKey {
			return false, fmt.Errorf("connector event source %q does not match job source %q", raw.Source, claim.SourceKey)
		}
		actorJSON, err := json.Marshal(raw.Actors)
		if err != nil {
			return false, fmt.Errorf("encode actor keys for event %s: %w", raw.ExternalID, err)
		}
		rawID, inserted, err := upsertRawEvent(ctx, tx, raw, actorJSON, successful)
		if err != nil {
			return false, err
		}
		if !successful && !inserted {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO normalization_jobs (raw_event_id, normalizer_version)
				VALUES ($1, $2)
				ON CONFLICT (raw_event_id, normalizer_version)
				DO UPDATE SET status = 'queued', retry_at = NULL, attempts = 0,
				              lease_owner = '', lease_expires_at = NULL, fencing_token = NULL,
				              last_error = '', updated_at = now()`,
			rawID, normalizerVersion); err != nil {
			return false, fmt.Errorf("enqueue raw event normalization: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs
		SET fetch_report =
			jsonb_set(
				jsonb_set(
					jsonb_set(fetch_report, '{pages}', COALESCE(fetch_report->'pages', '[]'::jsonb) || $2::jsonb, true),
					'{gaps}', COALESCE(fetch_report->'gaps', '[]'::jsonb) || $3::jsonb, true),
				'{errors}', COALESCE(fetch_report->'errors', '[]'::jsonb) || $4::jsonb, true)
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`,
		claim.SyncRunID, reportJSON, gapsJSON, errorsJSON); err != nil {
		return false, fmt.Errorf("append collection page report: %w", err)
	}
	for _, count := range report.ContributionCounts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO contribution_counts
				(id, person_id, forge_instance_id, activity_date, metric, kind, value)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			uuid.New(), claim.PersonID, claim.ForgeInstanceID,
			count.Date.UTC().Format("2006-01-02"), count.Metric, count.Kind, count.Value); err != nil {
			return false, fmt.Errorf("persist source contribution count: %w", err)
		}
	}
	for _, gap := range report.Gaps {
		if _, err := tx.Exec(ctx, `
			INSERT INTO sync_coverage
				(id, person_id, forge_instance_id, source_key, range_start, range_end,
				 status, gap_reason, sync_run_id, coverage_scope)
			VALUES ($1, $2, $3, $4, $5, $6, 'gap', $7, $8, $9)`,
			uuid.New(), claim.PersonID, claim.ForgeInstanceID, claim.SourceKey,
			gap.From.UTC(), gap.To.UTC(), gap.Reason, claim.SyncRunID, report.CoverageScope); err != nil {
			return false, fmt.Errorf("record collection coverage gap: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO collection_scan_gaps (job_id, range_start, range_end, gap_reason)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (job_id, range_start, range_end, gap_reason) DO NOTHING`,
			claim.ID, gap.From.UTC(), gap.To.UTC(), gap.Reason); err != nil {
			return false, fmt.Errorf("record logical scan gap: %w", err)
		}
	}
	if report.RateLimitBucket != "" {
		if err := recordRateLimitTx(ctx, tx, report.RateLimitBucket,
			report.RateLimitRemaining, report.RateLimitResetAt, reservationID); err != nil {
			return false, err
		}
		if report.RateLimitResetAt != nil {
			rateLimitExhausted := report.RateLimitRemaining != nil && *report.RateLimitRemaining == 0
			if _, err := tx.Exec(ctx, `
				UPDATE jobs
				SET rate_limit_reset_at = CASE WHEN $3 THEN $4::timestamptz ELSE NULL END,
				    updated_at = now()
				WHERE id = $1 AND fencing_token = $2`,
				claim.ID, claim.Token, rateLimitExhausted, report.RateLimitResetAt.UTC()); err != nil {
				return false, fmt.Errorf("store collection rate-limit reset: %w", err)
			}
		}
	}
	if !successful {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit partial collection page: %w", err)
		}
		return false, nil
	}
	scanComplete := false
	if page.Done {
		var gapCount int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM collection_scan_gaps WHERE job_id = $1`, claim.ID).Scan(&gapCount); err != nil {
			return false, fmt.Errorf("count scan coverage gaps: %w", err)
		}
		scanComplete = gapCount == 0
	}
	if err := advanceCollectionCursor(ctx, tx, claim, page.NextCursor, scanComplete); err != nil {
		return false, err
	}
	if page.Done {
		runStatus := "succeeded"
		if !scanComplete {
			runStatus = "partial"
		} else {
			start := claim.Window.From
			if start.IsZero() {
				start = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO sync_coverage
					(id, person_id, forge_instance_id, source_key, range_start, range_end,
					 status, sync_run_id, coverage_scope)
				VALUES ($1, $2, $3, $4, $5, $6, 'complete', $7, $8)`,
				uuid.New(), claim.PersonID, claim.ForgeInstanceID, claim.SourceKey,
				start.UTC(), claim.Window.To.UTC(), claim.SyncRunID, report.CoverageScope); err != nil {
				return false, fmt.Errorf("record completed collection coverage: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sync_runs SET status = $2, finished_at = now()
			WHERE id = $1 AND status = 'running' AND finished_at IS NULL`,
			claim.SyncRunID, runStatus); err != nil {
			return false, fmt.Errorf("finish completed collection run: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE jobs
			SET status = 'succeeded', lease_owner = '', lease_expires_at = NULL,
			    fencing_token = NULL, retry_at = NULL, last_error = '', updated_at = now()
			WHERE id = $1 AND fencing_token = $2`, claim.ID, claim.Token); err != nil {
			return false, fmt.Errorf("complete collection job: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit collection page: %w", err)
	}
	return page.Done, nil
}

// RetryCollectionJob releases a claimed job for a bounded retry or marks it
// failed after its configured attempt limit.
func (s *Store) RetryCollectionJob(ctx context.Context, claim JobClaim, cause error, retryAt time.Time) (retryErr error) {
	if cause == nil {
		return errors.New("collection retry requires a cause")
	}
	status := "queued"
	if claim.Attempts >= claim.MaxAttempts {
		status = "failed"
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection retry: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			retryErr = errors.Join(retryErr, fmt.Errorf("rollback collection retry: %w", err))
		}
	}()
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = $3, retry_at = CASE WHEN $3 = 'queued' THEN $4::timestamptz ELSE NULL END,
		    rate_limit_reset_at = CASE WHEN $3 = 'queued' THEN $4::timestamptz ELSE NULL END,
		    lease_owner = '', lease_expires_at = NULL, fencing_token = NULL,
		    last_error = $5, updated_at = now()
		WHERE id = $1 AND status = 'running' AND fencing_token = $2`,
		claim.ID, claim.Token, status, retryAt.UTC(), cause.Error())
	if err != nil {
		return fmt.Errorf("retry collection job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("retry collection job: %w", ErrLeaseLost)
	}
	var pageCount int
	if err := tx.QueryRow(ctx, `
		SELECT jsonb_array_length(COALESCE(fetch_report->'pages', '[]'::jsonb))
		FROM sync_runs WHERE id = $1`, claim.SyncRunID).Scan(&pageCount); err != nil {
		return fmt.Errorf("read failed collection report: %w", err)
	}
	runStatus := "failed"
	if pageCount > 0 {
		runStatus = "partial"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs SET status = $2, finished_at = now()
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`,
		claim.SyncRunID, runStatus); err != nil {
		return fmt.Errorf("finish failed collection run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection retry: %w", err)
	}
	return nil
}

// DeferCollectionJob returns a claimed job to the queue without consuming a
// fetch attempt, for example when a shared quota bucket is empty.
func (s *Store) DeferCollectionJob(ctx context.Context, claim JobClaim, retryAt time.Time) (deferErr error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection deferral: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			deferErr = errors.Join(deferErr, fmt.Errorf("rollback collection deferral: %w", err))
		}
	}()
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		SET status = 'queued', attempts = GREATEST(attempts - 1, 0), retry_at = $3,
		    rate_limit_reset_at = $3, lease_owner = '', lease_expires_at = NULL,
		    fencing_token = NULL, updated_at = now()
		WHERE id = $1 AND fencing_token = $2`, claim.ID, claim.Token, retryAt.UTC()); err != nil {
		return fmt.Errorf("defer collection job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs SET status = 'canceled', finished_at = now()
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`, claim.SyncRunID); err != nil {
		return fmt.Errorf("finish deferred sync run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection deferral: %w", err)
	}
	return nil
}

// FailCollectionJob permanently fails a job that cannot be processed by this
// worker configuration.
func (s *Store) FailCollectionJob(ctx context.Context, claim JobClaim, reason string) (failErr error) {
	if reason == "" {
		return errors.New("collection failure reason is required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection failure: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			failErr = errors.Join(failErr, fmt.Errorf("rollback collection failure: %w", err))
		}
	}()
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status = 'failed', lease_owner = '', lease_expires_at = NULL,
		                fencing_token = NULL, retry_at = NULL, last_error = $3, updated_at = now()
		WHERE id = $1 AND fencing_token = $2`, claim.ID, claim.Token, reason); err != nil {
		return fmt.Errorf("fail collection job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs SET status = 'failed', finished_at = now()
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`, claim.SyncRunID); err != nil {
		return fmt.Errorf("finish permanently failed sync run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection failure: %w", err)
	}
	return nil
}

// ReleaseCollectionJob returns a canceled live job to the due queue.
func (s *Store) ReleaseCollectionJob(ctx context.Context, claim JobClaim) (releaseErr error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin collection release: %w", err)
	}
	defer rollbackTx(ctx, tx, "collection release", &releaseErr)
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status = 'queued', due_at = now(), retry_at = NULL,
		                lease_owner = '', lease_expires_at = NULL, fencing_token = NULL,
		                updated_at = now()
		WHERE id = $1 AND fencing_token = $2`, claim.ID, claim.Token); err != nil {
		return fmt.Errorf("release canceled collection job: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sync_runs SET status = 'canceled', finished_at = now()
		WHERE id = $1 AND status = 'running' AND finished_at IS NULL`, claim.SyncRunID); err != nil {
		return fmt.Errorf("finish canceled collection run: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit collection release: %w", err)
	}
	return nil
}

func lockJobClaim(ctx context.Context, tx pgx.Tx, jobID, token uuid.UUID) error {
	var lockedID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT id FROM jobs
		WHERE id = $1 AND status = 'running' AND fencing_token = $2
		  AND lease_expires_at > now()
		FOR UPDATE`, jobID, token).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock collection job: %w", ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("lock collection job: %w", err)
	}
	return nil
}

func assignedActorKeys(ctx context.Context, tx pgx.Tx, personID, instanceID uuid.UUID) ([]event.ActorKey, error) {
	rows, err := tx.Query(ctx, `
		  SELECT ik.kind, ik.value, ik.forge_instance_id
		  FROM identity_assignments ia
		  JOIN identity_keys ik ON ik.id = ia.identity_key_id
		  WHERE ia.person_id = $1
		    AND (ik.forge_instance_id IS NULL OR ik.forge_instance_id = $2)
		  ORDER BY ik.kind, ik.normalized_value`, personID, instanceID)
	if err != nil {
		return nil, fmt.Errorf("load assigned collection identities: %w", err)
	}
	defer rows.Close()
	keys := make([]event.ActorKey, 0)
	for rows.Next() {
		var kind, value string
		var forgeID *uuid.UUID
		if err := rows.Scan(&kind, &value, &forgeID); err != nil {
			return nil, fmt.Errorf("scan assigned collection identity: %w", err)
		}
		keys = append(keys, event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.Kind(kind), Value: value},
			ForgeInstanceID: forgeID,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate assigned collection identities: %w", err)
	}
	return keys, nil
}

func upsertRawEvent(ctx context.Context, tx pgx.Tx, raw event.RawEvent, actorJSON []byte, successful bool) (uuid.UUID, bool, error) {
	eventID := uuid.New()
	query := `
		INSERT INTO raw_events
			(id, forge_instance_id, external_id, source_key, kind, occurred_at, payload,
			 actor_keys, payload_schema_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (forge_instance_id, external_id)
		DO NOTHING
		RETURNING id`
	if successful {
		query = `
			INSERT INTO raw_events
				(id, forge_instance_id, external_id, source_key, kind, occurred_at, payload,
				 actor_keys, payload_schema_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (forge_instance_id, external_id)
			DO UPDATE SET source_key = EXCLUDED.source_key,
			              kind = EXCLUDED.kind,
			              occurred_at = COALESCE(EXCLUDED.occurred_at, raw_events.occurred_at),
			              payload = EXCLUDED.payload,
			              actor_keys = EXCLUDED.actor_keys,
			              payload_schema_version = EXCLUDED.payload_schema_version,
			              observed_at = now()
			RETURNING id`
	}
	err := tx.QueryRow(ctx, query, eventID, raw.ForgeInstanceID, raw.ExternalID,
		string(raw.Source), string(raw.Kind), raw.OccurredAt, raw.Payload, actorJSON,
		raw.PayloadSchemaVersion).Scan(&eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `
			SELECT id FROM raw_events
			WHERE forge_instance_id = $1 AND external_id = $2`,
			raw.ForgeInstanceID, raw.ExternalID).Scan(&eventID)
		if err != nil {
			return uuid.Nil, false, fmt.Errorf("find existing raw event %s: %w", raw.ExternalID, err)
		}
		return eventID, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("persist raw event %s: %w", raw.ExternalID, err)
	}
	return eventID, true, nil
}

func advanceCollectionCursor(ctx context.Context, tx pgx.Tx, claim JobClaim, cursor string, scanComplete bool) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO checkpoints
			(person_id, forge_instance_id, source_key, cursor, incremental_cursor,
			 backfill_complete, completed_through)
		VALUES ($1, $2, $3, CASE WHEN $4 = 'backfill' THEN $5 ELSE '' END,
		                   CASE WHEN $4 = 'incremental' THEN $5 ELSE '' END,
		                   ($4 = 'backfill' AND $6),
		                   CASE WHEN $4 = 'incremental' AND $6 THEN $7::timestamptz ELSE NULL::timestamptz END)
		ON CONFLICT (person_id, forge_instance_id, source_key)
		DO UPDATE SET cursor = CASE WHEN $4 = 'backfill' THEN EXCLUDED.cursor ELSE checkpoints.cursor END,
		              incremental_cursor = CASE WHEN $4 = 'incremental' THEN EXCLUDED.incremental_cursor ELSE checkpoints.incremental_cursor END,
		              backfill_complete = CASE WHEN $4 = 'backfill' AND $6 THEN true ELSE checkpoints.backfill_complete END,
		              completed_through = CASE
		                  WHEN $4 = 'incremental' AND $6
		                  THEN GREATEST(COALESCE(checkpoints.completed_through, EXCLUDED.completed_through),
		                                EXCLUDED.completed_through)
		                  ELSE checkpoints.completed_through
		              END,
		              updated_at = now()`,
		claim.PersonID, claim.ForgeInstanceID, claim.SourceKey, claim.ScanMode, cursor,
		scanComplete, claim.Window.To.UTC()); err != nil {
		return fmt.Errorf("advance collection checkpoint: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET cursor = $3, updated_at = now()
		WHERE id = $1 AND fencing_token = $2`, claim.ID, claim.Token, cursor); err != nil {
		return fmt.Errorf("advance collection job cursor: %w", err)
	}
	return nil
}

func recordRateLimitTx(ctx context.Context, tx pgx.Tx, bucket string, remaining *int, resetAt *time.Time, reservationID uuid.UUID) error {
	if bucket == "" || remaining != nil && *remaining < 0 {
		return errors.New("rate-limit report has invalid bucket or counters")
	}
	if resetAt != nil {
		normalizedReset := resetAt.UTC().Truncate(time.Microsecond)
		resetAt = &normalizedReset
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO rate_limit_buckets (bucket_key) VALUES ($1)
		ON CONFLICT (bucket_key) DO NOTHING`, bucket); err != nil {
		return fmt.Errorf("record upstream rate limit: %w", err)
	}
	var currentRemaining *int
	var currentReset *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT remaining, reset_at FROM rate_limit_buckets
		WHERE bucket_key = $1 FOR UPDATE`, bucket).Scan(&currentRemaining, &currentReset); err != nil {
		return fmt.Errorf("lock upstream rate-limit bucket: %w", err)
	}
	if reservationID != uuid.Nil {
		tag, err := tx.Exec(ctx, `
			DELETE FROM rate_limit_reservations
			WHERE id = $1 AND bucket_key = $2`, reservationID, bucket)
		if err != nil {
			return fmt.Errorf("settle upstream quota reservation: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("settle upstream quota reservation: %w", ErrLeaseLost)
		}
	}
	var inFlight int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM rate_limit_reservations WHERE bucket_key = $1`, bucket).Scan(&inFlight); err != nil {
		return fmt.Errorf("count outstanding quota reservations: %w", err)
	}

	now := time.Now().UTC()
	newWindow := resetAt != nil && (currentReset == nil || resetAt.After(*currentReset))
	if currentReset != nil && !currentReset.After(now) {
		currentRemaining = nil
		currentReset = nil
		newWindow = resetAt != nil
	}
	var nextRemaining *int
	if remaining != nil {
		reportedAvailable := max(0, *remaining-inFlight)
		switch {
		case newWindow || currentRemaining == nil:
			nextRemaining = &reportedAvailable
		case reportedAvailable < *currentRemaining:
			nextRemaining = &reportedAvailable
		default:
			nextRemaining = currentRemaining
		}
	} else {
		nextRemaining = currentRemaining
	}
	nextReset := currentReset
	if resetAt != nil && (nextReset == nil || resetAt.After(*nextReset)) {
		nextReset = resetAt
	}
	_, err := tx.Exec(ctx, `
		UPDATE rate_limit_buckets
		SET remaining = $2, reset_at = $3, in_flight = $4, updated_at = now()
		WHERE bucket_key = $1`, bucket, nextRemaining, nextReset, inFlight)
	if err != nil {
		return fmt.Errorf("update upstream rate-limit bucket: %w", err)
	}
	return nil
}

// ReserveRateLimit reserves one request from a provider quota bucket.
func (s *Store) ReserveRateLimit(ctx context.Context, bucket string, claim JobClaim, lease time.Duration) (reservation QuotaReservation, reserveErr error) {
	if bucket == "" || lease <= 0 {
		return QuotaReservation{}, errors.New("rate-limit reservation requires bucket and positive lease")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return QuotaReservation{}, fmt.Errorf("begin rate-limit reservation: %w", err)
	}
	defer rollbackTx(ctx, tx, "rate-limit reservation", &reserveErr)
	if err := lockJobClaim(ctx, tx, claim.ID, claim.Token); err != nil {
		return QuotaReservation{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO rate_limit_buckets (bucket_key)
		VALUES ($1) ON CONFLICT (bucket_key) DO NOTHING`, bucket); err != nil {
		return QuotaReservation{}, fmt.Errorf("create rate-limit bucket: %w", err)
	}
	var remaining *int
	var resetAt *time.Time
	var inFlight int
	err = tx.QueryRow(ctx, `
		SELECT remaining, reset_at, in_flight
		FROM rate_limit_buckets WHERE bucket_key = $1 FOR UPDATE`, bucket).
		Scan(&remaining, &resetAt, &inFlight)
	if err != nil {
		return QuotaReservation{}, fmt.Errorf("load rate-limit bucket: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM rate_limit_reservations
		WHERE bucket_key = $1 AND expires_at <= now()`, bucket); err != nil {
		return QuotaReservation{}, fmt.Errorf("reclaim expired quota reservations: %w", err)
	}
	var remainingReservations int
	if resetAt != nil && !resetAt.After(time.Now().UTC()) {
		remaining = nil
		resetAt = nil
		if _, err := tx.Exec(ctx, `
			UPDATE rate_limit_buckets SET remaining = NULL, reset_at = NULL,
			                             updated_at = now()
			WHERE bucket_key = $1`, bucket); err != nil {
			return QuotaReservation{}, fmt.Errorf("reset expired rate-limit bucket: %w", err)
		}
	}
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM rate_limit_reservations WHERE bucket_key = $1`, bucket).
		Scan(&remainingReservations); err != nil {
		return QuotaReservation{}, fmt.Errorf("count live quota reservations: %w", err)
	}
	inFlight = remainingReservations
	reservation = QuotaReservation{Granted: true}
	switch {
	case remaining != nil && *remaining == 0:
		reservation.Granted = false
		reservation.ResetAt = resetAt
	case remaining == nil && inFlight > 0:
		reservation.Granted = false
		reservation.ResetAt = resetAt
	default:
		if remaining != nil {
			*remaining--
		}
		reservation.ID = uuid.New()
		if _, err := tx.Exec(ctx, `
			INSERT INTO rate_limit_reservations (id, bucket_key, job_id, fencing_token, expires_at)
			VALUES ($1, $2, $3, $4, now() + ($5 * interval '1 microsecond'))`,
			reservation.ID, bucket, claim.ID, claim.Token, lease.Microseconds()); err != nil {
			return QuotaReservation{}, fmt.Errorf("create provider quota reservation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE rate_limit_buckets
			SET remaining = $2, in_flight = $3 + 1, updated_at = now()
			WHERE bucket_key = $1`, bucket, remaining, inFlight); err != nil {
			return QuotaReservation{}, fmt.Errorf("reserve provider quota: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return QuotaReservation{}, fmt.Errorf("commit rate-limit reservation: %w", err)
	}
	if reservation.ResetAt != nil {
		value := reservation.ResetAt.UTC()
		reservation.ResetAt = &value
	}
	return reservation, nil
}

func rollbackTx(ctx context.Context, tx pgx.Tx, operation string, resultErr *error) {
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		*resultErr = errors.Join(*resultErr, fmt.Errorf("rollback %s: %w", operation, err))
	}
}

// ReleaseRateLimit settles an attempted request whose response could not be persisted.
func (s *Store) ReleaseRateLimit(ctx context.Context, reservationID uuid.UUID) (releaseErr error) {
	if reservationID == uuid.Nil {
		return errors.New("quota reservation ID is required")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin quota reservation settlement: %w", err)
	}
	defer rollbackTx(ctx, tx, "quota reservation settlement", &releaseErr)
	var bucket string
	err = tx.QueryRow(ctx, `
		SELECT bucket_key FROM rate_limit_reservations WHERE id = $1`, reservationID).Scan(&bucket)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit already settled quota reservation: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("find quota reservation: %w", err)
	}
	var lockedBucket string
	if err := tx.QueryRow(ctx, `
		SELECT bucket_key FROM rate_limit_buckets WHERE bucket_key = $1 FOR UPDATE`, bucket).Scan(&lockedBucket); err != nil {
		return fmt.Errorf("lock quota reservation bucket: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM rate_limit_reservations WHERE id = $1", reservationID); err != nil {
		return fmt.Errorf("settle quota reservation: %w", err)
	}
	var inFlight int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM rate_limit_reservations WHERE bucket_key = $1`, bucket).Scan(&inFlight); err != nil {
		return fmt.Errorf("count outstanding quota reservations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE rate_limit_buckets SET in_flight = $2, updated_at = now()
		WHERE bucket_key = $1`, bucket, inFlight); err != nil {
		return fmt.Errorf("update settled quota reservations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit quota reservation settlement: %w", err)
	}
	return nil
}

func nullableTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}
