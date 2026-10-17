package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/identity"
)

func TestStore_APIOnboardingAndIdentifierLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()

	person, initialJob, err := s.CreateGitHubPerson(ctx, "Octo Cat", "octocat", []string{"octo@example.com"})
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
		job, err := s.EnqueueGitHubHistoryWithKey(ctx, person.ID, initialJob.ForgeInstanceID, key)
		if err != nil {
			t.Fatalf("EnqueueGitHubHistoryWithKey(%q): %v", key, err)
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
