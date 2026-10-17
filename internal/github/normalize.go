package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/normalizer"
)

type issuePayload struct {
	Title         string     `json:"title"`
	HTMLURL       string     `json:"html_url"`
	CreatedAt     time.Time  `json:"created_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	MergedAt      *time.Time `json:"merged_at"`
	RepositoryURL string     `json:"repository_url"`
}

type reviewPayload struct {
	State      string `json:"state"`
	HTMLURL    string `json:"html_url"`
	Repository string `json:"repository"`
}

type releasePayload struct {
	Name       string `json:"name"`
	HTMLURL    string `json:"html_url"`
	Repository string `json:"repository"`
}

// NewHistoryNormalizer returns handlers for GitHub person-history records.
func NewHistoryNormalizer() (*normalizer.Registry, error) {
	registry := normalizer.New()
	handlers := []struct {
		kind event.Kind
		fn   normalizer.Handler
	}{
		{kind: "issue", fn: normalizeIssue},
		{kind: "pull_request", fn: normalizePullRequest},
		{kind: "review", fn: normalizeReview},
		{kind: "release", fn: normalizeRelease},
	}
	for _, handler := range handlers {
		if err := registry.Register(historySource, handler.kind, handler.fn); err != nil {
			return nil, fmt.Errorf("register GitHub %s normalizer: %w", handler.kind, err)
		}
	}
	return registry, nil
}

func normalizeIssue(raw event.RawEvent) ([]event.Action, error) {
	var payload issuePayload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode issue payload: %w", err)
	}
	if payload.CreatedAt.IsZero() {
		return nil, errors.New("issue payload has no creation timestamp")
	}
	project := repoName(payload.RepositoryURL, "https://api.github.com")
	if project == "" {
		project = projectFromExternalID(raw.ExternalID, "issue:")
	}
	actions := []event.Action{{
		Key: "opened", Kind: "issue_opened", OccurredAt: payload.CreatedAt,
		Project: project, Title: payload.Title, URL: payload.HTMLURL,
		Actors: raw.Actors,
	}}
	if payload.ClosedAt != nil && !payload.ClosedAt.IsZero() {
		actions = append(actions, event.Action{
			Key: "closed", Kind: "issue_closed", OccurredAt: *payload.ClosedAt,
			Project: project, Title: payload.Title, URL: payload.HTMLURL, Actors: raw.Actors,
		})
	}
	return actions, nil
}

func normalizePullRequest(raw event.RawEvent) ([]event.Action, error) {
	var payload issuePayload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode pull request payload: %w", err)
	}
	if payload.CreatedAt.IsZero() {
		return nil, errors.New("pull request payload has no creation timestamp")
	}
	project := repoName(payload.RepositoryURL, "https://api.github.com")
	if project == "" {
		project = projectFromExternalID(raw.ExternalID, "pull_request:")
	}
	actions := []event.Action{{
		Key: "opened", Kind: "pull_request_opened", OccurredAt: payload.CreatedAt,
		Project: project, Title: payload.Title, URL: payload.HTMLURL, Actors: raw.Actors,
	}}
	if payload.MergedAt != nil && !payload.MergedAt.IsZero() {
		actions = append(actions, event.Action{
			Key: "merged", Kind: "pull_request_merged", OccurredAt: *payload.MergedAt,
			Project: project, Title: payload.Title, URL: payload.HTMLURL, Actors: raw.Actors,
		})
	} else if payload.ClosedAt != nil && !payload.ClosedAt.IsZero() {
		actions = append(actions, event.Action{
			Key: "closed", Kind: "pull_request_closed", OccurredAt: *payload.ClosedAt,
			Project: project, Title: payload.Title, URL: payload.HTMLURL, Actors: raw.Actors,
		})
	}

	return actions, nil
}

func projectFromExternalID(externalID, prefix string) string {
	remainder := strings.TrimPrefix(externalID, prefix)
	project, _, ok := strings.Cut(remainder, ":")
	if !ok {
		return ""
	}
	return project
}

func normalizeReview(raw event.RawEvent) ([]event.Action, error) {
	var payload reviewPayload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode review payload: %w", err)
	}
	if raw.OccurredAt == nil || raw.OccurredAt.IsZero() {
		return nil, errors.New("review payload has no submission timestamp")
	}
	project := payload.Repository
	if project == "" {
		project = projectFromExternalID(raw.ExternalID, "review:")
	}
	return []event.Action{{
		Key: "submitted", Kind: "pull_request_reviewed", OccurredAt: *raw.OccurredAt,
		Project: project, Title: strings.TrimSpace(payload.State),
		URL: payload.HTMLURL, Actors: raw.Actors,
	}}, nil
}

func normalizeRelease(raw event.RawEvent) ([]event.Action, error) {
	var payload releasePayload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode release payload: %w", err)
	}
	if raw.OccurredAt == nil || raw.OccurredAt.IsZero() {
		return nil, errors.New("release payload has no publication timestamp")
	}
	project := payload.Repository
	if project == "" {
		project = projectFromExternalID(raw.ExternalID, "release:")
	}
	return []event.Action{{
		Key: "published", Kind: "release_published", OccurredAt: *raw.OccurredAt,
		Project: project, Title: payload.Name,
		URL: payload.HTMLURL, Actors: raw.Actors,
	}}, nil
}
