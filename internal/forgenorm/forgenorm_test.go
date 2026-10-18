// SPDX-License-Identifier: Apache-2.0

package forgenorm

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/normalizer"
)

func TestRegister_rejectsDuplicateSource(t *testing.T) {
	registry := normalizer.New()
	if err := Register(registry, "gitlab_history"); err != nil {
		t.Fatal(err)
	}
	if err := Register(registry, "gitlab_history"); err == nil {
		t.Error("registering the same source twice succeeded")
	}
	if err := Register(registry, "forgejo_history"); err != nil {
		t.Errorf("registering a second source: %v", err)
	}
}

func TestHandlers(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	merged := created.Add(48 * time.Hour)
	closed := created.Add(24 * time.Hour)
	raw := func(kind event.Kind, payload string, at *time.Time) event.RawEvent {
		return event.RawEvent{ForgeInstanceID: uuid.New(), Source: "gitlab_history", ExternalID: "x",
			Kind: kind, Payload: []byte(payload), PayloadSchemaVersion: 1, OccurredAt: at}
	}
	tests := []struct {
		name string
		fn   func(event.RawEvent) ([]event.Action, error)
		raw  event.RawEvent
		want []event.Kind
	}{
		{"open issue", issue, raw(KindIssue, `{"title":"t","project":"g/p","created_at":"2026-01-02T03:04:05Z"}`, nil), []event.Kind{"issue_opened"}},
		{"closed issue", issue, raw(KindIssue, `{"created_at":"2026-01-02T03:04:05Z","closed_at":"2026-01-03T03:04:05Z"}`, nil), []event.Kind{"issue_opened", "issue_closed"}},
		{"merged change", changeRequest, raw(KindChangeRequest, `{"created_at":"2026-01-02T03:04:05Z","merged_at":"2026-01-04T03:04:05Z","closed_at":"2026-01-04T03:04:05Z"}`, nil), []event.Kind{"pull_request_opened", "pull_request_merged"}},
		{"closed change", changeRequest, raw(KindChangeRequest, `{"created_at":"2026-01-02T03:04:05Z","closed_at":"2026-01-03T03:04:05Z"}`, nil), []event.Kind{"pull_request_opened", "pull_request_closed"}},
		{"merge only", mergeOnly, raw(KindChangeRequestMerged, `{"title":"t"}`, &merged), []event.Kind{"pull_request_merged"}},
		{"review", review, raw(KindReview, `{"state":"approved"}`, &merged), []event.Kind{"pull_request_reviewed"}},
		{"release", release, raw(KindRelease, `{"title":"v1"}`, &closed), []event.Kind{"release_published"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.fn(tt.raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d actions, want %d", len(got), len(tt.want))
			}
			for i, kind := range tt.want {
				if got[i].Kind != kind {
					t.Errorf("action %d kind = %q, want %q", i, got[i].Kind, kind)
				}
			}
		})
	}
	if _, err := issue(raw(KindIssue, `{}`, nil)); err == nil {
		t.Error("issue without creation time succeeded")
	}
	if _, err := release(raw(KindRelease, `{}`, nil)); err == nil {
		t.Error("release without timestamp succeeded")
	}
}
