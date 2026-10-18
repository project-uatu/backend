package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/project-uatu/backend/internal/config"
)

func TestRun_usageErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		users []string
	}{
		{name: "unknown command", args: []string{"upgrade"}},
		{name: "migrate with arguments", args: []string{"migrate", "now"}},
		{name: "serve with arguments", args: []string{"serve", "octocat"}},
		{name: "collect invalid login", args: []string{"collect", "-octocat"}},
		{name: "collect invalid fallback login", args: []string{"collect"}, users: []string{"bad_login"}},
		{name: "collect loop with users", args: []string{"collect", "--loop", "octocat"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{DatabaseURL: "not-a-database-url", Users: tt.users, GitHubToken: "test-token"}

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
		GitHubToken:            "test-token",
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

func TestRun_collectRequiresToken(t *testing.T) {
	for _, args := range [][]string{{"collect"}, {"collect", "octocat"}, {"collect", "--loop"}} {
		err := run(t.Context(), args, config.Config{}, testLogger(), io.Discard)
		var usageErr *usageError
		if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("run(%q) error = %v, want token requirement before database or upstream access", args, err)
		}
	}
}

func TestRun_serveRequiresAPIKeyBeforeOpeningDatabase(t *testing.T) {
	cfg := config.Config{DatabaseURL: "invalid"}
	err := run(t.Context(), []string{"serve"}, cfg, testLogger(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "UATU_API_KEY") {
		t.Fatalf("run(serve) error = %v, want API key requirement before database access", err)
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

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRunWithHealth_stopsWorkerWhenHealthListenerFails(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	ready := func(context.Context) error { return nil }
	work := func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- runWithHealth(t.Context(), listener.Addr().String(), ready, work, testLogger())
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "health endpoints") {
			t.Errorf("runWithHealth error = %v, want health listener error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker kept running after the health listener failed")
	}
}
