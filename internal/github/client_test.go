package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_Repositories_paginatesAndAuthenticates(t *testing.T) {
	var requests int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("Authorization = %q, want bearer token", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("page") == "" && r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q, want 100", r.URL.Query().Get("per_page"))
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", `<`+server.URL+`/users/octocat/repos?page=2>; rel="next"`)
			if _, err := w.Write([]byte(`[{"name":"one"}]`)); err != nil {
				t.Errorf("write first page: %v", err)
			}
		case "2":
			if _, err := w.Write([]byte(`[{"name":"two"}]`)); err != nil {
				t.Errorf("write second page: %v", err)
			}
		default:
			t.Errorf("unexpected request URL: %s", r.URL)
			http.Error(w, "unexpected page", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	client := NewClient("test-token", server.Client())
	client.baseURL = server.URL
	names, err := client.Repositories(context.Background(), "octocat")
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if got, want := strings.Join(names, ","), "one,two"; got != want {
		t.Errorf("Repositories = %q, want %q", got, want)
	}
	if requests != 2 {
		t.Errorf("request count = %d, want 2", requests)
	}
}

func TestClient_Repositories_rejectsExternalPaginationURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<https://example.com/steal-token>; rel="next"`)
		if _, err := w.Write([]byte(`[]`)); err != nil {
			t.Errorf("write page: %v", err)
		}
	}))
	defer server.Close()

	client := NewClient("secret", server.Client())
	client.baseURL = server.URL
	_, err := client.Repositories(context.Background(), "octocat")
	if err == nil || !strings.Contains(err.Error(), "invalid GitHub pagination URL") {
		t.Fatalf("Repositories error = %v, want invalid pagination URL", err)
	}
}

func TestClient_Repositories_rejectsInvalidUsername(t *testing.T) {
	client := NewClient("", nil)
	for _, user := range []string{"", "..", "octo/cat", "-octocat", "octocat-"} {
		if _, err := client.Repositories(context.Background(), user); err == nil {
			t.Errorf("Repositories(%q) error = nil, want validation error", user)
		}
	}
}
