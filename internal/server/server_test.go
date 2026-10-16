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

	"github.com/project-uatu/backend/internal/store"
)

type testStore struct {
	snapshot store.Snapshot
	found    bool
	err      error
	calls    int
}

func (s *testStore) Get(_ context.Context, _ store.Source, _ string) (store.Snapshot, bool, error) {
	s.calls++
	return s.snapshot, s.found, s.err
}

func TestHandler_repositories(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	stored := store.Snapshot{
		Source: store.SourceGitHubRepositories, User: "octocat",
		Data: []byte(`{"repositories":["repo-one"]}`), FetchedAt: fetchedAt,
	}
	tests := []struct {
		name         string
		path         string
		store        *testStore
		wantStatus   int
		wantBody     string
		wantModified string
		wantGets     int
	}{
		{
			name:         "stored",
			path:         "/github/users/octocat/repositories",
			store:        &testStore{snapshot: stored, found: true},
			wantStatus:   http.StatusOK,
			wantBody:     `{"repositories":["repo-one"]}`,
			wantModified: fetchedAt.Format(http.TimeFormat),
			wantGets:     1,
		},
		{
			name:       "missing",
			path:       "/github/users/octocat/repositories",
			store:      &testStore{},
			wantStatus: http.StatusNotFound,
			wantBody:   "{\"error\":\"no stored data for user\"}\n",
			wantGets:   1,
		},
		{
			name:       "invalid login",
			path:       "/github/users/-octocat/repositories",
			store:      &testStore{},
			wantStatus: http.StatusBadRequest,
			wantBody:   "{\"error\":\"invalid GitHub login\"}\n",
		},
		{
			name:       "store failure",
			path:       "/github/users/octocat/repositories",
			store:      &testStore{err: errors.New("database failed")},
			wantStatus: http.StatusInternalServerError,
			wantBody:   "{\"error\":\"failed to read stored data\"}\n",
			wantGets:   1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := serve(t, NewHandler(tt.store, testLogger()), tt.path)

			if res.Code != tt.wantStatus {
				t.Errorf("GET %s status = %d, want %d", tt.path, res.Code, tt.wantStatus)
			}
			if got := res.Body.String(); got != tt.wantBody {
				t.Errorf("GET %s body = %q, want %q", tt.path, got, tt.wantBody)
			}
			if got := res.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("GET %s Content-Type = %q, want application/json", tt.path, got)
			}
			if got := res.Header().Get("Last-Modified"); got != tt.wantModified {
				t.Errorf("GET %s Last-Modified = %q, want %q", tt.path, got, tt.wantModified)
			}
			if tt.store.calls != tt.wantGets {
				t.Errorf("GET %s store reads = %d, want %d", tt.path, tt.store.calls, tt.wantGets)
			}
		})
	}
}

func TestHandler_removedRoutes(t *testing.T) {
	for _, path := range []string{"/", "/gh/users/octocat/contributions"} {
		t.Run(path, func(t *testing.T) {
			s := &testStore{found: true}
			res := serve(t, NewHandler(s, testLogger()), path)
			if res.Code != http.StatusNotFound {
				t.Errorf("GET %s status = %d, want %d", path, res.Code, http.StatusNotFound)
			}
			if s.calls != 0 {
				t.Errorf("GET %s store reads = %d, want 0", path, s.calls)
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
