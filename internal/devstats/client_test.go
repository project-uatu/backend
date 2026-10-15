package devstats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_Contributions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		var request struct {
			API     string            `json:"api"`
			Payload map[string]string `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.API != "GithubIDContributions" || request.Payload["github_id"] != "octocat" {
			t.Errorf("request = %+v, want contribution query for octocat", request)
		}
		if _, err := w.Write([]byte(`{"contributions":2,"issues":1,"prs":0}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	client := NewClient(server.Client())
	client.baseURL = server.URL
	got, err := client.Contributions(context.Background(), "octocat")
	if err != nil {
		t.Fatalf("Contributions: %v", err)
	}
	want := Contribution{Contributions: 2, Issues: 1, PRs: 0}
	if got != want {
		t.Errorf("Contributions = %+v, want %+v", got, want)
	}
}

func TestClient_Contributions_returnsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewClient(server.Client())
	client.baseURL = server.URL
	_, err := client.Contributions(context.Background(), "octocat")
	if err == nil {
		t.Fatal("Contributions error = nil, want upstream status error")
	}
}
