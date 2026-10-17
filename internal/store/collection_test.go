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
	report.RateLimitRemaining = intValue(3)
	rateLimitReset := time.Now().UTC().Add(time.Hour)
	report.RateLimitResetAt = &rateLimitReset
	firstPage := event.Page{Events: []event.RawEvent{raw}, NextCursor: "page-2"}
	if done, err := s.PersistCollectionPage(ctx, claim, firstPage, report, true, 1); err != nil || done {
		t.Fatalf("PersistCollectionPage(first) = done %t, err %v; want false, nil", done, err)
	}
	claim.Cursor = "page-2"

	changedPayload := raw
	changedPayload.Payload = json.RawMessage(`{"value":"from-failed-response"}`)
	partialReport := event.FetchReport{Errors: []string{"upstream request failed"}}
	if done, err := s.PersistCollectionPage(ctx, claim,
		event.Page{Events: []event.RawEvent{changedPayload}}, partialReport, false, 1); err != nil || done {
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

	if err := s.RetryCollectionJob(ctx, claim, errors.New("temporary fetch failure"), time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("RetryCollectionJob: %v", err)
	}
	claim, found, err = s.ClaimCollectionJob(ctx, "worker-two", time.Minute)
	if err != nil || !found {
		t.Fatalf("ClaimCollectionJob(retry) = found %t, err %v; want true, nil", found, err)
	}
	if claim.Cursor != "page-2" {
		t.Errorf("retried job cursor = %q, want page-2", claim.Cursor)
	}
	if done, err := s.PersistCollectionPage(ctx, claim, event.Page{Done: true},
		event.FetchReport{}, true, 1); err != nil || !done {
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
	var jobStatus string
	if err := s.db.QueryRow(ctx, "SELECT status FROM jobs WHERE id = $1", job.ID).Scan(&jobStatus); err != nil {
		t.Fatalf("load completed job: %v", err)
	}
	if jobStatus != "succeeded" {
		t.Errorf("job status = %q, want succeeded", jobStatus)
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
	input.IdempotencyKey = ""
	coalesced, err := s.EnqueueCollectionJob(ctx, input)
	if err != nil {
		t.Fatalf("EnqueueCollectionJob(coalesce without idempotency key): %v", err)
	}
	if coalesced.ID != first.ID || !coalesced.Window.From.Equal(input.Window.From) ||
		!coalesced.Window.To.Equal(input.Window.To) {
		t.Errorf("active job coalescing = %+v, want existing job %s with merged window %+v",
			coalesced, first.ID, input.Window)
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
		event.FetchReport{}, true, 1); err == nil {
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
	_, err = s.PersistCollectionPage(ctx, claims[0], event.Page{Done: true}, event.FetchReport{}, true, 1)
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
		event.FetchReport{}, true, 1); err != nil {
		t.Fatalf("PersistCollectionPage: %v", err)
	}
	normalization, found, err := s.ClaimNormalizationJob(ctx, "normalizer", time.Minute)
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
	staleNormalization, found, err := s.ClaimNormalizationJob(ctx, "expired-normalizer", 20*time.Millisecond)
	if err != nil || !found {
		t.Fatalf("ClaimNormalizationJob(expiring) = found %t, err %v; want true, nil", found, err)
	}
	time.Sleep(35 * time.Millisecond)
	normalization, found, err = s.ClaimNormalizationJob(ctx, "normalizer-v2", time.Minute)
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
	if job.ID != collection.ID || claim.ID == uuid.Nil {
		t.Errorf("test fixtures job %s / claim %s were not assigned", job.ID, claim.ID)
	}
}

func TestStore_rateLimitReservationsCoordinateWorkers(t *testing.T) {
	s := testStore(t)
	ctx := t.Context()
	const bucket = "forge:shared"
	first, err := s.ReserveRateLimit(ctx, bucket)
	if err != nil || !first.Granted {
		t.Fatalf("ReserveRateLimit(first) = %+v, %v; want granted", first, err)
	}
	second, err := s.ReserveRateLimit(ctx, bucket)
	if err != nil || second.Granted {
		t.Fatalf("ReserveRateLimit(concurrent) = %+v, %v; want blocked while budget unknown", second, err)
	}
	if err := s.ReleaseRateLimit(ctx, bucket); err != nil {
		t.Fatalf("ReleaseRateLimit: %v", err)
	}
	first, err = s.ReserveRateLimit(ctx, bucket)
	if err != nil || !first.Granted {
		t.Fatalf("ReserveRateLimit(after release) = %+v, %v; want granted", first, err)
	}
	remaining := 1
	reset := time.Now().UTC().Add(time.Hour)
	if _, err := s.db.Exec(ctx, `
		UPDATE rate_limit_buckets SET remaining = $2, reset_at = $3, in_flight = 0
		WHERE bucket_key = $1`, bucket, remaining, reset); err != nil {
		t.Fatalf("record rate limit: %v", err)
	}
	reserved, err := s.ReserveRateLimit(ctx, bucket)
	if err != nil || !reserved.Granted {
		t.Fatalf("ReserveRateLimit(remaining request) = %+v, %v; want granted", reserved, err)
	}
	blocked, err := s.ReserveRateLimit(ctx, bucket)
	if err != nil || blocked.Granted || blocked.ResetAt == nil {
		t.Fatalf("ReserveRateLimit(exhausted) = %+v, %v; want reset-delayed", blocked, err)
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
