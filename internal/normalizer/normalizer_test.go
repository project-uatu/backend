// SPDX-License-Identifier: Apache-2.0

package normalizer

import (
	"errors"
	"testing"
	"time"

	"github.com/project-uatu/backend/internal/event"
)

func TestRegistry_Normalize_routesAndValidates(t *testing.T) {
	registry := New()
	occurredAt := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := registry.Register("forge", "change", func(event.RawEvent) ([]event.Action, error) {
		return []event.Action{{Key: "opened", Kind: "change_opened", OccurredAt: occurredAt}}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	actions, err := registry.Normalize(event.RawEvent{Source: "forge", Kind: "change", ExternalID: "1"})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(actions) != 1 || actions[0].Key != "opened" {
		t.Errorf("Normalize actions = %+v, want opened action", actions)
	}
}

func TestRegistry_Normalize_rejectsMissingDuplicateAndInvalidHandlers(t *testing.T) {
	if err := New().Register("", "change", func(event.RawEvent) ([]event.Action, error) { return nil, nil }); err == nil {
		t.Error("Register(empty source) error = nil, want error")
	}
	registry := New()
	handlerErr := errors.New("bad payload")
	if err := registry.Register("forge", "change", func(event.RawEvent) ([]event.Action, error) {
		return nil, handlerErr
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Register("forge", "change", func(event.RawEvent) ([]event.Action, error) { return nil, nil }); err == nil {
		t.Error("Register(duplicate) error = nil, want error")
	}
	if _, err := registry.Normalize(event.RawEvent{Source: "missing", Kind: "change"}); err == nil {
		t.Error("Normalize(unregistered) error = nil, want error")
	}
	if _, err := registry.Normalize(event.RawEvent{Source: "forge", Kind: "change"}); !errors.Is(err, handlerErr) {
		t.Errorf("Normalize(handler error) = %v, want %v", err, handlerErr)
	}
}
