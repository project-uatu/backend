// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// SourceGitLabHistory identifies person-centric GitLab activity collection.
	SourceGitLabHistory = "gitlab_history"
	// SourceGitLabCommits identifies GitLab commit collection.
	SourceGitLabCommits = "gitlab_commits"
	// SourceForgejoHistory identifies person-centric Forgejo activity collection.
	SourceForgejoHistory = "forgejo_history"
	// SourceForgejoCommits identifies Forgejo commit collection.
	SourceForgejoCommits = "forgejo_commits"
)

// ErrUnknownForge reports a forge host that is not enabled for collection.
var ErrUnknownForge = errors.New("forge is not supported")

// SourceProfile describes one enabled forge and its person-history source.
type SourceProfile struct {
	Key       string
	CommitKey string
	Provider  string
	Host      string
	BaseURL   string
	// RequireNodeID and RequireCreatedAt state which optional account
	// metadata the forge always provides.
	RequireNodeID    bool
	RequireCreatedAt bool
}

var sourceProfiles = []SourceProfile{
	{Key: SourceGitHubHistory, CommitKey: SourceGitHubCommits, Provider: "github", Host: "github.com",
		BaseURL: "https://github.com", RequireNodeID: true, RequireCreatedAt: true},
	{Key: SourceGitLabHistory, CommitKey: SourceGitLabCommits, Provider: "gitlab", Host: "gitlab.com",
		BaseURL: "https://gitlab.com"},
	{Key: SourceForgejoHistory, CommitKey: SourceForgejoCommits, Provider: "forgejo", Host: "codeberg.org",
		BaseURL: "https://codeberg.org"},
}

// SourceProfiles returns the enabled forge profiles.
func SourceProfiles() []SourceProfile {
	return append([]SourceProfile(nil), sourceProfiles...)
}

// ProfileForHost returns the profile for an enabled forge host.
func ProfileForHost(host string) (SourceProfile, bool) {
	for _, profile := range sourceProfiles {
		if profile.Host == host {
			return profile, true
		}
	}
	return SourceProfile{}, false
}

// ProfileForProvider returns the profile for a forge provider.
func ProfileForProvider(provider string) (SourceProfile, bool) {
	for _, profile := range sourceProfiles {
		if profile.Provider == provider {
			return profile, true
		}
	}
	return SourceProfile{}, false
}

// ProfileForCommitSource returns the forge profile for a commit source key.
func ProfileForCommitSource(source string) (SourceProfile, bool) {
	for _, profile := range sourceProfiles {
		if profile.CommitKey == source {
			return profile, true
		}
	}
	return SourceProfile{}, false
}

func accountSourceKeys() []string {
	keys := make([]string, 0, len(sourceProfiles))
	for _, profile := range sourceProfiles {
		keys = append(keys, profile.Key)
	}
	return keys
}

// ForgeForHost returns the forge instance and profile for an enabled host.
func (s *Store) ForgeForHost(ctx context.Context, host string) (ForgeInstance, SourceProfile, error) {
	profile, ok := ProfileForHost(host)
	if !ok {
		return ForgeInstance{}, SourceProfile{}, ErrUnknownForge
	}
	forge, err := s.CreateForgeInstance(ctx, profile.Provider, profile.BaseURL)
	if err != nil {
		return ForgeInstance{}, SourceProfile{}, fmt.Errorf("load %s forge: %w", profile.Host, err)
	}
	return forge, profile, nil
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func profileForForge(ctx context.Context, q rowQuerier, forgeID uuid.UUID) (SourceProfile, error) {
	var provider string
	err := q.QueryRow(ctx, "SELECT provider FROM forge_instances WHERE id = $1", forgeID).Scan(&provider)
	if errors.Is(err, pgx.ErrNoRows) {
		return SourceProfile{}, ErrUnknownForge
	}
	if err != nil {
		return SourceProfile{}, fmt.Errorf("load forge provider: %w", err)
	}
	profile, ok := ProfileForProvider(provider)
	if !ok {
		return SourceProfile{}, ErrUnknownForge
	}
	return profile, nil
}

func isAccountSource(key string) bool {
	for _, profile := range sourceProfiles {
		if profile.Key == key {
			return true
		}
	}
	return false
}

func isCommitSource(key string) bool {
	_, ok := ProfileForCommitSource(key)
	return ok
}

// GitHubForge returns the canonical GitHub.com forge instance.
func (s *Store) GitHubForge(ctx context.Context) (ForgeInstance, error) {
	forge, _, err := s.ForgeForHost(ctx, "github.com")
	return forge, err
}
