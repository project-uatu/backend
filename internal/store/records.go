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

// Person is a tracked contributor.
type Person struct {
	ID          uuid.UUID
	DisplayName *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ForgeInstance identifies one hosted source platform.
type ForgeInstance struct {
	ID        uuid.UUID
	Provider  string
	BaseURL   string
	CreatedAt time.Time
}

// IdentityKey is a source-specific identifier or a global email address.
type IdentityKey struct {
	ID              uuid.UUID
	ForgeInstanceID *uuid.UUID
	Kind            string
	Value           string
	Normalized      string
}

// IdentityClaim records evidence associating a key with a person.
type IdentityClaim struct {
	ID            uuid.UUID
	IdentityKeyID uuid.UUID
	PersonID      uuid.UUID
	Method        string
	AssertedBy    string
	ReviewedBy    string
	Evidence      json.RawMessage
	Disposition   string
	AssertedAt    time.Time
}

// IdentityClaimInput describes a proposed, accepted or rejected identity claim.
type IdentityClaimInput struct {
	IdentityKey IdentityKey
	PersonID    uuid.UUID
	Method      string
	AssertedBy  string
	ReviewedBy  string
	Evidence    json.RawMessage
	Disposition string
}

// CreatePerson creates a tracked person.
func (s *Store) CreatePerson(ctx context.Context, displayName *string) (Person, error) {
	person := Person{ID: uuid.New(), DisplayName: displayName}
	err := s.db.QueryRow(ctx, `
		INSERT INTO persons (id, display_name)
		VALUES ($1, $2)
		RETURNING created_at, updated_at`, person.ID, displayName).
		Scan(&person.CreatedAt, &person.UpdatedAt)
	if err != nil {
		return Person{}, fmt.Errorf("create person: %w", err)
	}
	return person, nil
}

// DeletePerson removes a person and all person-scoped records.
func (s *Store) DeletePerson(ctx context.Context, personID uuid.UUID) (deleteErr error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin person deletion: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			deleteErr = errors.Join(deleteErr, fmt.Errorf("rollback person deletion: %w", err))
		}
	}()
	tag, err := tx.Exec(ctx, "DELETE FROM persons WHERE id = $1", personID)
	if err != nil {
		return fmt.Errorf("delete person %s: %w", personID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("delete person %s: %w", personID, ErrNotFound)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM identity_keys key
		WHERE NOT EXISTS (
			SELECT 1 FROM identity_claims claim
			WHERE claim.identity_key_id = key.id
		)`); err != nil {
		return fmt.Errorf("delete orphaned identity keys: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit person deletion: %w", err)
	}
	return nil
}

// ErrNotFound indicates a requested stored record does not exist.
var ErrNotFound = errors.New("record not found")

// CreateForgeInstance creates a forge instance or returns its existing record.
func (s *Store) CreateForgeInstance(ctx context.Context, provider, baseURL string) (ForgeInstance, error) {
	if provider == "" || baseURL == "" {
		return ForgeInstance{}, errors.New("forge provider and base URL are required")
	}
	instance := ForgeInstance{ID: uuid.New(), Provider: provider, BaseURL: baseURL}
	err := s.db.QueryRow(ctx, `
		INSERT INTO forge_instances (id, provider, base_url)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, base_url)
		DO UPDATE SET provider = EXCLUDED.provider
		RETURNING id, created_at`, instance.ID, provider, baseURL).
		Scan(&instance.ID, &instance.CreatedAt)
	if err != nil {
		return ForgeInstance{}, fmt.Errorf("create forge instance: %w", err)
	}
	return instance, nil
}

// ClaimIdentity stores identity evidence and updates the accepted assignment
// when the key is unassigned or already assigned to the same person. A claim
// conflicting with another person's assignment is recorded as a conflict.
func (s *Store) ClaimIdentity(ctx context.Context, input IdentityClaimInput) (claim IdentityClaim, claimErr error) {
	key := input.IdentityKey
	if key.Kind == "" || key.Value == "" || key.Normalized == "" || input.PersonID == uuid.Nil || input.Method == "" {
		return IdentityClaim{}, errors.New("identity key, person, and claim method are required")
	}
	if !json.Valid(input.Evidence) && len(input.Evidence) > 0 {
		return IdentityClaim{}, errors.New("identity claim evidence must be valid JSON")
	}
	if input.Disposition == "" {
		input.Disposition = "proposed"
	}
	switch input.Disposition {
	case "proposed", "accepted", "rejected":
	default:
		return IdentityClaim{}, fmt.Errorf("invalid identity claim disposition %q", input.Disposition)
	}
	if input.AssertedBy == "" {
		input.AssertedBy = "system"
	}
	if input.Method == "manual" && input.Disposition == "accepted" && input.ReviewedBy == "" {
		return IdentityClaim{}, errors.New("accepted manual identity claims require a reviewer")
	}
	if key.Kind == "email" && key.ForgeInstanceID != nil {
		return IdentityClaim{}, errors.New("email identity keys must be global")
	}
	if key.Kind != "email" && key.ForgeInstanceID == nil {
		return IdentityClaim{}, errors.New("forge identity keys require an instance")
	}
	if len(input.Evidence) == 0 {
		input.Evidence = json.RawMessage(`{}`)
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return IdentityClaim{}, fmt.Errorf("begin identity claim: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			claimErr = errors.Join(claimErr, fmt.Errorf("rollback identity claim: %w", err))
		}
	}()
	if err := lockEventIdentityChanges(ctx, tx); err != nil {
		return IdentityClaim{}, err
	}

	key.ID = uuid.New()
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity_keys (id, forge_instance_id, kind, value, normalized_value)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT DO NOTHING`, key.ID, key.ForgeInstanceID, key.Kind, key.Value, key.Normalized); err != nil {
		return IdentityClaim{}, fmt.Errorf("insert identity key: %w", err)
	}
	if key.ForgeInstanceID == nil {
		err = tx.QueryRow(ctx, `
			SELECT id FROM identity_keys
			WHERE forge_instance_id IS NULL AND kind = $1 AND normalized_value = $2
			FOR UPDATE`, key.Kind, key.Normalized).Scan(&key.ID)
	} else {
		err = tx.QueryRow(ctx, `
			SELECT id FROM identity_keys
			WHERE forge_instance_id = $1 AND kind = $2 AND normalized_value = $3
			FOR UPDATE`, key.ForgeInstanceID, key.Kind, key.Normalized).Scan(&key.ID)
	}
	if err != nil {
		return IdentityClaim{}, fmt.Errorf("load identity key: %w", err)
	}

	disposition := input.Disposition
	var previousPerson uuid.UUID
	if disposition == "accepted" {
		var assignedPerson uuid.UUID
		err = tx.QueryRow(ctx,
			"SELECT person_id FROM identity_assignments WHERE identity_key_id = $1",
			key.ID).Scan(&assignedPerson)
		switch {
		case err == nil && assignedPerson != input.PersonID && input.Method != "manual":
			disposition = "conflict"
		case err == nil && assignedPerson != input.PersonID:
			previousPerson = assignedPerson
		case err == nil || errors.Is(err, pgx.ErrNoRows):
		default:
			return IdentityClaim{}, fmt.Errorf("read identity assignment: %w", err)
		}
	}

	claim = IdentityClaim{
		ID:            uuid.New(),
		IdentityKeyID: key.ID,
		PersonID:      input.PersonID,
		Method:        input.Method,
		AssertedBy:    input.AssertedBy,
		ReviewedBy:    input.ReviewedBy,
		Evidence:      input.Evidence,
		Disposition:   disposition,
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO identity_claims
			(id, identity_key_id, person_id, method, asserted_by, reviewed_by, evidence, disposition, reviewed_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8, CASE WHEN $6 <> '' THEN now() END)
		RETURNING asserted_at`, claim.ID, claim.IdentityKeyID, claim.PersonID,
		claim.Method, claim.AssertedBy, claim.ReviewedBy, claim.Evidence, claim.Disposition).
		Scan(&claim.AssertedAt)
	if err != nil {
		return IdentityClaim{}, fmt.Errorf("insert identity claim: %w", err)
	}
	if disposition == "accepted" {
		_, err := tx.Exec(ctx, `
			INSERT INTO identity_assignments (identity_key_id, person_id, claim_id, claim_disposition)
			VALUES ($1, $2, $3, 'accepted')
			ON CONFLICT (identity_key_id)
			DO UPDATE SET person_id = EXCLUDED.person_id,
			              claim_id = EXCLUDED.claim_id,
			              assigned_at = now()`,
			key.ID, input.PersonID, claim.ID)
		if err != nil {
			return IdentityClaim{}, fmt.Errorf("assign identity key: %w", err)
		}
		if previousPerson != uuid.Nil {
			if err := moveEventPeople(ctx, tx, key.ID, previousPerson, input.PersonID, claim.ID); err != nil {
				return IdentityClaim{}, err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE event_people ep
				SET identity_claim_id = $3
				FROM identity_claims previous_claim
				WHERE ep.identity_claim_id = previous_claim.id
				  AND previous_claim.identity_key_id = $1
				  AND ep.person_id = $2`, key.ID, input.PersonID, claim.ID); err != nil {
				return IdentityClaim{}, fmt.Errorf("refresh event person identity claims: %w", err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return IdentityClaim{}, fmt.Errorf("commit identity claim: %w", err)
	}
	return claim, nil
}

func moveEventPeople(ctx context.Context, tx pgx.Tx, identityKeyID, fromPersonID, toPersonID, claimID uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		DELETE FROM event_people previous
		USING identity_claims previous_claim
		WHERE previous.identity_claim_id = previous_claim.id
		  AND previous_claim.identity_key_id = $1
		  AND previous_claim.person_id = $2
		  AND EXISTS (
			SELECT 1 FROM event_people current
			WHERE current.normalized_event_id = previous.normalized_event_id
			  AND current.person_id = $3
			  AND current.role = previous.role
		  )`, identityKeyID, fromPersonID, toPersonID)
	if err != nil {
		return fmt.Errorf("remove duplicate event person links during identity reassignment: %w", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE event_people previous
		SET person_id = $3, identity_claim_id = $4
		FROM identity_claims previous_claim
		WHERE previous.identity_claim_id = previous_claim.id
		  AND previous_claim.identity_key_id = $1
		  AND previous_claim.person_id = $2`,
		identityKeyID, fromPersonID, toPersonID, claimID)
	if err != nil {
		return fmt.Errorf("move event person links during identity reassignment: %w", err)
	}
	return nil
}
