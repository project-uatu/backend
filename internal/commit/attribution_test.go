// SPDX-License-Identifier: Apache-2.0

package commit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
	"github.com/project-uatu/backend/internal/normalizer"
)

func TestResolveAttribution(t *testing.T) {
	forgeID := uuid.New()
	request := event.PageRequest{
		ForgeInstanceID: forgeID,
		IdentityKeys: []event.ActorKey{
			{IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "42"}, ForgeInstanceID: &forgeID},
			{IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "dev@Example.com"}},
		},
	}

	tests := []struct {
		name       string
		accountID  string
		email      string
		message    string
		wantRole   string
		wantBasis  string
		wantActors int
	}{
		{
			name:      "linked primary account",
			accountID: "42", email: "other@example.com",
			wantRole: "author", wantBasis: "account_id", wantActors: 1,
		},
		{
			name:      "different linked account blocks email attribution",
			accountID: "99", email: "dev@example.com",
		},
		{
			name:     "verified numeric noreply address",
			email:    "42+jane@users.noreply.github.com",
			wantRole: "author", wantBasis: "noreply", wantActors: 1,
		},
		{
			name:  "legacy noreply address does not verify account",
			email: "jane@users.noreply.github.com",
		},
		{
			name:  "malformed numeric noreply address does not verify account",
			email: "42+@users.noreply.github.com",
		},
		{
			name:     "assigned primary email",
			email:    "dev@example.COM",
			wantRole: "author", wantBasis: "assigned_email", wantActors: 1,
		},
		{
			name:     "assigned co-author trailer",
			email:    "other@example.com",
			message:  "A change\n\nReviewed-by: reviewer@example.com\nCo-authored-by: Dev Name <dev@example.com>\n",
			wantRole: "co_author", wantBasis: "coauthored_by", wantActors: 1,
		},
		{
			name:    "trailer name does not establish identity",
			email:   "other@example.com",
			message: "A change\n\nCo-authored-by: Dev Name <unassigned@example.com>\n",
		},
		{
			name:    "body mention is not a trailer",
			email:   "other@example.com",
			message: "A change\n\nCo-authored-by: Dev Name <dev@example.com>\n\nMore body text",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ResolveAttribution(request, "github", test.accountID, test.email, test.message)
			if len(got.Actors) != test.wantActors {
				t.Fatalf("ResolveAttribution() returned %d actors, want %d", len(got.Actors), test.wantActors)
			}
			if test.wantActors == 0 {
				if got.Basis != "" {
					t.Errorf("ResolveAttribution() basis = %q, want empty", got.Basis)
				}
				return
			}
			if got.Actors[0].Role != test.wantRole {
				t.Errorf("ResolveAttribution() role = %q, want %q", got.Actors[0].Role, test.wantRole)
			}
			if got.Basis != test.wantBasis {
				t.Errorf("ResolveAttribution() basis = %q, want %q", got.Basis, test.wantBasis)
			}
		})
	}
}

func TestGitHubAuthorFilters_includesVerifiedNoreplyAddress(t *testing.T) {
	forgeID := uuid.New()
	request := event.PageRequest{
		ForgeInstanceID: forgeID, CurrentLogin: "jane",
		IdentityKeys: []event.ActorKey{{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: "42"},
			ForgeInstanceID: &forgeID,
		}},
	}
	got := GitHubAuthorFilters(request)
	if len(got) != 2 || got[0] != "jane" || got[1] != "42+jane@users.noreply.github.com" {
		t.Errorf("GitHubAuthorFilters() = %v, want login and verified noreply email", got)
	}
}

func TestResolveAttribution_doesNotMatchAnotherForgeAccount(t *testing.T) {
	forgeID := uuid.New()
	otherForgeID := uuid.New()
	request := event.PageRequest{
		ForgeInstanceID: forgeID,
		IdentityKeys: []event.ActorKey{{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: "42"},
			ForgeInstanceID: &otherForgeID,
		}},
	}
	got := ResolveAttribution(request, "github", "42", "", "")
	if len(got.Actors) != 0 {
		t.Errorf("ResolveAttribution() returned %v actors, want none", got.Actors)
	}
}

func TestCommitNormalizer(t *testing.T) {
	registry := normalizer.New()
	if err := Register(registry, "github_commits"); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	payload, err := json.Marshal(Payload{
		SHA: "abc123", Repository: "owner/repo", URL: "https://github.com/owner/repo/commit/abc123",
		AuthoredAt: at, AttributionBasis: "account_id",
	})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := registry.Normalize(event.RawEvent{
		Source: "github_commits", Kind: Kind, Payload: payload, ExternalID: "commit:1:abc123",
		OccurredAt: &at, PayloadSchemaVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Kind != ActionKind || actions[0].Project != "owner/repo" {
		t.Errorf("Normalize() = %+v, want one commit_authored action for owner/repo", actions)
	}
}

func TestDecodeCursor(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "initial"},
		{name: "valid", value: `{"repository":1,"page":2,"pages":1,"author_filter":1,"verified":true}`},
		{name: "valid repository fingerprint", value: `{"page":1,"repository_set":"` + RepositorySetHash([]string{"owner/repo"}) + `"}`},
		{name: "invalid repository fingerprint", value: `{"page":1,"repository_set":"owner/repo"}`, wantErr: true},
		{name: "malformed", value: `{`, wantErr: true},
		{name: "invalid position", value: `{"page":0}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := DecodeCursor(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("DecodeCursor() error = %v, want error %t", err, test.wantErr)
			}
			if err == nil {
				encoded, err := got.Encode()
				if err != nil {
					t.Fatal(err)
				}
				if encoded == "" {
					t.Error("Encode() returned an empty cursor")
				}
			}
		})
	}
}

func TestRepositorySetHash_doesNotPersistRepositoryPaths(t *testing.T) {
	value := RepositorySetHash([]string{"owner/repo"})
	if value == "" || strings.Contains(value, "owner/repo") || len(value) != 64 {
		t.Errorf("RepositorySetHash() = %q; want an opaque SHA-256 fingerprint", value)
	}
	if RepositorySetHash([]string{"a", "bc"}) == RepositorySetHash([]string{"ab", "c"}) {
		t.Error("RepositorySetHash() collided for distinct repository sequences")
	}
	if RepositorySetHash([]string{"b", "a"}) != RepositorySetHash([]string{"a", "b"}) {
		t.Error("RepositorySetHash() changed when repository order changed")
	}
}

func TestCoverageScope_disclosesGitHubAuthorFiltering(t *testing.T) {
	githubScope := CoverageScope("github", 50, 20)
	if !strings.Contains(githubScope, "co-author-only") {
		t.Errorf("CoverageScope(github) = %q; want co-author limitation", githubScope)
	}
	if strings.Contains(CoverageScope("gitlab", 50, 20), "co-author-only") {
		t.Error("CoverageScope(gitlab) reports a GitHub-only limitation")
	}
}

func TestSelectRepositories(t *testing.T) {
	got, capped, err := SelectRepositories([]string{"z/project", "a/project", "z/project"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !capped || len(got) != 1 || got[0] != "a/project" {
		t.Errorf("SelectRepositories() = %v, %t, want [a/project], true", got, capped)
	}
}
