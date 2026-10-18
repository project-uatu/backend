package forgejo

import (
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

const page1 = `[
 {"id":9,"op_type":"create_issue","act_user_id":5,"content":"[\"12\",\"Bug title\"]","created":"2025-12-30T10:00:00+01:00","is_private":false,
  "act_user":{"email":"leak@example.com"},"repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}},
 {"id":8,"op_type":"merge_pull_request","act_user_id":5,"content":"[\"7\",\"Fix\"]","created":"2025-12-29T10:00:00Z",
  "repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}},
 {"id":7,"op_type":"approve_pull_request","act_user_id":5,"content":"[\"7\",\"secret review body\"]","created":"2025-12-28T10:00:00Z",
  "repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}},
 {"id":6,"op_type":"create_issue","act_user_id":5,"content":"[\"1\",\"private\"]","created":"2025-12-27T10:00:00Z",
  "repo":{"id":4,"full_name":"org/hidden","private":true,"html_url":"https://codeberg.org/org/hidden"}},
 {"id":5,"op_type":"create_issue","act_user_id":99,"content":"[\"1\",\"someone else\"]","created":"2025-12-27T10:00:00Z",
  "repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}},
 {"id":4,"op_type":"publish_release","act_user_id":5,"ref_name":"refs/tags/v1.0","content":"v1.0","created":"2025-12-26T10:00:00Z",
  "repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}},
 {"id":3,"op_type":"comment_issue","act_user_id":5,"content":"[\"1\",\"hi\"]","created":"2025-12-25T10:00:00Z",
  "repo":{"id":3,"full_name":"org/app","private":false,"html_url":"https://codeberg.org/org/app"}}]`

func fakeCodeberg(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("request without User-Agent")
		}
		switch {
		case r.URL.Path == "/users/jane_doe":
			_, _ = w.Write([]byte(`{"id":5,"login":"jane_doe","created":"2020-01-01T00:00:00Z","email":"leak@example.com"}`))
		case strings.HasSuffix(r.URL.Path, "/activities/feeds"):
			if r.URL.Query().Get("only-performed-by") != "true" {
				t.Error("feed must request only own activity")
			}
			if r.URL.Query().Get("page") == "1" {
				_, _ = w.Write([]byte(page1))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/heatmap"):
			_, _ = w.Write([]byte(`[{"timestamp":1760283900,"contributions":1},{"timestamp":1760285700,"contributions":2}]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func drain(t *testing.T, c *Connector, request event.PageRequest) ([]event.RawEvent, *event.ResolvedAccount, []event.FetchReport) {
	t.Helper()
	var all []event.RawEvent
	var resolved *event.ResolvedAccount
	var reports []event.FetchReport
	for range 20 {
		page, report, err := c.FetchPage(t.Context(), request)
		if err != nil {
			t.Fatalf("FetchPage: %v", err)
		}
		if page.ResolvedAccount != nil {
			resolved = page.ResolvedAccount
		}
		all = append(all, page.Events...)
		reports = append(reports, report)
		if page.Done {
			return all, resolved, reports
		}
		request.Cursor = page.NextCursor
	}
	t.Fatal("scan did not finish")
	return nil, nil, nil
}

func TestConnector_collectsOnlyPublicOwnActivity(t *testing.T) {
	server := fakeCodeberg(t)
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "backfill",
		OnboardingLogin: "jane_doe", PageSize: 50,
	}
	events, resolved, reports := drain(t, c, request)
	if resolved == nil || resolved.AccountID != "5" || resolved.CreatedAt.IsZero() {
		t.Fatalf("resolved = %+v", resolved)
	}
	want := map[string]bool{
		"issue:3:12": true, "merge:3:7": true, "activity:7": true, "release:3:v1.0": true,
	}
	for _, e := range events {
		if !want[e.ExternalID] {
			t.Errorf("unexpected event %q", e.ExternalID)
		}
		delete(want, e.ExternalID)
		if err := e.Validate(request.ForgeInstanceID); err != nil {
			t.Errorf("event %s invalid: %v", e.ExternalID, err)
		}
		if p := string(e.Payload); strings.Contains(p, "leak@example.com") || strings.Contains(p, "secret review body") {
			t.Errorf("event %s payload leaks data: %s", e.ExternalID, p)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing events %v", want)
	}
	last := reports[len(reports)-1]
	if len(last.ContributionCounts) != 1 || last.ContributionCounts[0].Value != 3 {
		t.Errorf("heatmap counts = %+v, want one day totalling 3", last.ContributionCounts)
	}
	if len(last.Gaps) != 1 {
		t.Errorf("gaps = %+v, want the feed-boundary gap since the account predates the earliest entry", last.Gaps)
	}
}

func TestConnector_incrementalStopsAtCutoff(t *testing.T) {
	server := fakeCodeberg(t)
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "incremental",
		OnboardingLogin: "jane_doe", PageSize: 50,
		Window: event.Window{From: time.Date(2025, 12, 29, 12, 0, 0, 0, time.UTC)},
	}
	events, _, reports := drain(t, c, request)
	if len(events) != 2 {
		t.Errorf("got %d events, want entries within the overlap window only", len(events))
	}
	if gaps := reports[len(reports)-1].Gaps; len(gaps) != 0 {
		t.Errorf("incremental scan reported gaps %+v", gaps)
	}
}

func TestParseContent(t *testing.T) {
	tests := []struct{ in, index, title string }{
		{`["12","Title"]`, "12", "Title"},
		{`["12"]`, "12", ""},
		{`["x","Title"]`, "", ""},
		{`not json`, "", ""},
	}
	for _, tt := range tests {
		if index, title := parseContent(tt.in); index != tt.index || title != tt.title {
			t.Errorf("parseContent(%q) = %q, %q", tt.in, index, title)
		}
	}
}

func TestConnector_refreshesRenamedAccountByID(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch {
		case r.URL.Path == "/users/search":
			if r.URL.Query().Get("uid") != "5" {
				t.Errorf("uid = %q, want 5", r.URL.Query().Get("uid"))
			}
			_, _ = w.Write([]byte(`{"ok":true,"data":[{"id":5,"login":"new_name","created":"2020-01-01T00:00:00Z"}]}`))
		case strings.HasSuffix(r.URL.Path, "/activities/feeds"), strings.HasSuffix(r.URL.Path, "/heatmap"):
			if !strings.HasPrefix(r.URL.Path, "/users/new_name/") {
				t.Errorf("path %q used a stale login", r.URL.Path)
			}
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "backfill",
		CurrentLogin: "old_name", PageSize: 50,
		IdentityKeys: []event.ActorKey{{IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "5"}}},
	}
	_, resolved, _ := drain(t, c, request)
	if resolved == nil || resolved.Login != "new_name" || resolved.AccountID != "5" {
		t.Fatalf("resolved = %+v, want refreshed login new_name", resolved)
	}
}

func TestConnector_incrementalGapNeedsHeatmapEvidence(t *testing.T) {
	heatmap := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/users/search":
			_, _ = w.Write([]byte(`{"data":[{"id":5,"login":"jane_doe"}]}`))
		case strings.HasSuffix(r.URL.Path, "/activities/feeds"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/heatmap"):
			_, _ = w.Write([]byte(heatmap))
		}
	}))
	defer server.Close()
	c := New(Options{APIURL: server.URL, HTTPClient: server.Client()})
	from := time.Now().UTC().AddDate(0, 0, -30)
	request := event.PageRequest{
		PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, ScanMode: "incremental",
		CurrentLogin: "jane_doe", PageSize: 50, Window: event.Window{From: from, To: time.Now().UTC()},
		IdentityKeys: []event.ActorKey{{IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "5"}}},
	}
	gaps := func() int {
		_, _, reports := drain(t, c, request)
		n := 0
		for _, report := range reports {
			n += len(report.Gaps)
		}
		return n
	}
	heatmap = `[]`
	if n := gaps(); n != 0 {
		t.Errorf("quiet account reported %d gaps, want 0", n)
	}
	heatmap = `[{"timestamp":` + strconv.FormatInt(from.AddDate(0, 0, 3).Unix(), 10) + `,"contributions":4}]`
	if n := gaps(); n != 1 {
		t.Errorf("hidden contributions reported %d gaps, want 1", n)
	}
}

func TestConnector_rejectsBadInput(t *testing.T) {
	c := New(Options{APIURL: "http://127.0.0.1:1"})
	request := event.PageRequest{PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: Source, PageSize: 50, OnboardingLogin: "bad/login"}
	if _, _, err := c.FetchPage(t.Context(), request); err == nil {
		t.Error("invalid login succeeded")
	}
	request.OnboardingLogin, request.Cursor = "jane", "{bad"
	if _, _, err := c.FetchPage(t.Context(), request); err == nil {
		t.Error("invalid cursor succeeded")
	}
}
