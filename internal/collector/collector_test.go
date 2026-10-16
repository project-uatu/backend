package collector

import (
	"context"
	"errors"
	"testing"

	"github.com/project-uatu/backend/internal/store"
)

type memoryStore struct {
	source store.Source
	user   string
	data   []byte
	saves  int
	err    error
}

func (s *memoryStore) Save(_ context.Context, source store.Source, user string, data []byte) error {
	s.saves++
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

func TestCollector_Collect_persistsRepositoriesAsJSON(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "repositories", names: []string{"first", "second"}, want: `{"repositories":["first","second"]}`},
		{name: "none", names: nil, want: `{"repositories":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &memoryStore{}
			c := New(s, repositoryStub{names: tt.names})

			if err := c.Collect(t.Context(), store.SourceGitHubRepositories, "octocat"); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if s.source != store.SourceGitHubRepositories || s.user != "octocat" {
				t.Errorf("saved key = %s/%s, want %s/octocat", s.source, s.user, store.SourceGitHubRepositories)
			}
			if got := string(s.data); got != tt.want {
				t.Errorf("saved data = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCollector_Collect_doesNotSaveUpstreamFailure(t *testing.T) {
	s := &memoryStore{}
	upstreamErr := errors.New("offline")
	c := New(s, repositoryStub{err: upstreamErr})

	err := c.Collect(t.Context(), store.SourceGitHubRepositories, "octocat")
	if !errors.Is(err, upstreamErr) {
		t.Fatalf("Collect error = %v, want %v", err, upstreamErr)
	}
	if s.saves != 0 {
		t.Errorf("saves after upstream failure = %d, want 0", s.saves)
	}
}

func TestCollector_Collect_rejectsUnknownSource(t *testing.T) {
	s := &memoryStore{}
	c := New(s, repositoryStub{})

	if err := c.Collect(t.Context(), store.Source("unknown"), "octocat"); err == nil {
		t.Fatal("Collect(unknown source) error = nil, want error")
	}
	if s.saves != 0 {
		t.Errorf("saves for unknown source = %d, want 0", s.saves)
	}
}

func TestCollector_Collect_returnsSaveFailure(t *testing.T) {
	saveErr := errors.New("disk full")
	c := New(&memoryStore{err: saveErr}, repositoryStub{names: []string{"first"}})

	if err := c.Collect(t.Context(), store.SourceGitHubRepositories, "octocat"); !errors.Is(err, saveErr) {
		t.Errorf("Collect error = %v, want %v", err, saveErr)
	}
}
