package gitlab

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/identity"
)

const fakeProjects = `{
 "10": {"id":10,"path_with_namespace":"pub/app","web_url":"https://gitlab.com/pub/app","visibility":"public"},
 "20": {"id":20,"path_with_namespace":"corp/secret","web_url":"https://gitlab.com/corp/secret","visibility":"private"}}`

func fakeGitLab(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	var projects map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fakeProjects), &projects); err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.URL.Path)
		if r.Header.Get("User-Agent") == "" {
			t.Error("request without User-Agent")
		}
		w.Header().Set("RateLimit-Remaining", "400")
		switch {
		case r.URL.Path == "/users":
			if r.URL.Query().Get("username") != "jane.doe" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"id":42,"username":"jane.doe","email":"leak@example.com"}]`))
		case r.URL.Path == "/issues":
			if r.URL.Query().Get("author_id") != "42" || r.URL.Query().Get("page") != "1" {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[
			 {"iid":1,"project_id":10,"title":"public issue","state":"closed","web_url":"https://gitlab.com/pub/app/-/issues/1","created_at":"2024-02-01T10:00:00Z","closed_at":"2024-02-02T10:00:00Z","author":{"email":"leak@example.com"}},
			 {"iid":2,"project_id":20,"title":"private issue","state":"opened","web_url":"x","created_at":"2024-02-01T10:00:00Z"}]`))
		case r.URL.Path == "/merge_requests":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/users/42/events":
			_, _ = w.Write([]byte(`[{"id":99,"project_id":10,"target_iid":7,"target_title":"Add thing","created_at":"2024-03-01T10:00:00Z"}]`))
		case strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/releases"):
			_, _ = w.Write([]byte(`[
			 {"tag_name":"v1","name":"","released_at":"2024-04-01T00:00:00Z","author":{"id":42}},
			 {"tag_name":"v0","released_at":"2024-01-01T00:00:00Z","author":{"id":7}}]`))
		case strings.HasPrefix(r.URL.Path, "/projects/"):
			id := strings.TrimPrefix(r.URL.Path, "/projects/")
			if id == "pub/app" {
				id = "10"
			}
			if project, ok := projects[id]; ok {
				_, _ = w.Write(project)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
}

func drain(t *testing.T, c *Connector, request event.PageRequest) ([]event.RawEvent, *event.ResolvedAccount, event.FetchReport) {
	t.Helper()
	var all []event.RawEvent
	var resolved *event.ResolvedAccount
	var last event.FetchReport
	for range 200 {
		page, report, err := c.FetchPage(t.Context(), request)
		if err != nil {
			t.Fatalf("FetchPage: %v", err)
		}
		if page.ResolvedAccount != nil {
			resolved = page.ResolvedAccount
			request.IdentityKeys = append(request.IdentityKeys, event.ActorKey{
				IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: page.ResolvedAccount.AccountID},
			})
		}
		all = append(all, page.Events...)
		last = report
		if page.Done {
			return all, resolved, last
		}
		request.Cursor = page.NextCursor
	}
	t.Fatal("scan did not finish")
	return nil, nil, last
}

func TestConnector_collectsOnlyPublicActivity(t *testing.T) {
	var seen []string
	server := fakeGitLab(t, &seen)
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "backfill",
		OnboardingLogin: "jane.doe", PageSize: 50,
		Window:            event.Window{From: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)},
		KnownRepositories: []string{"pub/app"},
	}
	events, resolved, report := drain(t, c, request)
	if resolved == nil || resolved.AccountID != "42" || resolved.Login != "jane.doe" || !resolved.CreatedAt.IsZero() {
		t.Fatalf("resolved account = %+v, want ID 42 without creation time", resolved)
	}
	got := map[string]event.RawEvent{}
	for _, e := range events {
		got[e.ExternalID] = e
		if err := e.Validate(request.ForgeInstanceID); err != nil {
			t.Errorf("event %s invalid: %v", e.ExternalID, err)
		}
		if strings.Contains(string(e.Payload), "leak@example.com") || strings.Contains(string(e.Payload), "secret") {
			t.Errorf("event %s payload leaks data: %s", e.ExternalID, e.Payload)
		}
	}
	for _, id := range []string{"issue:10:1", "approval:99", "release:pub/app:v1"} {
		if _, ok := got[id]; !ok {
			t.Errorf("missing event %q; got %v", id, keys(got))
		}
	}
	if _, ok := got["issue:20:2"]; ok {
		t.Error("private project issue was persisted")
	}
	if len(got) != 3 {
		t.Errorf("got %d events %v, want 3", len(got), keys(got))
	}
	if report.RateLimitBucket != "gitlab/gitlab.com/api" || report.CoverageScope == "" {
		t.Errorf("report = %+v", report)
	}
}

func TestConnector_unavailableApprovalsBecomeGap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "incremental",
		IdentityKeys: []event.ActorKey{{IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "42"}}},
		Window:       event.Window{From: time.Now().Add(-time.Hour)}, PageSize: 50,
	}
	gaps := 0
	for range 20 {
		page, report, err := c.FetchPage(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		gaps += len(report.Gaps)
		if page.Done {
			break
		}
		request.Cursor = page.NextCursor
	}
	if gaps != 1 {
		t.Errorf("gaps = %d, want 1", gaps)
	}
}

func TestConnector_rejectsBadInput(t *testing.T) {
	c := New(Options{APIURL: "http://127.0.0.1:1"})
	base := event.PageRequest{PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, PageSize: 50}
	bad := base
	bad.OnboardingLogin = "-bad"
	if _, _, err := c.FetchPage(t.Context(), bad); err == nil {
		t.Error("invalid login succeeded")
	}
	bad = base
	bad.Cursor = "{not json"
	if _, _, err := c.FetchPage(t.Context(), bad); err == nil {
		t.Error("invalid cursor succeeded")
	}
}

func keys(m map[string]event.RawEvent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
