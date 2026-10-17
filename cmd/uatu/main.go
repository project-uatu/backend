// Command uatu collects open source activity and serves stored results.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/project-uatu/backend/internal/collector"
	"github.com/project-uatu/backend/internal/config"
	"github.com/project-uatu/backend/internal/github"
	"github.com/project-uatu/backend/internal/normalizer"
	"github.com/project-uatu/backend/internal/server"
	"github.com/project-uatu/backend/internal/store"
)

const usage = `Usage:
  uatu [serve]             serve stored results over HTTP (default)
  uatu collect [user ...]  collect legacy repository snapshots for users
  uatu collect             process due event work once when no users are configured
  uatu collect --loop      run the event worker until interrupted
  uatu help                show this help
`

// usageError reports invalid command-line usage.
type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load configuration", "err", err)
		os.Exit(1)
	}
	err = run(ctx, os.Args[1:], cfg, logger, os.Stdout)
	if err == nil {
		return
	}
	var usageErr *usageError
	if errors.As(err, &usageErr) {
		fmt.Fprintf(os.Stderr, "uatu: %v\n\n%s", err, usage)
		os.Exit(2)
	}
	logger.Error("uatu stopped", "err", err)
	os.Exit(1)
}

func run(ctx context.Context, args []string, cfg config.Config, logger *slog.Logger, stdout io.Writer) error {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	switch command {
	case "help", "-h", "-help", "--help":
		_, err := io.WriteString(stdout, usage)
		return err
	case "serve":
		if len(args) > 0 {
			return &usageError{err: fmt.Errorf("serve takes no arguments, got %q", args)}
		}
		return serve(ctx, cfg, logger)
	case "collect":
		if len(args) > 0 && args[0] == "--loop" {
			if len(args) != 1 {
				return &usageError{err: fmt.Errorf("collect --loop takes no user arguments")}
			}
			return collectWork(ctx, cfg, true, logger)
		}
		if len(args) == 0 && len(cfg.Users) == 0 {
			return collectWork(ctx, cfg, false, logger)
		}
		users, err := config.ParseUsers(args, cfg.Users)
		if err != nil {
			return &usageError{err: err}
		}
		for _, user := range users {
			if !github.ValidLogin(user) {
				return &usageError{err: fmt.Errorf("invalid GitHub login %q", user)}
			}
		}
		return collect(ctx, cfg, users, logger)
	default:
		return &usageError{err: fmt.Errorf("unknown command %q", command)}
	}
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	return server.Run(ctx, cfg.Address, server.NewHandler(db, logger), logger)
}

func collect(ctx context.Context, cfg config.Config, users []string, logger *slog.Logger) error {
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	c := collector.New(db, github.NewClient(cfg.GitHubToken, nil))
	return collectUsers(ctx, c, collector.Sources(), users, logger)
}

func collectWork(ctx context.Context, cfg config.Config, loop bool, logger *slog.Logger) error {
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get collector hostname: %w", err)
	}
	runner, err := collector.NewRunner(db, nil, normalizer.New(), logger, collector.RunnerOptions{
		Owner: fmt.Sprintf("%s-%d", host, os.Getpid()),
	})
	if err != nil {
		return fmt.Errorf("configure collection worker: %w", err)
	}
	if loop {
		return runner.Run(ctx)
	}
	return runner.Drain(ctx)
}

func storeOptions(cfg config.Config) store.Options {
	return store.Options{
		URL:            cfg.DatabaseURL,
		SSLMode:        cfg.DatabaseSSLMode,
		MinConns:       cfg.DatabaseMinConns,
		MaxConns:       cfg.DatabaseMaxConns,
		ConnectTimeout: cfg.DatabaseConnectTimeout,
	}
}

// sourceCollector collects one source for one user.
type sourceCollector interface {
	Collect(context.Context, store.Source, string) error
}

// collectUsers attempts every source for every user, stopping early only when
// ctx is canceled. It returns all collection errors joined.
func collectUsers(ctx context.Context, c sourceCollector, sources []store.Source, users []string, logger *slog.Logger) error {
	var errs []error
	for _, user := range users {
		for _, source := range sources {
			if err := ctx.Err(); err != nil {
				return errors.Join(append(errs, err)...)
			}
			if err := c.Collect(ctx, source, user); err != nil {
				errs = append(errs, fmt.Errorf("%s/%s: %w", source, user, err))
				continue
			}
			logger.InfoContext(ctx, "collection stored", "source", source, "user", user)
		}
	}
	return errors.Join(errs...)
}
