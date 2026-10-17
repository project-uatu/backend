package event

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/identity"
)

func TestPageRequest_Validate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		request PageRequest
		wantErr bool
	}{
		{
			name: "valid unbounded history",
			request: PageRequest{
				PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: "github",
				PageSize: 100,
			},
		},
		{
			name: "invalid page size",
			request: PageRequest{
				PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: "github",
				PageSize: 501,
			},
			wantErr: true,
		},
		{
			name: "reversed interval",
			request: PageRequest{
				PersonID: uuid.New(), ForgeInstanceID: uuid.New(), Source: "github",
				PageSize: 100,
				Window: Window{
					From: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
					To:   time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				},
			},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.request.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("PageRequest.Validate() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestRawEvent_Validate_checksInstanceAndPayload(t *testing.T) {
	instance := uuid.New()
	event := RawEvent{
		ForgeInstanceID: instance, Source: "github", ExternalID: "issue:1",
		Kind: "issue", Payload: json.RawMessage(`{"id":1}`), PayloadSchemaVersion: 1,
		Actors: []Actor{{
			Key:  ActorKey{IdentityKey: identity.Key{Kind: identity.KindEmail, Value: "dev@example.com"}},
			Role: "author",
		}},
	}
	if err := event.Validate(instance); err != nil {
		t.Fatalf("RawEvent.Validate() error = %v, want nil", err)
	}
	if err := event.Validate(uuid.New()); err == nil {
		t.Error("RawEvent.Validate(other instance) error = nil, want error")
	}
	event.Payload = json.RawMessage(`not-json`)
	if err := event.Validate(instance); err == nil {
		t.Error("RawEvent.Validate(invalid payload) error = nil, want error")
	}
}

func TestActorKey_Validate_requiresForgeScope(t *testing.T) {
	key := ActorKey{IdentityKey: identity.Key{Kind: identity.KindLogin, Value: "octocat"}}
	if err := key.Validate(); err == nil {
		t.Fatal("ActorKey.Validate(login without instance) error = nil, want error")
	}
	instance := uuid.New()
	key.ForgeInstanceID = &instance
	if err := key.Validate(); err != nil {
		t.Fatalf("ActorKey.Validate(scoped login) error = %v, want nil", err)
	}
}
