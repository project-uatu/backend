package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestOpen_requiresPostgreSQLURL(t *testing.T) {
	_, err := Open(t.Context(), Options{
		MinConns: 0, MaxConns: 1, ConnectTimeout: time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "UATU_DATABASE_URL is required") {
		t.Fatalf("Open without URL error = %v, want required URL error", err)
	}
}

func TestStore_persistsSnapshots(t *testing.T) {
	store := testStore(t)
	if _, ok, err := store.Get(t.Context(), SourceGitHubRepositories, "octocat"); err != nil || ok {
		t.Fatalf("Get on empty store = ok %v, err %v; want false, nil", ok, err)
	}
	for _, data := range [][]byte{[]byte(`{"repositories":["first"]}`), []byte(`{"repositories":["second"]}`)} {
		if err := store.Save(t.Context(), SourceGitHubRepositories, "octocat", data); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	snapshot, ok, err := store.Get(t.Context(), SourceGitHubRepositories, "octocat")
	if err != nil || !ok {
		t.Fatalf("Get saved snapshot = ok %v, err %v; want true, nil", ok, err)
	}
	var saved struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(snapshot.Data, &saved); err != nil {
		t.Fatalf("decode saved snapshot: %v", err)
	}
	if len(saved.Repositories) != 1 || saved.Repositories[0] != "second" {
		t.Errorf("snapshot repositories = %v, want [second]", saved.Repositories)
	}
	if snapshot.FetchedAt.IsZero() || snapshot.FetchedAt.Location() != time.UTC {
		t.Errorf("snapshot FetchedAt = %v, want non-zero UTC time", snapshot.FetchedAt)
	}
	if _, ok, err := store.Get(t.Context(), Source("other"), "octocat"); err != nil {
		t.Fatalf("Get other source: %v", err)
	} else if ok {
		t.Error("snapshot leaked across sources")
	}
}

func TestOpen_serializesConcurrentMigrations(t *testing.T) {
	options, cleanup := testDatabase(t)
	defer cleanup()

	const clients = 4
	stores := make([]*Store, clients)
	errs := make([]error, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Go(func() {
			stores[i], errs[i] = Open(t.Context(), options)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Open client %d: %v", i, err)
		}
		defer stores[i].Close()
	}

	var count int
	if err := stores[0].db.QueryRow(t.Context(),
		"SELECT count(*) FROM schema_migrations WHERE version = 1").Scan(&count); err != nil {
		t.Fatalf("count initial migrations: %v", err)
	}
	if count != 1 {
		t.Errorf("initial migration rows = %d, want 1", count)
	}
}

func TestOpen_rejectsMigrationChecksumChange(t *testing.T) {
	options, cleanup := testDatabase(t)
	defer cleanup()
	store := openTestStore(t, options)

	if _, err := store.db.Exec(t.Context(),
		"UPDATE schema_migrations SET checksum = 'invalid' WHERE version = 1"); err != nil {
		t.Fatalf("change migration checksum: %v", err)
	}
	if _, err := Open(t.Context(), options); err == nil || !strings.Contains(err.Error(), "checksum does not match") {
		t.Fatalf("Open modified migration error = %v, want checksum mismatch", err)
	}
}

func TestOpen_rejectsNewerSchema(t *testing.T) {
	options, cleanup := testDatabase(t)
	defer cleanup()
	store := openTestStore(t, options)
	if _, err := store.db.Exec(t.Context(), `
		INSERT INTO schema_migrations (version, name, checksum)
		VALUES (99, 'future.sql', 'future')`); err != nil {
		t.Fatalf("insert newer schema version: %v", err)
	}
	if _, err := Open(t.Context(), options); err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open newer schema error = %v, want newer-version error", err)
	}
}

func TestStore_personTimelineAndIdentityConflicts(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	nameOne, nameTwo := "one", "two"
	personOne, err := store.CreatePerson(ctx, &nameOne)
	if err != nil {
		t.Fatalf("CreatePerson one: %v", err)
	}
	personTwo, err := store.CreatePerson(ctx, &nameTwo)
	if err != nil {
		t.Fatalf("CreatePerson two: %v", err)
	}
	instance, err := store.CreateForgeInstance(ctx, "github", "https://github.com")
	if err != nil {
		t.Fatalf("CreateForgeInstance: %v", err)
	}
	key := IdentityKey{
		ForgeInstanceID: &instance.ID,
		Kind:            "login",
		Value:           "OctoCat",
		Normalized:      "octocat",
	}
	claimOne, err := store.ClaimIdentity(ctx, IdentityClaimInput{
		IdentityKey: key,
		PersonID:    personOne.ID,
		Method:      "manual",
		ReviewedBy:  "maintainer",
		Disposition: "accepted",
	})
	if err != nil {
		t.Fatalf("ClaimIdentity one: %v", err)
	}
	claimTwo, err := store.ClaimIdentity(ctx, IdentityClaimInput{
		IdentityKey: key,
		PersonID:    personTwo.ID,
		Method:      "automatic",
		Disposition: "accepted",
	})
	if err != nil {
		t.Fatalf("ClaimIdentity conflict: %v", err)
	}
	if claimTwo.Disposition != "conflict" {
		t.Errorf("conflicting claim disposition = %q, want conflict", claimTwo.Disposition)
	}

	eventID, err := store.SaveRawEvent(ctx, RawEventInput{
		ForgeInstanceID:      instance.ID,
		ExternalID:           "pull:1",
		Kind:                 "pull_request",
		Payload:              json.RawMessage(`{"number":1}`),
		PayloadSchemaVersion: 1,
	})
	if err != nil {
		t.Fatalf("SaveRawEvent: %v", err)
	}
	openedAt := time.Date(2020, 1, 3, 12, 0, 0, 0, time.UTC)
	mergedAt := time.Date(2024, 5, 8, 15, 30, 0, 0, time.UTC)
	actions := []NormalizedActionInput{
		{ActionKey: "opened", Kind: "pull_request_opened", OccurredAt: openedAt, Project: "owner/repo"},
		{ActionKey: "merged", Kind: "pull_request_merged", OccurredAt: mergedAt, Project: "owner/repo"},
	}
	if err := store.ReplaceNormalizedActions(ctx, eventID, 1, actions); err != nil {
		t.Fatalf("ReplaceNormalizedActions: %v", err)
	}
	var openedID, mergedID uuid.UUID
	if err := store.db.QueryRow(ctx, `
		SELECT id FROM normalized_events WHERE raw_event_id = $1 AND action_key = 'opened' AND is_current`,
		eventID).Scan(&openedID); err != nil {
		t.Fatalf("load opened action: %v", err)
	}
	if err := store.db.QueryRow(ctx, `
		SELECT id FROM normalized_events WHERE raw_event_id = $1 AND action_key = 'merged' AND is_current`,
		eventID).Scan(&mergedID); err != nil {
		t.Fatalf("load merged action: %v", err)
	}
	for _, actionID := range []uuid.UUID{openedID, mergedID} {
		if err := store.ReplaceEventPeople(ctx, actionID, []EventPersonInput{{
			PersonID: personOne.ID, Role: "author", IdentityClaimID: &claimOne.ID,
		}}); err != nil {
			t.Fatalf("ReplaceEventPeople: %v", err)
		}
	}

	entries, err := store.Timeline(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline: %v", err)
	}
	if len(entries) != 2 || !entries[0].OccurredAt.Equal(mergedAt) || !entries[1].OccurredAt.Equal(openedAt) {
		t.Errorf("Timeline entries = %+v, want merged then opened across years", entries)
	}
	tx, err := store.db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin timeline index check: %v", err)
	}
	rollback := func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback timeline index check: %v", err)
		}
	}
	if _, err := tx.Exec(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		rollback()
		t.Fatalf("disable sequential scans for index check: %v", err)
	}
	planRows, err := tx.Query(ctx, `
		EXPLAIN SELECT normalized_event_id FROM event_people
		WHERE person_id = $1 AND occurred_at >= $2 AND occurred_at < $3
		ORDER BY occurred_at DESC, normalized_event_id`,
		personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		rollback()
		t.Fatalf("explain timeline index query: %v", err)
	}
	var plan strings.Builder
	for planRows.Next() {
		var line string
		if err := planRows.Scan(&line); err != nil {
			planRows.Close()
			rollback()
			t.Fatalf("scan timeline query plan: %v", err)
		}
		plan.WriteString(line)
	}
	planRows.Close()
	if err := planRows.Err(); err != nil {
		rollback()
		t.Fatalf("read timeline query plan: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback timeline index check: %v", err)
	}
	if !strings.Contains(plan.String(), "event_people_timeline_idx") {
		t.Errorf("timeline query plan = %q, want event_people_timeline_idx", plan.String())
	}
	buckets, err := store.TimelineCounts(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), TimeBucketMonth)
	if err != nil {
		t.Fatalf("TimelineCounts: %v", err)
	}
	if len(buckets) != 2 || buckets[0].Count != 1 || buckets[1].Count != 1 {
		t.Errorf("monthly timeline buckets = %+v, want two buckets with one action each", buckets)
	}

	actions[0].OccurredAt = time.Date(2023, 1, 3, 12, 0, 0, 0, time.UTC)
	if err := store.ReplaceNormalizedActions(ctx, eventID, 1, actions); err != nil {
		t.Fatalf("re-collect normalized actions: %v", err)
	}
	entries, err = store.Timeline(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline after re-collection: %v", err)
	}
	if len(entries) != 2 || !entries[1].OccurredAt.Equal(openedAt) {
		t.Errorf("timeline after re-collection = %+v, want original action time", entries)
	}

	actions[0].OccurredAt = openedAt
	if err := store.ReplaceNormalizedActions(ctx, eventID, 2, actions); err != nil {
		t.Fatalf("re-normalize actions: %v", err)
	}
	entries, err = store.Timeline(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline after re-normalization: %v", err)
	}
	if len(entries) != 2 || !entries[1].OccurredAt.Equal(openedAt) {
		t.Errorf("timeline after re-normalization = %+v, want both actions with original dates", entries)
	}
	if err := store.ReplaceNormalizedActions(ctx, eventID, 3, actions); err != nil {
		t.Fatalf("re-normalize actions with a third version: %v", err)
	}
	entries, err = store.Timeline(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline after third normalizer version: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("timeline after third normalizer version = %+v, want both actions without duplicate links", entries)
	}
	var currentActionCount int
	if err := store.db.QueryRow(ctx, `
		SELECT count(*) FROM normalized_events
		WHERE raw_event_id = $1 AND is_current`, eventID).Scan(&currentActionCount); err != nil {
		t.Fatalf("count current normalized actions: %v", err)
	}
	if currentActionCount != 2 {
		t.Errorf("current normalized action count = %d, want 2 across versions", currentActionCount)
	}

	observedAt := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, value := range []int64{2, 3} {
		if err := store.AddContributionCount(ctx, ContributionCountInput{
			PersonID: personOne.ID, ForgeInstanceID: instance.ID,
			ActivityDate: openedAt, Metric: "contributions", Kind: "commit",
			Value: value, ObservedAt: observedAt.Add(time.Duration(value) * time.Hour),
		}); err != nil {
			t.Fatalf("AddContributionCount: %v", err)
		}
	}
	counts, err := store.ContributionCounts(ctx, personOne.ID, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 2, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("ContributionCounts: %v", err)
	}
	if len(counts) != 2 || counts[0].Value != 2 || counts[1].Value != 3 {
		t.Errorf("contribution count observations = %+v, want both observations", counts)
	}

	run, err := store.StartSyncRun(ctx, personOne.ID, instance.ID, "github-history")
	if err != nil {
		t.Fatalf("StartSyncRun: %v", err)
	}
	if err := store.RecordCoverage(ctx, personOne.ID, instance.ID, "github-history",
		time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		"complete", "", &run.ID); err != nil {
		t.Fatalf("RecordCoverage: %v", err)
	}
	if err := store.FinishSyncRun(ctx, run.ID, "succeeded", json.RawMessage(`{"pages":4}`)); err != nil {
		t.Fatalf("FinishSyncRun: %v", err)
	}
	coverage, err := store.Coverage(ctx, personOne.ID, instance.ID, "github-history",
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Coverage: %v", err)
	}
	if len(coverage) != 1 || coverage[0].Status != "complete" {
		t.Errorf("Coverage = %+v, want one complete interval", coverage)
	}
	if err := store.RecordCoverage(ctx, personOne.ID, instance.ID, "github-history",
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		"gap", "upstream history limit", nil); err != nil {
		t.Fatalf("RecordCoverage gap: %v", err)
	}
	coverage, err = store.Coverage(ctx, personOne.ID, instance.ID, "github-history",
		time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Coverage with gap: %v", err)
	}
	if len(coverage) != 2 || coverage[0].Status != "complete" || coverage[1].Status != "gap" {
		t.Errorf("Coverage with gap = %+v, want complete interval then gap", coverage)
	}

	manualReassignment, err := store.ClaimIdentity(ctx, IdentityClaimInput{
		IdentityKey: key,
		PersonID:    personTwo.ID,
		Method:      "manual",
		ReviewedBy:  "maintainer",
		Disposition: "accepted",
	})
	if err != nil {
		t.Fatalf("manual identity reassignment: %v", err)
	}
	if manualReassignment.Disposition != "accepted" {
		t.Errorf("manual reassignment disposition = %q, want accepted", manualReassignment.Disposition)
	}
	entries, err = store.Timeline(ctx, personOne.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline after reassignment: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("old person's timeline after reassignment = %+v, want no stale links", entries)
	}
	entries, err = store.Timeline(ctx, personTwo.ID, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("Timeline after reassignment for new person: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("new person's timeline after reassignment = %+v, want both actions", entries)
	}
	if err := store.DeletePerson(ctx, personTwo.ID); err != nil {
		t.Fatalf("DeletePerson: %v", err)
	}
	var assignmentCount int
	if err := store.db.QueryRow(ctx,
		"SELECT count(*) FROM identity_assignments WHERE person_id = $1", personTwo.ID).
		Scan(&assignmentCount); err != nil {
		t.Fatalf("count assignments after deletion: %v", err)
	}
	if assignmentCount != 0 {
		t.Errorf("person assignments after deletion = %d, want 0", assignmentCount)
	}
	if err := store.DeletePerson(ctx, personOne.ID); err != nil {
		t.Fatalf("DeletePerson for final identity owner: %v", err)
	}
	var identityKeyCount int
	if err := store.db.QueryRow(ctx,
		"SELECT count(*) FROM identity_keys WHERE id = $1", claimOne.IdentityKeyID).Scan(&identityKeyCount); err != nil {
		t.Fatalf("count identity keys after final owner deletion: %v", err)
	}
	if identityKeyCount != 0 {
		t.Errorf("identity keys after deleting their final owner = %d, want 0", identityKeyCount)
	}
	var rawEventCount int
	if err := store.db.QueryRow(ctx,
		"SELECT count(*) FROM raw_events WHERE id = $1", eventID).Scan(&rawEventCount); err != nil {
		t.Fatalf("count raw events after person deletion: %v", err)
	}
	if rawEventCount != 1 {
		t.Errorf("raw events after person deletion = %d, want shared source event retained", rawEventCount)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	options, cleanup := testDatabase(t)
	t.Cleanup(cleanup)
	return openTestStore(t, options)
}

func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	store, err := Open(t.Context(), options)
	if err != nil {
		t.Fatalf("Open test store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})
	return store
}

func testDatabase(t *testing.T) (Options, func()) {
	t.Helper()
	databaseURL := os.Getenv("UATU_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("UATU_TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(t.Context(), databaseURL)
	if err != nil {
		t.Fatalf("connect to isolated PostgreSQL test database: %v", err)
	}
	schema := "uatu_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("create isolated test schema: %v", err)
	}
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	return Options{
			URL:            parsed.String(),
			SSLMode:        "verify-full",
			MinConns:       0,
			MaxConns:       4,
			ConnectTimeout: 5 * time.Second,
		}, func() {
			if _, err := admin.Exec(context.Background(),
				"DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
				t.Errorf("drop isolated test schema: %v", err)
			}
			if err := admin.Close(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("close test database connection: %v", err)
			}
		}
}

func TestOpen_rejectsMalformedURLWithoutLeakingCredentials(t *testing.T) {
	secret := "do-not-log"
	_, err := Open(t.Context(), Options{
		URL:            "postgres://user:" + secret + "@[invalid",
		SSLMode:        "verify-full",
		MaxConns:       1,
		ConnectTimeout: time.Second,
	})
	if err == nil {
		t.Fatal("Open malformed URL error = nil, want error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Open error exposed credential: %v", err)
	}
}

func TestOpen_requiresValidPoolBounds(t *testing.T) {
	tests := []Options{
		{URL: "postgres://localhost/uatu", MaxConns: 0, ConnectTimeout: time.Second},
		{URL: "postgres://localhost/uatu", MinConns: 2, MaxConns: 1, ConnectTimeout: time.Second},
	}
	for i, options := range tests {
		if _, err := Open(t.Context(), options); err == nil {
			t.Errorf("Open options %d error = nil, want pool bounds error", i)
		}
	}
}

func TestWithDefaultSSLMode(t *testing.T) {
	tests := []struct {
		name string
		url  string
		mode string
		want string
		err  bool
	}{
		{name: "secure default", url: "postgres://localhost/uatu", mode: "verify-full", want: "verify-full"},
		{name: "explicit URL mode", url: "postgres://localhost/uatu?sslmode=disable", mode: "verify-full", want: "disable"},
		{name: "invalid scheme", url: "sqlite://uatu.db", mode: "verify-full", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := withDefaultSSLMode(tt.url, tt.mode)
			if (err != nil) != tt.err {
				t.Fatalf("withDefaultSSLMode error = %v, wantErr %v", err, tt.err)
			}
			if err == nil {
				parsed, parseErr := url.Parse(got)
				if parseErr != nil {
					t.Fatalf("parse output URL: %v", parseErr)
				}
				if mode := parsed.Query().Get("sslmode"); mode != tt.want {
					t.Errorf("sslmode = %q, want %q", mode, tt.want)
				}
			}
		})
	}
}

func TestMigrations_loadSequentialVersions(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("loadMigrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("loadMigrations returned no migrations")
	}
	for i, migration := range migrations {
		if migration.version != i+1 || migration.checksum == "" {
			t.Errorf("migration at %d = %+v, want sequential version and checksum", i, migration)
		}
	}
}
