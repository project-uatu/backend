// Package collector coordinates upstream clients and persistent storage.
package collector

import (
	"context"
	"encoding/json"
	"fmt"

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

// repositoryList is the stored JSON form of SourceGitHubRepositories.
type repositoryList struct {
	Repositories []string `json:"repositories"`
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
	return []store.Source{store.SourceGitHubRepositories}
}

// Collect fetches one source for a user and saves the successful result.
// Nothing is saved when the fetch fails.
func (c *Collector) Collect(ctx context.Context, source store.Source, user string) error {
	var data []byte
	switch source {
	case store.SourceGitHubRepositories:
		names, err := c.github.Repositories(ctx, user)
		if err != nil {
			return fmt.Errorf("collect GitHub repositories for %s: %w", user, err)
		}
		if names == nil {
			names = []string{}
		}
		data, err = json.Marshal(repositoryList{Repositories: names})
		if err != nil {
			return fmt.Errorf("encode GitHub repositories for %s: %w", user, err)
		}
	default:
		return fmt.Errorf("unsupported source %q", source)
	}
	if err := c.store.Save(ctx, source, user, data); err != nil {
		return fmt.Errorf("save %s result for %s: %w", source, user, err)
	}
	return nil
}
