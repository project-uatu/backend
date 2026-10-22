// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

type commitScanFixture struct {
	s        *Store
	forge    ForgeInstance
	personID uuid.UUID
}

func newCommitScanFixture(t *testing.T) commitScanFixture {
	t.Helper()
	s := testStore(t)
	forge := githubForge(t, s)
	tracked, err := s.TrackGitHubAccount(t.Context(), forge.ID, GitHubAccount{
		AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
		AccountCreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("TrackGitHubAccount: %v", err)
	}
	return commitScanFixture{s: s, forge: forge, personID: tracked.PersonID}
}

func (f commitScanFixture) claim(t *testing.T, source string) JobClaim {
	t.Helper()
	claim, found, err := f.s.ClaimCollectionJobForSources(t.Context(), "worker", time.Minute, []string{source})
	if err != nil || !found {
		t.Fatalf("claim %s = %t, %v", source, found, err)
	}
	return claim
}

func (f commitScanFixture) completeHistory(t *testing.T, repositories ...string) {
	t.Helper()
	if _, err := f.s.EnqueueHistory(t.Context(), f.personID, f.forge.ID); err != nil {
		t.Fatalf("EnqueueHistory: %v", err)
	}
	claim := f.claim(t, SourceGitHubHistory)
	if _, err := f.s.PersistCollectionPage(t.Context(), claim, event.Page{Done: true},
		event.FetchReport{KnownRepositories: repositories}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage(history): %v", err)
	}
}

func TestStore_commitScanBackfillsRepositoriesAddedAfterCompletion(t *testing.T) {
	f := newCommitScanFixture(t)
	ctx := t.Context()
	completeCommits := func() {
		t.Helper()
		claim := f.claim(t, SourceGitHubCommits)
		if _, err := f.s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
			event.FetchReport{}, true, 2, uuid.Nil); err != nil {
			t.Fatalf("PersistCollectionPage(commits): %v", err)
		}
	}
	queuedMode := func() string {
		t.Helper()
		var mode string
		if err := f.s.db.QueryRow(ctx, `
			SELECT scan_mode FROM jobs
			WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = $3 AND status = 'queued'`,
			f.personID, f.forge.ID, SourceGitHubCommits).Scan(&mode); err != nil {
			t.Fatalf("load queued commit job: %v", err)
		}
		return mode
	}

	f.completeHistory(t)
	completeCommits()
	f.completeHistory(t, "octo/app")
	if mode := queuedMode(); mode != "backfill" {
		t.Fatalf("commit scan after first seed = %q, want backfill", mode)
	}
	completeCommits()
	f.completeHistory(t, "octo/app")
	if mode := queuedMode(); mode != "incremental" {
		t.Errorf("commit scan with all seeds scanned = %q, want incremental", mode)
	}
	completeCommits()
	f.completeHistory(t, "octo/app", "octo/new")
	if mode := queuedMode(); mode != "backfill" {
		t.Errorf("commit scan after a new seed = %q, want backfill", mode)
	}
}

func TestStore_commitScansMergeActorsAcrossPeopleInEitherOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			f := newCommitScanFixture(t)
			ctx := t.Context()
			f.completeHistory(t)
			claim := f.claim(t, SourceGitHubCommits)
			actor := func(value, role string) event.Actor {
				return event.Actor{Key: event.ActorKey{
					IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: value},
					ForgeInstanceID: &f.forge.ID,
				}, Role: role}
			}
			scans := [][]event.Actor{{actor("42", "author")}, {actor("43", "co_author")}}
			if reverse {
				scans[0], scans[1] = scans[1], scans[0]
			}
			occurredAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			for index, actors := range scans {
				raw := event.RawEvent{
					ForgeInstanceID: f.forge.ID, Source: event.Source(SourceGitHubCommits), Kind: "commit",
					ExternalID: "commit:7:abc", OccurredAt: &occurredAt, Actors: actors,
					Payload: json.RawMessage(`{"sha":"abc"}`), PayloadSchemaVersion: 1,
				}
				if _, err := f.s.PersistCollectionPage(ctx, claim, event.Page{Events: []event.RawEvent{raw}, NextCursor: fmt.Sprintf("page-%d", index)},
					event.FetchReport{}, true, 2, uuid.Nil); err != nil {
					t.Fatalf("PersistCollectionPage(commit scan): %v", err)
				}
			}
			var stored []event.Actor
			if err := f.s.db.QueryRow(ctx,
				`SELECT actor_keys FROM raw_events WHERE external_id = 'commit:7:abc'`).Scan(&stored); err != nil {
				t.Fatalf("load actor keys: %v", err)
			}
			if len(stored) != 2 {
				t.Errorf("stored actors = %+v, want both people's evidence", stored)
			}
			if got := countRows(t, f.s, `SELECT count(*) FROM raw_event_actor_keys`); got != 2 {
				t.Errorf("indexed actor identities = %d, want 2", got)
			}
		})
	}
}

func TestDropRedundantCoAuthors(t *testing.T) {
	both, other := uuid.New(), uuid.New()
	links := []personLink{
		{personID: both, role: "co_author"}, {personID: both, role: "author"},
		{personID: other, role: "co_author"},
	}
	got := dropRedundantCoAuthors(links)
	if len(got) != 2 || got[0].role != "author" || got[1].personID != other {
		t.Errorf("dropRedundantCoAuthors() = %+v, want author for one person and co-author for the other", got)
	}
}
