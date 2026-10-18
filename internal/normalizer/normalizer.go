// SPDX-License-Identifier: Apache-2.0

// Package normalizer transforms stored raw events into contribution actions.
package normalizer

import (
	"errors"
	"fmt"
	"sync"

	"github.com/project-uatu/backend/internal/event"
)

// Handler transforms one raw event into zero or more contribution actions.
type Handler func(event.RawEvent) ([]event.Action, error)

// Registry routes raw events to source- and kind-specific pure handlers.
type Registry struct {
	mu       sync.RWMutex
	handlers map[handlerKey]Handler
}

type handlerKey struct {
	source event.Source
	kind   event.Kind
}

// New creates an empty normalizer registry.
func New() *Registry {
	return &Registry{handlers: make(map[handlerKey]Handler)}
}

// Register adds a handler for one source event kind.
func (r *Registry) Register(source event.Source, kind event.Kind, handler Handler) error {
	if source == "" || kind == "" || handler == nil {
		return errors.New("normalizer registration requires source, kind and handler")
	}
	key := handlerKey{source: source, kind: kind}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.handlers[key]; exists {
		return fmt.Errorf("normalizer already registered for %s/%s", source, kind)
	}
	r.handlers[key] = handler
	return nil
}

// Normalize transforms a raw source event into contribution actions.
func (r *Registry) Normalize(raw event.RawEvent) ([]event.Action, error) {
	r.mu.RLock()
	handler, ok := r.handlers[handlerKey{source: raw.Source, kind: raw.Kind}]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no normalizer registered for %s/%s", raw.Source, raw.Kind)
	}
	actions, err := handler(raw)
	if err != nil {
		return nil, fmt.Errorf("normalize %s/%s event %s: %w", raw.Source, raw.Kind, raw.ExternalID, err)
	}
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		if err := action.Validate(); err != nil {
			return nil, fmt.Errorf("validate normalized event %s: %w", raw.ExternalID, err)
		}
		if _, ok := seen[action.Key]; ok {
			return nil, fmt.Errorf("normalized event %s has duplicate action key %q", raw.ExternalID, action.Key)
		}
		seen[action.Key] = struct{}{}
	}
	return actions, nil
}
