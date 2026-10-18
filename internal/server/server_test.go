package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/store"
)

type testStore struct {
	apiStore
	entries        []store.TimelineEntry
	coverage       []store.CoverageInterval
	counts         []store.ContributionCount
	err            error
	readyErr       error
	apiPersonErr   error
	createErr      error
	identifiersErr error
	enqueueErr     error
	jobErr         error
	forge          store.ForgeInstance
	createdPerson  store.Person
	createdJob     store.CollectionJob
	apiJob         store.APIJob
	createCalls    int
	lastForge      store.ForgeInstance
	queuedKey      string
	person         uuid.UUID
	from           time.Time
	to             time.Time
	limit          int
	calls          int
}

const testAPIKey = "01234567890123456789012345678901"

func testOptions() Options {
	return Options{APIKey: testAPIKey, CORSOrigins: []string{"https://frontend.example"}}
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

func (s *testStore) ActivityPage(_ context.Context, filter store.ActivityFilter) (store.ActivityPage, error) {
	s.calls++
	s.person, s.from, s.to, s.limit = filter.PersonID, filter.From, filter.To, filter.Limit
	return store.ActivityPage{Entries: s.entries}, s.err
}

func (s *testStore) APIPersonByID(_ context.Context, personID uuid.UUID) (store.APIPerson, error) {
	return store.APIPerson{ID: personID}, s.apiPersonErr
}

func (s *testStore) PersonSourceCoverage(context.Context, uuid.UUID) ([]store.APISourceCoverage, error) {
	return nil, nil
}

func (s *testStore) CreateOnboardedPerson(_ context.Context, forge store.ForgeInstance, displayName, _ string, _ []string) (store.Person, store.CollectionJob, error) {
	s.createCalls++
	s.lastForge = forge
	person := s.createdPerson
	if person.ID == uuid.Nil {
		person.ID = uuid.New()
		person.DisplayName = &displayName
		person.CreatedAt = time.Now().UTC()
		s.createdPerson = person
	}
	job := s.createdJob
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
		job.PersonID = person.ID
		job.SourceKey = store.SourceGitHubHistory
		s.createdJob = job
	}
	return person, job, s.createErr
}

func (s *testStore) APIJobByID(_ context.Context, jobID uuid.UUID) (store.APIJob, error) {
	if s.jobErr != nil {
		return store.APIJob{}, s.jobErr
	}
	job := s.apiJob
	if job.ID == uuid.Nil {
		job = store.APIJob{
			ID: jobID, PersonID: s.createdPerson.ID, Source: store.SourceGitHubHistory,
			Status: "queued", MaxAttempts: 10, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
	}
	return job, nil
}

func (s *testStore) ForgeForHost(_ context.Context, host string) (store.ForgeInstance, store.SourceProfile, error) {
	profile, ok := store.ProfileForHost(host)
	if !ok {
		return store.ForgeInstance{}, store.SourceProfile{}, store.ErrUnknownForge
	}
	if s.forge.ID == uuid.Nil {
		s.forge.ID = uuid.New()
	}
	forge := s.forge
	forge.Provider, forge.BaseURL = profile.Provider, profile.BaseURL
	return forge, profile, nil
}

func (s *testStore) UpdatePersonIdentifiers(context.Context, uuid.UUID, uuid.UUID, []store.APIIdentity, []store.APIIdentity) error {
	return s.identifiersErr
}

func (s *testStore) EnqueueHistoryWithKey(_ context.Context, personID, _ uuid.UUID, key string) (store.CollectionJob, error) {
	s.queuedKey = key
	job := s.createdJob
	if job.ID == uuid.Nil {
		job.ID = uuid.New()
		job.PersonID = personID
		job.SourceKey = store.SourceGitHubHistory
		s.createdJob = job
	}
	return job, s.enqueueErr
}

func TestHandler_activity(t *testing.T) {
	personID := uuid.New()
	entry := store.TimelineEntry{
		EventID: uuid.New(), Kind: "issue_opened", Role: "author",
		OccurredAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Project:    "owner/repo", Title: "A contribution", URL: "https://github.com/owner/repo/issues/1",
	}
	tests := []struct {
		name         string
		path         string
		store        *testStore
		status       int
		body         string
		errorMessage string
		wantCalls    int
		wantLimit    int
	}{
		{
			name:      "stored activity",
			path:      "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store:     &testStore{entries: []store.TimelineEntry{entry}},
			status:    http.StatusOK,
			body:      `{"data":[{"event_id":"` + entry.EventID.String() + `","kind":"issue_opened","role":"author","occurred_at":"2026-01-02T03:04:05Z","project":"owner/repo","title":"A contribution","url":"https://github.com/owner/repo/issues/1"}],"page":{"has_more":false}}` + "\n",
			wantCalls: 1, wantLimit: 100,
		},
		{
			name:  "invalid person",
			path:  "/v1/persons/nope/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store: &testStore{}, status: http.StatusBadRequest,
			errorMessage: "invalid person ID",
		},
		{
			name:  "missing range",
			path:  "/v1/persons/" + personID.String() + "/activity",
			store: &testStore{}, status: http.StatusBadRequest,
			errorMessage: "from is required",
		},
		{
			name:  "invalid limit",
			path:  "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z&limit=501",
			store: &testStore{}, status: http.StatusBadRequest,
			errorMessage: "limit must be between 1 and 500",
		},
		{
			name:  "duplicate range parameter",
			path:  "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&from=2026-01-02T00:00:00Z&to=2026-02-01T00:00:00Z",
			store: &testStore{}, status: http.StatusBadRequest,
			errorMessage: "from must be provided once",
		},
		{
			name:  "store failure",
			path:  "/v1/persons/" + personID.String() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			store: &testStore{err: errors.New("database failed")}, status: http.StatusInternalServerError,
			errorMessage: "failed to read activity", wantCalls: 1, wantLimit: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(tt.store, testLogger(), testOptions()), tt.path)
			if res.Code != tt.status {
				t.Errorf("GET %s status = %d, want %d", tt.path, res.Code, tt.status)
			}
			if tt.errorMessage != "" {
				var response struct {
					Error struct {
						Code      string `json:"code"`
						Message   string `json:"message"`
						RequestID string `json:"request_id"`
					} `json:"error"`
				}
				if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if response.Error.Message != tt.errorMessage || response.Error.RequestID == "" {
					t.Errorf("GET %s error = %+v, want message %q and request ID", tt.path, response.Error, tt.errorMessage)
				}
			} else if got := res.Body.String(); got != tt.body {
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

func TestHandler_createPerson(t *testing.T) {
	s := &testStore{}
	req := httptest.NewRequest(http.MethodPost, "/v1/persons", strings.NewReader(
		`{"login":"octocat","display_name":"Octo Cat","emails":["octo@example.com"]}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /v1/persons status = %d, want %d: %s", res.Code, http.StatusCreated, res.Body.String())
	}
	if s.createCalls != 1 || res.Header().Get("Location") == "" {
		t.Errorf("POST /v1/persons calls/location = %d/%q, want one call and job Location", s.createCalls, res.Header().Get("Location"))
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("POST /v1/persons Content-Type = %q, want application/json", got)
	}
	if strings.Contains(res.Body.String(), "octo@example.com") || strings.Contains(res.Body.String(), `"octocat"`) {
		t.Errorf("POST /v1/persons exposed identity values: %s", res.Body.String())
	}

	invalid := &testStore{}
	badRequest := httptest.NewRequest(http.MethodPost, "/v1/persons", strings.NewReader(
		`{"login":"octocat","extra":"not allowed"}`))
	badRequest.Header.Set("Authorization", "Bearer "+testAPIKey)
	badRequest.Header.Set("Content-Type", "application/json")
	badResponse := httptest.NewRecorder()
	NewHandler(invalid, testLogger(), testOptions()).ServeHTTP(badResponse, badRequest)
	if badResponse.Code != http.StatusBadRequest || invalid.createCalls != 0 {
		t.Errorf("POST /v1/persons with unknown field = %d and %d store calls, want 400 and zero", badResponse.Code, invalid.createCalls)
	}
}

func TestHandler_createPersonRejectsInvalidBodies(t *testing.T) {
	for _, tt := range []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "trailing JSON", contentType: "application/json", body: `{"login":"octocat"} {}`, wantStatus: http.StatusBadRequest},
		{name: "wrong media type", contentType: "text/plain", body: `{"login":"octocat"}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "oversized", contentType: "application/json", body: strings.Repeat(" ", maxRequestBody+1), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "NUL display name", contentType: "application/json", body: `{"login":"octocat","display_name":"\u0000"}`, wantStatus: http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &testStore{}
			req := httptest.NewRequest(http.MethodPost, "/v1/persons", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", tt.contentType)
			res := httptest.NewRecorder()
			NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
			if res.Code != tt.wantStatus || s.createCalls != 0 {
				t.Errorf("POST /v1/persons status/calls = %d/%d, want %d/0", res.Code, s.createCalls, tt.wantStatus)
			}
		})
	}
}

func TestHandler_patchPersonMapsPendingResolutionToConflict(t *testing.T) {
	personID := uuid.New()
	req := httptest.NewRequest(http.MethodPatch, "/v1/persons/"+personID.String(),
		strings.NewReader(`{"remove":[{"kind":"login","value":"octocat"}]}`))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	s := &testStore{identifiersErr: store.ErrOnboardingResolutionPending}
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("PATCH /v1/persons status = %d, want %d: %s", res.Code, http.StatusConflict, res.Body.String())
	}
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode PATCH error response: %v", err)
	}
	if response.Error.Message != "account resolution is still pending" {
		t.Errorf("PATCH error message = %q, want safe pending-resolution explanation", response.Error.Message)
	}
}

func TestHandler_activityReturnsNotFoundForUnknownPerson(t *testing.T) {
	s := &testStore{apiPersonErr: store.ErrNotFound}
	path := "/v1/persons/" + uuid.NewString() + "/activity?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z"
	res := serve(t, NewHandler(s, testLogger(), testOptions()), path)
	if res.Code != http.StatusNotFound || s.calls != 0 {
		t.Errorf("GET %s status/store calls = %d/%d, want 404/0", path, res.Code, s.calls)
	}
}

func TestHandler_rejectsMalformedQueryEncoding(t *testing.T) {
	for _, query := range []string{"limit=%zz", "q=%00"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/persons", nil)
		req.URL.RawQuery = query
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		res := httptest.NewRecorder()
		NewHandler(&testStore{}, testLogger(), testOptions()).ServeHTTP(res, req)
		if res.Code != http.StatusBadRequest {
			t.Errorf("GET /v1/persons with query %q status = %d, want 400", query, res.Code)
		}
	}
}

func TestHandler_syncPersonRejectsChunkedBody(t *testing.T) {
	s := &testStore{}
	req := httptest.NewRequest(http.MethodPost, "/v1/persons/"+uuid.NewString()+"/sync", strings.NewReader("unexpected"))
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Idempotency-Key", "sync-request-1")
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || s.queuedKey != "" {
		t.Errorf("POST /sync with unknown-length body = %d and key %q, want 400 and no enqueue", res.Code, s.queuedKey)
	}
}

func TestHandler_syncPersonRequiresOneIdempotencyKey(t *testing.T) {
	s := &testStore{}
	req := httptest.NewRequest(http.MethodPost, "/v1/persons/"+uuid.NewString()+"/sync", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Add("Idempotency-Key", "first")
	req.Header.Add("Idempotency-Key", "second")
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest || s.queuedKey != "" {
		t.Errorf("POST /sync with duplicate idempotency keys = %d and %q, want 400 and no enqueue", res.Code, s.queuedKey)
	}
}

func TestHandler_syncPersonQueuesJob(t *testing.T) {
	s := &testStore{}
	personID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/v1/persons/"+personID.String()+"/sync", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Idempotency-Key", "sync-request-1")
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusAccepted || s.queuedKey != "sync-request-1" {
		t.Fatalf("POST /sync status/key = %d/%q, want 202/sync-request-1: %s", res.Code, s.queuedKey, res.Body.String())
	}
	if !strings.HasPrefix(res.Header().Get("Location"), "/v1/jobs/") {
		t.Errorf("POST /sync Location = %q, want job URL", res.Header().Get("Location"))
	}
	if got := res.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("POST /sync Content-Type = %q, want application/json", got)
	}
}

func TestHandler_syncPersonMapsUnavailableOnboardingTargetToConflict(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/persons/"+uuid.NewString()+"/sync", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Idempotency-Key", "sync-request-no-target")
	s := &testStore{enqueueErr: store.ErrOnboardingTargetUnavailable}
	res := httptest.NewRecorder()
	NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusConflict {
		t.Fatalf("POST /sync status = %d, want %d: %s", res.Code, http.StatusConflict, res.Body.String())
	}
	var response struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode POST /sync error response: %v", err)
	}
	if response.Error.Message != "account resolution target is unavailable" {
		t.Errorf("POST /sync error message = %q, want safe target-unavailable explanation", response.Error.Message)
	}
}

func TestDecodePageCursor_bindsAndAuthenticates(t *testing.T) {
	cursor := pageCursor{Kind: "persons", Filter: "octo", ID: uuid.New()}
	token, err := encodePageCursor(testAPIKey, cursor)
	if err != nil {
		t.Fatalf("encodePageCursor: %v", err)
	}
	decoded, err := decodePageCursor(testAPIKey, token, "persons", "octo")
	if err != nil || decoded.ID != cursor.ID {
		t.Fatalf("decodePageCursor = %+v, %v; want cursor ID %s", decoded, err, cursor.ID)
	}
	for _, test := range []struct {
		name   string
		secret string
		kind   string
		filter string
	}{
		{name: "wrong secret", secret: "another secret", kind: "persons", filter: "octo"},
		{name: "wrong filter", secret: testAPIKey, kind: "persons", filter: "other"},
		{name: "wrong endpoint", secret: testAPIKey, kind: "activity", filter: "octo"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodePageCursor(test.secret, token, test.kind, test.filter); err == nil {
				t.Error("decodePageCursor error = nil, want rejection")
			}
		})
	}
}

func TestHandler_removedSnapshotRoute(t *testing.T) {
	s := &testStore{}
	res := serve(t, NewHandler(s, testLogger(), testOptions()), "/github/users/octocat/repositories")
	if res.Code != http.StatusNotFound || s.calls != 0 {
		t.Errorf("legacy snapshot route status/calls = %d/%d, want 404/0", res.Code, s.calls)
	}
}

func TestHandler_methodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/v1/persons", nil)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	res := httptest.NewRecorder()
	NewHandler(&testStore{}, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /v1/persons status = %d, want %d: %s", res.Code, http.StatusMethodNotAllowed, res.Body.String())
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
			want: `{"data":[{"forge_instance_id":"` + forgeID.String() + `","source":"github_history","from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z","status":"complete","scope":"known public repositories","recorded_at":"2026-01-01T00:00:00Z"}]}` + "\n",
		},
		{
			name: "aggregate counts",
			path: path + "/counts?from=2026-01-01T00:00:00Z&to=2026-02-01T00:00:00Z",
			data: &testStore{counts: []store.ContributionCount{{
				ForgeInstanceID: forgeID, ActivityDate: from, Metric: "daily_total",
				Value: 7, ObservedAt: from,
			}}},
			want: `{"data":[{"forge_instance_id":"` + forgeID.String() + `","date":"2026-01-01T00:00:00Z","metric":"daily_total","value":7,"observed_at":"2026-01-01T00:00:00Z"}]}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(tt.data, testLogger(), testOptions()), tt.path)
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
	res := serve(t, NewHandler(&testStore{}, testLogger(), testOptions()), "/healthz")
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
			res := serve(t, NewHandler(&testStore{readyErr: tt.err}, testLogger(), testOptions()), "/readyz")
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
	res := serve(t, NewHandler(&testStore{readyErr: errors.New("down")}, testLogger(), testOptions()), "/healthz")
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

func TestHandler_apiAuthentication(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		origin        string
		method        string
		wantStatus    int
		wantAllow     string
	}{
		{name: "missing token", method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "invalid token", authorization: "Bearer invalid", method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authorization: "Basic " + testAPIKey, method: http.MethodGet, wantStatus: http.StatusUnauthorized},
		{name: "wrong origin", authorization: "Bearer " + testAPIKey, origin: "https://evil.example", method: http.MethodGet, wantStatus: http.StatusForbidden},
		{name: "allowed origin", authorization: "Bearer " + testAPIKey, origin: "https://frontend.example", method: http.MethodGet, wantStatus: http.StatusNotFound, wantAllow: "https://frontend.example"},
		{name: "allowed preflight", origin: "https://frontend.example", method: http.MethodOptions, wantStatus: http.StatusNoContent, wantAllow: "https://frontend.example"},
		{name: "disallowed preflight header", origin: "https://frontend.example", method: http.MethodOptions, wantStatus: http.StatusForbidden, wantAllow: "https://frontend.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/v1/unknown", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.name == "allowed preflight" {
				req.Header.Set("Access-Control-Request-Method", "GET")
				req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
			}
			if tt.name == "disallowed preflight header" {
				req.Header.Set("Access-Control-Request-Method", "GET")
				req.Header.Set("Access-Control-Request-Headers", "x-secret")
			}
			res := httptest.NewRecorder()
			NewHandler(&testStore{}, testLogger(), testOptions()).ServeHTTP(res, req)
			if res.Code != tt.wantStatus {
				t.Errorf("%s /v1/unknown status = %d, want %d", tt.method, res.Code, tt.wantStatus)
			}
			if got := res.Header().Get("Access-Control-Allow-Origin"); got != tt.wantAllow {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tt.wantAllow)
			}
			if tt.wantAllow != "" && res.Header().Get("Access-Control-Expose-Headers") != "Location, X-Request-ID" {
				t.Errorf("Access-Control-Expose-Headers = %q, want Location and request ID", res.Header().Get("Access-Control-Expose-Headers"))
			}
			if res.Header().Get("X-Request-ID") == "" {
				t.Error("response has no request ID")
			}
			if got := res.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store for private API routes", got)
			}
			if tt.wantStatus == http.StatusUnauthorized && res.Header().Get("WWW-Authenticate") == "" {
				t.Error("unauthorized response has no WWW-Authenticate header")
			}
		})
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/persons", nil)
	req.Header.Add("Authorization", "Bearer "+testAPIKey)
	req.Header.Add("Authorization", "Bearer "+testAPIKey)
	res := httptest.NewRecorder()
	NewHandler(&testStore{}, testLogger(), testOptions()).ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Errorf("GET /v1/persons with duplicate Authorization headers status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
}

func serve(t *testing.T, handler http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if strings.HasPrefix(path, "/v1") {
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHandler_createPersonSelectsForge(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantStatus   int
		wantProvider string
	}{
		{"default is GitHub", `{"login":"octocat"}`, http.StatusCreated, "github"},
		{"GitLab", `{"forge":"gitlab.com","login":"jane.doe"}`, http.StatusCreated, "gitlab"},
		{"Codeberg", `{"forge":"codeberg.org","login":"jane_doe-1"}`, http.StatusCreated, "forgejo"},
		{"unknown forge", `{"forge":"git.example.com","login":"jane"}`, http.StatusBadRequest, ""},
		{"login invalid for GitHub", `{"forge":"github.com","login":"jane.doe"}`, http.StatusBadRequest, ""},
		{"login invalid for GitLab", `{"forge":"gitlab.com","login":"-jane"}`, http.StatusBadRequest, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &testStore{}
			req := httptest.NewRequest(http.MethodPost, "/v1/persons", strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			NewHandler(s, testLogger(), testOptions()).ServeHTTP(res, req)
			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", res.Code, tt.wantStatus, res.Body.String())
			}
			if tt.wantProvider != "" && s.lastForge.Provider != tt.wantProvider {
				t.Errorf("forge provider = %q, want %q", s.lastForge.Provider, tt.wantProvider)
			}
			if tt.wantProvider == "" && s.createCalls != 0 {
				t.Errorf("store create calls = %d, want zero for rejected request", s.createCalls)
			}
		})
	}
}

func TestHandler_patchPersonValidatesLoginsForSelectedForge(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"GitLab handle with dot", `{"forge":"gitlab.com","add":[{"kind":"login","value":"jane.doe"}]}`, http.StatusOK},
		{"GitLab handle on remove", `{"forge":"gitlab.com","remove":[{"kind":"login","value":"jane.doe"}]}`, http.StatusOK},
		{"same handle on GitHub", `{"add":[{"kind":"login","value":"jane.doe"}]}`, http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPatch, "/v1/persons/"+uuid.NewString(), strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			req.Header.Set("Content-Type", "application/json")
			res := httptest.NewRecorder()
			NewHandler(&testStore{}, testLogger(), testOptions()).ServeHTTP(res, req)
			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", res.Code, tt.wantStatus, res.Body.String())
			}
		})
	}
}
