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

// ClaimNormalizationJob claims one due normalization item and loads its raw event.
func (s *Store) ClaimNormalizationJob(ctx context.Context, owner string, lease time.Duration) (claim NormalizationClaim, found bool, claimErr error) {
	if owner == "" || lease <= 0 {
		return NormalizationClaim{}, false, errors.New("normalization claim requires owner and positive lease")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return NormalizationClaim{}, false, fmt.Errorf("begin normalization claim: %w", err)
	}
	defer rollbackTx(ctx, tx, "normalization claim", &claimErr)
	if _, err := tx.Exec(ctx, `
		UPDATE normalization_jobs
		SET status = 'failed', lease_owner = '', lease_expires_at = NULL,
		    fencing_token = NULL, last_error = 'maximum attempts exhausted after lease expiry',
		    updated_at = now()
		WHERE status = 'running' AND lease_expires_at <= now() AND attempts >= max_attempts`); err != nil {
		return NormalizationClaim{}, false, fmt.Errorf("fail exhausted normalization jobs: %w", err)
	}
	err = tx.QueryRow(ctx, `
		SELECT raw_event_id, normalizer_version, attempts, max_attempts
		FROM normalization_jobs
		WHERE (status = 'queued' AND GREATEST(due_at, COALESCE(retry_at, due_at)) <= now()
		       AND attempts < max_attempts)
		   OR (status = 'running' AND lease_expires_at <= now() AND attempts < max_attempts)
		ORDER BY GREATEST(due_at, COALESCE(retry_at, due_at)), created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED`).Scan(&claim.RawEventID, &claim.NormalizerVersion, &claim.Attempts, &claim.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return NormalizationClaim{}, false, fmt.Errorf("commit empty normalization claim: %w", err)
		}
		return NormalizationClaim{}, false, nil
	}
	if err != nil {
		return NormalizationClaim{}, false, fmt.Errorf("select due normalization job: %w", err)
	}
	claim.Owner = owner
	claim.Token = uuid.New()
	err = tx.QueryRow(ctx, `
		UPDATE normalization_jobs
		SET status = 'running', attempts = attempts + 1, lease_owner = $3,
		    lease_expires_at = now() + ($4 * interval '1 microsecond'),
		    fencing_token = $5, updated_at = now()
		WHERE raw_event_id = $1 AND normalizer_version = $2
		RETURNING attempts, lease_expires_at`,
		claim.RawEventID, claim.NormalizerVersion, owner, lease.Microseconds(), claim.Token).
		Scan(&claim.Attempts, &claim.LeaseExpiry)
	if err != nil {
		return NormalizationClaim{}, false, fmt.Errorf("assign normalization lease: %w", err)
	}
	if err := loadRawEvent(ctx, tx, claim.RawEventID, &claim.Event); err != nil {
		return NormalizationClaim{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NormalizationClaim{}, false, fmt.Errorf("commit normalization claim: %w", err)
	}
	claim.LeaseExpiry = claim.LeaseExpiry.UTC()
	return claim, true, nil
}

// RenewNormalizationLease extends a live normalization claim.
func (s *Store) RenewNormalizationLease(ctx context.Context, claim NormalizationClaim, lease time.Duration) error {
	if lease <= 0 {
		return errors.New("normalization lease duration must be positive")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs
		SET lease_expires_at = now() + ($3 * interval '1 microsecond'), updated_at = now()
		WHERE raw_event_id = $1 AND normalizer_version = $2
		  AND status = 'running' AND fencing_token = $4 AND lease_expires_at > now()`,
		claim.RawEventID, claim.NormalizerVersion, lease.Microseconds(), claim.Token)
	if err != nil {
		return fmt.Errorf("renew normalization lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("renew normalization lease: %w", ErrLeaseLost)
	}
	return nil
}

// CompleteNormalization atomically saves normalized actions and identity links.
func (s *Store) CompleteNormalization(ctx context.Context, claim NormalizationClaim, actions []event.Action) (completeErr error) {
	seenActions := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if err := action.Validate(); err != nil {
			return fmt.Errorf("validate normalized action: %w", err)
		}
		if _, exists := seenActions[action.Key]; exists {
			return fmt.Errorf("duplicate normalized action key %q", action.Key)
		}
		seenActions[action.Key] = struct{}{}
		if err := validateActors(action.Actors); err != nil {
			return fmt.Errorf("validate normalized action actors: %w", err)
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin normalized action persistence: %w", err)
	}
	defer rollbackTx(ctx, tx, "normalized action persistence", &completeErr)
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return err
	}
	var lockedEventID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT raw_event_id FROM normalization_jobs
		WHERE raw_event_id = $1 AND normalizer_version = $2
		  AND status = 'running' AND fencing_token = $3 AND lease_expires_at > now()
		FOR UPDATE`, claim.RawEventID, claim.NormalizerVersion, claim.Token).Scan(&lockedEventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("complete normalization: %w", ErrLeaseLost)
	}
	if err != nil {
		return fmt.Errorf("lock normalization job: %w", err)
	}
	previousLinks := make(map[string][]personLink)
	rows, err := tx.Query(ctx, `
		SELECT ne.action_key, ep.person_id, ep.role, ep.identity_claim_id
		FROM normalized_events ne
		JOIN event_people ep ON ep.normalized_event_id = ne.id
		WHERE ne.raw_event_id = $1 AND ne.is_current
		ORDER BY ne.action_key, ep.person_id, ep.role
		FOR UPDATE OF ne`, claim.RawEventID)
	if err != nil {
		return fmt.Errorf("load prior normalization links: %w", err)
	}
	for rows.Next() {
		var actionKey string
		var link personLink
		if err := rows.Scan(&actionKey, &link.personID, &link.role, &link.claimID); err != nil {
			rows.Close()
			return fmt.Errorf("scan prior normalization link: %w", err)
		}
		previousLinks[actionKey] = append(previousLinks[actionKey], link)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate prior normalization links: %w", err)
	}
	rows.Close()
	if _, err := tx.Exec(ctx, `
		UPDATE normalized_events SET is_current = false
		WHERE raw_event_id = $1 AND is_current`, claim.RawEventID); err != nil {
		return fmt.Errorf("mark prior actions non-current: %w", err)
	}
	for _, action := range actions {
		var normalizedID uuid.UUID
		var occurredAt time.Time
		err := tx.QueryRow(ctx, `
			INSERT INTO normalized_events
				(id, raw_event_id, normalizer_version, action_key, kind, occurred_at,
				 project, title, url, is_current)
			VALUES ($1, $2, $3, $4, $5,
				COALESCE((SELECT occurred_at FROM normalized_events
				          WHERE raw_event_id = $2 AND action_key = $4
				          ORDER BY created_at, normalizer_version LIMIT 1), $6),
				$7, $8, $9, true)
			ON CONFLICT (raw_event_id, normalizer_version, action_key)
			DO UPDATE SET kind = EXCLUDED.kind, project = EXCLUDED.project,
			              title = EXCLUDED.title, url = EXCLUDED.url, is_current = true
			RETURNING id, occurred_at`,
			uuid.New(), claim.RawEventID, claim.NormalizerVersion, action.Key,
			string(action.Kind), action.OccurredAt.UTC(), action.Project, action.Title, action.URL).
			Scan(&normalizedID, &occurredAt)
		if err != nil {
			return fmt.Errorf("save normalized action %s: %w", action.Key, err)
		}
		links := make([]personLink, 0, len(action.Actors))
		if action.Actors == nil {
			links = previousLinks[action.Key]
		} else {
			for _, actor := range action.Actors {
				link, found, err := resolveActor(ctx, tx, actor)
				if err != nil {
					return fmt.Errorf("resolve normalized action actor: %w", err)
				}
				if found {
					links = append(links, link)
				}
			}
		}
		for _, link := range links {
			if _, err := tx.Exec(ctx, `
				INSERT INTO event_people
					(normalized_event_id, person_id, role, occurred_at, identity_claim_id)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (normalized_event_id, person_id, role)
				DO UPDATE SET occurred_at = EXCLUDED.occurred_at,
				              identity_claim_id = EXCLUDED.identity_claim_id`,
				normalizedID, link.personID, link.role, occurredAt, link.claimID); err != nil {
				return fmt.Errorf("save normalized event person link: %w", err)
			}
		}
	}
	tag, err := tx.Exec(ctx, `
		UPDATE normalization_jobs
		SET status = 'succeeded', lease_owner = '', lease_expires_at = NULL,
		    fencing_token = NULL, retry_at = NULL, last_error = '', updated_at = now()
		WHERE raw_event_id = $1 AND normalizer_version = $2
		  AND status = 'running' AND fencing_token = $3`,
		claim.RawEventID, claim.NormalizerVersion, claim.Token)
	if err != nil {
		return fmt.Errorf("complete normalization job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("complete normalization job: %w", ErrLeaseLost)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit normalized action persistence: %w", err)
	}
	return nil
}

// RetryNormalizationJob releases failed work with a due retry or marks it failed.
func (s *Store) RetryNormalizationJob(ctx context.Context, claim NormalizationClaim, cause error, retryAt time.Time) error {
	if cause == nil {
		return errors.New("normalization retry requires a cause")
	}
	status := "queued"
	if claim.Attempts >= claim.MaxAttempts {
		status = "failed"
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs
		SET status = $3, retry_at = CASE WHEN $3 = 'queued' THEN $4::timestamptz ELSE NULL END,
		    lease_owner = '', lease_expires_at = NULL, fencing_token = NULL,
		    last_error = $5, updated_at = now()
		WHERE raw_event_id = $1 AND normalizer_version = $2
		  AND status = 'running' AND fencing_token = $6 AND lease_expires_at > now()`,
		claim.RawEventID, claim.NormalizerVersion, status, retryAt.UTC(), cause.Error(), claim.Token)
	if err != nil {
		return fmt.Errorf("retry normalization job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("retry normalization job: %w", ErrLeaseLost)
	}
	return nil
}

// FailNormalizationJob permanently fails work that cannot be handled by the
// configured normalizer registry.
func (s *Store) FailNormalizationJob(ctx context.Context, claim NormalizationClaim, reason string) error {
	if reason == "" {
		return errors.New("normalization failure reason is required")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs
		SET status = 'failed', lease_owner = '', lease_expires_at = NULL,
		    fencing_token = NULL, retry_at = NULL, last_error = $3, updated_at = now()
		WHERE raw_event_id = $1 AND normalizer_version = $2
		  AND status = 'running' AND fencing_token = $4 AND lease_expires_at > now()`,
		claim.RawEventID, claim.NormalizerVersion, reason, claim.Token)
	if err != nil {
		return fmt.Errorf("fail normalization job: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("fail normalization job: %w", ErrLeaseLost)
	}
	return nil
}

// QueueRenormalization adds a bounded batch of raw events for a new normalizer version.
func (s *Store) QueueRenormalization(ctx context.Context, version int, after uuid.UUID, limit int) (last uuid.UUID, done bool, queueErr error) {
	if version < 1 || limit < 1 || limit > 1000 {
		return uuid.Nil, false, errors.New("renormalization requires a positive version and a batch limit from 1 to 1000")
	}
	rows, err := s.db.Query(ctx,
		"SELECT id FROM raw_events WHERE id > $1 ORDER BY id LIMIT $2",
		after, limit+1)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("select raw events for renormalization: %w", err)
	}
	defer rows.Close()
	rawEventIDs := make([]uuid.UUID, 0, limit+1)
	for rows.Next() {
		var rawEventID uuid.UUID
		if err := rows.Scan(&rawEventID); err != nil {
			return uuid.Nil, false, fmt.Errorf("scan raw event for renormalization: %w", err)
		}
		rawEventIDs = append(rawEventIDs, rawEventID)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, false, fmt.Errorf("iterate raw events for renormalization: %w", err)
	}
	rows.Close()
	done = len(rawEventIDs) <= limit
	if !done {
		rawEventIDs = rawEventIDs[:limit]
	}
	if len(rawEventIDs) == 0 {
		return uuid.Nil, true, nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("begin renormalization enqueue: %w", err)
	}
	defer rollbackTx(ctx, tx, "renormalization enqueue", &queueErr)
	for _, rawEventID := range rawEventIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO normalization_jobs (raw_event_id, normalizer_version)
			VALUES ($1, $2)
			ON CONFLICT (raw_event_id, normalizer_version) DO NOTHING`,
			rawEventID, version); err != nil {
			return uuid.Nil, false, fmt.Errorf("enqueue raw event %s for renormalization: %w", rawEventID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, false, fmt.Errorf("commit renormalization enqueue: %w", err)
	}
	return rawEventIDs[len(rawEventIDs)-1], done, nil
}

func loadRawEvent(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, rawEventID uuid.UUID, raw *event.RawEvent) error {
	var occurredAt *time.Time
	var actorJSON []byte
	err := q.QueryRow(ctx, `
		SELECT forge_instance_id, source_key, external_id, kind, occurred_at,
		       payload, payload_schema_version, actor_keys
		FROM raw_events WHERE id = $1`, rawEventID).Scan(
		&raw.ForgeInstanceID, &raw.Source, &raw.ExternalID, &raw.Kind, &occurredAt,
		&raw.Payload, &raw.PayloadSchemaVersion, &actorJSON)
	if err != nil {
		return fmt.Errorf("load raw event %s: %w", rawEventID, err)
	}
	if occurredAt != nil {
		value := occurredAt.UTC()
		raw.OccurredAt = &value
	}
	if err := json.Unmarshal(actorJSON, &raw.Actors); err != nil {
		return fmt.Errorf("decode raw event actor keys: %w", err)
	}
	return nil
}

type actorResolver interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

type personLink struct {
	personID uuid.UUID
	role     string
	claimID  uuid.UUID
}

func resolveActor(ctx context.Context, q actorResolver, actor event.Actor) (personLink, bool, error) {
	var provider string
	if actor.Key.ForgeInstanceID != nil {
		if err := q.QueryRow(ctx, "SELECT provider FROM forge_instances WHERE id = $1",
			*actor.Key.ForgeInstanceID).Scan(&provider); err != nil {
			return personLink{}, false, fmt.Errorf("load actor forge provider: %w", err)
		}
	}
	normalized, err := identity.Normalize(provider, actor.Key.IdentityKey.Kind, actor.Key.IdentityKey.Value)
	if err != nil {
		return personLink{}, false, fmt.Errorf("normalize actor identity: %w", err)
	}
	var result personLink
	if actor.Key.ForgeInstanceID == nil {
		err = q.QueryRow(ctx, `
			SELECT ia.person_id, ia.claim_id
			FROM identity_keys ik
			JOIN identity_assignments ia ON ia.identity_key_id = ik.id
			WHERE ik.forge_instance_id IS NULL AND ik.kind = $1 AND ik.normalized_value = $2`,
			actor.Key.IdentityKey.Kind, normalized).Scan(&result.personID, &result.claimID)
	} else {
		err = q.QueryRow(ctx, `
			SELECT ia.person_id, ia.claim_id
			FROM identity_keys ik
			JOIN identity_assignments ia ON ia.identity_key_id = ik.id
			WHERE ik.forge_instance_id = $1 AND ik.kind = $2 AND ik.normalized_value = $3`,
			*actor.Key.ForgeInstanceID, actor.Key.IdentityKey.Kind, normalized).
			Scan(&result.personID, &result.claimID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return personLink{}, false, nil
	}
	if err != nil {
		return personLink{}, false, fmt.Errorf("resolve assigned actor identity: %w", err)
	}
	result.role = actor.Role
	return result, true, nil
}

func validateActors(actors []event.Actor) error {
	seen := make(map[string]struct{}, len(actors))
	for _, actor := range actors {
		if actor.Role == "" {
			return errors.New("actor role is required")
		}
		if err := actor.Key.Validate(); err != nil {
			return err
		}
		key := string(actor.Key.IdentityKey.Kind) + "\x00" + actor.Key.IdentityKey.Value +
			"\x00" + actor.Role
		if actor.Key.ForgeInstanceID != nil {
			key += "\x00" + actor.Key.ForgeInstanceID.String()
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate actor role %q", actor.Role)
		}
		seen[key] = struct{}{}
	}
	return nil
}
