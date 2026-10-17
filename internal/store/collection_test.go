package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

func TestStore_collectionPagesPreserveDataAndAdvanceCoverage(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forgejo", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	windowEnd := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	job, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		ScanMode: "backfill", Window: event.Window{To: windowEnd},
	})
	if err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker-one", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v; want true, nil", found, err)
	}
	if claim.ID != job.ID || claim.SyncRunID == uuid.Nil {
		t.Fatalf("claimed job = %+v, want job %s with a sync run", claim, job.ID)
	}

	raw := event.RawEvent{
		ForgeInstanceID: forge.ID, Source: "forge", ExternalID: "change:1", Kind: "change",
		Payload: json.RawMessage(`{"value":"original"}`), PayloadSchemaVersion: 1,
		Actors: []event.Actor{{
			Key:  event.ActorKey{IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "dev@example.com"}},
			Role: "author",
		}},
	}
	report := event.FetchReport{RateLimitBucket: "forge:shared"}
	report.CoverageScope = "public repository activity"
	report.ContributionCounts = []event.ContributionCount{{
		Date: windowEnd.Add(-24 * time.Hour), Metric: "daily_total", Value: 7,
	}}
	report.RateLimitRemaining = intValue(3)
	rateLimitReset := time.Now().UTC().Add(time.Hour)
	report.RateLimitResetAt = &rateLimitReset
	firstPage := event.Page{Events: []event.RawEvent{raw}, NextCursor: "page-2"}
	if done, err := s.PersistCollectionPage(ctx, claim, firstPage, report, true, 1, uuid.Nil); err != nil || done {
		t.Fatalf("PersistCollectionPage(first) = done %t, err %v; want false, nil", done, err)
	}
	claim.Cursor = "page-2"

	changedPayload := raw
	changedPayload.Payload = json.RawMessage(`{"value":"from-failed-response"}`)
	partialReport := event.FetchReport{Errors: []string{"upstream request failed"}}
	if done, err := s.PersistCollectionPage(ctx, claim,
		event.Page{Events: []event.RawEvent{changedPayload}}, partialReport, false, 1, uuid.Nil); err != nil || done {
		t.Fatalf("PersistCollectionPage(partial) = done %t, err %v; want false, nil", done, err)
	}
	var payload string
	if err := s.db.QueryRow(ctx, `
		SELECT payload::text FROM raw_events
		WHERE forge_instance_id = $1 AND external_id = $2`, forge.ID, raw.ExternalID).Scan(&payload); err != nil {
		t.Fatalf("load persisted raw payload: %v", err)
	}
	if payload != `{"value": "original"}` && payload != `{"value":"original"}` {
		t.Errorf("raw payload after partial response = %s, want original payload", payload)
	}
	var cursor string
	if err := s.db.QueryRow(ctx, `
		SELECT cursor FROM checkpoints WHERE person_id = $1
		  AND forge_instance_id = $2 AND source_key = $3`,
		person.ID, forge.ID, "forge").Scan(&cursor); err != nil {
		t.Fatalf("load checkpoint after partial response: %v", err)
	}
	if cursor != "page-2" {
		t.Errorf("checkpoint after partial response = %q, want page-2", cursor)
	}
	var backfillComplete bool
	if err := s.db.QueryRow(ctx, `
		SELECT backfill_complete FROM checkpoints WHERE person_id = $1
		  AND forge_instance_id = $2 AND source_key = $3`,
		person.ID, forge.ID, "forge").Scan(&backfillComplete); err != nil {
		t.Fatalf("load incomplete checkpoint: %v", err)
	}
	if backfillComplete {
		t.Error("backfill_complete after incomplete page = true, want false")
	}
	var jobRateLimitReset *time.Time
	if err := s.db.QueryRow(ctx, "SELECT rate_limit_reset_at FROM jobs WHERE id = $1", job.ID).
		Scan(&jobRateLimitReset); err != nil {
		t.Fatalf("load job rate-limit reset: %v", err)
	}
	if jobRateLimitReset != nil {
		t.Errorf("job rate-limit reset = %v, want nil while quota remains", jobRateLimitReset)
	}
	var normalizationCount int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM normalization_jobs WHERE normalizer_version = 1`).Scan(&normalizationCount); err != nil {
		t.Fatalf("count normalization jobs: %v", err)
	}
	if normalizationCount != 1 {
		t.Errorf("normalization jobs = %d, want 1 despite duplicate partial event", normalizationCount)
	}
	counts, err := s.ContributionCounts(ctx, person.ID, windowEnd.Add(-24*time.Hour), windowEnd)
	if err != nil {
		t.Fatalf("ContributionCounts after collection page: %v", err)
	}
	if len(counts) != 1 || counts[0].Value != 7 || counts[0].Metric != "daily_total" {
		t.Errorf("persisted contribution counts = %+v, want separate daily aggregate", counts)
	}

	if err := s.RetryCollectionJob(ctx, claim, errors.New("temporary fetch failure"), time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("RetryCollectionJob: %v", err)
	}
	if _, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		ScanMode: "backfill", Window: event.Window{To: windowEnd.Add(time.Hour)},
	}); !errors.Is(err, ErrCollectionJobActive) {
		t.Errorf("EnqueueCollectionJob(changed retry window) error = %v, want ErrCollectionJobActive", err)
	}
	claim, found, err = s.ClaimCollectionJob(ctx, "worker-two", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob(retry) = found %t, err %v; want true, nil", found, err)
	}
	if claim.Cursor != "page-2" {
		t.Errorf("retried job cursor = %q, want page-2", claim.Cursor)
	}
	if done, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{CoverageScope: "public repository activity"}, true, 1, uuid.Nil); err != nil || !done {
		t.Fatalf("PersistCollectionPage(final) = done %t, err %v; want true, nil", done, err)
	}
	coverage, err := s.Coverage(ctx, person.ID, forge.ID, "forge",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), windowEnd)
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(coverage) != 1 || coverage[0].Status != "complete" {
		t.Errorf("Coverage = %+v, want one complete interval", coverage)
	}
	if coverage[0].CoverageScope != "public repository activity" {
		t.Errorf("coverage scope = %q, want public repository activity", coverage[0].CoverageScope)
	}
	var jobStatus string
	if err := s.db.QueryRow(ctx, "SELECT status FROM jobs WHERE id = $1", job.ID).Scan(&jobStatus); err != nil {
		t.Fatalf("load completed job: %v", err)
	}
	if jobStatus != "succeeded" {
		t.Errorf("job status = %q, want succeeded", jobStatus)
	}
	if err := s.db.QueryRow(ctx, `
		SELECT backfill_complete FROM checkpoints WHERE person_id = $1
		  AND forge_instance_id = $2 AND source_key = $3`,
		person.ID, forge.ID, "forge").Scan(&backfillComplete); err != nil {
		t.Fatalf("load completed checkpoint: %v", err)
	}
	if !backfillComplete {
		t.Error("backfill_complete after complete scan = false, want true")
	}
}

func TestStore_scanGapsSurviveRetriesAndPreventCompletion(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}

	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	windowEnd := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	job, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		Window: event.Window{To: windowEnd},
	})
	if err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker-one", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v; want true, nil", found, err)
	}
	gap := event.Gap{
		From: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), Reason: "source limit",
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{NextCursor: "next"},
		event.FetchReport{Gaps: []event.Gap{gap}}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage(first): %v", err)
	}
	if err := s.RetryCollectionJob(ctx, claim, errors.New("temporary failure"), time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("RetryCollectionJob: %v", err)
	}
	claim, found, err = s.ClaimCollectionJob(ctx, "worker-two", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob(retry) = found %t, err %v; want true, nil", found, err)
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage(final): %v", err)
	}
	var backfillComplete bool
	if err := s.db.QueryRow(ctx, `
		SELECT backfill_complete FROM checkpoints
		WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = $3`,
		person.ID, forge.ID, "forge").Scan(&backfillComplete); err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if backfillComplete {
		t.Error("backfill_complete after scan with a prior gap = true, want false")
	}
	if count := countRows(t, s, `
		SELECT count(*) FROM sync_coverage
		WHERE person_id = $1 AND forge_instance_id = $2 AND status = 'complete'`,
		person.ID, forge.ID); count != 0 {
		t.Errorf("complete coverage rows after scan with gap = %d, want 0", count)
	}
	var status string
	if err := s.db.QueryRow(ctx, `
		SELECT status FROM sync_runs WHERE id = (
			SELECT sync_run_id FROM jobs WHERE id = $1
		)`, job.ID).Scan(&status); err != nil {
		t.Fatalf("load final sync-run status: %v", err)
	}
	if status != "partial" {
		t.Errorf("final sync-run status = %q, want partial", status)
	}
}

func TestStore_collectionJobsCoalesceAndHonorIdempotency(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	input := CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		ScanMode: "incremental", IdempotencyKey: "sync-1",
		Window: event.Window{
			From: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	first, err := s.EnqueueCollectionJob(ctx, input)
	if err != nil {
		t.Fatalf("EnqueueCollectionJob(first): %v", err)
	}
	input.Window.From = time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC)
	input.Window.To = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	second, err := s.EnqueueCollectionJob(ctx, input)
	if err != nil {
		t.Fatalf("EnqueueCollectionJob(duplicate): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("coalesced job ID = %s, want %s", second.ID, first.ID)
	}
	if !second.Window.From.Equal(first.Window.From) || !second.Window.To.Equal(first.Window.To) {
		t.Errorf("idempotent job window = %+v, want original window %+v", second.Window, first.Window)
	}
	if count := countRows(t, s, "SELECT count(*) FROM jobs WHERE person_id = $1", person.ID); count != 1 {
		t.Errorf("pending jobs = %d, want 1", count)
	}
	if _, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		ScanMode: "incremental",
		Window: event.Window{
			From: time.Date(2023, 12, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC),
		},
	}); !errors.Is(err, ErrCollectionJobActive) {
		t.Errorf("EnqueueCollectionJob(changed scan) error = %v, want ErrCollectionJobActive", err)
	}
	input.IdempotencyKey = ""
	input.Window = second.Window
	coalesced, err := s.EnqueueCollectionJob(ctx, input)
	if err != nil {
		t.Fatalf("EnqueueCollectionJob(coalesce without idempotency key): %v", err)
	}
	if coalesced.ID != first.ID || !coalesced.Window.From.Equal(first.Window.From) ||
		!coalesced.Window.To.Equal(first.Window.To) {
		t.Errorf("active job coalescing = %+v, want existing job %s with original window %+v",
			coalesced, first.ID, first.Window)
	}
	input.IdempotencyKey = "sync-2"
	coalesced, err = s.EnqueueCollectionJob(ctx, input)
	if err != nil || coalesced.ID != first.ID {
		t.Fatalf("EnqueueCollectionJob(second key) = %+v, %v; want same coalesced job", coalesced, err)
	}
	var associations int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM job_idempotency_keys WHERE job_id = $1`, first.ID).Scan(&associations); err != nil {
		t.Fatalf("count job idempotency associations: %v", err)
	}
	if associations != 2 {
		t.Errorf("job idempotency associations = %d, want both accepted keys", associations)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v; want true, nil", found, err)
	}
	runningRequest := input
	runningRequest.IdempotencyKey = ""
	runningJob, err := s.EnqueueCollectionJob(ctx, runningRequest)
	if err != nil || runningJob.ID != claim.ID {
		t.Fatalf("EnqueueCollectionJob(same running scan) = %+v, %v; want claimed job %s", runningJob, err, claim.ID)
	}
	if count := countRows(t, s, `
		SELECT count(*) FROM jobs WHERE person_id = $1 AND forge_instance_id = $2
		  AND source_key = $3 AND status IN ('queued', 'running')`,
		person.ID, forge.ID, "forge"); count != 1 {
		t.Errorf("active source jobs after coalescing = %d, want 1", count)
	}
	runningRequest.Window.To = runningRequest.Window.To.Add(time.Hour)
	if _, err := s.EnqueueCollectionJob(ctx, runningRequest); !errors.Is(err, ErrCollectionJobActive) {
		t.Errorf("EnqueueCollectionJob(changed running scan) error = %v, want ErrCollectionJobActive", err)
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	repeated, err := s.EnqueueCollectionJob(ctx, input)
	if err != nil || repeated.ID != first.ID {
		t.Errorf("EnqueueCollectionJob(repeated accepted key) = %+v, %v; want completed job %s",
			repeated, err, first.ID)
	}
}

func TestStore_collectionPageRollsBackWhenNormalizationEnqueueFails(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	_, err = s.db.Exec(ctx, `
		CREATE FUNCTION reject_normalization_enqueue() RETURNS trigger
		LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'forced normalization enqueue failure';
		END;
		$$`)
	if err != nil {
		t.Fatalf("create normalization failure function: %v", err)
	}
	_, err = s.db.Exec(ctx, `
		CREATE TRIGGER reject_normalization_enqueue
		BEFORE INSERT ON normalization_jobs
		FOR EACH ROW EXECUTE FUNCTION reject_normalization_enqueue()`)
	if err != nil {
		t.Fatalf("create normalization failure trigger: %v", err)
	}
	job, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		Window: event.Window{To: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v; want true, nil", found, err)
	}
	raw := event.RawEvent{
		ForgeInstanceID: forge.ID, Source: "forge", ExternalID: "rollback:1", Kind: "change",
		Payload: json.RawMessage(`{"id":1}`), PayloadSchemaVersion: 1,
	}
	if _, err := s.PersistCollectionPage(ctx, claim,
		event.Page{Events: []event.RawEvent{raw}, NextCursor: "next"},
		event.FetchReport{}, true, 1, uuid.Nil); err == nil {
		t.Fatal("PersistCollectionPage error = nil, want normalization enqueue failure")
	}
	if count := countRows(t, s, `
		SELECT count(*) FROM raw_events WHERE forge_instance_id = $1 AND external_id = $2`,
		forge.ID, raw.ExternalID); count != 0 {
		t.Errorf("raw events after failed page = %d, want 0", count)
	}
	if count := countRows(t, s, `
		SELECT count(*) FROM checkpoints WHERE person_id = $1 AND forge_instance_id = $2`,
		person.ID, forge.ID); count != 0 {
		t.Errorf("checkpoints after failed page = %d, want 0", count)
	}
	if count := countRows(t, s, "SELECT count(*) FROM jobs WHERE id = $1 AND status = 'running'", job.ID); count != 1 {
		t.Errorf("running job after failed page transaction = %d, want 1", count)
	}
}

func TestStore_collectionJobClaimsAreExclusiveAndFenceStaleWorkers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	job, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		Window: event.Window{To: time.Now().UTC()},
	})
	if err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	start := make(chan struct{})
	type result struct {
		claim JobClaim
		found bool
		err   error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, owner := range []string{"worker-a", "worker-b"} {
		owner := owner
		workers.Go(func() {
			<-start
			claim, found, err := s.ClaimCollectionJob(ctx, owner, 20*time.Millisecond)
			results <- result{claim: claim, found: found, err: err}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	var claims []JobClaim
	for result := range results {
		if result.err != nil {
			t.Fatalf("ClaimCollectionJob concurrently: %v", result.err)
		}
		if result.found {
			claims = append(claims, result.claim)
		}
	}
	if len(claims) != 1 {
		t.Fatalf("simultaneous claims = %d, want 1", len(claims))
	}
	time.Sleep(35 * time.Millisecond)
	newClaim, found, err := s.ClaimCollectionJob(ctx, "worker-c", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob after lease expiry = found %t, err %v; want true, nil", found, err)
	}
	if newClaim.Token == claims[0].Token {
		t.Fatal("reclaimed collection job reused the stale fencing token")
	}
	if err := s.RenewCollectionLease(ctx, claims[0], time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("RenewCollectionLease(stale) error = %v, want ErrLeaseLost", err)
	}
	_, err = s.PersistCollectionPage(ctx, claims[0], event.Page{Done: true}, event.FetchReport{}, true, 1, uuid.Nil)
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("PersistCollectionPage(stale) error = %v, want ErrLeaseLost", err)
	}
	if newClaim.ID != job.ID {
		t.Errorf("reclaimed job ID = %s, want %s", newClaim.ID, job.ID)
	}
	if err := s.ReleaseCollectionJob(ctx, newClaim); err != nil {
		t.Fatalf("ReleaseCollectionJob: %v", err)
	}
}

func TestStore_normalizationIsReplayableAndVersioned(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forgejo", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	claim, err := s.ClaimIdentity(ctx, IdentityClaimInput{
		IdentityKey: IdentityKey{
			ForgeInstanceID: &forge.ID, Kind: string(identity.KindLogin),
			Value: "dev", Normalized: "dev",
		},
		PersonID: person.ID, Method: "manual", ReviewedBy: "maintainer",
		Disposition: "accepted",
	})
	if err != nil {
		t.Fatalf("ClaimIdentity: %v", err)
	}
	end := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	job, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		Window: event.Window{To: end},
	})
	if err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	collection, found, err := s.ClaimCollectionJob(ctx, "collector", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v", found, err)
	}
	raw := event.RawEvent{
		ForgeInstanceID: forge.ID, Source: "forge", ExternalID: "change:2", Kind: "change",
		Payload: json.RawMessage(`{"id":2}`), PayloadSchemaVersion: 1,
		Actors: []event.Actor{{
			Key: event.ActorKey{
				IdentityKey:     identity.Key{Kind: identity.KindLogin, Value: "dev"},
				ForgeInstanceID: &forge.ID,
			},
			Role: "author",
		}},
	}
	if _, err := s.PersistCollectionPage(ctx, collection,
		event.Page{Events: []event.RawEvent{raw}, Done: true},
		event.FetchReport{}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	normalization, found, err := s.ClaimNormalizationJob(ctx, "normalizer", 1, time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob = found %t, err %v", found, err)
	}
	if normalization.Event.ExternalID != raw.ExternalID || len(normalization.Event.Actors) != 1 {
		t.Fatalf("loaded raw event = %+v, want saved event and actor key", normalization.Event)
	}
	action := event.Action{
		Key: "opened", Kind: "change_opened", OccurredAt: time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC),
		Actors: raw.Actors,
	}
	if err := s.CompleteNormalization(ctx, normalization, []event.Action{action}); err != nil {
		t.Fatalf("CompleteNormalization: %v", err)
	}
	entries, err := s.Timeline(ctx, person.ID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), end, 10)
	if err != nil || len(entries) != 1 || entries[0].Role != "author" {
		t.Fatalf("Timeline after normalization = %+v, %v; want one author action", entries, err)
	}

	last, done, err := s.QueueRenormalization(ctx, 2, uuid.Nil, 10)
	if err != nil || !done || last == uuid.Nil {
		t.Fatalf("QueueRenormalization = %s, done %t, err %v; want one queued record", last, done, err)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs SET status = 'queued', attempts = 0, retry_at = NULL,
		                              fencing_token = NULL, lease_owner = '', lease_expires_at = NULL
		WHERE raw_event_id = $1 AND normalizer_version = 1`, normalization.RawEventID); err != nil {
		t.Fatalf("queue version-1 work alongside replay: %v", err)
	}
	staleNormalization, found, err := s.ClaimNormalizationJob(ctx, "expired-normalizer", 2, 20*time.Millisecond)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(expiring) = found %t, err %v; want true, nil", found, err)
	}
	if staleNormalization.NormalizerVersion != 2 {
		t.Fatalf("claimed normalization version = %d, want requested version 2", staleNormalization.NormalizerVersion)
	}
	time.Sleep(35 * time.Millisecond)
	normalization, found, err = s.ClaimNormalizationJob(ctx, "normalizer-v2", 2, time.Minute)
	if err != nil || !found || normalization.NormalizerVersion != 2 {
		t.Fatalf("ClaimNormalizationJob(v2) = %+v, found %t, err %v", normalization, found, err)
	}
	if err := s.CompleteNormalization(ctx, staleNormalization, []event.Action{action}); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("CompleteNormalization(stale) error = %v, want ErrLeaseLost", err)
	}
	action.OccurredAt = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := s.CompleteNormalization(ctx, normalization, []event.Action{action}); err != nil {
		t.Fatalf("CompleteNormalization(v2): %v", err)
	}
	entries, err = s.Timeline(ctx, person.ID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), end, 10)
	wantOccurredAt := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	if err != nil || len(entries) != 1 || !entries[0].OccurredAt.Equal(wantOccurredAt) {
		t.Errorf("Timeline after normalization replay = %+v, %v; want one action at %v", entries, err, wantOccurredAt)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs SET status = 'queued', attempts = 0, retry_at = NULL,
		                              fencing_token = NULL, lease_owner = '', lease_expires_at = NULL
		WHERE raw_event_id = $1 AND normalizer_version = 1`, normalization.RawEventID); err != nil {
		t.Fatalf("requeue older normalizer version: %v", err)
	}
	older, found, err := s.ClaimNormalizationJob(ctx, "normalizer-v1", 1, time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(v1 replay) = found %t, err %v; want true, nil", found, err)
	}
	if err := s.CompleteNormalization(ctx, older, []event.Action{action}); err != nil {
		t.Fatalf("CompleteNormalization(v1 replay): %v", err)
	}
	entries, err = s.Timeline(ctx, person.ID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), end, 10)
	if err != nil || len(entries) != 1 || !entries[0].OccurredAt.Equal(wantOccurredAt) {
		t.Errorf("Timeline after old-version replay = %+v, %v; want the newer action at %v", entries, err, wantOccurredAt)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs SET status = 'queued', attempts = 0, retry_at = NULL,
		                              fencing_token = NULL, lease_owner = '', lease_expires_at = NULL
		WHERE raw_event_id = $1 AND normalizer_version = 2`, normalization.RawEventID); err != nil {
		t.Fatalf("requeue current normalizer version: %v", err)
	}
	current, found, err := s.ClaimNormalizationJob(ctx, "normalizer-v2-replay", 2, time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(v2 replay) = found %t, err %v; want true, nil", found, err)
	}
	action.Actors = []event.Actor{}
	if err := s.CompleteNormalization(ctx, current, []event.Action{action}); err != nil {
		t.Fatalf("CompleteNormalization(v2 actor replacement): %v", err)
	}
	entries, err = s.Timeline(ctx, person.ID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), end, 10)
	if err != nil || len(entries) != 0 {
		t.Errorf("Timeline after actor removal = %+v, %v; want no stale person link", entries, err)
	}
	if _, done, err := s.QueueRenormalization(ctx, 3, uuid.Nil, 10); err != nil || !done {
		t.Fatalf("QueueRenormalization(v3) = done %t, err %v; want complete batch", done, err)
	}
	emptyVersion, found, err := s.ClaimNormalizationJob(ctx, "normalizer-v3", 3, time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(v3) = found %t, err %v; want true, nil", found, err)
	}
	if err := s.CompleteNormalization(ctx, emptyVersion, nil); err != nil {
		t.Fatalf("CompleteNormalization(empty v3): %v", err)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE normalization_jobs SET status = 'queued', attempts = 0, retry_at = NULL,
		                              fencing_token = NULL, lease_owner = '', lease_expires_at = NULL
		WHERE raw_event_id = $1 AND normalizer_version = 1`, normalization.RawEventID); err != nil {
		t.Fatalf("requeue old normalizer after empty output: %v", err)
	}
	oldAfterEmpty, found, err := s.ClaimNormalizationJob(ctx, "normalizer-v1-again", 1, time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(old after empty output) = found %t, err %v; want true, nil", found, err)
	}
	if err := s.CompleteNormalization(ctx, oldAfterEmpty, []event.Action{action}); err != nil {
		t.Fatalf("CompleteNormalization(old after empty output): %v", err)
	}
	entries, err = s.Timeline(ctx, person.ID,
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), end, 10)
	if err != nil || len(entries) != 0 {
		t.Errorf("Timeline after old replay behind empty newer output = %+v, %v; want no current action", entries, err)
	}
	if job.ID != collection.ID || claim.ID == uuid.Nil {
		t.Errorf("test fixtures job %s / claim %s were not assigned", job.ID, claim.ID)
	}
}

func TestStore_rateLimitReservationsCoordinateWorkers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	claims := make([]JobClaim, 3)
	for i := range claims {
		person, err := s.CreatePerson(ctx, nil)
		if err != nil {
			t.Fatalf("CreatePerson(%d): %v", i, err)
		}
		if _, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
			PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
			Window: event.Window{To: time.Now().UTC()},
		}); err != nil {
			t.Fatalf("EnqueueCollectionJob(%d): %v", i, err)
		}
		claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
		if err != nil || !found {
			t.Fatalf("ClaimCollectionJob(%d) = found %t, err %v", i, found, err)
		}
		claims[i] = claim
	}

	const bucket = "forge:shared"
	reset := time.Now().UTC().Add(time.Hour)
	if _, err := s.db.Exec(ctx, `
		INSERT INTO rate_limit_buckets (bucket_key, remaining, reset_at)
		VALUES ($1, 2, $2)`, bucket, reset); err != nil {
		t.Fatalf("initialize rate-limit bucket: %v", err)
	}
	first, err := s.ReserveRateLimit(ctx, bucket, claims[0], time.Minute)
	if err != nil || !first.Granted || first.ID == uuid.Nil {
		t.Fatalf("ReserveRateLimit(first) = %+v, %v; want identified reservation", first, err)
	}
	second, err := s.ReserveRateLimit(ctx, bucket, claims[1], time.Minute)
	if err != nil || !second.Granted || second.ID == uuid.Nil {
		t.Fatalf("ReserveRateLimit(second) = %+v, %v; want identified reservation", second, err)
	}
	reportedOne := 1
	if _, err := s.PersistCollectionPage(ctx, claims[0], event.Page{Done: true},
		event.FetchReport{
			RateLimitBucket: bucket, RateLimitRemaining: &reportedOne, RateLimitResetAt: &reset,
		}, true, 1, first.ID); err != nil {
		t.Fatalf("PersistCollectionPage(first quota report): %v", err)
	}
	staleHigherReport := 1
	if _, err := s.PersistCollectionPage(ctx, claims[1], event.Page{Done: true},
		event.FetchReport{
			RateLimitBucket: bucket, RateLimitRemaining: &staleHigherReport, RateLimitResetAt: &reset,
		}, true, 1, second.ID); err != nil {
		t.Fatalf("PersistCollectionPage(second quota report): %v", err)
	}
	blocked, err := s.ReserveRateLimit(ctx, bucket, claims[2], time.Minute)
	if err != nil || blocked.Granted || blocked.ResetAt == nil {
		t.Fatalf("ReserveRateLimit(exhausted) = %+v, %v; want reset-delayed", blocked, err)
	}
}

func TestStore_rateLimitReservationsAreReclaimedAfterWorkerExpiry(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forge", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	if _, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		Window: event.Window{To: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = found %t, err %v; want true, nil", found, err)
	}
	first, err := s.ReserveRateLimit(ctx, "forge:unknown", claim, time.Minute)
	if err != nil || !first.Granted {
		t.Fatalf("ReserveRateLimit(first) = %+v, %v; want granted", first, err)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE rate_limit_reservations SET expires_at = now() - interval '1 second'
		WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("expire abandoned reservation: %v", err)
	}
	reclaimed, err := s.ReserveRateLimit(ctx, "forge:unknown", claim, time.Minute)
	if err != nil || !reclaimed.Granted || reclaimed.ID == first.ID {
		t.Fatalf("ReserveRateLimit(reclaimed) = %+v, %v; want a new granted reservation", reclaimed, err)
	}
}
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := s.db.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

func intValue(value int) *int { return &value }

func TestStore_EnqueueGitHubHistoryUsesBackfillThenOverlap(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	forge, err := s.CreateForgeInstance(ctx, "github", "https://github.com")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	createdAt := time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC)
	tracked, err := s.TrackGitHubAccount(ctx, forge.ID, GitHubAccount{
		AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
		AccountCreatedAt: createdAt,
	})
	if err != nil {
		t.Fatalf("TrackGitHubAccount: %v", err)
	}
	initial, err := s.EnqueueGitHubHistory(ctx, tracked.PersonID, forge.ID)
	if err != nil {
		t.Fatalf("EnqueueGitHubHistory(initial): %v", err)
	}
	if initial.ScanMode != "backfill" || !initial.Window.From.Equal(createdAt) {
		t.Errorf("initial GitHub scan = mode %q, from %s; want backfill from account creation %s",
			initial.ScanMode, initial.Window.From, createdAt)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "backfill-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = %t, %v; want initial backfill", found, err)
	}
	// Exercise the conflict-update checkpoint branch as well as scan completion.
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{NextCursor: "page-2"},
		event.FetchReport{}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage(first): %v", err)
	}
	claim.Cursor = "page-2"
	if _, err := s.EnqueueGitHubHistory(ctx, tracked.PersonID, forge.ID); !errors.Is(err, ErrCollectionJobActive) {
		t.Fatalf("EnqueueGitHubHistory(interrupted) error = %v, want existing resumable work", err)
	}
	if err := s.ReleaseCollectionJob(ctx, claim); err != nil {
		t.Fatalf("ReleaseCollectionJob: %v", err)
	}
	claim, found, err = s.ClaimCollectionJob(ctx, "resumed-worker", time.Minute)
	if err != nil || !found || claim.ID != initial.ID || claim.Cursor != "page-2" ||
		!claim.Window.To.Equal(initial.Window.To) {
		t.Fatalf("resumed scan = %+v, found %t, error %v; want original job, window and cursor", claim, found, err)
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{KnownRepositories: []string{"other/repo"}}, true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage(completed backfill): %v", err)
	}

	completedThrough := claim.Window.To
	incremental, err := s.EnqueueGitHubHistory(ctx, tracked.PersonID, forge.ID)
	if err != nil {
		t.Fatalf("EnqueueGitHubHistory(incremental): %v", err)
	}
	wantFrom := completedThrough.Add(-7 * 24 * time.Hour)
	if incremental.ScanMode != "incremental" || !incremental.Window.From.Equal(wantFrom) {
		t.Errorf("incremental scan = mode %q, from %s; want from %s with seven-day overlap",
			incremental.ScanMode, incremental.Window.From, wantFrom)
	}
	nextClaim, found, err := s.ClaimCollectionJob(ctx, "incremental-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob(incremental) = %t, %v", found, err)
	}
	if len(nextClaim.KnownRepositories) != 1 || nextClaim.KnownRepositories[0] != "other/repo" {
		t.Errorf("incremental known repositories = %v, want persisted other/repo", nextClaim.KnownRepositories)
	}
}

func TestStore_CompletedBackfillSetsWatermarkOnFirstPage(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	person, err := s.CreatePerson(ctx, nil)
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}
	forge, err := s.CreateForgeInstance(ctx, "forgejo", "https://forge.example")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	windowEnd := time.Date(2025, 1, 20, 12, 0, 0, 0, time.UTC)
	if _, err := s.EnqueueCollectionJob(ctx, CollectionJobInput{
		PersonID: person.ID, ForgeInstanceID: forge.ID, SourceKey: "forge",
		ScanMode: "backfill", Window: event.Window{To: windowEnd},
	}); err != nil {
		t.Fatalf("EnqueueCollectionJob: %v", err)
	}
	claim, found, err := s.ClaimCollectionJob(ctx, "worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob = %t, %v", found, err)
	}
	if _, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true}, event.FetchReport{},
		true, 1, uuid.Nil); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	var complete bool
	var through *time.Time
	if err := s.db.QueryRow(ctx, `
		SELECT backfill_complete, completed_through FROM checkpoints
		WHERE person_id = $1 AND forge_instance_id = $2 AND source_key = 'forge'`,
		person.ID, forge.ID).Scan(&complete, &through); err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if !complete || through == nil || !through.Equal(windowEnd) {
		t.Errorf("checkpoint = complete %t, through %v; want complete through %s", complete, through, windowEnd)
	}
}
