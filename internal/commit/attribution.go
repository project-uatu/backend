// SPDX-License-Identifier: Apache-2.0

// Package commit resolves commit author evidence against already assigned identities.
package commit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
	"github.com/project-uatu/backend/internal/normalizer"
)

// Kind identifies an upstream commit record.
const Kind event.Kind = "commit"

// ActionKind identifies an authored commit contribution.
const ActionKind event.Kind = "commit_authored"

// Payload contains commit metadata that is safe to persist.
type Payload struct {
	SHA              string    `json:"sha"`
	Repository       string    `json:"repository"`
	URL              string    `json:"url"`
	AuthoredAt       time.Time `json:"authored_at"`
	Merge            bool      `json:"merge"`
	AttributionBasis string    `json:"attribution_basis"`
}

// Attribution contains only actor identities already assigned to the tracked person.
type Attribution struct {
	Actors []event.Actor
	Basis  string
}

// Register adds the commit normalizer for one source.
func Register(registry *normalizer.Registry, source event.Source) error {
	if err := registry.Register(source, Kind, normalize); err != nil {
		return fmt.Errorf("register %s commit normalizer: %w", source, err)
	}
	return nil
}

// ResolveAttribution matches a commit author or co-author trailer to assigned identities.
func ResolveAttribution(request event.PageRequest, provider, accountID, authorEmail, message string) Attribution {
	if accountID != "" {
		if actor, ok := assignedAccount(request, accountID); ok {
			return Attribution{Actors: []event.Actor{actor}, Basis: "account_id"}
		}
	}
	if accountID == "" && provider == "github" {
		if id, ok := githubNoreplyAccountID(authorEmail); ok {
			if actor, assigned := assignedAccount(request, id); assigned {
				return Attribution{Actors: []event.Actor{actor}, Basis: "noreply"}
			}
		}
	}
	if accountID == "" || provider != "github" {
		if actor, ok := assignedEmail(request, provider, authorEmail); ok {
			return Attribution{Actors: []event.Actor{actor}, Basis: "assigned_email"}
		}
	}
	for _, email := range coAuthorEmails(message) {
		if actor, ok := assignedEmail(request, provider, email); ok {
			actor.Role = "co_author"
			return Attribution{Actors: []event.Actor{actor}, Basis: "coauthored_by"}
		}
	}
	return Attribution{}
}

func githubNoreplyAccountID(email string) (string, bool) {
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email {
		return "", false
	}
	local, domain, ok := strings.Cut(address.Address, "@")
	if !ok || !strings.EqualFold(domain, "users.noreply.github.com") {
		return "", false
	}
	id, login, ok := strings.Cut(local, "+")
	if !ok || login == "" || strings.Contains(login, "+") || len(login) > 39 {
		return "", false
	}
	for _, digit := range id {
		if digit < '0' || digit > '9' {
			return "", false
		}
	}
	for index, character := range login {
		valid := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' && index > 0 && index < len(login)-1
		if !valid {
			return "", false
		}
	}
	accountID, err := strconv.ParseInt(id, 10, 64)
	if err != nil || accountID <= 0 {
		return "", false
	}
	return strconv.FormatInt(accountID, 10), true
}

// GitHubAuthorFilters returns deterministic author query values without putting identities in cursors.
func GitHubAuthorFilters(request event.PageRequest) []string {
	filters := make([]string, 0, len(request.IdentityKeys)+1)
	seen := make(map[string]struct{}, len(request.IdentityKeys)+1)
	add := func(value string) {
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		filters = append(filters, value)
	}
	add(request.CurrentLogin)
	if request.CurrentLogin != "" {
		for _, key := range request.IdentityKeys {
			if key.IdentityKey.Kind == identity.KindAccountID &&
				key.ForgeInstanceID != nil && *key.ForgeInstanceID == request.ForgeInstanceID {
				accountID, err := strconv.ParseInt(key.IdentityKey.Value, 10, 64)
				if err == nil && accountID > 0 {
					add(strconv.FormatInt(accountID, 10) + "+" + request.CurrentLogin + "@users.noreply.github.com")
				}
			}
		}
	}
	var emails []string
	for _, key := range request.IdentityKeys {
		if key.IdentityKey.Kind != identity.KindEmail {
			continue
		}
		normalized, err := identity.Normalize("github", identity.KindEmail, key.IdentityKey.Value)
		if err == nil {
			emails = append(emails, normalized)
		}
	}
	slices.Sort(emails)
	for _, email := range emails {
		add(email)
	}
	return filters
}

func normalize(raw event.RawEvent) ([]event.Action, error) {
	var payload Payload
	if err := json.Unmarshal(raw.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode commit payload: %w", err)
	}
	if payload.SHA == "" || payload.Repository == "" || payload.URL == "" ||
		payload.AuthoredAt.IsZero() || payload.Merge {
		return nil, errors.New("commit payload is incomplete or describes a merge commit")
	}
	switch payload.AttributionBasis {
	case "account_id", "noreply", "assigned_email", "coauthored_by":
	default:
		return nil, errors.New("commit payload has an invalid attribution basis")
	}
	return []event.Action{{
		Key: "authored", Kind: ActionKind, OccurredAt: payload.AuthoredAt.UTC(),
		Project: payload.Repository, URL: payload.URL, Actors: raw.Actors,
	}}, nil
}

func assignedAccount(request event.PageRequest, accountID string) (event.Actor, bool) {
	for _, key := range request.IdentityKeys {
		if key.IdentityKey.Kind == identity.KindAccountID &&
			key.IdentityKey.Value == accountID &&
			key.ForgeInstanceID != nil &&
			*key.ForgeInstanceID == request.ForgeInstanceID {
			return event.Actor{Key: key, Role: "author"}, true
		}
	}
	return event.Actor{}, false
}

func assignedEmail(request event.PageRequest, provider, email string) (event.Actor, bool) {
	if email == "" {
		return event.Actor{}, false
	}
	normalized, err := identity.Normalize(provider, identity.KindEmail, email)
	if err != nil {
		return event.Actor{}, false
	}
	for _, key := range request.IdentityKeys {
		if key.IdentityKey.Kind != identity.KindEmail {
			continue
		}
		assigned, err := identity.Normalize(provider, identity.KindEmail, key.IdentityKey.Value)
		if err == nil && assigned == normalized {
			return event.Actor{Key: key, Role: "author"}, true
		}
	}
	return event.Actor{}, false
}

func coAuthorEmails(message string) []string {
	lines := strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n")
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 {
		return nil
	}
	start := end
	for start >= 0 {
		line := strings.TrimSpace(lines[start])
		if line == "" {
			break
		}
		key, _, ok := strings.Cut(line, ":")
		if !ok || !validTrailerKey(key) {
			break
		}
		start--
	}
	if start == end || start < 0 || strings.TrimSpace(lines[start]) != "" {
		return nil
	}
	emails := make([]string, 0, end-start)
	for _, line := range lines[start+1 : end+1] {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || !strings.EqualFold(key, "Co-authored-by") {
			continue
		}
		address, err := mail.ParseAddress(strings.TrimSpace(value))
		if err == nil {
			emails = append(emails, address.Address)
		}
	}
	return emails
}

func validTrailerKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r < 'A' || r > 'Z' {
			if r < 'a' || r > 'z' {
				if r < '0' || r > '9' {
					if r != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}
