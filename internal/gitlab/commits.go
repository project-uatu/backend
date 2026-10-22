// SPDX-License-Identifier: Apache-2.0

package gitlab

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
	"github.com/project-uatu/backend/internal/identity"
)

// CommitSource is the durable source key for GitLab commit events.
const CommitSource event.Source = "gitlab_commits"

// CommitConnector collects bounded commits from known public GitLab.com projects.
type CommitConnector struct {
	client   *forgehttp.Client
	maxRepos int
	maxPages int
}

// NewCommitConnector creates a GitLab commit connector with optional scan limits.
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
		return nil, errors.New("GitLab commit limits must be positive")
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

// QuotaBucket names the GitLab.com API quota.
func (c *CommitConnector) QuotaBucket() string { return quotaBucket }

// FetchPage fetches one bounded page of commits for a known public project.
func (c *CommitConnector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	if request.Source != CommitSource {
		return event.Page{}, event.FetchReport{}, errors.New("GitLab commit request has an unexpected source")
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
		RateLimitBucket: quotaBucket, CoverageScope: commit.CoverageScope("gitlab", c.maxRepos, c.maxPages),
	}
	if (request.Cursor == "" || repositorySetChanged) && capped {
		gap, err := commit.CoverageGap(request, floorDate,
			fmt.Sprintf("commit repository limit reached; scanning the first %d repositories in stable path order", c.maxRepos), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
	}
	if !hasAssignedEmail(request) {
		gap, err := commit.CoverageGap(request, floorDate,
			"GitLab commit attribution requires an assigned email identity", true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		return event.Page{Done: true}, report, nil
	}
	if len(repositories) == 0 {
		gap, err := commit.CoverageGap(request, floorDate,
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
	if repository == "" {
		return event.Page{}, report, errors.New("known GitLab project path is empty")
	}
	if !cursor.Verified {
		done, next, err := c.verifyProject(ctx, request, repository, len(repositories), &cursor, &report)
		if err != nil {
			return event.Page{}, report, err
		}
		if done {
			return event.Page{Done: true}, report, nil
		}
		return event.Page{NextCursor: next}, report, nil
	}
	query := url.Values{
		"page":     {strconv.Itoa(cursor.Page)},
		"per_page": {strconv.Itoa(commit.PageSize)},
	}
	if !request.Window.From.IsZero() {
		query.Set("since", request.Window.From.UTC().Format(time.RFC3339))
	}
	if !request.Window.To.IsZero() {
		query.Set("until", request.Window.To.UTC().Format(time.RFC3339))
	}
	path := fmt.Sprintf("/projects/%s/repository/commits", url.PathEscape(repository))
	response, upstream, err := c.client.Get(ctx, path, query, quotaBucket)
	mergeReport(&report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusConflict ||
			statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		if err := appendProjectGap(request, repository, &report); err != nil {
			return event.Page{}, report, err
		}
		advanceCommitRepository(&cursor)
		return finishGitLabCommitPage(nil, cursor, len(repositories), report)
	}
	if err != nil {
		return event.Page{}, report, fmt.Errorf("list GitLab commits: %w", err)
	}
	var records []gitLabCommit
	if err := json.Unmarshal(response.Body, &records); err != nil {
		return event.Page{}, report, fmt.Errorf("decode GitLab commits: %w", err)
	}
	events := make([]event.RawEvent, 0, len(records))
	for _, record := range records {
		raw, include, err := buildGitLabCommit(request, repository, cursor.RepositoryID, record)
		if err != nil {
			return event.Page{Events: events}, report, err
		}
		if include {
			events = append(events, raw)
		}
	}
	cursor.Pages++
	nextPage := strings.TrimSpace(response.Header.Get("X-Next-Page"))
	hasNext := nextPage != ""
	if hasNext {
		page, err := strconv.Atoi(nextPage)
		if err != nil || page <= cursor.Page {
			return event.Page{Events: events}, report, errors.New("GitLab returned an invalid next-page header")
		}
	}
	if cursor.Pages >= c.maxPages && hasNext {
		gap, err := commit.CoverageGap(request, floorDate,
			fmt.Sprintf("commit page limit reached for %s", repository), true)
		if err != nil {
			return event.Page{}, report, err
		}
		report.Gaps = append(report.Gaps, gap)
		advanceCommitRepository(&cursor)
	} else if hasNext {
		cursor.Page, _ = strconv.Atoi(nextPage)
	} else {
		advanceCommitRepository(&cursor)
	}
	return finishGitLabCommitPage(events, cursor, len(repositories), report)
}

type gitLabCommit struct {
	ID           string    `json:"id"`
	WebURL       string    `json:"web_url"`
	AuthorEmail  string    `json:"author_email"`
	AuthoredDate time.Time `json:"authored_date"`
	Message      string    `json:"message"`
	ParentIDs    []string  `json:"parent_ids"`
}

func (c *CommitConnector) verifyProject(
	ctx context.Context, request event.PageRequest, repository string, repositoryCount int,
	cursor *commit.Cursor, report *event.FetchReport,
) (bool, string, error) {
	response, upstream, err := c.client.Get(ctx, "/projects/"+url.PathEscape(repository), nil, quotaBucket)
	mergeReport(report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) &&
		(statusErr.StatusCode == http.StatusNotFound ||
			statusErr.StatusCode == http.StatusUnavailableForLegalReasons) {
		if err := appendProjectGap(request, repository, report); err != nil {
			return false, "", err
		}
		advanceCommitRepository(cursor)
		next, err := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, err
	}
	if err != nil {
		return false, "", fmt.Errorf("verify GitLab project: %w", err)
	}
	var project struct {
		ID         int64     `json:"id"`
		Path       string    `json:"path_with_namespace"`
		WebURL     string    `json:"web_url"`
		Visibility string    `json:"visibility"`
		ForkedFrom *struct{} `json:"forked_from_project"`
	}
	if err := json.Unmarshal(response.Body, &project); err != nil {
		return false, "", fmt.Errorf("decode GitLab project: %w", err)
	}
	if project.ID <= 0 || project.Path != repository || project.Visibility != "public" ||
		project.ForkedFrom != nil {
		if err := appendProjectGap(request, repository, report); err != nil {
			return false, "", err
		}
		advanceCommitRepository(cursor)
		next, err := cursor.Encode()
		return cursor.Repository >= repositoryCount, next, err
	}
	cursor.RepositoryID = project.ID
	cursor.Verified = true
	next, err := cursor.Encode()
	return false, next, err
}

func buildGitLabCommit(request event.PageRequest, repository string, repositoryID int64, record gitLabCommit) (event.RawEvent, bool, error) {
	if len(record.ParentIDs) > 1 {
		return event.RawEvent{}, false, nil
	}
	if record.ID == "" || repositoryID <= 0 || record.AuthoredDate.IsZero() ||
		record.WebURL == "" || len(record.ParentIDs) == 0 {
		return event.RawEvent{}, false, errors.New("GitLab returned an incomplete commit record")
	}
	attribution := commit.ResolveAttribution(request, "gitlab", "", record.AuthorEmail, record.Message)
	if len(attribution.Actors) == 0 {
		return event.RawEvent{}, false, nil
	}
	authoredAt := record.AuthoredDate.UTC()
	payload, err := json.Marshal(commit.Payload{
		SHA: record.ID, Repository: repository, URL: record.WebURL, AuthoredAt: authoredAt,
		AttributionBasis: attribution.Basis,
	})
	if err != nil {
		return event.RawEvent{}, false, fmt.Errorf("encode GitLab commit payload: %w", err)
	}
	return event.RawEvent{
		ForgeInstanceID: request.ForgeInstanceID, Source: CommitSource,
		ExternalID: fmt.Sprintf("commit:%d:%s", repositoryID, record.ID),
		Kind:       commit.Kind, OccurredAt: &authoredAt, Payload: payload,
		PayloadSchemaVersion: 1, Actors: attribution.Actors,
	}, true, nil
}

func hasAssignedEmail(request event.PageRequest) bool {
	for _, key := range request.IdentityKeys {
		if key.IdentityKey.Kind == identity.KindEmail {
			return true
		}
	}
	return false
}

func appendProjectGap(request event.PageRequest, repository string, report *event.FetchReport) error {
	gap, err := commit.CoverageGap(request, floorDate,
		fmt.Sprintf("repository is unavailable, not public, or a fork: %s", repository), true)
	if err != nil {
		return err
	}
	report.Gaps = append(report.Gaps, gap)
	return nil
}

func advanceCommitRepository(cursor *commit.Cursor) {
	cursor.Repository++
	cursor.Page = 1
	cursor.Pages = 0
	cursor.RepositoryID = 0
	cursor.Verified = false
}

func finishGitLabCommitPage(
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
