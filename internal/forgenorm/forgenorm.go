// SPDX-License-Identifier: Apache-2.0

// Package forgenorm normalizes the canonical public-activity payload emitted
// by the GitLab and Forgejo connectors into contribution actions.
package forgenorm

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/normalizer"
)

// Payload is the schema version 1 payload for issue, change_request, review
// and release raw events. Connectors build it from upstream records so that
// private and unrelated upstream fields are never persisted.
//
// A change_request payload with MergedAt set means the tracked person
// performed the merge: the merged action is credited to the raw event's actors,
// so connectors must leave MergedAt empty when someone else merged it.
type Payload struct {
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	Project    string     `json:"project"`
	State      string     `json:"state,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	ClosedAt   *time.Time `json:"closed_at,omitempty"`
	MergedAt   *time.Time `json:"merged_at,omitempty"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

// Raw event kinds produced by forge connectors.
const (
	KindIssue         event.Kind = "issue"
	KindChangeRequest event.Kind = "change_request"
	// KindChangeRequestMerged records only the merge of a change request whose
	// opening is not available from the source.
	KindChangeRequestMerged event.Kind = "change_request_merged"
	KindReview              event.Kind = "review"
	KindRelease             event.Kind = "release"
)

// Register adds the forge handlers for source to registry.
func Register(registry *normalizer.Registry, source event.Source) error {
	handlers := map[event.Kind]normalizer.Handler{
		KindIssue: issue, KindChangeRequest: changeRequest, KindChangeRequestMerged: mergeOnly, KindReview: review, KindRelease: release,
	}
	for kind, handler := range handlers {
		if err := registry.Register(source, kind, handler); err != nil {
			return fmt.Errorf("register %s %s normalizer: %w", source, kind, err)
		}
	}
	return nil
}

func decode(raw event.RawEvent) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(raw.Payload, &p); err != nil {
		return p, fmt.Errorf("decode %s payload: %w", raw.Kind, err)
	}
	return p, nil
}

func issue(raw event.RawEvent) ([]event.Action, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if p.CreatedAt == nil || p.CreatedAt.IsZero() {
		return nil, errors.New("issue payload has no creation timestamp")
	}
	actions := []event.Action{action("opened", "issue_opened", *p.CreatedAt, p, raw)}
	if p.ClosedAt != nil && !p.ClosedAt.IsZero() {
		actions = append(actions, action("closed", "issue_closed", *p.ClosedAt, p, raw))
	}
	return actions, nil
}

func changeRequest(raw event.RawEvent) ([]event.Action, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if p.CreatedAt == nil || p.CreatedAt.IsZero() {
		return nil, errors.New("change request payload has no creation timestamp")
	}
	actions := []event.Action{action("opened", "pull_request_opened", *p.CreatedAt, p, raw)}
	switch {
	case p.MergedAt != nil && !p.MergedAt.IsZero():
		actions = append(actions, action("merged", "pull_request_merged", *p.MergedAt, p, raw))
	case p.ClosedAt != nil && !p.ClosedAt.IsZero():
		actions = append(actions, action("closed", "pull_request_closed", *p.ClosedAt, p, raw))
	}
	return actions, nil
}

func mergeOnly(raw event.RawEvent) ([]event.Action, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	at := occurred(raw, p)
	if at.IsZero() {
		return nil, errors.New("merge payload has no timestamp")
	}
	return []event.Action{action("merged", "pull_request_merged", at, p, raw)}, nil
}

func review(raw event.RawEvent) ([]event.Action, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	at := occurred(raw, p)
	if at.IsZero() {
		return nil, errors.New("review payload has no timestamp")
	}
	a := action("submitted", "pull_request_reviewed", at, p, raw)
	a.Title = strings.TrimSpace(p.State)
	return []event.Action{a}, nil
}

func release(raw event.RawEvent) ([]event.Action, error) {
	p, err := decode(raw)
	if err != nil {
		return nil, err
	}
	at := occurred(raw, p)
	if at.IsZero() {
		return nil, errors.New("release payload has no publication timestamp")
	}
	return []event.Action{action("published", "release_published", at, p, raw)}, nil
}

func occurred(raw event.RawEvent, p Payload) time.Time {
	switch {
	case raw.OccurredAt != nil && !raw.OccurredAt.IsZero():
		return *raw.OccurredAt
	case p.OccurredAt != nil:
		return *p.OccurredAt
	}
	return time.Time{}
}

func action(key string, kind event.Kind, at time.Time, p Payload, raw event.RawEvent) event.Action {
	return event.Action{
		Key: key, Kind: kind, OccurredAt: at.UTC(), Project: p.Project,
		Title: p.Title, URL: p.URL, Actors: raw.Actors,
	}
}
