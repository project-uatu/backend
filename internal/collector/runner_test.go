// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/store"
)

type runnerStore struct {
	collectionClaim       *store.JobClaim
	collectionClaims      []store.JobClaim
	normalization         *store.NormalizationClaim
	pages                 []persistedPage
	persistErrors         []error
	retryCause            error
	retryAt               time.Time
	deferredAt            time.Time
	failedReason          string
	failCalls             int
	released              bool
	quota                 store.QuotaReservation
	quotaBucket           string
	completed             int
	normalizedRetry       int
	collectionRenewals    int
	normalizationRenewals int
	releasedQuota         int
	claimErr              error
	claimErrs             int
	onClaimErr            func()
}

type persistedPage struct {
	page       event.Page
	report     event.FetchReport
	successful bool
	cursor     string
	windowFrom time.Time
}

func (s *runnerStore) ClaimCollectionJobForSources(context.Context, string, time.Duration, []string) (store.JobClaim, bool, error) {
	if s.collectionClaim != nil {
		claim := *s.collectionClaim
		s.collectionClaim = nil
		return claim, true, nil
	}
	if len(s.collectionClaims) == 0 {
		return store.JobClaim{}, false, nil
	}
	claim := s.collectionClaims[0]
	s.collectionClaims = s.collectionClaims[1:]
	return claim, true, nil
}

func (s *runnerStore) RenewCollectionLease(context.Context, store.JobClaim, time.Duration) error {
	s.collectionRenewals++
	return nil
}

func (s *runnerStore) PersistCollectionPage(_ context.Context, claim store.JobClaim, page event.Page, report event.FetchReport, successful bool, _ int, _ uuid.UUID) (bool, error) {
	s.pages = append(s.pages, persistedPage{
		page: page, report: report, successful: successful, cursor: claim.Cursor,
		windowFrom: claim.Window.From,
	})
	if len(s.persistErrors) > 0 {
		err := s.persistErrors[0]
		s.persistErrors = s.persistErrors[1:]
		if err != nil {
			return false, err
		}
	}
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
	s.failCalls++
	return nil
}

func (s *runnerStore) ReleaseCollectionJob(context.Context, store.JobClaim) error {
	s.released = true
	return nil
}

func (s *runnerStore) ClaimNormalizationJob(_ context.Context, _ string, version int, _ time.Duration) (store.NormalizationClaim, bool, error) {
	if s.claimErr != nil {
		s.claimErrs++
		if s.onClaimErr != nil {
			s.onClaimErr()
		}
		return store.NormalizationClaim{}, false, s.claimErr
	}
	if s.normalization == nil || s.normalization.NormalizerVersion != version {
		return store.NormalizationClaim{}, false, nil
	}
	claim := *s.normalization
	s.normalization = nil
	return claim, true, nil
}

func (s *runnerStore) RenewNormalizationLease(context.Context, store.NormalizationClaim, time.Duration) error {
	s.normalizationRenewals++
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

func (s *runnerStore) ReserveRateLimit(_ context.Context, bucket string, _ store.JobClaim, _ time.Duration) (store.QuotaReservation, error) {
	s.quotaBucket = bucket
	return s.quota, nil
}

func (s *runnerStore) ReleaseRateLimit(context.Context, uuid.UUID) error {
	s.releasedQuota++
	return nil
}

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

func TestRunner_RunOnce_tracksResolvedAccountBeforePersistingPage(t *testing.T) {
	personID, forgeID := uuid.New(), uuid.New()
	createdAt := time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC)
	storeStub := &runnerStore{collectionClaim: &store.JobClaim{
		CollectionJob: store.CollectionJob{
			ID: uuid.New(), PersonID: personID, ForgeInstanceID: forgeID,
			SourceKey: "github_history", ScanMode: "backfill",
			Window: event.Window{To: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		Token: uuid.New(), SyncRunID: uuid.New(),
	}, quota: store.QuotaReservation{Granted: true, ID: uuid.New()}}
	connector := &pageConnector{
		source: "github_history", bucket: "github/core",
		fetch: func(_ context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
			if request.Cursor == "resolved" {
				return event.Page{Done: true}, event.FetchReport{RateLimitBucket: "github/core"}, nil
			}
			return event.Page{
				NextCursor: "resolved",
				ResolvedAccount: &event.ResolvedAccount{
					AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat", CreatedAt: createdAt,
				},
			}, event.FetchReport{RateLimitBucket: "github/core"}, nil
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if err != nil || !worked {
		t.Fatalf("RunOnce() = %t, %v; want true, nil", worked, err)
	}
	if len(storeStub.pages) != 2 || storeStub.pages[0].page.ResolvedAccount == nil ||
		storeStub.pages[0].page.ResolvedAccount.AccountID != "42" {
		t.Errorf("resolved account page = %+v; want resolved account to be persisted with its page", storeStub.pages)
	}
	if len(storeStub.pages) != 2 || !storeStub.pages[0].windowFrom.Equal(createdAt) {
		t.Errorf("persisted page windows = %+v, want resolved start %s on first page", storeStub.pages, createdAt)
	}
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
		KnownRepositories: []string{"other/repo"},
		Token:             uuid.New(), SyncRunID: uuid.New(),
	}, quota: store.QuotaReservation{Granted: true, ID: uuid.New()}}
	connector := &pageConnector{
		source: "test", bucket: "test:shared",
		fetch: func(_ context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
			if request.PageSize != defaultPageSize {
				t.Errorf("FetchPage page size = %d, want %d", request.PageSize, defaultPageSize)
			}
			if len(request.KnownRepositories) != 1 || request.KnownRepositories[0] != "other/repo" {
				t.Errorf("FetchPage known repositories = %v, want other/repo from claim", request.KnownRepositories)
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
	if len(connector.requests) > 0 && connector.requests[0].ScanMode != "backfill" {
		t.Errorf("first request scan mode = %q, want backfill", connector.requests[0].ScanMode)
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

func TestRunner_DrainFailsConflictingOnboardingAndContinues(t *testing.T) {
	now := time.Now().UTC()
	claims := []store.JobClaim{
		{
			CollectionJob: store.CollectionJob{
				ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: uuid.New(),
				SourceKey: "github_history", ScanMode: "backfill",
				Window: event.Window{To: now},
			},
			Token: uuid.New(), SyncRunID: uuid.New(),
		},
		{
			CollectionJob: store.CollectionJob{
				ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: uuid.New(),
				SourceKey: "github_history", ScanMode: "backfill",
				Window: event.Window{To: now},
			},
			Token: uuid.New(), SyncRunID: uuid.New(),
		},
	}
	storeStub := &runnerStore{
		collectionClaims: claims,
		persistErrors:    []error{store.ErrIdentityConflict, nil},
		quota:            store.QuotaReservation{Granted: true, ID: uuid.New()},
	}
	connector := &pageConnector{
		source: "github_history", bucket: "github/core",
		fetch: func(_ context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
			page := event.Page{Done: true}
			if request.PersonID == claims[0].PersonID {
				page.ResolvedAccount = &event.ResolvedAccount{
					AccountID: "42", NodeID: "MDQ6VXNlcjQy", Login: "octocat",
					CreatedAt: time.Date(2011, 1, 25, 18, 44, 36, 0, time.UTC),
				}
			}
			return page, event.FetchReport{RateLimitBucket: "github/core"}, nil
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))

	if err := runner.Drain(t.Context()); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(storeStub.pages) != 2 || storeStub.failCalls != 1 ||
		storeStub.failedReason != "resolved GitHub account is assigned to another person" {
		t.Errorf("Drain persistence/failure = %d pages, %d failures (%q); want both jobs processed and first failed",
			len(storeStub.pages), storeStub.failCalls, storeStub.failedReason)
	}
	if storeStub.releasedQuota != 1 {
		t.Errorf("released quota reservations = %d, want 1 after persistence conflict", storeStub.releasedQuota)
	}
}

func TestRunner_RunOnce_boundsEventsOnFetchError(t *testing.T) {
	instanceID := uuid.New()
	storeStub := &runnerStore{
		collectionClaim: &store.JobClaim{
			CollectionJob: store.CollectionJob{
				ID: uuid.New(), PersonID: uuid.New(), ForgeInstanceID: instanceID,
				SourceKey: "test", Window: event.Window{To: time.Now().UTC()},
			},
			Token: uuid.New(), SyncRunID: uuid.New(),
		},
		quota: store.QuotaReservation{Granted: true, ID: uuid.New()},
	}
	events := make([]event.RawEvent, 501)
	for i := range events {
		events[i] = event.RawEvent{
			ForgeInstanceID: instanceID, Source: "test", ExternalID: fmt.Sprintf("event-%d", i),
			Kind: "change", Payload: json.RawMessage(`{"id":1}`), PayloadSchemaVersion: 1,
		}
	}
	connector := &pageConnector{
		source: "test", bucket: "test:shared",
		fetch: func(context.Context, event.PageRequest) (event.Page, event.FetchReport, error) {
			return event.Page{Events: events, Done: true}, event.FetchReport{}, errors.New("partial fetch failure")
		},
	}
	runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if !worked || !errors.Is(err, ErrInvalidConnectorPage) {
		t.Fatalf("RunOnce(oversized failed page) = %t, %v; want true, invalid page", worked, err)
	}
	if len(storeStub.pages) != 1 {
		t.Fatalf("persisted pages = %d, want 1", len(storeStub.pages))
	}
	if got := len(storeStub.pages[0].page.Events); got != defaultPageSize {
		t.Errorf("persisted invalid events = %d, want bounded page size %d", got, defaultPageSize)
	}
	if storeStub.failedReason == "" || storeStub.retryCause != nil {
		t.Errorf("invalid page failure = %q, retry = %v; want permanent failure without retry", storeStub.failedReason, storeStub.retryCause)
	}
	if storeStub.releasedQuota != 0 {
		t.Errorf("settled quota reservations = %d, want 0 after successful partial-page persistence", storeStub.releasedQuota)
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

func TestRunner_RunOnce_onlyClaimsSupportedNormalizerVersion(t *testing.T) {
	storeStub := &runnerStore{normalization: &store.NormalizationClaim{
		RawEventID: uuid.New(), NormalizerVersion: 2,
		Event: event.RawEvent{Source: "test", Kind: "change", ExternalID: "stored"},
	}}
	runner := newTestRunner(t, storeStub, nil, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		return nil, nil
	}))
	worked, err := runner.RunOnce(t.Context())
	if worked || err != nil {
		t.Fatalf("RunOnce(unsupported version) = %t, %v; want no work and no error", worked, err)
	}
	if storeStub.completed != 0 || storeStub.normalization == nil {
		t.Errorf("unsupported normalization claim consumed: completed=%d claim=%+v", storeStub.completed, storeStub.normalization)
	}
}

type versionedConnector struct {
	pageConnector
	version int
}

func (c *versionedConnector) NormalizerVersion() int { return c.version }

func TestRunner_RunOnce_claimsVersionsOfItsVersionedConnectors(t *testing.T) {
	connector := &versionedConnector{pageConnector: pageConnector{source: "test_commits"}, version: 2}
	for _, version := range []int{1, 2} {
		storeStub := &runnerStore{normalization: &store.NormalizationClaim{
			RawEventID: uuid.New(), NormalizerVersion: version,
			Event: event.RawEvent{Source: "test_commits", Kind: "commit", ExternalID: "stored"},
		}}
		runner := newTestRunner(t, storeStub, []Connector{connector}, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
			return nil, nil
		}))
		if worked, err := runner.RunOnce(t.Context()); !worked || err != nil || storeStub.completed != 1 {
			t.Errorf("RunOnce(version %d) = %t, %v, completed %d; want claimed", version, worked, err, storeStub.completed)
		}
	}
}

func TestRunner_RunOnce_renewsLongNormalizationLease(t *testing.T) {
	storeStub := &runnerStore{normalization: &store.NormalizationClaim{
		RawEventID: uuid.New(), NormalizerVersion: 1,
		Event: event.RawEvent{Source: "test", Kind: "change", ExternalID: "slow"},
	}}
	runner, err := NewRunner(storeStub, nil, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		time.Sleep(100 * time.Millisecond)
		return nil, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), RunnerOptions{
		Owner: "slow-normalizer", LeaseDuration: 30 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	worked, err := runner.RunOnce(t.Context())
	if !worked || err != nil {
		t.Fatalf("RunOnce(slow normalization) = %t, %v; want true, nil", worked, err)
	}
	if storeStub.normalizationRenewals < 1 {
		t.Errorf("normalization lease renewals = %d, want at least 1", storeStub.normalizationRenewals)
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

func TestRunner_Run_resumesWorkAfterDatabaseOutage(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	storeStub := &runnerStore{claimErr: errors.New("connection refused")}
	storeStub.onClaimErr = func() {
		if storeStub.claimErrs == 3 {
			// The database "recovers" and work becomes available.
			storeStub.claimErr = nil
			storeStub.normalization = &store.NormalizationClaim{
				RawEventID: uuid.New(), NormalizerVersion: 1,
				Event: event.RawEvent{Source: "test", Kind: "change", ExternalID: "after-outage"},
			}
		}
	}
	done := make(chan struct{})
	runner, err := NewRunner(storeStub, nil, normalizerFunc(func(event.RawEvent) ([]event.Action, error) {
		defer close(done)
		return nil, nil
	}), slog.New(slog.NewTextHandler(io.Discard, nil)), RunnerOptions{
		Owner: "test", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	errc := make(chan error, 1)
	go func() { errc <- runner.Run(ctx) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("worker did not process work after the database recovered")
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("Run error = %v, want context.Canceled", err)
	}
	if storeStub.claimErrs < 3 {
		t.Errorf("failed claim attempts = %d, want at least 3 retries", storeStub.claimErrs)
	}
	if storeStub.completed != 1 {
		t.Errorf("completed normalizations = %d, want 1 after recovery", storeStub.completed)
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
