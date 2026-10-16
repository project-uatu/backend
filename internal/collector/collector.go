// Package collector coordinates upstream clients and persistent storage.
package collector

import (
	"context"
	"fmt"
	"strings"

	"github.com/project-uatu/backend/internal/store"
)

// snapshotStore is the persistence needed by Collector.
type snapshotStore interface {
	Save(context.Context, store.Source, string, []byte) error
}

// repositoryClient retrieves GitHub repository names.
type repositoryClient interface {
	Repositories(context.Context, string) ([]string, error)
}

// Collector collects source data and persists successful results.
type Collector struct {
	store  snapshotStore
	github repositoryClient
}

// UpstreamError identifies a failure while querying an external data source.
type UpstreamError struct {
	Err error
}

// Error returns the underlying upstream error.
func (e *UpstreamError) Error() string {
	if e == nil || e.Err == nil {
		return "upstream request failed"
	}
	return e.Err.Error()
}

// Unwrap returns the underlying upstream error.
func (e *UpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// New creates a collector with its required dependencies.
func New(s snapshotStore, githubClient repositoryClient) *Collector {
	return &Collector{
		store:  s,
		github: githubClient,
	}
}

// Sources returns the data sources currently supported by Uatu.
func Sources() []store.Source {
	return []store.Source{store.SourceGitHubRepos}
}

// Collect fetches one source for a user and saves the successful result.
func (c *Collector) Collect(ctx context.Context, source store.Source, user string) ([]byte, error) {
	var data []byte
	switch source {
	case store.SourceGitHubRepos:
		names, err := c.github.Repositories(ctx, user)
		if err != nil {
			return nil, &UpstreamError{Err: fmt.Errorf("collect GitHub repositories for %s: %w", user, err)}
		}
		data = []byte(strings.Join(names, ", "))
	default:
		return nil, fmt.Errorf("unsupported source %q", source)
	}
	if err := c.store.Save(ctx, source, user, data); err != nil {
		return nil, fmt.Errorf("save %s result for %s: %w", source, user, err)
	}
	return data, nil
}
