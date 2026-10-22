// SPDX-License-Identifier: Apache-2.0

package github

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

func TestCommitConnector_pagesAndAttributesLinkedAuthor(t *testing.T) {
	forgeID := uuid.New()
	var commitRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/app":
			_, _ = w.Write([]byte(`{"id":7,"full_name":"org/app","private":false,"visibility":"public"}`))
		case "/repos/org/app/commits":
			commitRequests++
			if r.URL.Query().Get("author") != "jane" {
				t.Errorf("author filter = %q, want jane", r.URL.Query().Get("author"))
			}
			if r.URL.Query().Get("per_page") != "100" {
				t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
			}
			w.Header().Set("Link", `</repos/org/app/commits?page=2>; rel="next"`)
			_, _ = w.Write([]byte(`[{
				"sha":"abc","html_url":"https://github.com/org/app/commit/abc",
				"commit":{"message":"private message","author":{"email":"private@example.com","date":"2025-01-02T03:04:05Z"}},
				"author":{"id":42},"parents":[{"sha":"parent"}]
			}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient("", server.Client())
	client.baseURL = server.URL
	connector, err := NewCommitConnector(client, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: forgeID, Source: CommitSource, PageSize: 100,
		CurrentLogin: "jane", KnownRepositories: []string{"org/app"},
		IdentityKeys: []event.ActorKey{{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: "42"},
			ForgeInstanceID: &forgeID,
		}},
	}
	verify, _, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(verify) error = %v", err)
	}
	request.Cursor = verify.NextCursor
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage(commits) error = %v", err)
	}
	if page.Done || page.NextCursor == "" || len(page.Events) != 1 {
		t.Fatalf("page = %+v, want one event and a next cursor", page)
	}
	raw := page.Events[0]
	if err := raw.Validate(forgeID); err != nil {
		t.Fatalf("event validation error = %v", err)
	}
	if raw.ExternalID != "commit:7:abc" || raw.Kind != "commit" ||
		len(raw.Actors) != 1 || raw.Actors[0].Role != "author" {
		t.Errorf("event = %+v, want linked author on commit:7:abc", raw)
	}
	if strings.Contains(string(raw.Payload), "private@example.com") ||
		strings.Contains(string(raw.Payload), "private message") {
		t.Errorf("payload contains private commit details: %s", raw.Payload)
	}
	if report.RateLimitBucket != commitBucket || commitRequests != 1 {
		t.Errorf("bucket = %q, commit requests = %d", report.RateLimitBucket, commitRequests)
	}
}

func TestCommitConnector_skipsPrivateRepository(t *testing.T) {
	testCommitConnector_skipsRepository(t, `{"id":7,"full_name":"org/app","private":true,"visibility":"private"}`)
}

func TestCommitConnector_skipsFork(t *testing.T) {
	testCommitConnector_skipsRepository(t, `{"id":7,"full_name":"org/app","private":false,"fork":true,"visibility":"public"}`)
}

func testCommitConnector_skipsRepository(t *testing.T, metadata string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(metadata))
	}))
	defer server.Close()
	client := NewClient("", server.Client())
	client.baseURL = server.URL
	connector, err := NewCommitConnector(client, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"org/app"},
		Window:            event.Window{From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage() error = %v", err)
	}
	if !page.Done || len(page.Events) != 0 || len(report.Gaps) != 1 {
		t.Errorf("page = %+v, report gaps = %+v; want done, no events, one gap", page, report.Gaps)
	}
}

func TestBuildGitHubCommit_acceptsRootCommitAndRejectsMissingParents(t *testing.T) {
	forgeID := uuid.New()
	request := event.PageRequest{
		ForgeInstanceID: forgeID, Source: CommitSource,
		IdentityKeys: []event.ActorKey{{
			IdentityKey: identity.Key{Kind: identity.KindAccountID, Value: "42"}, ForgeInstanceID: &forgeID,
		}},
	}
	tests := []struct {
		name    string
		parents string
		wantErr bool
	}{
		{name: "root commit", parents: `"parents":[]`},
		{name: "missing parents", wantErr: true},
		{name: "null parents", parents: `"parents":null`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := `{"sha":"abc","html_url":"https://github.com/org/app/commit/abc",
				"commit":{"author":{"date":"2025-01-02T03:04:05Z"}},"author":{"id":42}`
			if test.parents != "" {
				body += "," + test.parents
			}
			var record githubCommit
			if err := json.Unmarshal([]byte(body+"}"), &record); err != nil {
				t.Fatal(err)
			}
			_, ok, err := buildGitHubCommit(request, "org/app", 7, record)
			if (err != nil) != test.wantErr || (err == nil && !ok) {
				t.Errorf("buildGitHubCommit() ok = %t, err = %v; wantErr %t", ok, err, test.wantErr)
			}
		})
	}
}
