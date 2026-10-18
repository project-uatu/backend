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
	// SourceForgejoHistory identifies person-centric Forgejo activity collection.
	SourceForgejoHistory = "forgejo_history"
)

// ErrUnknownForge reports a forge host that is not enabled for collection.
var ErrUnknownForge = errors.New("forge is not supported")

// SourceProfile describes one enabled forge and its person-history source.
type SourceProfile struct {
	Key      string
	Provider string
	Host     string
	BaseURL  string
	// RequireNodeID and RequireCreatedAt state which optional account
	// metadata the forge always provides.
	RequireNodeID    bool
	RequireCreatedAt bool
}

var sourceProfiles = []SourceProfile{
	{Key: SourceGitHubHistory, Provider: "github", Host: "github.com",
		BaseURL: "https://github.com", RequireNodeID: true, RequireCreatedAt: true},
	{Key: SourceGitLabHistory, Provider: "gitlab", Host: "gitlab.com",
		BaseURL: "https://gitlab.com"},
	{Key: SourceForgejoHistory, Provider: "forgejo", Host: "codeberg.org",
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

// GitHubForge returns the canonical GitHub.com forge instance.
func (s *Store) GitHubForge(ctx context.Context) (ForgeInstance, error) {
	forge, _, err := s.ForgeForHost(ctx, "github.com")
	return forge, err
}
