// SPDX-License-Identifier: Apache-2.0

package forgehttp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/project-uatu/backend/internal/event"
)

func TestParseRateLimit(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name          string
		header        http.Header
		wantLimit     int
		wantRemaining int
		wantReset     time.Time
	}{
		{
			name: "GitLab headers",
			header: http.Header{
				"Ratelimit-Limit": {"500"}, "Ratelimit-Remaining": {"497"},
				"Ratelimit-Reset": {"1792331880"},
			},
			wantLimit: 500, wantRemaining: 497, wantReset: time.Unix(1792331880, 0).UTC(),
		},
		{
			name: "IETF draft headers",
			header: http.Header{
				"Ratelimit":        {`"baseline";r=1996;t=600`},
				"Ratelimit-Policy": {`"baseline";q=2000;w=600`},
			},
			wantLimit: 2000, wantRemaining: 1996, wantReset: now.Add(600 * time.Second),
		},
		{
			name:          "Retry-After exhausts the bucket",
			header:        http.Header{"Retry-After": {"30"}, "Ratelimit-Remaining": {"7"}},
			wantRemaining: 0, wantReset: now.Add(30 * time.Second),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var report event.FetchReport
			ParseRateLimit(tt.header, now, &report)
			if tt.wantLimit != 0 && (report.RateLimitLimit == nil || *report.RateLimitLimit != tt.wantLimit) {
				t.Errorf("limit = %v, want %d", report.RateLimitLimit, tt.wantLimit)
			}
			if report.RateLimitRemaining == nil || *report.RateLimitRemaining != tt.wantRemaining {
				t.Errorf("remaining = %v, want %d", report.RateLimitRemaining, tt.wantRemaining)
			}
			if report.RateLimitResetAt == nil || !report.RateLimitResetAt.Equal(tt.wantReset) {
				t.Errorf("reset = %v, want %v", report.RateLimitResetAt, tt.wantReset)
			}
		})
	}
}

func TestClient_getSendsIdentifyingHeadersAndReportsStatus(t *testing.T) {
	var gotAgent, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent, gotAuth = r.Header.Get("User-Agent"), r.Header.Get("Authorization")
		if r.URL.Path == "/limited" {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "secret upstream body", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := New(Options{BaseURL: server.URL + "/", Token: "tok", HTTPClient: server.Client()})

	res, report, err := client.Get(t.Context(), "/ok", url.Values{"a": {"b"}}, "bucket")
	if err != nil || string(res.Body) != `{"ok":true}` || report.RateLimitBucket != "bucket" {
		t.Fatalf("Get(/ok) = %v, %+v, %v", res, report, err)
	}
	if !strings.HasPrefix(gotAgent, "uatu/") || !strings.Contains(gotAgent, "project-uatu/backend") || gotAuth != "Bearer tok" {
		t.Errorf("headers: User-Agent %q, Authorization %q", gotAgent, gotAuth)
	}

	_, report, err = client.Get(t.Context(), "/limited", nil, "bucket")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("Get(/limited) error = %v, want 429 StatusError", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks response content: %v", err)
	}
	if report.RateLimitRemaining == nil || *report.RateLimitRemaining != 0 || report.RateLimitResetAt == nil {
		t.Errorf("429 report = %+v, want exhausted bucket with reset", report)
	}
}

func TestClient_getBoundsResponseAndHonorsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", MaxBody+1)))
	}))
	defer server.Close()
	client := New(Options{BaseURL: server.URL, HTTPClient: server.Client()})
	if _, _, err := client.Get(t.Context(), "/big", nil, "b"); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("oversized response error = %v, want size error", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := client.Get(ctx, "/x", nil, "b"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled request error = %v, want context.Canceled", err)
	}
}

func TestHasNextPage(t *testing.T) {
	if !HasNextPage(http.Header{"Link": {`<https://x/?page=2>; rel="next", <https://x/?page=5>; rel="last"`}}) {
		t.Error("HasNextPage with rel=next = false")
	}
	if HasNextPage(http.Header{"Link": {`<https://x/?page=1>; rel="prev"`}}) {
		t.Error("HasNextPage without rel=next = true")
	}
}
