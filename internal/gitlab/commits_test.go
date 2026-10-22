// SPDX-License-Identifier: Apache-2.0

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

func TestCommitConnector_pagesAndMatchesAssignedEmail(t *testing.T) {
	var commitRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/projects/team%2Fapp":
			_, _ = w.Write([]byte(`{"id":9,"path_with_namespace":"team/app","visibility":"public"}`))
		case "/projects/team%2Fapp/repository/commits":
			commitRequests++
			if r.URL.Query().Get("per_page") != "100" ||
				r.URL.Query().Get("since") == "" || r.URL.Query().Get("until") == "" {
				t.Errorf("commit query = %v; expected bounded 100-item request", r.URL.Query())
			}
			switch r.URL.Query().Get("page") {
			case "1":
				w.Header().Set("X-Next-Page", "2")
				_, _ = w.Write([]byte(`[{
					"id":"abc","web_url":"https://gitlab.com/team/app/-/commit/abc",
					"author_email":"writer@example.com","authored_date":"2025-01-02T03:04:05Z",
					"message":"private message","parent_ids":["parent"]
				}]`))
			default:
				_, _ = w.Write([]byte(`[{
					"id":"def","web_url":"https://gitlab.com/team/app/-/commit/def",
					"author_email":"writer@example.com","authored_date":"2025-01-03T03:04:05Z",
					"message":"merge","parent_ids":["one","two"]
				}]`))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	connector, err := NewCommitConnector(Options{APIURL: server.URL, HTTPClient: server.Client()}, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"team/app"},
		IdentityKeys: []event.ActorKey{{
			IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "writer@example.com"},
		}},
		Window: event.Window{
			From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			To:   time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	verify, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(verify) error = %v", err)
	}
	request.Cursor = verify.NextCursor
	first, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(page 1) error = %v", err)
	}
	request.Cursor = first.NextCursor
	second, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(page 2) error = %v", err)
	}
	if !second.Done || len(first.Events) != 1 || len(second.Events) != 0 {
		t.Fatalf("first = %+v, second = %+v; want one authored event and merge excluded", first, second)
	}
	raw := first.Events[0]
	if raw.ExternalID != "commit:9:abc" || len(raw.Actors) != 1 ||
		raw.Actors[0].Role != "author" || raw.Actors[0].Key.IdentityKey.Value != "writer@example.com" {
		t.Errorf("event = %+v, want assigned email author", raw)
	}
	if strings.Contains(string(raw.Payload), "writer@example.com") ||
		strings.Contains(string(raw.Payload), "private message") {
		t.Errorf("payload contains private commit details: %s", raw.Payload)
	}
	if commitRequests != 2 || report.RateLimitBucket != quotaBucket {
		t.Errorf("commit requests = %d, bucket = %q", commitRequests, report.RateLimitBucket)
	}
}

func TestCommitConnector_requiresAssignedEmail(t *testing.T) {
	connector, err := NewCommitConnector(Options{APIURL: "http://127.0.0.1:1"}, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"team/app"},
	}
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage() error = %v", err)
	}
	if !page.Done || len(report.Gaps) != 1 {
		t.Errorf("page = %+v, gaps = %+v; want done with one attribution gap", page, report.Gaps)
	}
}

func TestCommitConnector_skipsForkProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":9,"path_with_namespace":"team/app","visibility":"public","forked_from_project":{"id":8}}`))
	}))
	defer server.Close()
	connector, err := NewCommitConnector(Options{APIURL: server.URL, HTTPClient: server.Client()}, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"team/app"},
		IdentityKeys:      []event.ActorKey{{IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "writer@example.com"}}},
	}
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage() error = %v", err)
	}
	if !page.Done || len(page.Events) != 0 || len(report.Gaps) != 1 {
		t.Errorf("page = %+v, gaps = %+v; want done, no events, one gap", page, report.Gaps)
	}
}

func TestBuildGitLabCommit_acceptsRootCommitAndRejectsMissingParents(t *testing.T) {
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource,
		IdentityKeys: []event.ActorKey{{IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "writer@example.com"}}},
	}
	tests := []struct {
		name    string
		parents string
		wantErr bool
	}{
		{name: "root commit", parents: `"parent_ids":[]`},
		{name: "missing parents", wantErr: true},
		{name: "null parents", parents: `"parent_ids":null`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"id":"abc","web_url":"https://gitlab.com/org/app/-/commit/abc",
				"author_email":"writer@example.com","authored_date":"2025-01-02T03:04:05Z"`
			if test.parents != "" {
				body += "," + test.parents
			}
			var record gitLabCommit
			if err := json.Unmarshal([]byte(body+"}"), &record); err != nil {
				t.Fatal(err)
			}
			_, ok, err := buildGitLabCommit(request, "org/app", 7, record)
			if (err != nil) != test.wantErr || (err == nil && !ok) {
				t.Errorf("buildGitLabCommit() ok = %t, err = %v; wantErr %t", ok, err, test.wantErr)
			}
		})
	}
}
