package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/project-uatu/backend/internal/devstats"
	"github.com/project-uatu/backend/internal/store"
)

type memoryStore struct {
	source store.Source
	user   string
	data   []byte
	err    error
}

func (s *memoryStore) Save(_ context.Context, source store.Source, user string, data []byte) error {
	s.source, s.user, s.data = source, user, data
	return s.err
}

type repositoryStub struct {
	names []string
	err   error
}

func (r repositoryStub) Repositories(context.Context, string) ([]string, error) {
	return r.names, r.err
}

type devstatsStub struct {
	result devstats.Contribution
	err    error
}

func (d devstatsStub) Contributions(context.Context, string) (devstats.Contribution, error) {
	return d.result, d.err
}

func TestCollector_Collect_persistsSuccessfulData(t *testing.T) {
	s := &memoryStore{}
	c := New(s, repositoryStub{names: []string{"first", "second"}}, devstatsStub{})

	data, err := c.Collect(t.Context(), store.SourceGitHubRepos, "octocat")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := string(data), "first, second"; got != want {
		t.Errorf("Collect data = %q, want %q", got, want)
	}
	if s.source != store.SourceGitHubRepos || s.user != "octocat" || string(s.data) != string(data) {
		t.Errorf("saved snapshot = %s/%s %q, want GitHub/octocat %q", s.source, s.user, s.data, data)
	}
}

func TestCollector_Collect_doesNotSaveUpstreamFailure(t *testing.T) {
	s := &memoryStore{}
	c := New(s, repositoryStub{err: errors.New("offline")}, devstatsStub{})

	_, err := c.Collect(t.Context(), store.SourceGitHubRepos, "octocat")
	var upstreamErr *UpstreamError
	if !errors.As(err, &upstreamErr) {
		t.Fatalf("Collect error = %v, want UpstreamError", err)
	}
	if s.source != "" || s.data != nil {
		t.Errorf("saved data after upstream failure = %s/%s %q, want none", s.source, s.user, s.data)
	}
}

func TestCollector_Collect_persistsDevStatsAsJSON(t *testing.T) {
	s := &memoryStore{}
	c := New(s, repositoryStub{}, devstatsStub{
		result: devstats.Contribution{Contributions: 2, Issues: 1, PRs: 0},
	})

	data, err := c.Collect(t.Context(), store.SourceDevStats, "octocat")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, want := string(data), `{"contributions":2,"issues":1,"prs":0}`; got != want {
		t.Errorf("Collect data = %q, want %q", got, want)
	}
}
