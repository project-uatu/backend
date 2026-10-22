// SPDX-License-Identifier: Apache-2.0

package github

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

// CommitSource is the durable source key for GitHub commit events.
const CommitSource event.Source = "github_commits"

const commitBucket = "github/core"

// CommitConnector collects bounded commits from known public GitHub repositories.
type CommitConnector struct {
	client   *Client
	maxRepos int
	maxPages int
}

// NewCommitConnector creates a GitHub commit connector with optional scan limits.
func NewCommitConnector(client *Client, maxRepos, maxPages int) (*CommitConnector, error) {
	if client == nil {
		return nil, errors.New("GitHub commit connector requires a client")
	}
	if maxRepos == 0 {
		maxRepos = commit.DefaultMaxRepositories
	}
	if maxPages == 0 {
		maxPages = commit.DefaultMaxPages
	}
	if maxRepos < 1 || maxPages < 1 {
		return nil, errors.New("GitHub commit limits must be positive")
	}
	return &CommitConnector{client: client, maxRepos: maxRepos, maxPages: maxPages}, nil
}

// Source identifies the connector's durable commit stream.
func (c *CommitConnector) Source() event.Source { return CommitSource }

// QuotaBucket names the GitHub REST core quota.
func (c *CommitConnector) QuotaBucket() string { return commitBucket }

// FetchPage fetches one bounded page of commits for a known public repository.
func (c *CommitConnector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	if request.Source != CommitSource {
		return event.Page{}, event.FetchReport{}, errors.New("GitHub commit request has an unexpected source")
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
		RateLimitBucket: commitBucket, CoverageScope: commit.CoverageScope("github", c.maxRepos, c.maxPages),
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
		return event.Page{}, report, fmt.Errorf("invalid known GitHub repository %q", repository)
	}
	if !cursor.Verified {
		done, next, verifyErr := c.verifyRepository(
			ctx, request, repository, owner, name, len(repositories), &cursor, &report,
		)
		if verifyErr != nil {
			return event.Page{}, report, verifyErr
		}
		if done {
			return event.Page{Done: true}, report, nil
		}
		return event.Page{NextCursor: next}, report, nil
	}
	filters := commit.GitHubAuthorFilters(request)
	if cursor.AuthorFilter >= len(filters) {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("no verified author filter is available for %s", repository), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		advanceRepository(&cursor)
		page, err := commitPage(nil, cursor, len(repositories))
		return page, report, err
	}
	values := url.Values{
		"per_page": {"100"},
		"page":     {strconv.Itoa(cursor.Page)},
		"author":   {filters[cursor.AuthorFilter]},
	}
	if !request.Window.From.IsZero() {
		values.Set("since", request.Window.From.UTC().Format(time.RFC3339))
	}
	if !request.Window.To.IsZero() {
		values.Set("until", request.Window.To.UTC().Format(time.RFC3339))
	}
	target := c.client.baseURL + "/repos/" + url.PathEscape(owner) + "/" +
		url.PathEscape(name) + "/commits?" + values.Encode()
	body, headers, upstream, err := c.client.doWithHeaders(ctx, http.MethodGet, target, nil, commitBucket)
	mergeReport(&report, upstream)
	var statusErr *StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusConflict ||
			statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		gap, gapErr := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("repository is unavailable or has no default branch: %s", repository), true)
		if gapErr != nil {
			return event.Page{}, report, gapErr
		}
		report.Gaps = append(report.Gaps, gap)
		advanceRepository(&cursor)
		page, pageErr := commitPage(nil, cursor, len(repositories))
		return page, report, pageErr
	}
	if err != nil {
		return event.Page{}, report, fmt.Errorf("list GitHub commits: %w", err)
	}
	var records []githubCommit
	if err := json.Unmarshal(body, &records); err != nil {
		return event.Page{}, report, fmt.Errorf("decode GitHub commits: %w", err)
	}
	events := make([]event.RawEvent, 0, len(records))
	for _, record := range records {
		raw, include, err := buildGitHubCommit(request, repository, cursor.RepositoryID, record)
		if err != nil {
			return event.Page{Events: events}, report, err
		}
		if include {
			events = append(events, raw)
		}
	}
	cursor.Pages++
	hasNext := forgehttp.HasNextPage(headers)
	if cursor.Pages >= c.maxPages && (hasNext || cursor.AuthorFilter+1 < len(filters)) {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("commit page limit reached for %s", repository), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		advanceRepository(&cursor)
	} else if hasNext {
		cursor.Page++
	} else if cursor.AuthorFilter+1 < len(filters) {
		cursor.AuthorFilter++
		cursor.Page = 1
	} else {
		advanceRepository(&cursor)
	}
	page, err := commitPage(events, cursor, len(repositories))
	return page, report, err
}

type githubCommit struct {
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
	target := c.client.baseURL + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	body, upstream, err := c.client.do(ctx, http.MethodGet, target, nil, commitBucket)
	mergeReport(report, upstream)
	var statusErr *StatusError
	if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusNotFound ||
		statusErr.StatusCode == http.StatusConflict || statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		gap, gapErr := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("repository is unavailable: %s", repository), true)
		if gapErr != nil {
			return false, "", gapErr
		}
		report.Gaps = append(report.Gaps, gap)
		advanceRepository(cursor)
		next, encodeErr := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, encodeErr
	}
	if err != nil {
		return false, "", fmt.Errorf("verify GitHub repository: %w", err)
	}
	var metadata struct {
		ID         int64  `json:"id"`
		Private    bool   `json:"private"`
		Fork       bool   `json:"fork"`
		Visibility string `json:"visibility"`
		FullName   string `json:"full_name"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return false, "", fmt.Errorf("decode GitHub repository: %w", err)
	}
	if metadata.ID <= 0 || metadata.Private || metadata.Fork ||
		metadata.Visibility != "public" || metadata.FullName != repository {
		gap, err := commit.CoverageGap(request, time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
			fmt.Sprintf("repository is private, a fork, or otherwise unavailable for commit scanning: %s", repository), true)
		if err != nil {
			return false, "", err
		}
		report.Gaps = append(report.Gaps, gap)
		advanceRepository(cursor)
		next, err := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, err
	}
	cursor.Verified = true
	cursor.RepositoryID = metadata.ID
	next, err := cursor.Encode()
	return false, next, err
}

func buildGitHubCommit(request event.PageRequest, repository string, repositoryID int64, record githubCommit) (event.RawEvent, bool, error) {
	if len(record.Parents) > 1 {
		return event.RawEvent{}, false, nil
	}
	if record.SHA == "" || record.Commit.Author.Date.IsZero() || record.URL == "" ||
		repositoryID <= 0 || len(record.Parents) == 0 {
		return event.RawEvent{}, false, errors.New("GitHub returned an incomplete commit record")
	}
	accountID := ""
	if record.Author != nil {
		if record.Author.ID <= 0 {
			return event.RawEvent{}, false, errors.New("GitHub returned an invalid linked commit author")
		}
		accountID = strconv.FormatInt(record.Author.ID, 10)
	}
	attribution := commit.ResolveAttribution(request, "github", accountID,
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
		return event.RawEvent{}, false, fmt.Errorf("encode GitHub commit payload: %w", err)
	}
	return event.RawEvent{
		ForgeInstanceID: request.ForgeInstanceID, Source: CommitSource,
		ExternalID: fmt.Sprintf("commit:%d:%s", repositoryID, record.SHA),
		Kind:       commit.Kind, OccurredAt: &authoredAt, Payload: payload,
		PayloadSchemaVersion: 1, Actors: attribution.Actors,
	}, true, nil
}

func advanceRepository(cursor *commit.Cursor) {
	cursor.Repository++
	cursor.Page = 1
	cursor.Pages = 0
	cursor.AuthorFilter = 0
	cursor.RepositoryID = 0
	cursor.Verified = false
}

func commitPage(events []event.RawEvent, cursor commit.Cursor, repositoryCount int) (event.Page, error) {
	if cursor.Repository >= repositoryCount {
		return event.Page{Events: events, Done: true}, nil
	}
	next, err := cursor.Encode()
	if err != nil {
		return event.Page{}, err
	}
	return event.Page{Events: events, NextCursor: next}, nil
}
