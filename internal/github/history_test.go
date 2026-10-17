package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

func TestClient_ResolveAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/octocat" {
			t.Errorf("request path = %q, want /users/octocat", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q, want configured bearer token", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"id":42,"node_id":"MDQ6VXNlcjQy","login":"octocat","created_at":"2011-01-25T18:44:36Z"}`))
	}))
	defer server.Close()

	client := NewClient("test-token", server.Client())
	client.baseURL = server.URL
	account, err := client.ResolveAccount(t.Context(), "octocat")
	if err != nil {
		t.Fatalf("ResolveAccount: %v", err)
	}
	if account.ID != "42" || account.NodeID != "MDQ6VXNlcjQy" || account.Login != "octocat" {
		t.Errorf("ResolveAccount = %+v, want stable account identifiers", account)
	}
}

func TestHistoryConnector_SearchPaginatesAndValidatesActor(t *testing.T) {
	var calls int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		pageNumber := r.URL.Query().Get("page")
		if r.URL.Path != "/search/issues" {
			t.Errorf("request path = %q, want /search/issues", r.URL.Path)
		}
		if !strings.Contains(r.URL.Query().Get("q"), "author:octocat") ||
			!strings.Contains(r.URL.Query().Get("q"), "created:2020-01-01..2020-01-01") {
			t.Errorf("search query = %q, want authored one-day range", r.URL.Query().Get("q"))
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		switch pageNumber {
		case "1":
			_, _ = w.Write([]byte(`{
				"total_count":101,"incomplete_results":false,"items":[
				  {"number":3,"title":"mine","html_url":"https://github.com/owner/repo/issues/3",
				   "repository_url":"` + server.URL + `/repos/owner/repo",
				   "created_at":"2020-01-01T12:00:00Z","user":{"id":42},"body":"preserved source field"},
				  {"number":4,"title":"not mine","html_url":"https://github.com/owner/repo/issues/4",
				   "repository_url":"` + server.URL + `/repos/owner/repo",
				   "created_at":"2020-01-01T13:00:00Z","user":{"id":99}}
				]}`))
		case "2":
			_, _ = w.Write([]byte(`{
				"total_count":101,"incomplete_results":false,"items":[
				  {"number":5,"title":"second page","html_url":"https://github.com/owner/repo/issues/5",
				   "repository_url":"` + server.URL + `/repos/owner/repo",
				   "created_at":"2020-01-01T14:00:00Z","user":{"id":42}}
				]}`))
		default:
			t.Errorf("search page = %q, want 1 or 2", pageNumber)
			http.Error(w, "unexpected search page", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover","repos":["owner/repo"],
		"tasks":[{"kind":"authored_issue","start":"2020-01-01","end":"2020-01-01","page":1}]
	}`)
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if calls != 1 || report.RateLimitBucket != "github/search" {
		t.Errorf("FetchPage calls/bucket = %d/%q, want 1/github/search", calls, report.RateLimitBucket)
	}
	if len(page.Events) != 1 {
		t.Fatalf("FetchPage events = %d, want only the matching account", len(page.Events))
	}
	raw := page.Events[0]
	if raw.ExternalID != "issue:owner/repo:3" || raw.Actors[0].Key.IdentityKey.Value != "42" {
		t.Errorf("stored issue = %+v, want account 42 issue #3", raw)
	}
	if !strings.Contains(string(raw.Payload), `"body":"preserved source field"`) {
		t.Errorf("issue payload = %s, want original source fields preserved", raw.Payload)
	}
	if page.Done {
		t.Error("FetchPage done = true, want the second search page and releases queued")
	}
	request.Cursor = page.NextCursor
	secondPage, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(second page): %v", err)
	}
	if calls != 2 || len(secondPage.Events) != 1 ||
		secondPage.Events[0].ExternalID != "issue:owner/repo:5" {
		t.Errorf("second search page = calls %d, events %+v; want issue #5 from page two", calls, secondPage.Events)
	}
}

func TestHistoryConnector_PrioritizesPullRequestDetailsBeforeNextSearchPage(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" || r.URL.Query().Get("page") != "1" {
			t.Errorf("search request = %s page %q, want page 1", r.URL.Path, r.URL.Query().Get("page"))
		}
		_, _ = w.Write([]byte(`{
			"total_count":101,"incomplete_results":false,"items":[{
			  "number":12,"repository_url":"` + server.URL + `/repos/owner/repo",
			  "created_at":"2020-01-01T12:00:00Z","user":{"id":42}
			}]}`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover","repos":["owner/repo"],
		"tasks":[{"kind":"authored_pr","start":"2020-01-01","end":"2020-01-01","page":1}]
	}`)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	var state scanState
	if err := json.Unmarshal([]byte(page.NextCursor), &state); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if len(state.Tasks) != 2 || state.Tasks[0].Kind != "pull_request_details" ||
		state.Tasks[1].Kind != "authored_pr" || state.Tasks[1].Page != 2 {
		t.Errorf("follow-up tasks = %+v, want PR details before search page two", state.Tasks)
	}
}

func TestHistoryConnector_CollectsCountsWhenNoRepositoriesAreKnown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Errorf("request path = %q, want /search/issues", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"total_count":0,"incomplete_results":false,"items":[]}`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover",
		"tasks":[{"kind":"authored_issue","start":"2020-01-01","end":"2020-01-01","page":1}]
	}`)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	var state scanState
	if err := json.Unmarshal([]byte(page.NextCursor), &state); err != nil {
		t.Fatalf("decode cursor: %v", err)
	}
	if page.Done || len(state.Tasks) != 1 || state.Tasks[0].Kind != "counts" {
		t.Errorf("post-search state = done %t, tasks %+v; want GraphQL count task", page.Done, state.Tasks)
	}
	request.Cursor = page.NextCursor
	if bucket := connector.QuotaBucketFor(request); bucket != "github/graphql" {
		t.Errorf("QuotaBucketFor(count task) = %q, want github/graphql", bucket)
	}
}

func TestHistoryConnector_SearchSplitsCappedDateRanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":1000,"incomplete_results":false,"items":[]}`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover","repos":["owner/repo"],
		"tasks":[{"kind":"authored_pr","start":"2020-01-01","end":"2020-01-02","page":1}]
	}`)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	var state scanState
	if err := json.Unmarshal([]byte(page.NextCursor), &state); err != nil {
		t.Fatalf("decode split cursor: %v", err)
	}
	if len(state.Tasks) != 2 || state.Tasks[0].Start != "2020-01-01" ||
		state.Tasks[0].End != "2020-01-01" || state.Tasks[1].Start != "2020-01-02" {
		t.Errorf("split tasks = %+v, want two one-day ranges", state.Tasks)
	}
}

func TestHistoryConnector_VerifiesRepositoryBeforeStoringActivity(t *testing.T) {
	var verified bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search/issues":
			_, _ = w.Write([]byte(`{"total_count":1,"incomplete_results":false,"items":[{
				"number":7,"title":"private until checked","html_url":"https://github.com/owner/repo/issues/7",
				"repository_url":"` + server.URL + `/repos/owner/repo",
				"created_at":"2020-01-01T12:00:00Z","user":{"id":42}
			}]}`))
		case "/repos/owner/repo":
			verified = true
			_, _ = w.Write([]byte(`{"private":false,"visibility":"public"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover",
		"tasks":[{"kind":"authored_issue","start":"2020-01-01","end":"2020-01-01","page":1}]
	}`)
	first, firstReport, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(search): %v", err)
	}
	if len(first.Events) != 0 || len(firstReport.Errors) != 0 {
		t.Fatalf("FetchPage(search) = %+v, %+v; want no unverified events", first, firstReport)
	}
	request.Cursor = first.NextCursor
	second, secondReport, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(verify): %v", err)
	}
	if !verified || len(second.Events) != 1 || secondReport.RateLimitBucket != "github/core" {
		t.Fatalf("FetchPage(verify) = verified %t, events %d, bucket %q; want public verified event",
			verified, len(second.Events), secondReport.RateLimitBucket)
	}
}

func TestHistoryConnector_RejectsPrivateRepositoryEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo" {
			t.Errorf("request path = %q, want repository visibility lookup", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"private":true,"visibility":"private"}`))
	}))
	defer server.Close()

	record := json.RawMessage(`{"number":7,"user":{"id":42}}`)
	cursor, err := json.Marshal(scanState{
		Login: "octocat", AccountID: "42", Stage: "discover",
		Tasks: []scanTask{{Kind: "verify_repo", Repo: "owner/repo", EventType: "authored_issue", Records: []json.RawMessage{record}}},
	})
	if err != nil {
		t.Fatalf("encode test cursor: %v", err)
	}
	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, string(cursor))
	request.Cursor = string(cursor)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if len(page.Events) != 0 {
		t.Errorf("private repository events = %d, want zero", len(page.Events))
	}
}

func TestHistoryConnector_FetchesMergedPullRequestTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/12" {
			t.Errorf("request path = %q, want pull request details", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"number":12,"title":"merged change","html_url":"https://github.com/owner/repo/pull/12",
			"created_at":"2020-01-01T12:00:00Z","closed_at":"2020-01-03T12:00:00Z",
			"merged_at":"2020-01-03T11:00:00Z","user":{"id":42}
		}`))
	}))
	defer server.Close()
	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	cursor := `{"login":"octocat","account_id":"42","stage":"discover","repos":["owner/repo"],"tasks":[{"kind":"pull_request_details","owner":"owner","repo":"repo","number":12}]}`
	request := historyRequest(t, cursor)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].OccurredAt == nil {
		t.Fatalf("pull request page events = %+v, want one raw event", page.Events)
	}
	registry, err := NewHistoryNormalizer()
	if err != nil {
		t.Fatalf("NewHistoryNormalizer: %v", err)
	}
	actions, err := registry.Normalize(page.Events[0])
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(actions) != 2 || actions[1].Kind != "pull_request_merged" ||
		actions[1].OccurredAt.Hour() != 11 || actions[1].Project != "owner/repo" {
		t.Errorf("pull request actions = %+v, want source merged timestamp and project", actions)
	}
}

func TestHistoryConnector_FetchesReviewsWithStableRepositoryIdentity(t *testing.T) {
	reviewPayload := `{"id":41,"state":"APPROVED","html_url":"https://github.com/owner/repo/pull/12#pullrequestreview-41","submitted_at":"2020-01-02T03:04:05Z","user":{"id":42},"source_field":"preserved"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/12/reviews" {
			t.Errorf("request path = %q, want pull request reviews", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[` + reviewPayload + `,{
			"id":42,"state":"COMMENTED","submitted_at":"2020-01-02T03:04:05Z","user":{"id":99}
		}]`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover",
		"tasks":[{"kind":"reviews","owner":"owner","repo":"repo","number":12,"page":1}]
	}`)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("review events = %d, want only the matching reviewer", len(page.Events))
	}
	review := page.Events[0]
	if review.ExternalID != "review:owner/repo:41" || string(review.Payload) != reviewPayload {
		t.Errorf("review identity/payload = %q/%s, want stable repo ID and original JSON",
			review.ExternalID, review.Payload)
	}
	registry, err := NewHistoryNormalizer()
	if err != nil {
		t.Fatalf("NewHistoryNormalizer: %v", err)
	}
	actions, err := registry.Normalize(review)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(actions) != 1 || actions[0].Project != "owner/repo" {
		t.Errorf("normalized review = %+v, want owner/repo project", actions)
	}
}

func TestHistoryConnector_FetchesPublicReleaseAndPreservesSourcePayload(t *testing.T) {
	releasePayload := `{"id":77,"name":"v1","html_url":"https://github.com/owner/repo/releases/tag/v1","published_at":"2020-01-02T03:04:05Z","author":{"id":42},"source_field":"preserved"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/releases" {
			t.Errorf("request path = %q, want repository releases", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[` + releasePayload + `,{
			"id":78,"name":"other","published_at":"2020-01-02T03:04:05Z","author":{"id":99}
		}]`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"releases","repos":["owner/repo"],
		"tasks":[{"kind":"releases","repo":"owner/repo","page":1}]
	}`)
	page, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("release events = %d, want only the tracked author", len(page.Events))
	}
	release := page.Events[0]
	if release.ExternalID != "release:owner/repo:77" || string(release.Payload) != releasePayload {
		t.Errorf("release identity/payload = %q/%s, want stable repo ID and original JSON",
			release.ExternalID, release.Payload)
	}
	registry, err := NewHistoryNormalizer()
	if err != nil {
		t.Fatalf("NewHistoryNormalizer: %v", err)
	}
	actions, err := registry.Normalize(release)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(actions) != 1 || actions[0].Project != "owner/repo" {
		t.Errorf("normalized release = %+v, want owner/repo project", actions)
	}
}

func TestHistoryConnector_RateLimitRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, `{
		"login":"octocat","account_id":"42","stage":"discover",
		"tasks":[{"kind":"authored_issue","start":"2020-01-01","end":"2020-01-01","page":1}]
	}`)
	_, report, err := connector.FetchPage(t.Context(), request)
	if err == nil {
		t.Fatal("FetchPage error = nil, want rate-limit error")
	}
	if report.RateLimitRemaining == nil || *report.RateLimitRemaining != 0 ||
		report.RateLimitResetAt == nil || time.Until(*report.RateLimitResetAt) < 8*time.Second {
		t.Errorf("rate-limit report = %+v, want zero remaining and Retry-After reset", report)
	}
}

func TestHistoryConnector_RefreshesRenamedLoginFromStableNodeID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			t.Errorf("GraphQL request = %s %s, want POST /graphql", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"node":{"databaseId":42,"login":"new-login"}}}`))
	}))
	defer server.Close()

	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	request := historyRequest(t, "")
	request.AccountNodeID = "MDQ6VXNlcjQy"
	request.CurrentLogin = "old-login"
	if got := connector.QuotaBucketFor(request); got != "github/graphql" {
		t.Fatalf("QuotaBucketFor(initial node refresh) = %q, want github/graphql", got)
	}
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	var state scanState
	if err := json.Unmarshal([]byte(page.NextCursor), &state); err != nil {
		t.Fatalf("decode refreshed scan cursor: %v", err)
	}
	if state.Login != "new-login" || report.RateLimitBucket != "github/graphql" ||
		len(state.Tasks) == 0 || state.Tasks[0].Kind != "owned_repos" {
		t.Errorf("refreshed cursor/report = login %q, bucket %q, tasks %+v",
			state.Login, report.RateLimitBucket, state.Tasks)
	}
}

func TestHistoryConnector_StoresGraphQLAggregatesSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" || r.Method != http.MethodPost {
			t.Errorf("GraphQL request = %s %s, want POST /graphql", r.Method, r.URL.Path)
		}
		var request struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode GraphQL request: %v", err)
		}
		if !strings.Contains(request.Query, "includePrivateContributions:false") {
			t.Errorf("GraphQL query does not explicitly exclude private contributions: %q", request.Query)
		}
		_, _ = w.Write([]byte(`{"data":{"user":{"contributionsCollection":{
			"totalCommitContributions":4,"totalIssueContributions":2,
			"totalPullRequestContributions":3,"totalPullRequestReviewContributions":1,
			"contributionCalendar":{"weeks":[{"contributionDays":[
			  {"date":"2020-01-01","contributionCount":5}
			]}]}
		}}}}`))
	}))
	defer server.Close()
	connector := NewHistoryConnector(NewClient("", server.Client()))
	connector.client.baseURL = server.URL
	cursor := `{"login":"octocat","account_id":"42","stage":"counts","tasks":[{"kind":"counts","start":"2020-01-01T00:00:00Z","end":"2021-01-01T00:00:00Z"}]}`
	request := historyRequest(t, cursor)
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage: %v", err)
	}
	if len(page.Events) != 0 || !page.Done {
		t.Errorf("aggregate page events/done = %d/%t, want 0/true", len(page.Events), page.Done)
	}
	if len(report.ContributionCounts) != 5 || report.RateLimitBucket != "github/graphql" {
		t.Fatalf("FetchReport counts/bucket = %d/%q, want 5/graphql",
			len(report.ContributionCounts), report.RateLimitBucket)
	}
	if report.CoverageScope != historyCoverageScope {
		t.Errorf("final coverage scope = %q, want %q", report.CoverageScope, historyCoverageScope)
	}
	if report.ContributionCounts[0].Metric == "event" {
		t.Error("GraphQL aggregate was represented as a timeline event")
	}
}

func TestHistoryNormalizer_MapsSourceBackedActions(t *testing.T) {
	registry, err := NewHistoryNormalizer()
	if err != nil {
		t.Fatalf("NewHistoryNormalizer: %v", err)
	}
	occurred := time.Date(2022, 4, 1, 12, 0, 0, 0, time.UTC)
	forgeID := uuid.New()
	raw := event.RawEvent{
		ForgeInstanceID: forgeID, Source: historySource, ExternalID: "pull_request:owner/repo:12",
		Kind: "pull_request", OccurredAt: &occurred, PayloadSchemaVersion: 1,
		Payload: json.RawMessage(`{
			"title":"A change","html_url":"https://github.com/owner/repo/pull/12",
			"created_at":"2022-04-01T12:00:00Z","merged_at":"2022-04-03T12:00:00Z",
			"repository_url":"https://api.github.com/repos/owner/repo"
		}`),
		Actors: []event.Actor{{Key: event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: strconv.Itoa(42)},
			ForgeInstanceID: &forgeID,
		}, Role: "author"}},
	}
	actions, err := registry.Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(actions) != 2 || actions[0].Kind != "pull_request_opened" ||
		actions[1].Kind != "pull_request_merged" || actions[1].OccurredAt.Day() != 3 {
		t.Errorf("normalized actions = %+v, want opened and merged actions with source timestamps", actions)
	}
}

func historyRequest(t *testing.T, cursor string) event.PageRequest {
	t.Helper()
	forgeID := uuid.New()
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: forgeID, Source: historySource,
		PageSize: 100,
		IdentityKeys: []event.ActorKey{
			{IdentityKey: identity.Key{Kind: identity.KindLogin, Value: "octocat"}, ForgeInstanceID: &forgeID},
			{IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "42"}, ForgeInstanceID: &forgeID},
		},
		Window: event.Window{
			From: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC),
		},
		Cursor: cursor,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("validate test request: %v", err)
	}
	return request
}
