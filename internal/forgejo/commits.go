// SPDX-License-Identifier: Apache-2.0

package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/commit"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/forgehttp"
)

// CommitSource is the durable source key for Forgejo commit events.
const CommitSource event.Source = "forgejo_commits"

// CommitConnector collects bounded commits from known public Codeberg repositories.
type CommitConnector struct {
	client   *forgehttp.Client
	maxRepos int
	maxPages int
}

// NewCommitConnector creates a Codeberg commit connector with optional scan limits.
func NewCommitConnector(options Options, maxRepos, maxPages int) (*CommitConnector, error) {
	if options.APIURL == "" {
		options.APIURL = DefaultAPIURL
	}
	if maxRepos == 0 {
		maxRepos = commit.DefaultMaxRepositories
	}
	if maxPages == 0 {
		maxPages = commit.DefaultMaxPages
	}
	if maxRepos < 1 || maxPages < 1 {
		return nil, errors.New("codeberg commit limits must be positive")
	}
	return &CommitConnector{
		client: forgehttp.New(forgehttp.Options{
			BaseURL: options.APIURL, Token: options.Token, HTTPClient: options.HTTPClient,
		}),
		maxRepos: maxRepos, maxPages: maxPages,
	}, nil
}

// Source identifies the connector's durable commit stream.
func (c *CommitConnector) Source() event.Source { return CommitSource }

// QuotaBucket names the Codeberg API quota.
func (c *CommitConnector) QuotaBucket() string { return quotaBucket }

// FetchPage fetches one bounded page of commits for a known public repository.
func (c *CommitConnector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	if request.Source != CommitSource {
		return event.Page{}, event.FetchReport{}, errors.New("codeberg commit request has an unexpected source")
	}
	cursor, err := commit.DecodeCursor(request.Cursor)
	if err != nil {
		return event.Page{}, event.FetchReport{}, err
	}
	repositories, capped, err := commit.SelectRepositories(request.KnownRepositories, c.maxRepos)
	if err != nil {
		return event.Page{}, event.FetchReport{}, err
	}
	repositorySet := commit.RepositorySetHash(request.KnownRepositories)
	repositorySetChanged := cursor.RepositorySet != "" && cursor.RepositorySet != repositorySet
	if repositorySetChanged {
		cursor = commit.Cursor{Page: 1, RepositorySet: repositorySet}
	} else {
		cursor.RepositorySet = repositorySet
	}
	report := event.FetchReport{
		RateLimitBucket: quotaBucket, CoverageScope: commit.CoverageScope("forgejo", c.maxRepos, c.maxPages),
	}
	if (request.Cursor == "" || repositorySetChanged) && capped {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("commit repository limit reached; scanning the first %d repositories in stable path order", c.maxRepos), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
	}
	if len(repositories) == 0 {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			"no known public repositories are available for commit scanning", true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		return event.Page{Done: true}, report, nil
	}
	if cursor.Repository >= len(repositories) {
		return event.Page{Done: true}, report, nil
	}
	repository := repositories[cursor.Repository]
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return event.Page{}, report, fmt.Errorf("invalid known Codeberg repository %q", repository)
	}
	if !cursor.Verified {
		done, next, err := c.verifyRepository(ctx, request, repository, owner, name, len(repositories), &cursor, &report)
		if err != nil {
			return event.Page{}, report, err
		}
		if done {
			return event.Page{Done: true}, report, nil
		}
		return event.Page{NextCursor: next}, report, nil
	}
	query := url.Values{
		"page":  {strconv.Itoa(cursor.Page)},
		"limit": {strconv.Itoa(commit.PageSize)},
	}
	if !request.Window.From.IsZero() {
		query.Set("since", request.Window.From.UTC().Format(time.RFC3339))
	}
	if !request.Window.To.IsZero() {
		query.Set("until", request.Window.To.UTC().Format(time.RFC3339))
	}
	path := fmt.Sprintf("/repos/%s/%s/commits", url.PathEscape(owner), url.PathEscape(name))
	response, upstream, err := c.client.Get(ctx, path, query, quotaBucket)
	mergeReport(&report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusConflict ||
			statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		if err := appendForgejoCommitGap(request, repository, &report); err != nil {
			return event.Page{}, report, err
		}
		advanceForgejoCommitRepository(&cursor)
		return finishForgejoCommitPage(nil, cursor, len(repositories), report)
	}
	if err != nil {
		return event.Page{}, report, fmt.Errorf("list Codeberg commits: %w", err)
	}
	var records []forgejoCommit
	if err := json.Unmarshal(response.Body, &records); err != nil {
		return event.Page{}, report, fmt.Errorf("decode Codeberg commits: %w", err)
	}
	events := make([]event.RawEvent, 0, len(records))
	for _, record := range records {
		raw, include, err := buildForgejoCommit(request, repository, cursor.RepositoryID, record)
		if err != nil {
			return event.Page{Events: events}, report, err
		}
		if include {
			events = append(events, raw)
		}
	}
	cursor.Pages++
	hasNext := strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Hasmore")), "true")
	if cursor.Pages >= c.maxPages && hasNext {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("commit page limit reached for %s", repository), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		advanceForgejoCommitRepository(&cursor)
	} else if hasNext {
		cursor.Page++
	} else {
		advanceForgejoCommitRepository(&cursor)
	}
	return finishForgejoCommitPage(events, cursor, len(repositories), report)
}

type forgejoCommit struct {
	SHA    string `json:"sha"`
	URL    string `json:"html_url"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		ID int64 `json:"id"`
	} `json:"author"`
	Parents []json.RawMessage `json:"parents"`
}

func (c *CommitConnector) verifyRepository(
	ctx context.Context, request event.PageRequest, repository, owner, name string,
	repositoryCount int, cursor *commit.Cursor, report *event.FetchReport,
) (bool, string, error) {
	path := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	response, upstream, err := c.client.Get(ctx, path, nil, quotaBucket)
	mergeReport(report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusNotFound ||
			statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		if err := appendForgejoCommitGap(request, repository, report); err != nil {
			return false, "", err
		}
		advanceForgejoCommitRepository(cursor)
		next, err := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, err
	}
	if err != nil {
		return false, "", fmt.Errorf("verify Codeberg repository: %w", err)
	}
	var repo struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Fork     bool   `json:"fork"`
	}
	if err := json.Unmarshal(response.Body, &repo); err != nil {
		return false, "", fmt.Errorf("decode Codeberg repository: %w", err)
	}
	if repo.ID <= 0 || repo.Private || repo.Fork || repo.FullName != repository {
		if err := appendForgejoCommitGap(request, repository, report); err != nil {
			return false, "", err
		}
		advanceForgejoCommitRepository(cursor)
		next, err := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, err
	}
	cursor.RepositoryID = repo.ID
	cursor.Verified = true
	next, err := cursor.Encode()
	return false, next, err
}

func buildForgejoCommit(request event.PageRequest, repository string, repositoryID int64, record forgejoCommit) (event.RawEvent, bool, error) {
	if len(record.Parents) > 1 {
		return event.RawEvent{}, false, nil
	}
	if record.SHA == "" || repositoryID <= 0 || record.Commit.Author.Date.IsZero() ||
		record.URL == "" || len(record.Parents) == 0 {
		return event.RawEvent{}, false, errors.New("codeberg returned an incomplete commit record")
	}
	accountID := ""
	if record.Author != nil {
		if record.Author.ID <= 0 {
			return event.RawEvent{}, false, errors.New("codeberg returned an invalid linked commit author")
		}
		accountID = strconv.FormatInt(record.Author.ID, 10)
	}
	attribution := commit.ResolveAttribution(request, "forgejo", accountID,
		record.Commit.Author.Email, record.Commit.Message)
	if len(attribution.Actors) == 0 {
		return event.RawEvent{}, false, nil
	}
	authoredAt := record.Commit.Author.Date.UTC()
	payload, err := json.Marshal(commit.Payload{
		SHA: record.SHA, Repository: repository, URL: record.URL, AuthoredAt: authoredAt,
		AttributionBasis: attribution.Basis,
	})
	if err != nil {
		return event.RawEvent{}, false, fmt.Errorf("encode Codeberg commit payload: %w", err)
	}
	return event.RawEvent{
		ForgeInstanceID: request.ForgeInstanceID, Source: CommitSource,
		ExternalID: fmt.Sprintf("commit:%d:%s", repositoryID, record.SHA),
		Kind:       commit.Kind, OccurredAt: &authoredAt, Payload: payload,
		PayloadSchemaVersion: 1, Actors: attribution.Actors,
	}, true, nil
}

func appendForgejoCommitGap(request event.PageRequest, repository string, report *event.FetchReport) error {
	gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
		fmt.Sprintf("repository is unavailable, not public, or a fork: %s", repository), true)
	if err != nil {
		return err
	}
	report.Gaps = append(report.Gaps, gap)
	return nil
}

func advanceForgejoCommitRepository(cursor *commit.Cursor) {
	cursor.Repository++
	cursor.Page = 1
	cursor.Pages = 0
	cursor.RepositoryID = 0
	cursor.Verified = false
}

func finishForgejoCommitPage(
	events []event.RawEvent, cursor commit.Cursor, repositoryCount int, report event.FetchReport,
) (event.Page, event.FetchReport, error) {
	if cursor.Repository >= repositoryCount {
		return event.Page{Events: events, Done: true}, report, nil
	}
	next, err := cursor.Encode()
	if err != nil {
		return event.Page{}, report, err
	}
	return event.Page{Events: events, NextCursor: next}, report, nil
}
