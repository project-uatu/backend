package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/project-uatu/backend/internal/config"
	"github.com/project-uatu/backend/internal/store"
)

func TestRun_usageErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		users []string
	}{
		{name: "unknown command", args: []string{"migrate"}},
		{name: "serve with arguments", args: []string{"serve", "octocat"}},
		{name: "collect invalid login", args: []string{"collect", "-octocat"}},
		{name: "collect invalid fallback login", args: []string{"collect"}, users: []string{"bad_login"}},
		{name: "collect loop with users", args: []string{"collect", "--loop", "octocat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{DatabaseURL: "not-a-database-url", Users: tt.users}

			err := run(t.Context(), tt.args, cfg, testLogger(), io.Discard)
			var usageErr *usageError
			if !errors.As(err, &usageErr) {
				t.Fatalf("run(%q) error = %v, want usage error", tt.args, err)
			}
		})
	}
}

func TestRun_collectWithoutUsersRunsTheQueueWorker(t *testing.T) {
	cfg := config.Config{
		DatabaseURL:            "not-a-database-url",
		DatabaseMinConns:       0,
		DatabaseMaxConns:       1,
		DatabaseConnectTimeout: time.Second,
	}
	err := run(t.Context(), []string{"collect"}, cfg, testLogger(), io.Discard)
	var usageErr *usageError
	if errors.As(err, &usageErr) {
		t.Fatalf("run(collect) error = %v, want database connection error, not usage error", err)
	}
	if err == nil {
		t.Fatal("run(collect) error = nil, want database connection error")
	}
}

func TestRun_help(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out strings.Builder
			if err := run(t.Context(), []string{arg}, config.Config{}, testLogger(), &out); err != nil {
				t.Fatalf("run(%q) error = %v, want nil", arg, err)
			}
			if out.String() != usage {
				t.Errorf("run(%q) output = %q, want usage", arg, out.String())
			}
		})
	}
}

type call struct {
	source store.Source
	user   string
}

type fakeCollector struct {
	calls  []call
	fail   map[string]error
	onCall func()
}

func (c *fakeCollector) Collect(_ context.Context, source store.Source, user string) error {
	c.calls = append(c.calls, call{source: source, user: user})
	if c.onCall != nil {
		c.onCall()
	}
	return c.fail[user]
}

func TestCollectUsers_continuesAfterFailure(t *testing.T) {
	failure := errors.New("upstream down")
	c := &fakeCollector{fail: map[string]error{"first": failure}}
	sources := []store.Source{"a", "b"}

	err := collectUsers(t.Context(), c, sources, []string{"first", "second"}, testLogger())
	if !errors.Is(err, failure) {
		t.Errorf("collectUsers error = %v, want %v", err, failure)
	}
	if got := strings.Count(err.Error(), "upstream down"); got != 2 {
		t.Errorf("collectUsers error reports %d failures, want 2: %v", got, err)
	}
	if len(c.calls) != 4 {
		t.Errorf("collectUsers made %d calls, want 4: %v", len(c.calls), c.calls)
	}
}

func TestCollectUsers_succeeds(t *testing.T) {
	c := &fakeCollector{}
	if err := collectUsers(t.Context(), c, []store.Source{"a"}, []string{"first", "second"}, testLogger()); err != nil {
		t.Errorf("collectUsers error = %v, want nil", err)
	}
	if len(c.calls) != 2 {
		t.Errorf("collectUsers made %d calls, want 2", len(c.calls))
	}
}

func TestCollectUsers_stopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &fakeCollector{onCall: cancel}

	err := collectUsers(ctx, c, []store.Source{"a", "b"}, []string{"first", "second"}, testLogger())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("collectUsers error = %v, want %v", err, context.Canceled)
	}
	if len(c.calls) != 1 {
		t.Errorf("collectUsers made %d calls after cancellation, want 1", len(c.calls))
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
