// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/forgehttp"
	"github.com/project-uatu/backend/internal/identity"
)

const historySource event.Source = "github_history"

const historyCoverageScope = "authored issues, pull requests and reviews: public search; releases: known public repositories only"

// StatusError reports a GitHub response status without including its body.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GitHub returned HTTP %d", e.StatusCode)
}

type githubUser struct {
	ID        int64     `json:"id"`
	NodeID    string    `json:"node_id"`
	Login     string    `json:"login"`
	CreatedAt time.Time `json:"created_at"`
}

// Account is the stable GitHub identity resolved from a login.
type Account struct {
	ID        string
	NodeID    string
	Login     string
	CreatedAt time.Time
}

// ResolveAccount resolves a login to immutable GitHub account identifiers.
func (c *Client) ResolveAccount(ctx context.Context, login string) (Account, event.FetchReport, error) {
	if !ValidLogin(login) {
		return Account{}, event.FetchReport{}, fmt.Errorf("invalid GitHub login %q", login)
	}
	target := c.baseURL + "/users/" + url.PathEscape(login)
	body, report, err := c.do(ctx, http.MethodGet, target, nil, "github/core")
	if err != nil {
		return Account{}, report, fmt.Errorf("resolve GitHub account: %w", err)
	}
	var user githubUser
	if err := json.Unmarshal(body, &user); err != nil {
		return Account{}, report, fmt.Errorf("decode GitHub account: %w", err)
	}
	if user.ID <= 0 || user.NodeID == "" || !ValidLogin(user.Login) {
		return Account{}, report, errors.New("GitHub returned an invalid account identity")
	}
	if user.CreatedAt.IsZero() {
		return Account{}, report, errors.New("GitHub returned an account without a creation time")
	}
	return Account{
		ID: strconv.FormatInt(user.ID, 10), NodeID: user.NodeID,
		Login: user.Login, CreatedAt: user.CreatedAt.UTC(),
	}, report, nil
}

type scanTask struct {
	Kind      string            `json:"kind"`
	Start     string            `json:"start,omitempty"`
	End       string            `json:"end,omitempty"`
	Page      int               `json:"page,omitempty"`
	Repo      string            `json:"repo,omitempty"`
	Owner     string            `json:"owner,omitempty"`
	Number    int64             `json:"number,omitempty"`
	EventType string            `json:"event_type,omitempty"`
	Records   []json.RawMessage `json:"records,omitempty"`
}

type scanState struct {
	Tasks            []scanTask `json:"tasks"`
	Repos            []string   `json:"repos,omitempty"`
	Login            string     `json:"login"`
	AccountID        string     `json:"account_id"`
	NodeID           string     `json:"node_id,omitempty"`
	AccountCreatedAt time.Time  `json:"account_created_at,omitempty"`
	Stage            string     `json:"stage"`
	Updated          bool       `json:"updated,omitempty"`
}

// HistoryConnector pages through GitHub person activity using Search, core
// REST and GraphQL quota buckets independently.
type HistoryConnector struct {
	client *Client
}

// NewHistoryConnector creates a connector for public person contribution history.
func NewHistoryConnector(client *Client) *HistoryConnector {
	return &HistoryConnector{client: client}
}

// Source identifies this connector's stored activity stream.
func (c *HistoryConnector) Source() event.Source { return historySource }

// QuotaBucket is the default bucket for the runner's reservation interface.
func (c *HistoryConnector) QuotaBucket() string { return "github/search" }

// QuotaBucketFor selects the GitHub quota family used by the current cursor task.
func (c *HistoryConnector) QuotaBucketFor(request event.PageRequest) string {
	if request.Cursor == "" {
		if request.AccountNodeID != "" {
			return "github/graphql"
		}
		return "github/core"
	}
	state, err := decodeScanState(request)
	if err != nil || len(state.Tasks) == 0 {
		if err == nil {
			switch state.Stage {
			case "counts", "releases":
				return "github/graphql"
			case "discover":
				for _, repo := range state.Repos {
					owner, name, ok := strings.Cut(repo, "/")
					if ok && ValidLogin(owner) && name != "" {
						return "github/core"
					}
				}
				return "github/graphql"
			}
		}
		return "github/search"
	}
	switch state.Tasks[0].Kind {
	case "owned_repos", "verify_repo", "pull_request_details", "reviews", "releases":
		return "github/core"
	case "counts", "resolve_account":
		return "github/graphql"
	default:
		return "github/search"
	}
}

// FetchPage fetches one bounded page of person-scoped public activity.
func (c *HistoryConnector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	if c == nil || c.client == nil {
		return event.Page{}, event.FetchReport{}, errors.New("GitHub history connector requires a client")
	}
	state, err := decodeScanState(request)
	if err != nil {
		return event.Page{}, event.FetchReport{}, err
	}
	if state.Login == "" {
		state.Login = request.CurrentLogin
	}
	if state.Login == "" {
		return event.Page{}, event.FetchReport{}, errors.New("GitHub history requires an assigned login identity")
	}
	if !ValidLogin(state.Login) {
		return event.Page{}, event.FetchReport{}, errors.New("GitHub history contains an invalid login identity")
	}
	report := event.FetchReport{
		RateLimitBucket: c.QuotaBucketFor(request),
		CoverageScope:   historyCoverageScope,
	}
	if request.Cursor == "" && state.AccountID == "" {
		account, resolutionReport, err := c.client.ResolveAccount(ctx, state.Login)
		mergeReport(&report, resolutionReport)
		if err != nil {
			return event.Page{}, report, err
		}
		state.AccountID, state.NodeID, state.Login = account.ID, account.NodeID, account.Login
		state.AccountCreatedAt = account.CreatedAt
		start, end := request.Window.From, request.Window.To
		if start.IsZero() {
			start = account.CreatedAt
		}
		if end.IsZero() {
			end = time.Now().UTC()
		}
		state.Updated = request.ScanMode == "incremental"
		state.Stage = "discover"
		state.Tasks = []scanTask{{Kind: "owned_repos", Page: 1}}
		for _, repo := range request.KnownRepositories {
			state.Tasks = append(state.Tasks, scanTask{Kind: "verify_repo", Repo: repo})
		}
		for _, kind := range []string{"authored_issue", "authored_pr", "review_candidates"} {
			searchStart := start
			if kind == "review_candidates" && !state.Updated {
				searchStart = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			state.Tasks = append(state.Tasks, scanTask{
				Kind: kind, Start: searchStart.UTC().Format("2006-01-02"),
				End: end.Add(-time.Nanosecond).UTC().Format("2006-01-02"), Page: 1,
			})
		}
		cursorBytes, err := json.Marshal(state)
		if err != nil {
			return event.Page{}, report, fmt.Errorf("encode resolved GitHub history cursor: %w", err)
		}
		return event.Page{
			NextCursor: string(cursorBytes),
			ResolvedAccount: &event.ResolvedAccount{
				AccountID: account.ID, NodeID: account.NodeID, Login: account.Login, CreatedAt: account.CreatedAt,
			},
		}, report, nil
	}
	if request.Cursor == "" {
		start, end := request.Window.From, request.Window.To
		if start.IsZero() {
			start = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if end.IsZero() {
			end = time.Now().UTC()
		}
		state.Updated = request.ScanMode == "incremental" ||
			request.ScanMode == "" && !request.Window.From.IsZero()
		state.Stage = "discover"
		state.Tasks = []scanTask{{Kind: "owned_repos", Page: 1}}
		for _, repo := range request.KnownRepositories {
			state.Tasks = append(state.Tasks, scanTask{Kind: "verify_repo", Repo: repo})
		}
		if request.AccountNodeID != "" {
			state.NodeID = request.AccountNodeID
			if request.CurrentLogin != "" {
				state.Login = request.CurrentLogin
			}
			state.Tasks = append([]scanTask{{Kind: "resolve_account"}}, state.Tasks...)
		}
		for _, kind := range []string{"authored_issue", "authored_pr", "review_candidates"} {
			searchStart := start
			if kind == "review_candidates" && !state.Updated {
				searchStart = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			state.Tasks = append(state.Tasks, scanTask{
				Kind: kind, Start: searchStart.UTC().Format("2006-01-02"),
				End: end.Add(-time.Nanosecond).UTC().Format("2006-01-02"), Page: 1,
			})
		}
	}
	if len(state.Tasks) == 0 {
		c.advanceStage(&state, request)
	}
	if len(state.Tasks) == 0 {
		return event.Page{Done: true}, event.FetchReport{
			RateLimitBucket: "github/search",
			CoverageScope:   historyCoverageScope,
		}, nil
	}

	task := state.Tasks[0]
	repoCount := len(state.Repos)
	var rawEvents []event.RawEvent
	var fetchErr error
	switch task.Kind {
	case "owned_repos":
		rawEvents, fetchErr = c.fetchOwnedRepos(ctx, request, &state, task, &report)
	case "authored_issue", "authored_pr", "review_candidates":
		rawEvents, fetchErr = c.fetchSearch(ctx, request, &state, task, &report)
	case "verify_repo":
		rawEvents, fetchErr = c.verifyRepo(ctx, request, &state, task, &report)
	case "pull_request_details":
		rawEvents, fetchErr = c.fetchPullRequestDetails(ctx, request, &state, task, &report)
	case "reviews":
		rawEvents, fetchErr = c.fetchReviews(ctx, request, &state, task, &report)
	case "releases":
		rawEvents, fetchErr = c.fetchReleases(ctx, request, &state, task, &report)
	case "counts":
		fetchErr = c.fetchCounts(ctx, &state, task, &report)
	case "resolve_account":
		fetchErr = c.fetchCurrentAccount(ctx, &state, &report)
	default:
		return event.Page{}, report, fmt.Errorf("unknown GitHub history cursor task %q", task.Kind)
	}
	if fetchErr != nil {
		return event.Page{Events: rawEvents}, report, fetchErr
	}
	if len(state.Tasks) == 0 {
		c.advanceStage(&state, request)
	}
	report.KnownRepositories = append([]string(nil), state.Repos[repoCount:]...)
	cursorBytes, err := json.Marshal(state)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("encode GitHub history cursor: %w", err)
	}
	return event.Page{
		Events: rawEvents, NextCursor: string(cursorBytes), Done: len(state.Tasks) == 0,
	}, report, nil
}

func decodeScanState(request event.PageRequest) (scanState, error) {
	state := scanState{Login: request.OnboardingLogin}
	for _, key := range request.IdentityKeys {
		switch key.IdentityKey.Kind {
		case identity.KindLogin:
			if state.Login == "" {
				state.Login = key.IdentityKey.Value
			}
		case identity.KindAccountID:
			state.AccountID = key.IdentityKey.Value
		}
	}
	if request.Cursor != "" {
		var stored scanState
		if err := json.Unmarshal([]byte(request.Cursor), &stored); err != nil {
			return scanState{}, errors.New("invalid GitHub history cursor")
		}
		if stored.Login != "" {
			state.Login = stored.Login
		}
		if stored.AccountID != "" {
			state.AccountID = stored.AccountID
		}
		state.Tasks, state.Repos, state.Stage, state.Updated = stored.Tasks, stored.Repos, stored.Stage, stored.Updated
		state.NodeID = stored.NodeID
		state.AccountCreatedAt = stored.AccountCreatedAt
	}
	return state, nil
}

func (c *HistoryConnector) fetchCurrentAccount(ctx context.Context, state *scanState, report *event.FetchReport) error {
	query := `query($id:ID!) {
		node(id:$id) { ... on User { databaseId login } }
	}`
	payload, err := json.Marshal(map[string]any{
		"query": query, "variables": map[string]string{"id": state.NodeID},
	})
	if err != nil {
		return fmt.Errorf("encode GitHub account refresh query: %w", err)
	}
	body, upstreamReport, err := c.client.do(ctx, http.MethodPost, c.client.graphQLURL(), payload, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return err
	}
	var result struct {
		Data struct {
			Node *struct {
				DatabaseID int64  `json:"databaseId"`
				Login      string `json:"login"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode GitHub account refresh: %w", err)
	}
	if result.Data.Node == nil || result.Data.Node.DatabaseID <= 0 ||
		strconv.FormatInt(result.Data.Node.DatabaseID, 10) != state.AccountID ||
		!ValidLogin(result.Data.Node.Login) {
		return errors.New("GitHub node identity does not match the tracked account")
	}
	state.Login = result.Data.Node.Login
	popTask(state)
	return nil
}

func (c *HistoryConnector) advanceStage(state *scanState, request event.PageRequest) {
	if state.Stage == "discover" {
		state.Stage = "releases"
		for _, repo := range state.Repos {
			owner, name, ok := strings.Cut(repo, "/")
			if ok && ValidLogin(owner) && name != "" {
				state.Tasks = append(state.Tasks, scanTask{Kind: "releases", Repo: repo, Page: 1})
			}
		}
	}
	if len(state.Tasks) == 0 && state.Stage == "releases" {
		state.Stage = "counts"
		start, end := request.Window.From, request.Window.To
		if start.IsZero() {
			start = time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		if end.IsZero() {
			end = time.Now().UTC()
		}
		start = start.UTC().Truncate(24 * time.Hour)
		if !end.Equal(end.UTC().Truncate(24 * time.Hour)) {
			end = end.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
		}
		for start.Before(end) {
			next := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			if next.After(end) {
				next = end
			}
			state.Tasks = append(state.Tasks, scanTask{
				Kind: "counts", Start: start.Format(time.RFC3339), End: next.Format(time.RFC3339),
			})
			start = next
		}
	}
}

func (c *HistoryConnector) fetchOwnedRepos(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	target := fmt.Sprintf("%s/users/%s/repos?type=owner&sort=updated&direction=desc&per_page=100&page=%d",
		c.client.baseURL, url.PathEscape(state.Login), task.Page)
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var repos []struct {
		FullName   string `json:"full_name"`
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(body, &repos); err != nil {
		return nil, fmt.Errorf("decode GitHub repositories: %w", err)
	}
	for _, repo := range repos {
		if repo.FullName != "" && !repo.Private && (repo.Visibility == "" || repo.Visibility == "public") {
			addRepo(state, repo.FullName)
		}
	}
	if len(repos) < 100 {
		popTask(state)
	} else {
		state.Tasks[0].Page++
	}
	return nil, nil
}

func (c *HistoryConnector) fetchSearch(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	queuedTasks := append([]scanTask(nil), state.Tasks[1:]...)
	dateQualifier := "created"
	if state.Updated {
		dateQualifier = "updated"
	}
	query := ""
	switch task.Kind {
	case "authored_issue":
		query = "is:issue author:" + state.Login
	case "authored_pr":
		query = "is:pr author:" + state.Login
	case "review_candidates":
		query = "is:pr reviewed-by:" + state.Login
	}
	query += " is:public " + dateQualifier + ":" + task.Start + ".." + task.End
	values := url.Values{"q": {query}, "per_page": {"100"}, "page": {strconv.Itoa(max(task.Page, 1))}}
	target := c.client.baseURL + "/search/issues?" + values.Encode()
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var result struct {
		TotalCount        int               `json:"total_count"`
		IncompleteResults bool              `json:"incomplete_results"`
		Items             []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode GitHub search results: %w", err)
	}
	var events []event.RawEvent
	verifyTasks := make([]scanTask, 0)
	for _, item := range result.Items {
		var record searchRecord
		if err := json.Unmarshal(item, &record); err != nil {
			return events, fmt.Errorf("decode GitHub search record: %w", err)
		}
		repo := repoName(record.RepositoryURL, c.client.baseURL)
		if repo == "" {
			continue
		}
		if containsRepo(state.Repos, repo) {
			more, err := c.processSearchRecord(request, state, record, task.Kind, repo, item)
			if err != nil {
				return events, err
			}
			events = append(events, more...)
		} else {
			addVerifyRecord(&verifyTasks, task.Kind, repo, item)
		}
	}
	generatedTasks := append([]scanTask(nil), state.Tasks[1+len(queuedTasks):]...)
	state.Tasks = append(state.Tasks[:1], queuedTasks...)

	start, _ := time.Parse("2006-01-02", task.Start)
	end, _ := time.Parse("2006-01-02", task.End)
	needsSplit := result.IncompleteResults || result.TotalCount >= 1000
	if needsSplit {
		if !start.Before(end) {
			report.Gaps = append(report.Gaps, event.Gap{
				From: start, To: end.AddDate(0, 0, 1),
				Reason: "GitHub search was capped or incomplete for one UTC day",
			})
			popTask(state)
			state.Tasks = prependTasks(state.Tasks, verifyTasks, generatedTasks)
			return events, nil
		}
		children := splitSearchTask(task, start, end)
		popTask(state)
		state.Tasks = prependTasks(state.Tasks, verifyTasks, generatedTasks, children)
		return events, nil
	}
	pages := (result.TotalCount + 99) / 100
	if pages > 10 {
		pages = 10
	}
	if task.Page < pages && len(result.Items) > 0 {
		nextTask := task
		nextTask.Page++
		popTask(state)
		state.Tasks = prependTasks(state.Tasks, verifyTasks, generatedTasks, []scanTask{nextTask})
	} else {
		popTask(state)
		state.Tasks = prependTasks(state.Tasks, verifyTasks, generatedTasks)
	}
	return events, nil
}

func (c *HistoryConnector) verifyRepo(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	owner, repo, ok := strings.Cut(task.Repo, "/")
	if !ok || owner == "" || repo == "" {
		popTask(state)
		return nil, nil
	}
	target := fmt.Sprintf("%s/repos/%s/%s", c.client.baseURL, url.PathEscape(owner), url.PathEscape(repo))
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var repository struct {
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(body, &repository); err != nil {
		return nil, fmt.Errorf("decode GitHub repository visibility: %w", err)
	}
	if repository.Private || repository.Visibility != "public" {
		popTask(state)
		return nil, nil
	}
	addRepo(state, task.Repo)
	queuedTasks := append([]scanTask(nil), state.Tasks[1:]...)
	var events []event.RawEvent
	for _, data := range task.Records {
		var record searchRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return events, fmt.Errorf("decode verified GitHub search record: %w", err)
		}
		more, err := c.processSearchRecord(request, state, record, task.EventType, task.Repo, data)
		if err != nil {
			return events, err
		}
		events = append(events, more...)
	}
	generatedTasks := append([]scanTask(nil), state.Tasks[1+len(queuedTasks):]...)
	state.Tasks = append(state.Tasks[:1], queuedTasks...)
	popTask(state)
	state.Tasks = prependTasks(state.Tasks, generatedTasks)
	return events, nil
}

func (c *HistoryConnector) processSearchRecord(request event.PageRequest, state *scanState, record searchRecord, kind, repo string, payload json.RawMessage) ([]event.RawEvent, error) {
	if kind == "review_candidates" {
		owner, name, ok := strings.Cut(repo, "/")
		if ok && record.Number > 0 {
			addReviewTask(state, owner, name, record.Number)
		}
		return nil, nil
	}
	if record.User.ID <= 0 || strconv.FormatInt(record.User.ID, 10) != state.AccountID ||
		record.Number <= 0 {
		return nil, nil
	}
	if kind == "authored_pr" {
		owner, name, ok := strings.Cut(repo, "/")
		if ok {
			addPullRequestTask(state, owner, name, record.Number)
		}
		return nil, nil
	}
	raw, ok, err := issueRawEvent(request, record, kind, payload)
	if err != nil || !ok {
		return nil, err
	}
	return []event.RawEvent{raw}, nil
}

func (c *HistoryConnector) fetchPullRequestDetails(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	target := fmt.Sprintf("%s/repos/%s/%s/pulls/%d",
		c.client.baseURL, url.PathEscape(task.Owner), url.PathEscape(task.Repo), task.Number)
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var record searchRecord
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, fmt.Errorf("decode GitHub pull request: %w", err)
	}
	popTask(state)
	if record.User.ID <= 0 || strconv.FormatInt(record.User.ID, 10) != state.AccountID ||
		record.Number != task.Number || record.CreatedAt.IsZero() {
		return nil, nil
	}
	forgeID := request.ForgeInstanceID
	occurredAt := record.CreatedAt.UTC()
	if record.ID <= 0 {
		return nil, errors.New("GitHub pull request has no immutable ID")
	}
	return []event.RawEvent{{
		ForgeInstanceID: forgeID, Source: request.Source,
		ExternalID: fmt.Sprintf("pull_request:%d", record.ID),
		Kind:       "pull_request", OccurredAt: &occurredAt, Payload: body,
		PayloadSchemaVersion: 1,
		Actors: []event.Actor{{Key: event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: state.AccountID},
			ForgeInstanceID: &forgeID,
		}, Role: "author"}},
	}}, nil
}

func addVerifyRecord(tasks *[]scanTask, kind, repo string, record json.RawMessage) {
	for i := range *tasks {
		if (*tasks)[i].Repo == repo {
			(*tasks)[i].Records = append((*tasks)[i].Records, record)
			return
		}
	}
	*tasks = append(*tasks, scanTask{
		Kind: "verify_repo", Repo: repo, EventType: kind,
		Records: []json.RawMessage{record},
	})
}

func containsRepo(repos []string, repo string) bool {
	for _, known := range repos {
		if known == repo {
			return true
		}
	}
	return false
}

type searchRecord struct {
	ID            int64      `json:"id"`
	Number        int64      `json:"number"`
	Title         string     `json:"title"`
	HTMLURL       string     `json:"html_url"`
	RepositoryURL string     `json:"repository_url"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	MergedAt      *time.Time `json:"merged_at"`
	State         string     `json:"state"`
	User          struct {
		ID int64 `json:"id"`
	} `json:"user"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func issueRawEvent(request event.PageRequest, record searchRecord, taskKind string, payload json.RawMessage) (event.RawEvent, bool, error) {
	if record.CreatedAt.IsZero() {
		return event.RawEvent{}, false, nil
	}
	if !json.Valid(payload) {
		return event.RawEvent{}, false, errors.New("GitHub search record payload is invalid JSON")
	}
	if record.ID <= 0 {
		return event.RawEvent{}, false, errors.New("GitHub issue has no immutable ID")
	}
	kind := "issue"
	if taskKind == "authored_pr" {
		kind = "pull_request"
	}
	forgeID := request.ForgeInstanceID
	occurredAt := record.CreatedAt.UTC()
	return event.RawEvent{
		ForgeInstanceID:      forgeID,
		Source:               request.Source,
		ExternalID:           fmt.Sprintf("%s:%d", kind, record.ID),
		Kind:                 event.Kind(kind),
		OccurredAt:           &occurredAt,
		Payload:              append(json.RawMessage(nil), payload...),
		PayloadSchemaVersion: 1,
		Actors: []event.Actor{{Key: event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: strconv.FormatInt(record.User.ID, 10)},
			ForgeInstanceID: &forgeID,
		}, Role: "author"}},
	}, true, nil
}

func (c *HistoryConnector) fetchReviews(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	target := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100&page=%d",
		c.client.baseURL, url.PathEscape(task.Owner), url.PathEscape(task.Repo), task.Number, task.Page)
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var reviews []json.RawMessage
	if err := json.Unmarshal(body, &reviews); err != nil {
		return nil, fmt.Errorf("decode GitHub pull request reviews: %w", err)
	}
	var events []event.RawEvent
	for _, payload := range reviews {
		var review struct {
			ID          int64      `json:"id"`
			State       string     `json:"state"`
			HTMLURL     string     `json:"html_url"`
			SubmittedAt *time.Time `json:"submitted_at"`
			User        struct {
				ID int64 `json:"id"`
			} `json:"user"`
		}
		if err := json.Unmarshal(payload, &review); err != nil {
			return events, fmt.Errorf("decode GitHub pull request review: %w", err)
		}
		if review.ID <= 0 || review.SubmittedAt == nil || review.User.ID <= 0 ||
			strconv.FormatInt(review.User.ID, 10) != state.AccountID {
			continue
		}
		forgeID := request.ForgeInstanceID
		occurredAt := review.SubmittedAt.UTC()
		events = append(events, event.RawEvent{
			ForgeInstanceID: forgeID, Source: request.Source,
			ExternalID: fmt.Sprintf("review:%d", review.ID), Kind: "review",
			OccurredAt: &occurredAt, Payload: append(json.RawMessage(nil), payload...), PayloadSchemaVersion: 1,
			Actors: []event.Actor{{Key: event.ActorKey{
				IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: state.AccountID},
				ForgeInstanceID: &forgeID,
			}, Role: "reviewer"}},
		})
	}
	if len(reviews) < 100 {
		popTask(state)
	} else {
		state.Tasks[0].Page++
	}
	return events, nil
}

func (c *HistoryConnector) fetchReleases(ctx context.Context, request event.PageRequest, state *scanState, task scanTask, report *event.FetchReport) ([]event.RawEvent, error) {
	owner, repo, ok := strings.Cut(task.Repo, "/")
	if !ok || owner == "" || repo == "" {
		popTask(state)
		return nil, nil
	}
	target := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100&page=%d",
		c.client.baseURL, url.PathEscape(owner), url.PathEscape(repo), task.Page)
	body, upstreamReport, err := c.client.do(ctx, http.MethodGet, target, nil, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return nil, err
	}
	var releases []json.RawMessage
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("decode GitHub releases: %w", err)
	}
	var events []event.RawEvent
	allOlderThanWindow := state.Updated && !request.Window.From.IsZero() && len(releases) > 0
	for _, payload := range releases {
		var release struct {
			ID          int64      `json:"id"`
			Name        string     `json:"name"`
			HTMLURL     string     `json:"html_url"`
			PublishedAt *time.Time `json:"published_at"`
			Author      struct {
				ID int64 `json:"id"`
			} `json:"author"`
		}
		if err := json.Unmarshal(payload, &release); err != nil {
			return events, fmt.Errorf("decode GitHub release: %w", err)
		}
		if release.PublishedAt == nil || !release.PublishedAt.Before(request.Window.From) {
			allOlderThanWindow = false
		}
		if release.ID <= 0 || release.PublishedAt == nil || release.Author.ID <= 0 ||
			strconv.FormatInt(release.Author.ID, 10) != state.AccountID {
			continue
		}
		forgeID := request.ForgeInstanceID
		occurredAt := release.PublishedAt.UTC()
		events = append(events, event.RawEvent{
			ForgeInstanceID: forgeID, Source: request.Source,
			ExternalID: fmt.Sprintf("release:%d", release.ID), Kind: "release",
			OccurredAt: &occurredAt, Payload: append(json.RawMessage(nil), payload...), PayloadSchemaVersion: 1,
			Actors: []event.Actor{{Key: event.ActorKey{
				IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: state.AccountID},
				ForgeInstanceID: &forgeID,
			}, Role: "author"}},
		})
	}
	if len(releases) < 100 || allOlderThanWindow {
		popTask(state)
	} else {
		state.Tasks[0].Page++
	}
	return events, nil
}

func (c *HistoryConnector) fetchCounts(ctx context.Context, state *scanState, task scanTask, report *event.FetchReport) error {
	from, err := time.Parse(time.RFC3339, task.Start)
	if err != nil {
		return fmt.Errorf("parse GraphQL contribution window start: %w", err)
	}
	to, err := time.Parse(time.RFC3339, task.End)
	if err != nil {
		return fmt.Errorf("parse GraphQL contribution window end: %w", err)
	}
	if !from.Before(to) {
		return errors.New("GitHub contribution window start must precede its end")
	}
	from = from.UTC().Truncate(24 * time.Hour)
	if !to.Equal(to.UTC().Truncate(24 * time.Hour)) {
		to = to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
	}
	query := `query($login:String!, $from:DateTime!, $to:DateTime!) {
		user(login:$login) {
			contributionsCollection(from:$from,to:$to) {
				commits: commitContributionsByRepository(maxRepositories:100) {
					repository { visibility }
					contributions(first:100) { nodes { occurredAt commitCount } pageInfo { hasNextPage } }
				}
				issues: issueContributionsByRepository(maxRepositories:100) {
					repository { visibility }
					contributions(first:100) { nodes { occurredAt } pageInfo { hasNextPage } }
				}
				pull_requests: pullRequestContributionsByRepository(maxRepositories:100) {
					repository { visibility }
					contributions(first:100) { nodes { occurredAt } pageInfo { hasNextPage } }
				}
				reviews: pullRequestReviewContributionsByRepository(maxRepositories:100) {
					repository { visibility }
					contributions(first:100) { nodes { occurredAt } pageInfo { hasNextPage } }
				}
			}
		}
	}`
	payload, err := json.Marshal(map[string]any{
		"query": query,
		"variables": map[string]string{
			"login": state.Login, "from": from.Format(time.RFC3339), "to": to.Add(-time.Second).Format(time.RFC3339),
		},
	})
	if err != nil {
		return fmt.Errorf("encode GitHub GraphQL request: %w", err)
	}
	body, upstreamReport, err := c.client.do(ctx, http.MethodPost, c.client.graphQLURL(), payload, report.RateLimitBucket)
	mergeReport(report, upstreamReport)
	if err != nil {
		return err
	}
	type repositoryContributions struct {
		Repository struct {
			Visibility string `json:"visibility"`
		} `json:"repository"`
		Contributions struct {
			Nodes []struct {
				OccurredAt  time.Time `json:"occurredAt"`
				CommitCount int64     `json:"commitCount"`
			} `json:"nodes"`
			PageInfo struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pageInfo"`
		} `json:"contributions"`
	}
	var result struct {
		Errors []json.RawMessage `json:"errors"`
		Data   struct {
			User *struct {
				Collection map[string][]repositoryContributions `json:"contributionsCollection"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("decode GitHub GraphQL contributions: %w", err)
	}
	if len(result.Errors) > 0 || result.Data.User == nil {
		return errors.New("GitHub GraphQL contribution query failed")
	}
	collection := result.Data.User.Collection
	metrics := []string{"commits", "issues", "pull_requests", "reviews"}
	capped := false
	for _, metric := range metrics {
		repos, ok := collection[metric]
		if !ok {
			return fmt.Errorf("GitHub contribution response is missing %s", metric)
		}
		capped = capped || len(repos) >= 100
		for _, repo := range repos {
			switch repo.Repository.Visibility {
			case "PUBLIC", "PRIVATE", "INTERNAL":
			default:
				return errors.New("GitHub contribution repository has invalid visibility")
			}
			if repo.Repository.Visibility == "PUBLIC" {
				capped = capped || repo.Contributions.PageInfo.HasNextPage
			}
		}
	}
	if capped {
		popTask(state)
		days := int(to.Sub(from) / (24 * time.Hour))
		if days <= 1 {
			report.Gaps = append(report.Gaps, event.Gap{
				From: from, To: to, Reason: "GitHub public contribution aggregates capped for one UTC day",
			})
			return nil
		}
		midpoint := from.AddDate(0, 0, days/2)
		state.Tasks = prependTasks(state.Tasks, []scanTask{
			{Kind: "counts", Start: from.Format(time.RFC3339), End: midpoint.Format(time.RFC3339)},
			{Kind: "counts", Start: midpoint.Format(time.RFC3339), End: to.Format(time.RFC3339)},
		})
		return nil
	}
	counts := make(map[time.Time]map[string]int64)
	for date := from; date.Before(to); date = date.AddDate(0, 0, 1) {
		counts[date] = make(map[string]int64)
	}
	for _, metric := range metrics {
		for _, repo := range collection[metric] {
			if repo.Repository.Visibility != "PUBLIC" {
				continue
			}
			for _, node := range repo.Contributions.Nodes {
				if node.OccurredAt.IsZero() || node.OccurredAt.Before(from) || !node.OccurredAt.Before(to) {
					return errors.New("GitHub contribution has an invalid occurrence time")
				}
				value := int64(1)
				if metric == "commits" {
					value = node.CommitCount
				}
				if value < 0 {
					return errors.New("GitHub contribution has a negative commit count")
				}
				counts[node.OccurredAt.UTC().Truncate(24*time.Hour)][metric] += value
			}
		}
	}
	for date := from; date.Before(to); date = date.AddDate(0, 0, 1) {
		var total int64
		for _, metric := range metrics {
			value := counts[date][metric]
			report.ContributionCounts = append(report.ContributionCounts, event.ContributionCount{
				Date: date, Metric: metric, Value: value,
			})
			total += value
		}
		report.ContributionCounts = append(report.ContributionCounts, event.ContributionCount{
			Date: date, Metric: "daily_total", Value: total,
		})
	}
	popTask(state)
	return nil
}

func (c *Client) graphQLURL() string {
	if strings.HasSuffix(c.baseURL, "/graphql") {
		return c.baseURL
	}
	return strings.TrimSuffix(c.baseURL, "/") + "/graphql"
}

func (c *Client) do(ctx context.Context, method, target string, body []byte, bucket string) ([]byte, event.FetchReport, error) {
	response, _, report, err := c.doWithHeaders(ctx, method, target, body, bucket)
	return response, report, err
}

func (c *Client) doWithHeaders(ctx context.Context, method, target string, body []byte, bucket string) ([]byte, http.Header, event.FetchReport, error) {
	var report event.FetchReport
	var requestBody io.Reader
	if len(body) > 0 {
		requestBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, requestBody)
	if err != nil {
		return nil, nil, report, fmt.Errorf("build GitHub request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", forgehttp.UserAgent())
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, report, fmt.Errorf("request GitHub resource: %w", err)
	}
	headers := res.Header.Clone()
	report.RateLimitBucket = bucket
	report.RateLimitLimit = headerInt(res.Header, "X-RateLimit-Limit")
	report.RateLimitRemaining = headerInt(res.Header, "X-RateLimit-Remaining")
	if reset := headerInt64(res.Header, "X-RateLimit-Reset"); reset != nil {
		resetAt := time.Unix(*reset, 0).UTC()
		report.RateLimitResetAt = &resetAt
	}
	if retryAfter := res.Header.Get("Retry-After"); retryAfter != "" {
		if seconds, parseErr := strconv.Atoi(retryAfter); parseErr == nil && seconds >= 0 {
			resetAt := time.Now().UTC().Add(time.Duration(seconds) * time.Second)
			report.RateLimitResetAt = &resetAt
			zero := 0
			report.RateLimitRemaining = &zero
		} else if retryAt, parseErr := http.ParseTime(retryAfter); parseErr == nil {
			report.RateLimitResetAt = &retryAt
			zero := 0
			report.RateLimitRemaining = &zero
		}
	}
	response, readErr := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
	closeErr := res.Body.Close()
	if readErr != nil {
		return nil, headers, report, fmt.Errorf("read GitHub response: %w", readErr)
	}
	if closeErr != nil {
		return nil, headers, report, fmt.Errorf("close GitHub response: %w", closeErr)
	}
	if len(response) > responseLimit {
		return nil, headers, report, fmt.Errorf("GitHub response exceeded %d bytes", responseLimit)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, headers, report, &StatusError{StatusCode: res.StatusCode}
	}
	return response, headers, report, nil
}

func headerInt(header http.Header, key string) *int {
	value, err := strconv.Atoi(header.Get(key))
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func headerInt64(header http.Header, key string) *int64 {
	value, err := strconv.ParseInt(header.Get(key), 10, 64)
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func mergeReport(dst *event.FetchReport, src event.FetchReport) {
	if src.RateLimitLimit != nil {
		dst.RateLimitLimit = src.RateLimitLimit
	}
	if src.RateLimitRemaining != nil {
		dst.RateLimitRemaining = src.RateLimitRemaining
	}
	if src.RateLimitResetAt != nil {
		dst.RateLimitResetAt = src.RateLimitResetAt
	}
}

func splitSearchTask(task scanTask, start, end time.Time) []scanTask {
	midpoint := start.AddDate(0, 0, int(end.Sub(start).Hours()/24)/2)
	firstEnd := midpoint
	secondStart := midpoint.AddDate(0, 0, 1)
	return []scanTask{
		{Kind: task.Kind, Start: start.Format("2006-01-02"), End: firstEnd.Format("2006-01-02"), Page: 1},
		{Kind: task.Kind, Start: secondStart.Format("2006-01-02"), End: end.Format("2006-01-02"), Page: 1},
	}
}

func addRepo(state *scanState, repo string) {
	for _, known := range state.Repos {
		if known == repo {
			return
		}
	}
	state.Repos = append(state.Repos, repo)
}

func addReviewTask(state *scanState, owner, repo string, number int64) {
	for _, task := range state.Tasks {
		if task.Kind == "reviews" && task.Owner == owner && task.Repo == repo && task.Number == number {
			return
		}
	}
	state.Tasks = append(state.Tasks, scanTask{
		Kind: "reviews", Owner: owner, Repo: repo, Number: number, Page: 1,
	})
}

func addPullRequestTask(state *scanState, owner, repo string, number int64) {
	for _, task := range state.Tasks {
		if task.Kind == "pull_request_details" && task.Owner == owner &&
			task.Repo == repo && task.Number == number {
			return
		}
	}
	state.Tasks = append(state.Tasks, scanTask{
		Kind: "pull_request_details", Owner: owner, Repo: repo, Number: number,
	})
}

func popTask(state *scanState) {
	if len(state.Tasks) > 0 {
		state.Tasks = state.Tasks[1:]
	}
}

func prependTasks(queued []scanTask, batches ...[]scanTask) []scanTask {
	count := len(queued)
	for _, batch := range batches {
		count += len(batch)
	}
	tasks := make([]scanTask, 0, count)
	for _, batch := range batches {
		tasks = append(tasks, batch...)
	}
	return append(tasks, queued...)
}

func repoName(repositoryURL, baseURL string) string {
	parsed, err := url.Parse(repositoryURL)
	base, baseErr := url.Parse(baseURL)
	if err != nil || baseErr != nil || parsed.Host != base.Host {
		return ""
	}
	parts := strings.Split(strings.Trim(path.Clean(parsed.Path), "/"), "/")
	if len(parts) != 3 || parts[0] != "repos" {
		return ""
	}
	return parts[1] + "/" + parts[2]
}
