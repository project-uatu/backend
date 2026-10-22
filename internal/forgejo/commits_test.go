// SPDX-License-Identifier: Apache-2.0

package forgejo

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

func TestCommitConnector_pagesAndMatchesLinkedAccount(t *testing.T) {
	forgeID := uuid.New()
	var commitRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/org/app":
			_, _ = w.Write([]byte(`{"id":11,"full_name":"org/app","private":false}`))
		case "/repos/org/app/commits":
			commitRequests++
			if r.URL.Query().Get("limit") != "100" || r.URL.Query().Get("since") == "" ||
				r.URL.Query().Get("until") == "" {
				t.Errorf("commit query = %v; want bounded request", r.URL.Query())
			}
			if r.URL.Query().Get("sha") != "" {
				t.Error("request must use the repository's default branch")
			}
			if commitRequests == 1 {
				w.Header().Set("X-Hasmore", "true")
			}
			sha := "def"
			if commitRequests == 1 {
				sha = "abc"
			}
			_, _ = w.Write([]byte(`[{"sha":"` + sha + `","html_url":"https://codeberg.org/org/app/commit/` + sha + `",
				"commit":{"message":"private message","author":{"email":"hidden@example.com","date":"2025-01-02T03:04:05Z"}},
				"author":{"id":42},"parents":[{"sha":"parent"}]}]`))
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
		ForgeInstanceID: forgeID, Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"org/app"},
		IdentityKeys: []event.ActorKey{{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: "42"},
			ForgeInstanceID: &forgeID,
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
	if first.Done || !second.Done || len(first.Events) != 1 || len(second.Events) != 1 {
		t.Fatalf("first = %+v, second = %+v; want two pages", first, second)
	}
	raw := first.Events[0]
	if raw.ExternalID != "commit:11:abc" || len(raw.Actors) != 1 ||
		raw.Actors[0].Role != "author" {
		t.Errorf("event = %+v, want linked author on commit:11:abc", raw)
	}
	if strings.Contains(string(raw.Payload), "hidden@example.com") ||
		strings.Contains(string(raw.Payload), "private message") {
		t.Errorf("payload contains private commit details: %s", raw.Payload)
	}
	if commitRequests != 2 || report.RateLimitBucket != quotaBucket {
		t.Errorf("commit requests = %d, bucket = %q", commitRequests, report.RateLimitBucket)
	}
}

func TestCommitConnector_skipsPrivateRepository(t *testing.T) {
	testCommitConnector_skipsRepository(t, `{"id":11,"full_name":"org/app","private":true}`)
}

func TestCommitConnector_skipsFork(t *testing.T) {
	testCommitConnector_skipsRepository(t, `{"id":11,"full_name":"org/app","private":false,"fork":true}`)
}

func testCommitConnector_skipsRepository(t *testing.T, metadata string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(metadata))
	}))
	defer server.Close()
	connector, err := NewCommitConnector(Options{APIURL: server.URL, HTTPClient: server.Client()}, 5, 3)
	if err != nil {
		t.Fatal(err)
	}
	request := event.PageRequest{
		ForgeInstanceID: uuid.New(), Source: CommitSource, PageSize: 100,
		KnownRepositories: []string{"org/app"},
	}
	page, report, err := connector.FetchPage(t.Context(), request)
	if err != nil {
		t.Fatalf("FetchPage() error = %v", err)
	}
	if !page.Done || len(page.Events) != 0 || len(report.Gaps) != 1 {
		t.Errorf("page = %+v, gaps = %+v; want done, no events, one gap", page, report.Gaps)
	}
}

func TestBuildForgejoCommit_acceptsRootCommitAndRejectsMissingParents(t *testing.T) {
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
			body := `{"sha":"abc","html_url":"https://codeberg.org/org/app/commit/abc",
				"commit":{"author":{"date":"2025-01-02T03:04:05Z"}},"author":{"id":42}`
			if test.parents != "" {
				body += "," + test.parents
			}
			var record forgejoCommit
			if err := json.Unmarshal([]byte(body+"}"), &record); err != nil {
				t.Fatal(err)
			}
			_, ok, err := buildForgejoCommit(request, "org/app", 7, record)
			if (err != nil) != test.wantErr || (err == nil && !ok) {
				t.Errorf("buildForgejoCommit() ok = %t, err = %v; wantErr %t", ok, err, test.wantErr)
			}
		})
	}
}
