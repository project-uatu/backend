package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/store"
)

type testStore struct {
	entries  []store.TimelineEntry
	coverage []store.CoverageInterval
	counts   []store.ContributionCount
	err      error
	readyErr error
	person   uuid.UUID
	from     time.Time
	to       time.Time
	limit    int
	calls    int
}

func (s *testStore) Ready(context.Context) error { return s.readyErr }

func (s *testStore) CoverageForPerson(_ context.Context, person uuid.UUID, from, to time.Time) ([]store.CoverageInterval, error) {
	s.calls++
	s.person, s.from, s.to = person, from, to
	return s.coverage, s.err
}

func (s *testStore) ContributionCounts(_ context.Context, person uuid.UUID, from, to time.Time) ([]store.ContributionCount, error) {
	s.calls++
	s.person, s.from, s.to = person, from, to
	return s.counts, s.err
}

func (s *testStore) Timeline(_ context.Context, person uuid.UUID, from, to time.Time, limit int) ([]store.TimelineEntry, error) {
	s.calls++
	s.person, s.from, s.to, s.limit = person, from, to, limit
	return s.entries, s.err
}

func TestHandler_activity(t *testing.T) {
	personID := uuid.New()
	entry := store.TimelineEntry{
		EventID: uuid.New(), Kind: "issue_opened", Role: "author",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Project:    "owner/repo", Title: "A contribution", URL: "https://github.com/owner/repo/issues/1",
	}
	tests := []struct {
		name      string
		path      string
		store     *testStore
		status    int
		body      string
		wantCalls int
		wantLimit int
	}{
		{
			name:      "stored activity",
			path:      "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store:     &testStore{entries: []store.TimelineEntry{entry}},
			status:    http.StatusOK,
			body:      `[{"event_id":"` + entry.EventID.String() + `","kind":"issue_opened","role":"author","occurred_at":"2026-01-02T03:04:05Z","project":"owner/repo","title":"A contribution","url":"https://github.com/owner/repo/issues/1"}]` + "\n",
			wantCalls: 1, wantLimit: 100,
		},
		{
			name:  "invalid person",
			path:  "/v1/persons/nope/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store: &testStore{}, status: http.StatusBadRequest,
			body: "{\"error\":\"invalid person ID\"}\n",
		},
		{
			name:  "missing range",
			path:  "/v1/persons/" + personID.String() + "/activity",
			store: &testStore{}, status: http.StatusBadRequest,
			body: "{\"error\":\"from must be an RFC3339 timestamp\"}\n",
		},
		{
			name:  "invalid limit",
			path:  "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z&limit=501",
			store: &testStore{}, status: http.StatusBadRequest,
			body: "{\"error\":\"limit must be between 1 and 500\"}\n",
		},
		{
			name:  "store failure",
			path:  "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store: &testStore{err: errors.New("database failed")}, status: http.StatusInternalServerError,
			body: "{\"error\":\"failed to read activity\"}\n", wantCalls: 1, wantLimit: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(tt.store, testLogger()), tt.path)
			if res.Code != tt.status {
				t.Errorf("GET %s status = %d, want %d", tt.path, res.Code, tt.status)
			}
			if got := res.Body.String(); got != tt.body {
				t.Errorf("GET %s body = %q, want %q", tt.path, got, tt.body)
			}
			if got := tt.store.calls; got != tt.wantCalls {
				t.Errorf("GET %s store calls = %d, want %d", tt.path, got, tt.wantCalls)
			}
			if tt.wantCalls > 0 && tt.store.limit != tt.wantLimit {
				t.Errorf("Timeline limit = %d, want %d", tt.store.limit, tt.wantLimit)
			}
		})
	}
}

func TestHandler_removedSnapshotRoute(t *testing.T) {
	s := &testStore{}
	res := serve(t, NewHandler(s, testLogger()), "/github/users/octocat/repositories")
	if res.Code != http.StatusNotFound || s.calls != 0 {
		t.Errorf("legacy snapshot route status/calls = %d/%d, want 404/0", res.Code, s.calls)
	}
}

func TestHandler_coverageAndCounts(t *testing.T) {
	personID, forgeID := uuid.New(), uuid.New()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	path := "/v1/persons/" + personID.String()
	tests := []struct {
		name string
		path string
		data *testStore
		want string
	}{
		{
			name: "coverage",
			path: path + "/coverage?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			data: &testStore{coverage: []store.CoverageInterval{{
				ForgeInstanceID: forgeID, SourceKey: "github_history", StartsAt: from,
				EndsAt: to, Status: "complete", CoverageScope: "known public repositories",
				RecordedAt: from,
			}}},
			want: `[{"forge_instance_id":"` + forgeID.String() + `","source":"github_history","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z","status":"complete","scope":"known public repositories","recorded_at":"2026-01-01T00:00:00Z"}]` + "\n",
		},
		{
			name: "aggregate counts",
			path: path + "/counts?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			data: &testStore{counts: []store.ContributionCount{{
				ForgeInstanceID: forgeID, ActivityDate: from, Metric: "daily_total",
				Value: 7, ObservedAt: from,
			}}},
			want: `[{"forge_instance_id":"` + forgeID.String() + `","date":"2026-01-01T00:00:00Z","metric":"daily_total","value":7,"observed_at":"2026-01-01T00:00:00Z"}]` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(tt.data, testLogger()), tt.path)
			if res.Code != http.StatusOK || res.Body.String() != tt.want {
				t.Errorf("GET %s = %d %q, want 200 %q", tt.path, res.Code, res.Body.String(), tt.want)
			}
			if tt.data.calls != 1 || tt.data.person != personID {
				t.Errorf("coverage/count store calls = %d for %s, want one call for %s", tt.data.calls, tt.data.person, personID)
			}
		})
	}
}

func TestHandler_healthz(t *testing.T) {
	res := serve(t, NewHandler(&testStore{}, testLogger()), "/healthz")
	if res.Code != http.StatusNoContent {
		t.Errorf("GET /healthz status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestHandler_readyz(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"ready", nil, http.StatusNoContent},
		{"database unavailable", errors.New("connection refused"), http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(&testStore{readyErr: tt.err}, testLogger()), "/readyz")
			if res.Code != tt.want {
				t.Errorf("GET /readyz status = %d, want %d", res.Code, tt.want)
			}
			if res.Body.Len() != 0 {
				t.Errorf("GET /readyz body = %q, want empty", res.Body.String())
			}
		})
	}
}

func TestHandler_healthzIgnoresDatabaseOutage(t *testing.T) {
	res := serve(t, NewHandler(&testStore{readyErr: errors.New("down")}, testLogger()), "/healthz")
	if res.Code != http.StatusNoContent {
		t.Errorf("GET /healthz status = %d, want %d during database outage", res.Code, http.StatusNoContent)
	}
}

func TestHealthHandler(t *testing.T) {
	handler := NewHealthHandler(func(context.Context) error { return errors.New("down") }, testLogger())
	if res := serve(t, handler, "/healthz"); res.Code != http.StatusNoContent {
		t.Errorf("GET /healthz status = %d, want %d", res.Code, http.StatusNoContent)
	}
	if res := serve(t, handler, "/readyz"); res.Code != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz status = %d, want %d", res.Code, http.StatusServiceUnavailable)
	}
}

func serve(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
