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

	"github.com/project-uatu/backend/internal/collector"
	"github.com/project-uatu/backend/internal/store"
)

type testStore struct {
	snapshot store.Snapshot
	found    bool
	err      error
}

func (s *testStore) Get(_ context.Context, _ store.Source, _ string) (store.Snapshot, bool, error) {
	return s.snapshot, s.found, s.err
}

type testCollector struct {
	data      []byte
	err       error
	calls     int
	onCollect func([]byte)
}

func (c *testCollector) Collect(_ context.Context, _ store.Source, _ string) ([]byte, error) {
	c.calls++
	if c.err == nil && c.onCollect != nil {
		c.onCollect(c.data)
	}
	return c.data, c.err
}

func TestHandler_servesStoredSnapshot(t *testing.T) {
	fetchedAt := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	s := &testStore{snapshot: store.Snapshot{
		Source: store.SourceDevStats, User: "octocat",
		Data: []byte(`{"contributions":2}`), FetchedAt: fetchedAt,
	}, found: true}
	c := &testCollector{}
	handler := NewHandler(s, c, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/devscores/octocat", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Body.String(); got != `{"contributions":2}` {
		t.Errorf("body = %q, want stored JSON", got)
	}
	if got := res.Header().Get("Last-Modified"); got != fetchedAt.Format(http.TimeFormat) {
		t.Errorf("Last-Modified = %q, want %q", got, fetchedAt.Format(http.TimeFormat))
	}
	if c.calls != 0 {
		t.Errorf("collector calls = %d, want 0", c.calls)
	}
}

func TestHandler_collectsWhenSnapshotMissing(t *testing.T) {
	s := &testStore{snapshot: store.Snapshot{
		Source: store.SourceGitHubRepos, User: "octocat",
		Data: []byte("repo-one, repo-two"),
	}}
	c := &testCollector{data: []byte("repo-one, repo-two")}
	c.onCollect = func([]byte) { s.found = true }
	handler := NewHandler(s, c, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/gh/users/octocat/contributions", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", res.Code, http.StatusOK)
	}
	if got := res.Body.String(); got != "repo-one, repo-two" {
		t.Errorf("body = %q, want collected repositories", got)
	}
	if c.calls != 1 {
		t.Errorf("collector calls = %d, want 1", c.calls)
	}
}

func TestHandler_returnsBadGatewayForUpstreamFailure(t *testing.T) {
	s := &testStore{}
	c := &testCollector{err: &collector.UpstreamError{Err: errors.New("upstream down")}}
	handler := NewHandler(s, c, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/devscores/octocat", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", res.Code, http.StatusBadGateway)
	}
	if got := res.Body.String(); got != "{\"error\":\"failed to fetch upstream data\"}\n" {
		t.Errorf("body = %q, want generic JSON error", got)
	}
}

func TestHandler_returnsServerErrorForStoreFailure(t *testing.T) {
	s := &testStore{err: errors.New("database failed")}
	handler := NewHandler(s, &testCollector{}, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/devscores/octocat", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)

	if res.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", res.Code, http.StatusInternalServerError)
	}
}

func TestHandler_healthz(t *testing.T) {
	handler := NewHandler(&testStore{}, &testCollector{}, testLogger())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
