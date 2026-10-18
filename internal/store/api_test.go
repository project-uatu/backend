// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

func TestStore_APIOnboardingAndIdentifierLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	person, initialJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "Octo Cat", "octocat", []string{"octo@example.com"})
	if err != nil {
		t.Fatalf("CreateGitHubPerson: %v", err)
	}
	if person.ID == uuid.Nil || initialJob.PersonID != person.ID || initialJob.SourceKey != SourceGitHubHistory {
		t.Fatalf("CreateGitHubPerson = person %s, job %+v; want initial GitHub job for new person", person.ID, initialJob)
	}
	emptySummary, err := s.PersonSummaryByYear(ctx, person.ID)
	if err != nil || emptySummary == nil || len(emptySummary) != 0 {
		t.Errorf("PersonSummaryByYear(empty person) = %v, %v; want an empty array", emptySummary, err)
	}

	_, err = s.APIPersonByID(ctx, person.ID)
	if err != nil {
		t.Fatalf("APIPersonByID: %v", err)
	}
	matches, hasMore, err := s.ListAPIPersons(ctx, "octocat", uuid.Nil, 10)
	if err != nil || hasMore || len(matches) != 1 || matches[0].ID != person.ID {
		t.Errorf("ListAPIPersons(login search) = %v, %t, %v; want the person without returning identity keys", matches, hasMore, err)
	}
	jobStatus, err := s.APIJobByID(ctx, initialJob.ID)
	if err != nil {
		t.Fatalf("APIJobByID: %v", err)
	}
	if jobStatus.Status != "queued" || jobStatus.PersonID != person.ID {
		t.Errorf("API job = %+v, want queued job for person %s", jobStatus, person.ID)
	}

	for _, key := range []string{"sync-one", "sync-one"} {
		job, err := s.EnqueueHistoryWithKey(ctx, person.ID, initialJob.ForgeInstanceID, key)
		if err != nil {
			t.Fatalf("EnqueueHistoryWithKey(%q): %v", key, err)
		}
		if job.ID != initialJob.ID {
			t.Errorf("idempotent job ID = %s, want initial job %s", job.ID, initialJob.ID)
		}
	}

	forge, err := s.GitHubForge(ctx)
	if err != nil {
		t.Fatalf("GitHubForge: %v", err)
	}
	if err := s.UpdatePersonIdentifiers(ctx, person.ID, forge.ID,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "octocat-alt"}}, nil); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(add): %v", err)
	}
	matches, hasMore, err = s.ListAPIPersons(ctx, "octocat-alt", uuid.Nil, 10)
	if err != nil || hasMore || len(matches) != 1 || matches[0].ID != person.ID {
		t.Fatalf("ListAPIPersons after add = %v, %t, %v; want the person", matches, hasMore, err)
	}

	if err := s.UpdatePersonIdentifiers(ctx, person.ID, forge.ID, nil,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "octocat-alt"}}); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(remove): %v", err)
	}
	matches, hasMore, err = s.ListAPIPersons(ctx, "octocat-alt", uuid.Nil, 10)
	if err != nil || hasMore || len(matches) != 0 {
		t.Errorf("ListAPIPersons after remove = %v, %t, %v; want no alias match", matches, hasMore, err)
	}

	coverageStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	coverageEnd := coverageStart.AddDate(1, 0, 0)
	if err := s.RecordCoverage(ctx, person.ID, forge.ID, SourceGitHubHistory,
		coverageStart, coverageEnd, "complete", "", nil); err != nil {
		t.Fatalf("RecordCoverage: %v", err)
	}
	if err := s.RecordCoverage(ctx, person.ID, forge.ID, "another_source",
		coverageStart, coverageEnd, "gap", "source limitation", nil); err != nil {
		t.Fatalf("RecordCoverage(gap): %v", err)
	}
	sourceCoverage, err := s.PersonSourceCoverage(ctx, person.ID)
	if err != nil {
		t.Fatalf("PersonSourceCoverage: %v", err)
	}
	if len(sourceCoverage) != 2 || sourceCoverage[0].Source != "another_source" ||
		sourceCoverage[1].Source != SourceGitHubHistory || sourceCoverage[1].Status != "complete" ||
		sourceCoverage[1].From == nil || !sourceCoverage[1].From.Equal(coverageStart) ||
		sourceCoverage[1].To == nil || !sourceCoverage[1].To.Equal(coverageEnd) {
		t.Errorf("PersonSourceCoverage = %+v, want a complete and a gap summary", sourceCoverage)
	}
	if sourceCoverage[0].Status != "partial" || sourceCoverage[0].GapCount != 1 ||
		sourceCoverage[0].From != nil || sourceCoverage[0].To != nil {
		t.Errorf("gap-only source coverage = %+v, want partial with one gap and no complete range", sourceCoverage[0])
	}
}

func TestStore_ActivityPageUsesDeterministicKeyset(t *testing.T) {
	fixture := newIdentityEventFixture(t, true)
	ctx := t.Context()
	occurredAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	actions := []NormalizedActionInput{
		{ActionKey: "opened", Kind: "pull_request_opened", OccurredAt: occurredAt, Project: "owner/repo"},
		{ActionKey: "reviewed", Kind: "pull_request_reviewed", OccurredAt: occurredAt, Project: "owner/repo"},
	}
	if err := fixture.store.ReplaceNormalizedActions(ctx, fixture.rawEventID, 2, actions); err != nil {
		t.Fatalf("ReplaceNormalizedActions: %v", err)
	}
	for _, action := range actions {
		var actionID uuid.UUID
		if err := fixture.store.db.QueryRow(ctx, `
			SELECT id FROM normalized_events
			WHERE raw_event_id = $1 AND action_key = $2 AND is_current`,
			fixture.rawEventID, action.ActionKey).Scan(&actionID); err != nil {
			t.Fatalf("load normalized action %q: %v", action.ActionKey, err)
		}
		if err := fixture.store.ReplaceEventPeople(ctx, actionID, []EventPersonInput{{
			PersonID: fixture.oldPerson.ID, Role: "author", IdentityClaimID: &fixture.claim.ID,
		}}); err != nil {
			t.Fatalf("ReplaceEventPeople(%q): %v", action.ActionKey, err)
		}
	}

	filter := ActivityFilter{
		PersonID: fixture.oldPerson.ID,
		From:     occurredAt.Add(-time.Hour),
		To:       occurredAt.Add(time.Hour),
		Limit:    1,
	}
	first, err := fixture.store.ActivityPage(ctx, filter)
	if err != nil {
		t.Fatalf("ActivityPage(first): %v", err)
	}
	if len(first.Entries) != 1 || !first.HasMore {
		t.Fatalf("ActivityPage(first) = %+v, want one entry and another page", first)
	}
	filter.After = &ActivityCursor{
		OccurredAt: first.Entries[0].OccurredAt,
		EventID:    first.Entries[0].EventID,
		Role:       first.Entries[0].Role,
	}
	second, err := fixture.store.ActivityPage(ctx, filter)
	if err != nil {
		t.Fatalf("ActivityPage(second): %v", err)
	}
	if len(second.Entries) != 1 || second.HasMore || second.Entries[0].EventID == first.Entries[0].EventID {
		t.Fatalf("ActivityPage(second) = %+v, want the other entry and no further page", second)
	}
	summary, err := fixture.store.PersonSummaryByYear(ctx, fixture.oldPerson.ID)
	if err != nil {
		t.Fatalf("PersonSummaryByYear: %v", err)
	}
	if len(summary) != 2 || summary[0].Count != 1 || summary[1].Count != 1 {
		t.Errorf("PersonSummaryByYear = %+v, want both normalized kinds counted once", summary)
	}
	projects, hasMore, err := fixture.store.PersonProjects(ctx, fixture.oldPerson.ID, nil, nil, "", 10)
	if err != nil || hasMore || len(projects) != 1 || projects[0].Project != "owner/repo" || projects[0].Count != 2 {
		t.Errorf("PersonProjects = %+v, %t, %v; want one project with two actions", projects, hasMore, err)
	}
}

func TestStore_APIOnboardingProtectsPendingLoginAndLeavesJobsClaimable(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	firstPerson, firstJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "octocat", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson(octocat): %v", err)
	}
	_, secondJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "zebra", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson(zebra): %v", err)
	}
	if err := s.UpdatePersonIdentifiers(ctx, firstPerson.ID, firstJob.ForgeInstanceID, nil,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "octocat"}}); !errors.Is(err, ErrOnboardingResolutionPending) {
		t.Fatalf("UpdatePersonIdentifiers(remove pending login) error = %v, want ErrOnboardingResolutionPending", err)
	}
	firstClaim, found, err := s.ClaimCollectionJob(ctx, "worker-one", time.Minute)
	if err != nil || !found || firstClaim.ID != firstJob.ID {
		t.Fatalf("ClaimCollectionJob(first) = %+v, %t, %v; want first queued onboarding job", firstClaim, found, err)
	}
	secondClaim, found, err := s.ClaimCollectionJob(ctx, "worker-two", time.Minute)
	if err != nil || !found || secondClaim.ID != secondJob.ID {
		t.Fatalf("ClaimCollectionJob(second) = %+v, %t, %v; want other queued onboarding job", secondClaim, found, err)
	}
}

func TestStore_APIOnboardingRetainsTargetLoginAndIndexesActorIdentities(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, job, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "octocat", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson: %v", err)
	}
	if err := s.UpdatePersonIdentifiers(ctx, person.ID, job.ForgeInstanceID,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "zebra"}}, nil); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(add alias): %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = %t, %v; want onboarding job", found, err)
	}
	if claim.OnboardingLogin != "octocat" {
		t.Errorf("claimed onboarding login = %q, want original login octocat", claim.OnboardingLogin)
	}

	raw := event.RawEvent{
		ForgeInstanceID:      job.ForgeInstanceID,
		Source:               event.Source(SourceGitHubHistory),
		ExternalID:           "indexed-actors",
		Kind:                 "activity",
		Payload:              json.RawMessage(`{}`),
		PayloadSchemaVersion: 1,
		Actors: []event.Actor{
			{Key: event.ActorKey{
				IdentityKey:     identity.Key{Kind: identity.KindLogin, Value: "Zebra"},
				ForgeInstanceID: &job.ForgeInstanceID,
			}, Role: "author"},
			{Key: event.ActorKey{
				IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "octo@EXAMPLE.COM"},
			}, Role: "author"},
		},
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{
		Events: []event.RawEvent{raw}, Done: true,
		ResolvedAccount: &event.ResolvedAccount{
			AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
			CreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
		},
	}, event.FetchReport{}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	matches, hasMore, err := s.ListAPIPersons(ctx, "zebra", uuid.Nil, 10)
	if err != nil || hasMore || len(matches) != 1 || matches[0].ID != person.ID {
		t.Errorf("ListAPIPersons after account resolution = %v, %t, %v; want preserved alias for person %s",
			matches, hasMore, err, person.ID)
	}
	if err := s.UpdatePersonIdentifiers(ctx, person.ID, job.ForgeInstanceID,
		[]APIIdentity{
			{Kind: identity.KindEmail, Value: "octo@example.com"},
		}, nil); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(add indexed actors): %v", err)
	}
	if got := countRows(t, s, `
		SELECT count(*) FROM normalization_jobs nj
		JOIN raw_events re ON re.id = nj.raw_event_id
		WHERE re.external_id = 'indexed-actors'`); got != 1 {
		t.Errorf("normalization jobs after actor identity additions = %d, want 1 deduplicated job", got)
	}
	if got := countRows(t, s, `
		SELECT count(*) FROM raw_event_actor_keys
		WHERE raw_event_id = (SELECT id FROM raw_events WHERE external_id = 'indexed-actors')
		  AND ((kind = 'login' AND normalized_value = 'zebra')
		       OR (kind = 'email' AND normalized_value = 'octo@example.com'))`); got != 2 {
		t.Errorf("indexed normalized actor identities = %d, want login and email entries", got)
	}
}

func TestStore_ResolveAccountConflictRollsBackCollectionPage(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	forge, err := s.GitHubForge(ctx)
	if err != nil {
		t.Fatalf("GitHubForge: %v", err)
	}
	_, err = s.TrackGitHubAccount(ctx, forge.ID, GitHubAccount{
		AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
		AccountCreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("TrackGitHubAccount: %v", err)
	}
	person, job, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "zebra", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found || claim.ID != job.ID {
		t.Fatalf("ClaimCollectionJob = %+v, %t, %v; want onboarding job", claim, found, err)
	}

	raw := event.RawEvent{
		ForgeInstanceID:      forge.ID,
		Source:               event.Source(SourceGitHubHistory),
		ExternalID:           "conflicting-account",
		Kind:                 "activity",
		Payload:              json.RawMessage(`{}`),
		PayloadSchemaVersion: 1,
	}
	_, err = s.PersistCollectionPage(ctx, claim, event.Page{
		Events: []event.RawEvent{raw}, Done: true,
		ResolvedAccount: &event.ResolvedAccount{
			AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
			CreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
		},
	}, event.FetchReport{}, true, 1, uuid.Nil)
	if !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("PersistCollectionPage(conflicting account) error = %v, want ErrIdentityConflict", err)
	}
	if got := countRows(t, s, "SELECT count(*) FROM raw_events WHERE external_id = 'conflicting-account'"); got != 0 {
		t.Errorf("raw events after conflicting account = %d, want transaction rollback", got)
	}
	var accountCount int
	if err := s.db.QueryRow(ctx, "SELECT count(*) FROM github_accounts WHERE person_id = $1", person.ID).Scan(&accountCount); err != nil {
		t.Fatalf("count accounts attached to onboarding person: %v", err)
	}
	if accountCount != 0 {
		t.Errorf("GitHub accounts attached to onboarding person = %d, want 0", accountCount)
	}
	if err := s.FailCollectionJob(ctx, claim, "resolved GitHub account is assigned to another person"); err != nil {
		t.Fatalf("FailCollectionJob after conflict: %v", err)
	}
	var status string
	if err := s.db.QueryRow(ctx, "SELECT status FROM jobs WHERE id = $1", job.ID).Scan(&status); err != nil {
		t.Fatalf("load failed conflict job: %v", err)
	}
	if status != "failed" {
		t.Errorf("conflict job status = %q, want failed", status)
	}
}

func TestStore_EnqueueHistoryPreservesFailedOnboardingTarget(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, originalJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "octocat", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson: %v", err)
	}
	if err := s.UpdatePersonIdentifiers(ctx, person.ID, originalJob.ForgeInstanceID,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "zebra"}}, nil); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(add alias): %v", err)
	}
	if _, err := s.db.Exec(ctx, "UPDATE jobs SET status = 'failed' WHERE id = $1", originalJob.ID); err != nil {
		t.Fatalf("fail original onboarding job: %v", err)
	}

	replacement, err := s.EnqueueHistoryWithKey(ctx, person.ID, originalJob.ForgeInstanceID, "replacement-sync")
	if err != nil {
		t.Fatalf("EnqueueHistoryWithKey: %v", err)
	}
	if replacement.OnboardingLogin != "octocat" {
		t.Errorf("replacement onboarding login = %q, want original target octocat", replacement.OnboardingLogin)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found || claim.ID != replacement.ID || claim.OnboardingLogin != "octocat" {
		t.Fatalf("ClaimCollectionJob = %+v, %t, %v; want replacement targeting octocat", claim, found, err)
	}
}

func TestStore_EnqueueWithoutOnboardingTargetFailsAndInvalidJobDoesNotBlockQueue(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	invalidPerson, invalidJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "octocat", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson(octocat): %v", err)
	}
	if _, err := s.db.Exec(ctx, "UPDATE jobs SET status = 'failed' WHERE id = $1", invalidJob.ID); err != nil {
		t.Fatalf("fail onboarding job: %v", err)
	}
	if err := s.UpdatePersonIdentifiers(ctx, invalidPerson.ID, invalidJob.ForgeInstanceID, nil,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "octocat"}}); err != nil {
		t.Fatalf("UpdatePersonIdentifiers(remove failed target): %v", err)
	}
	if _, err := s.EnqueueHistoryWithKey(ctx, invalidPerson.ID, invalidJob.ForgeInstanceID, "no-target"); !errors.Is(err, ErrOnboardingTargetUnavailable) {
		t.Errorf("EnqueueHistoryWithKey without target error = %v, want ErrOnboardingTargetUnavailable", err)
	}
	if _, err := s.db.Exec(ctx, "UPDATE jobs SET status = 'queued' WHERE id = $1", invalidJob.ID); err != nil {
		t.Fatalf("requeue invalid legacy job: %v", err)
	}

	_, validJob, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "zebra", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson(zebra): %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found || claim.ID != validJob.ID {
		t.Fatalf("ClaimCollectionJob = %+v, %t, %v; want valid queued job after invalid job is failed", claim, found, err)
	}
	var status string
	if err := s.db.QueryRow(ctx, "SELECT status FROM jobs WHERE id = $1", invalidJob.ID).Scan(&status); err != nil {
		t.Fatalf("load invalid job status: %v", err)
	}
	if status != "failed" {
		t.Errorf("invalid queued job status = %q, want failed", status)
	}
}

func TestStore_CollectCLIReusesPendingAPIOnboarding(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, job, err := s.CreateOnboardedPerson(ctx, githubForge(t, s), "", "octocat", nil)
	if err != nil {
		t.Fatalf("CreateGitHubPerson: %v", err)
	}
	account, err := s.TrackGitHubAccountForLogin(ctx, job.ForgeInstanceID, "octocat", GitHubAccount{
		AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octo-cat",
		AccountCreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("TrackGitHubAccountForLogin: %v", err)
	}
	if account.PersonID != person.ID {
		t.Fatalf("tracked person = %s, want API-onboarded person %s", account.PersonID, person.ID)
	}
	if got := countRows(t, s, "SELECT count(*) FROM persons"); got != 1 {
		t.Errorf("person count after CLI account tracking = %d, want no duplicate person", got)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "collector", time.Minute)
	if err != nil || !found || claim.ID != job.ID || claim.PersonID != person.ID ||
		claim.CurrentAccountLogin != "octo-cat" {
		t.Fatalf("ClaimCollectionJob after CLI tracking = %+v, %t, %v; want pending API job", claim, found, err)
	}
}

func githubForge(t *testing.T, s *Store) ForgeInstance {
	t.Helper()
	forge, err := s.GitHubForge(t.Context())
	if err != nil {
		t.Fatalf("GitHubForge: %v", err)
	}
	return forge
}
