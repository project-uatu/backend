package store

import (
	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"testing"
	"time"

	"github.com/project-uatu/backend/internal/identity"
)

func TestStore_forgeAccountsAreIsolatedAndNeedNoGitHubMetadata(t *testing.T) {
	s := testStore(t)
	gitlab, _, err := s.ForgeForHost(t.Context(), "gitlab.com")
	if err != nil {
		t.Fatal(err)
	}
	codeberg, _, err := s.ForgeForHost(t.Context(), "codeberg.org")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.TrackGitHubAccountForLogin(t.Context(), gitlab.ID, "jane", SourceAccount{AccountID: "42", Login: "jane"})
	if err != nil {
		t.Fatalf("track GitLab account without node ID or creation time: %v", err)
	}
	b, err := s.TrackGitHubAccountForLogin(t.Context(), codeberg.ID, "jane", SourceAccount{AccountID: "42", Login: "jane"})
	if err != nil {
		t.Fatalf("track Codeberg account: %v", err)
	}
	if a.PersonID == b.PersonID {
		t.Error("the same login and account ID on two forges resolved to one person")
	}

	github := githubForge(t, s)
	_, err = s.TrackGitHubAccountForLogin(t.Context(), github.ID, "jane", SourceAccount{AccountID: "42", Login: "jane"})
	if err == nil {
		t.Error("GitHub account without node ID and creation time was accepted")
	}
}

func TestStore_historyJobsAreEnqueuedForEachForge(t *testing.T) {
	s := testStore(t)
	for _, host := range []string{"gitlab.com", "codeberg.org"} {
		forge, _, err := s.ForgeForHost(t.Context(), host)
		if err != nil {
			t.Fatal(err)
		}
		tracked, err := s.TrackGitHubAccountForLogin(t.Context(), forge.ID, "jane", SourceAccount{
			AccountID: "42", Login: "jane",
		})
		if err != nil {
			t.Fatalf("%s: track: %v", host, err)
		}
		if _, err := s.EnqueueHistory(t.Context(), tracked.PersonID, forge.ID); err != nil {
			t.Errorf("%s: enqueue history: %v", host, err)
		}
	}
}

func TestStore_loginAddedForNewForgeQueuesOnboarding(t *testing.T) {
	s := testStore(t)
	github := githubForge(t, s)
	person, _, err := s.CreateOnboardedPerson(t.Context(), github, "Jane", "jane", nil)
	if err != nil {
		t.Fatal(err)
	}
	gitlab, _, err := s.ForgeForHost(t.Context(), "gitlab.com")
	if err != nil {
		t.Fatal(err)
	}
	var jobs int
	count := func() int {
		if err := s.db.QueryRow(t.Context(), `
			SELECT count(*) FROM jobs
			WHERE person_id = $1 AND forge_instance_id = $2`, person.ID, gitlab.ID).Scan(&jobs); err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if count() != 0 {
		t.Fatal("GitLab job existed before a GitLab login was added")
	}
	if err := s.UpdatePersonIdentifiers(t.Context(), person.ID, gitlab.ID,
		[]APIIdentity{{Kind: identity.KindLogin, Value: "jane.doe"}}, nil); err != nil {
		t.Fatalf("add GitLab login: %v", err)
	}
	if count() != 1 {
		t.Errorf("GitLab onboarding jobs = %d, want 1", count())
	}
}

func TestStore_legacyGitHubViewsStayWritableForPreviousBinaries(t *testing.T) {
	s := testStore(t)
	github := githubForge(t, s)
	person, _, err := s.CreateOnboardedPerson(t.Context(), github, "Jane", "jane", nil)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	insert := `
		INSERT INTO github_accounts (forge_instance_id, person_id, account_id, node_id, login, account_created_at)
		VALUES ($1, $2, '7', 'N7', 'jane', $3)
		ON CONFLICT (forge_instance_id, account_id) DO UPDATE SET login = EXCLUDED.login, updated_at = now()`
	for range 2 {
		if _, err := s.db.Exec(t.Context(), insert, github.ID, person.ID, created); err != nil {
			t.Fatalf("insert through legacy github_accounts view: %v", err)
		}
	}
	var n int
	if err := s.db.QueryRow(t.Context(), `
		SELECT count(*) FROM source_accounts WHERE forge_instance_id = $1 AND account_id = '7'`, github.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("source_accounts rows = %d, %v; want 1", n, err)
	}
	if _, err := s.db.Exec(t.Context(), `
		INSERT INTO github_known_repositories (person_id, forge_instance_id, repository)
		VALUES ($1, $2, 'octo/app') ON CONFLICT DO NOTHING`, person.ID, github.ID); err != nil {
		t.Fatalf("insert through legacy known-repositories view: %v", err)
	}
	if err := s.db.QueryRow(t.Context(), `SELECT count(*) FROM known_repositories`).Scan(&n); err != nil || n != 1 {
		t.Errorf("known_repositories rows = %d, %v; want 1", n, err)
	}
}

func TestStore_claimsAreLimitedToWorkerSources(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	gitlab, _, err := s.ForgeForHost(ctx, "gitlab.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOnboardedPerson(ctx, gitlab, "Jane", "jane.doe", nil); err != nil {
		t.Fatalf("CreateOnboardedPerson: %v", err)
	}
	if _, found, err := s.ClaimCollectionJobForSources(ctx, "github-only", time.Minute, []string{SourceGitHubHistory}); err != nil || found {
		t.Fatalf("worker without the GitLab connector claimed = %t, %v; want no claim", found, err)
	}
	claim, found, err := s.ClaimCollectionJobForSources(ctx, "gitlab", time.Minute, []string{SourceGitHubHistory, "gitlab_history"})
	if err != nil || !found || claim.SourceKey != "gitlab_history" {
		t.Fatalf("worker with the GitLab connector claim = %+v, %t, %v", claim, found, err)
	}
}

func TestStore_boundaryGapsStillLetBackfillComplete(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	gitlab, _, err := s.ForgeForHost(ctx, "gitlab.com")
	if err != nil {
		t.Fatal(err)
	}
	person, _, err := s.CreateOnboardedPerson(ctx, gitlab, "Jane", "jane.doe", nil)
	if err != nil {
		t.Fatalf("CreateOnboardedPerson: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = %t, %v", found, err)
	}
	gap := event.Gap{
		From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC),
		Reason: "source limit", Boundary: true,
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{Gaps: []event.Gap{gap}}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	var complete bool
	if err := s.db.QueryRow(ctx, `
		SELECT backfill_complete FROM checkpoints WHERE person_id = $1 AND source_key = $2`,
		person.ID, "gitlab_history").Scan(&complete); err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if !complete {
		t.Error("backfill_complete = false, want true so syncs become incremental")
	}
	if n := countRows(t, s, `SELECT count(*) FROM sync_coverage WHERE person_id = $1 AND status = 'gap'`, person.ID); n != 1 {
		t.Errorf("gap coverage rows = %d, want the boundary gap kept visible", n)
	}
	if n := countRows(t, s, `SELECT count(*) FROM sync_coverage WHERE person_id = $1 AND status = 'complete'`, person.ID); n != 0 {
		t.Errorf("complete coverage rows = %d, want 0", n)
	}
}
