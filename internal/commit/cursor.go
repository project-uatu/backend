// SPDX-License-Identifier: Apache-2.0

package commit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
)

const (
	// DefaultMaxRepositories is the default per-person, per-forge seed limit.
	DefaultMaxRepositories = 50
	// DefaultMaxPages is the default page limit for each repository.
	DefaultMaxPages = 20
	// PageSize is the maximum number of commits requested in one page.
	PageSize = 100
)

// CoverageScope describes the repository and page limits applied to a scan.
func CoverageScope(provider string, maxRepositories, maxPages int) string {
	scope := fmt.Sprintf(
		"authored non-merge commits on the default branch of up to %d known public non-fork repositories, at most %d pages of %d commits each",
		maxRepositories, maxPages, PageSize,
	)
	if provider == "github" {
		scope += "; author-filtered results do not discover co-author-only matches outside the returned commits"
	}
	return scope
}

// Cursor stores resumable position without repository paths or email filters.
type Cursor struct {
	Repository    int    `json:"repository"`
	Page          int    `json:"page"`
	Pages         int    `json:"pages"`
	AuthorFilter  int    `json:"author_filter,omitempty"`
	RepositoryID  int64  `json:"repository_id,omitempty"`
	Verified      bool   `json:"verified,omitempty"`
	RepositorySet string `json:"repository_set,omitempty"`
}

// RepositorySetHash identifies the selected repository list without storing paths in the cursor.
func RepositorySetHash(repositories []string) string {
	repositories = slices.Clone(repositories)
	slices.Sort(repositories)
	repositories = slices.Compact(repositories)
	var value strings.Builder
	for _, repository := range repositories {
		fmt.Fprintf(&value, "%d:%s", len(repository), repository)
	}
	sum := sha256.Sum256([]byte(value.String()))
	return hex.EncodeToString(sum[:])
}

// DecodeCursor returns a validated cursor, initializing an empty cursor at the beginning.
func DecodeCursor(value string) (Cursor, error) {
	if value == "" {
		return Cursor{Page: 1}, nil
	}
	var cursor Cursor
	if err := json.Unmarshal([]byte(value), &cursor); err != nil {
		return Cursor{}, errors.New("invalid commit cursor")
	}
	if cursor.Repository < 0 || cursor.Page < 1 || cursor.Pages < 0 ||
		cursor.AuthorFilter < 0 || cursor.RepositoryID < 0 {
		return Cursor{}, errors.New("invalid commit cursor position")
	}
	if cursor.RepositorySet != "" {
		value, err := hex.DecodeString(cursor.RepositorySet)
		if err != nil || len(value) != sha256.Size {
			return Cursor{}, errors.New("invalid commit cursor repository set")
		}
	}
	return cursor, nil
}

// Encode serializes the cursor without provider identities.
func (c Cursor) Encode() (string, error) {
	value, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode commit cursor: %w", err)
	}
	return string(value), nil
}

// SelectRepositories returns the stable prefix within the configured cap.
func SelectRepositories(repositories []string, maximum int) ([]string, bool, error) {
	if maximum < 1 {
		return nil, false, errors.New("maximum commit repositories must be positive")
	}
	selected := slices.Clone(repositories)
	slices.Sort(selected)
	selected = slices.Compact(selected)
	if len(selected) <= maximum {
		return selected, false, nil
	}
	return selected[:maximum], true, nil
}

// CoverageGap creates a bounded gap for a source limit or unavailable interval.
func CoverageGap(request event.PageRequest, floor time.Time, reason string, boundary bool) (event.Gap, error) {
	from, to := request.Window.From.UTC(), request.Window.To.UTC()
	if from.IsZero() {
		from = floor.UTC()
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	if reason == "" || from.IsZero() || !from.Before(to) {
		return event.Gap{}, errors.New("commit coverage gap requires a valid range and reason")
	}
	return event.Gap{From: from, To: to, Reason: reason, Boundary: boundary}, nil
}
