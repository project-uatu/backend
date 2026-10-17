// Package event defines the typed values exchanged by collection, normalization and storage.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/identity"
)

// Source identifies an upstream event source.
type Source string

// Kind identifies a source event or normalized contribution action.
type Kind string

// ActorKey identifies one person on a source event.
type ActorKey struct {
	IdentityKey     identity.Key `json:"identity_key"`
	ForgeInstanceID *uuid.UUID   `json:"forge_instance_id,omitempty"`
}

// Actor associates an identity key with a role on an event.
type Actor struct {
	Key  ActorKey `json:"key"`
	Role string   `json:"role"`
}

// Window is a half-open source scan interval. A zero From means the beginning
// of available history.
type Window struct {
	From time.Time
	To   time.Time
}

// PageRequest identifies one bounded connector request.
type PageRequest struct {
	PersonID          uuid.UUID
	ForgeInstanceID   uuid.UUID
	Source            Source
	ScanMode          string
	IdentityKeys      []ActorKey
	AccountNodeID     string
	CurrentLogin      string
	KnownRepositories []string
	Window            Window
	Cursor            string
	PageSize          int
}

// RawEvent is a source record before normalization.
type RawEvent struct {
	ForgeInstanceID      uuid.UUID       `json:"forge_instance_id"`
	Source               Source          `json:"source"`
	ExternalID           string          `json:"external_id"`
	Kind                 Kind            `json:"kind"`
	OccurredAt           *time.Time      `json:"occurred_at,omitempty"`
	Payload              json.RawMessage `json:"payload"`
	PayloadSchemaVersion int             `json:"payload_schema_version"`
	Actors               []Actor         `json:"actors"`
}

// Page is one bounded result from a connector.
type Page struct {
	Events     []RawEvent
	NextCursor string
	Done       bool
}

// Gap records a source interval that could not be fully scanned.
type Gap struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Reason string    `json:"reason"`
}

// FetchReport records page, rate-limit and coverage information. It remains
// valid when a connector returns an error.
type FetchReport struct {
	Requests           int                 `json:"requests"`
	Pages              int                 `json:"pages"`
	RateLimitBucket    string              `json:"rate_limit_bucket,omitempty"`
	RateLimitLimit     *int                `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining *int                `json:"rate_limit_remaining,omitempty"`
	RateLimitResetAt   *time.Time          `json:"rate_limit_reset_at,omitempty"`
	Gaps               []Gap               `json:"gaps,omitempty"`
	Errors             []string            `json:"errors,omitempty"`
	ContributionCounts []ContributionCount `json:"contribution_counts,omitempty"`
	CoverageScope      string              `json:"coverage_scope,omitempty"`
	KnownRepositories  []string            `json:"known_repositories,omitempty"`
}

// ContributionCount is an upstream aggregate observation, not a timeline event.
type ContributionCount struct {
	Date   time.Time `json:"date"`
	Metric string    `json:"metric"`
	Kind   string    `json:"kind,omitempty"`
	Value  int64     `json:"value"`
}

// Action is one normalized contribution derived from a raw event.
type Action struct {
	Key        string
	Kind       Kind
	OccurredAt time.Time
	Project    string
	Title      string
	URL        string
	Actors     []Actor
}

// Validate checks that the request describes a valid, bounded page fetch.
func (r PageRequest) Validate() error {
	if r.PersonID == uuid.Nil || r.ForgeInstanceID == uuid.Nil || r.Source == "" {
		return errors.New("page request requires person, forge instance and source")
	}
	if r.PageSize < 1 || r.PageSize > 500 {
		return errors.New("page size must be between 1 and 500")
	}
	if !r.Window.To.IsZero() && !r.Window.From.IsZero() && !r.Window.From.Before(r.Window.To) {
		return errors.New("page window must have a start before its end")
	}
	for _, key := range r.IdentityKeys {
		if err := key.Validate(); err != nil {
			return fmt.Errorf("validate request identity key: %w", err)
		}
	}
	return nil
}

// Validate checks that a source actor key is correctly scoped.
func (k ActorKey) Validate() error {
	if err := k.IdentityKey.Validate(); err != nil {
		return err
	}
	if k.IdentityKey.Kind == identity.KindEmail {
		if k.ForgeInstanceID != nil {
			return errors.New("email actor keys must be global")
		}
		return nil
	}
	if k.ForgeInstanceID == nil || *k.ForgeInstanceID == uuid.Nil {
		return errors.New("forge actor keys require a forge instance")
	}
	return nil
}

// Validate checks that the raw source record is safe to persist.
func (e RawEvent) Validate(forgeInstanceID uuid.UUID) error {
	if e.ForgeInstanceID != forgeInstanceID || e.ForgeInstanceID == uuid.Nil ||
		e.Source == "" || e.ExternalID == "" || e.Kind == "" {
		return errors.New("raw event requires matching forge instance, source, external ID and kind")
	}
	if !json.Valid(e.Payload) || e.PayloadSchemaVersion < 1 {
		return errors.New("raw event requires valid JSON payload and positive schema version")
	}
	for _, actor := range e.Actors {
		if actor.Role == "" {
			return errors.New("raw event actor role is required")
		}
		if err := actor.Key.Validate(); err != nil {
			return fmt.Errorf("validate raw event actor: %w", err)
		}
	}
	return nil
}

// Validate checks that a normalized action can be persisted.
func (a Action) Validate() error {
	if a.Key == "" || a.Kind == "" || a.OccurredAt.IsZero() {
		return errors.New("normalized action requires key, kind and occurrence time")
	}
	for _, actor := range a.Actors {
		if actor.Role == "" {
			return errors.New("normalized action actor role is required")
		}
		if err := actor.Key.Validate(); err != nil {
			return fmt.Errorf("validate normalized action actor: %w", err)
		}
	}
	return nil
}
