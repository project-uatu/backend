package collector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/store"
)

type runnerStore struct {
	collectionClaim *store.JobClaim
	normalization   *store.NormalizationClaim
	pages           []persistedPage
	retryCause      error
	retryAt         time.Time
	deferredAt      time.Time
	failedReason    string
	released        bool
	quota           store.QuotaReservation
	quotaBucket     string
	completed       int
	normalizedRetry int
}

type persistedPage struct {
	page       event.Page
	report     event.FetchReport
	successful bool
	cursor     string
}

func (s *runnerStore) ClaimCollectionJob(context.Context, string, time.Duration) (store.JobClaim, bool, error) {
	if s.collectionClaim == nil {
		return store.JobClaim{}, false, nil
	}
	claim := *s.collectionClaim
	s.collectionClaim = nil
	return claim, true, nil
}

func (*runnerStore) RenewCollectionLease(context.Context, store.JobClaim, time.Duration) error {
	return nil
}

func (s *runnerStore) PersistCollectionPage(_ context.Context, claim store.JobClaim, page event.Page, report event.FetchReport, successful bool, _ int) (bool, error) {
	s.pages = append(s.pages, persistedPage{page: page, report: report, successful: successful, cursor: claim.Cursor})
	return successful && page.Done, nil
}

func (s *runnerStore) RetryCollectionJob(_ context.Context, _ store.JobClaim, cause error, retryAt time.Time) error {
	s.retryCause, s.retryAt = cause, retryAt
	return nil
}

func (s *runnerStore) DeferCollectionJob(_ context.Context, _ store.JobClaim, retryAt time.Time) error {
	s.deferredAt = retryAt
	return nil
}

func (s *runnerStore) FailCollectionJob(_ context.Context, _ store.JobClaim, reason string) error {
	s.failedReason = reason
	return nil
}

func (s *runnerStore) ReleaseCollectionJob(context.Context, store.JobClaim) error {
	s.released = true
	return nil
}

func (s *runnerStore) ClaimNormalizationJob(context.Context, string, time.Duration) (store.NormalizationClaim, bool, error) {
	if s.normalization == nil {
		return store.NormalizationClaim{}, false, nil
	}
	claim := *s.normalization
	s.normalization = nil
	return claim, true, nil
}

func (*runnerStore) RenewNormalizationLease(context.Context, store.NormalizationClaim, time.Duration) error {
	return nil
}

func (s *runnerStore) CompleteNormalization(context.Context, store.NormalizationClaim, []event.Action) error {
	s.completed++
	return nil
}

func (s *runnerStore) RetryNormalizationJob(context.Context, store.NormalizationClaim, error, time.Time) error {
	s.normalizedRetry++
	return nil
}

func (*runnerStore) FailNormalizationJob(context.Context, store.NormalizationClaim, string) error {
	return nil
}

func (s *runnerStore) ReserveRateLimit(_ context.Context, bucket string) (store.QuotaReservation, error) {
	s.quotaBucket = bucket
	return s.quota, nil
}

func (*runnerStore) ReleaseRateLimit(context.Context, string) error { return nil }

type pageConnector struct {
	source   event.Source
	bucket   string
	fetch    func(context.Context, event.PageRequest) (event.Page, event.FetchReport, error)
	requests []event.PageRequest
}

func (c *pageConnector) Source() event.Source { return c.source }
func (c *pageConnector) QuotaBucket() string  { return c.bucket }
func (c *pageConnector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	c.requests = append(c.requests, request)
	return c.fetch(ctx, request)
}

type normalizerFunc func(event.RawEvent) ([]event.Action, error)

func (f normalizerFunc) Normalize(raw event.RawEvent) ([]event.Action, error) {
	return f(raw)
}

func TestRunner_RunOnce_persistsBoundedPages(t *testing.T) {
	instanceID := uuid.New()
	personID := uuid.New()
	window := event.Window{To: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
	raw := event.RawEvent{
		ForgeInstanceID: instanceID, Source: "test", ExternalID: "event-1", Kind: "change",
		Payload: json.RawMessage(`{"id":1}`), PayloadSchemaVersion: 1,
	}
	storeStub := &runnerStore{collectionClaim: &store.JobClaim{
		CollectionJob: store.CollectionJob{
			ID: uuid.New(), PersonID: personID, ForgeInstanceID: instanceID,
			SourceKey: "test", ScanMode: "backfill", Window: window,
		},
		Token: uuid.New(), SyncRunID: uuid.New(),
	}, quota: store.QuotaReservation{Granted: true}}
	connector := &pageConnector{
		source: "test", bucket: "test:shared",
		fetch: func(_ context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
			if request.PageSize != defaultPageSize {
				t.Errorf("FetchPage page size = %d, want %d", request.PageSize, defaultPageSize)
			}
			switch request.Cursor {
			case "":
				return event.Page{Events: []event.RawEvent{raw}, NextCursor: "next"}, event.FetchReport{}, nil
			case "next":
				return event.Page{Done: true}, event.FetchReport{}, nil
			default:
				t.Errorf("FetchPage cursor = %q, want empty or next", request.Cursor)
				return event.Page{}, event.FetchReport{}, errors.New("unexpected cursor")
			}
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %t, %v; want true, nil", worked, err)
	}
	if len(connector.requests) != 2 || connector.requests[1].Cursor != "next" {
		t.Errorf("connector requests = %+v, want second cursor next", connector.requests)
	}
	if len(storeStub.pages) != 2 || !storeStub.pages[0].successful || !storeStub.pages[1].successful {
		t.Errorf("persisted pages = %+v, want two successful pages", storeStub.pages)
	}
	if len(storeStub.pages) == 0 {
		t.Fatal("persisted pages = none, want two")
	}
	if storeStub.pages[0].report.RateLimitBucket != "test:shared" {
		t.Errorf("persisted quota bucket = %q, want test:shared", storeStub.pages[0].report.RateLimitBucket)
	}
}

func TestRunner_RunOnce_persistsPartialReportAndRetriesWithoutAdvancingCursor(t *testing.T) {
	instanceID := uuid.New()
	fetchErr := errors.New("private upstream detail")
	storeStub := &runnerStore{collectionClaim: &store.JobClaim{
		CollectionJob: store.CollectionJob{
			ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: instanceID,
			SourceKey: "test", ScanMode: "backfill", Cursor: "stable",
			Attempts: 1, MaxAttempts: 5,
			Window: event.Window{To: time.Now().UTC()},
		},
		Token: uuid.New(), SyncRunID: uuid.New(),
	}}
	connector := &pageConnector{
		source: "test",
		fetch: func(context.Context, event.PageRequest) (event.Page, event.FetchReport, error) {
			return event.Page{Events: []event.RawEvent{{
					ForgeInstanceID: instanceID, Source: "test", ExternalID: "partial", Kind: "change",
					Payload: json.RawMessage(`{"id":1}`), PayloadSchemaVersion: 1,
				}}}, event.FetchReport{
					Requests: 1, Errors: []string{"private upstream detail"},
				}, fetchErr
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %t, %v; want true, nil", worked, err)
	}
	if len(storeStub.pages) != 1 || storeStub.pages[0].successful {
		t.Fatalf("persisted pages = %+v, want one partial page", storeStub.pages)
	}
	if storeStub.pages[0].cursor != "stable" || storeStub.pages[0].page.NextCursor != "" {
		t.Errorf("partial page advanced cursor: %+v", storeStub.pages[0])
	}
	if storeStub.retryCause == nil || storeStub.retryAt.IsZero() {
		t.Errorf("retry = %v at %v, want due retry", storeStub.retryCause, storeStub.retryAt)
	}
	if got := storeStub.pages[0].report.Errors; len(got) != 2 ||
		got[0] != "source reported an error" ||
		got[1] != "upstream or normalization operation failed" {
		t.Errorf("persisted errors = %q, want sanitized source and fetch failures", got)
	}
}

func TestRunner_RunOnce_defersWhenQuotaIsEmpty(t *testing.T) {
	resetAt := time.Now().UTC().Add(time.Hour)
	storeStub := &runnerStore{
		collectionClaim: &store.JobClaim{
			CollectionJob: store.CollectionJob{
				ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: uuid.New(),
				SourceKey: "test", Window: event.Window{To: time.Now().UTC()},
			},
			Token: uuid.New(), SyncRunID: uuid.New(),
		},
		quota: store.QuotaReservation{Granted: false, ResetAt: &resetAt},
	}
	connector := &pageConnector{source: "test", bucket: "shared"}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	if worked, err := runner.RunOnce(t.Context()); err != nil || !worked {
		t.Fatalf("RunOnce() = %t, %v; want true, nil", worked, err)
	}
	if len(connector.requests) != 0 {
		t.Errorf("FetchPage calls = %d, want 0 while quota is empty", len(connector.requests))
	}
	if storeStub.quotaBucket != "shared" {
		t.Errorf("reserved bucket = %q, want shared", storeStub.quotaBucket)
	}
	if !storeStub.deferredAt.Equal(resetAt) {
		t.Errorf("deferred retry time = %v, want quota reset %v", storeStub.deferredAt, resetAt)
	}
}

func TestRunner_RunOnce_releasesClaimWhenFetchIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	storeStub := &runnerStore{collectionClaim: &store.JobClaim{
		CollectionJob: store.CollectionJob{
			ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: uuid.New(),
			SourceKey: "test", Window: event.Window{To: time.Now().UTC()},
		},
		Token: uuid.New(), SyncRunID: uuid.New(),
	}}
	connector := &pageConnector{
		source: "test",
		fetch: func(ctx context.Context, _ event.PageRequest) (event.Page, event.FetchReport, error) {
			cancel()
			return event.Page{}, event.FetchReport{}, ctx.Err()
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(ctx)
	if !worked || !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce(canceled fetch) = %t, %v; want true, context.Canceled", worked, err)
	}
	if !storeStub.released || storeStub.retryCause != nil {
		t.Errorf("canceled claim released=%t retry=%v; want release without retry", storeStub.released, storeStub.retryCause)
	}
}

func TestRunner_RunOnce_normalizesStoredEvent(t *testing.T) {
	storeStub := &runnerStore{normalization: &store.NormalizationClaim{
		RawEventID: uuid.New(), NormalizerVersion: 1,
		Event: event.RawEvent{Source: "test", Kind: "change", ExternalID: "stored"},
	}}
	runner := newTestRunner(t, storeStub, nil, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return []event.Action{{
			Key: "opened", Kind: "change_opened", OccurredAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		}}, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %t, %v; want true, nil", worked, err)
	}
	if storeStub.completed != 1 {
		t.Errorf("completed normalizations = %d, want 1", storeStub.completed)
	}
}

func TestRunner_Run_stopsOnCancellation(t *testing.T) {
	storeStub := &runnerStore{}
	runner, err := NewRunner(storeStub, nil, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), RunnerOptions{
		Owner: "test", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := runner.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Run(canceled) error = %v, want context.Canceled", err)
	}
}

func newTestRunner(t *testing.T, s durableWork, connectors []Connector, transformer Normalizer) *Runner {
	t.Helper()
	runner, err := NewRunner(s, connectors, transformer,
		slog.New(slog.NewTextHandler(io.Discard, nil)), RunnerOptions{Owner: "test-worker"})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return runner
}
