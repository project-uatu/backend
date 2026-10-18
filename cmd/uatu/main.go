// SPDX-License-Identifier: Apache-2.0

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
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/forgejo"
	"github.com/project-uatu/backend/internal/forgenorm"
	"github.com/project-uatu/backend/internal/github"
	"github.com/project-uatu/backend/internal/gitlab"
	"github.com/project-uatu/backend/internal/server"
	"github.com/project-uatu/backend/internal/store"
)

const usage = `Usage:
  uatu [serve]             serve stored results over HTTP (default)
  uatu migrate             apply database migrations and exit
  uatu collect [user ...]  collect public contribution history for GitHub users
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
	case "migrate":
		if len(args) > 0 {
			return &usageError{err: fmt.Errorf("migrate takes no arguments, got %q", args)}
		}
		if err := store.Migrate(ctx, storeOptions(cfg)); err != nil {
			return err
		}
		logger.InfoContext(ctx, "database migrations applied")
		return nil
	case "collect":
		if cfg.ForgeEnabled("github.com") && cfg.GitHubToken == "" {
			return &usageError{err: errors.New("collect requires GITHUB_TOKEN for GitHub identity refresh and contribution counts")}
		}
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
		return collectPeople(ctx, cfg, users, logger)
	default:
		return &usageError{err: fmt.Errorf("unknown command %q", command)}
	}
}

func serve(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if len(cfg.APIKey) < 32 {
		return errors.New("serve requires UATU_API_KEY with at least 32 bytes")
	}
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	handler := server.NewHandler(db, logger, server.Options{
		APIKey: cfg.APIKey, CORSOrigins: cfg.CORSOrigins,
	})
	return server.Run(ctx, cfg.Address, handler, logger)
}

func collectPeople(ctx context.Context, cfg config.Config, users []string, logger *slog.Logger) error {
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	forge, err := db.CreateForgeInstance(ctx, "github", "https://github.com")
	if err != nil {
		return fmt.Errorf("create GitHub forge instance: %w", err)
	}
	client := github.NewClient(cfg.GitHubToken, nil)
	for _, login := range users {
		account, _, err := client.ResolveAccount(ctx, login)
		if err != nil {
			return fmt.Errorf("resolve GitHub user %s: %w", login, err)
		}
		tracked, err := db.TrackGitHubAccountForLogin(ctx, forge.ID, login, store.GitHubAccount{
			AccountID: account.ID, NodeID: account.NodeID, Login: account.Login,
			AccountCreatedAt: account.CreatedAt,
		})
		if err != nil {
			return fmt.Errorf("track GitHub user %s: %w", login, err)
		}
		logger.InfoContext(ctx, "GitHub account tracked", "login", tracked.Login, "person_id", tracked.PersonID)
		if _, err := db.EnqueueHistory(ctx, tracked.PersonID, forge.ID); err != nil {
			if !errors.Is(err, store.ErrCollectionJobActive) {
				return fmt.Errorf("enqueue GitHub history for %s: %w", login, err)
			}
			logger.InfoContext(ctx, "resuming existing GitHub history scan", "person_id", tracked.PersonID)
		}
	}
	return runCollectionWorker(ctx, db, cfg, logger, false)
}

func collectWork(ctx context.Context, cfg config.Config, loop bool, logger *slog.Logger) error {
	db, err := store.Open(ctx, storeOptions(cfg))
	if err != nil {
		return err
	}
	defer db.Close()
	if !loop {
		return runCollectionWorker(ctx, db, cfg, logger, false)
	}
	return runWithHealth(ctx, cfg.HealthAddress, db.Ready, func(ctx context.Context) error {
		return runCollectionWorker(ctx, db, cfg, logger, true)
	}, logger)
}

// runWithHealth runs work beside a health listener. Either one stopping,
// including a listener that cannot bind, ends the other.
func runWithHealth(ctx context.Context, addr string, ready server.ReadyFunc, work func(context.Context) error, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	healthErr := make(chan error, 1)
	go func() {
		err := server.Run(ctx, addr, server.NewHealthHandler(ready, logger), logger)
		cancel()
		healthErr <- err
	}()
	workErr := work(ctx)
	cancel()
	if err := <-healthErr; err != nil {
		return errors.Join(workErr, fmt.Errorf("serve worker health endpoints: %w", err))
	}
	return workErr
}

func runCollectionWorker(ctx context.Context, db *store.Store, cfg config.Config, logger *slog.Logger, loop bool) error {
	host, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("get collector hostname: %w", err)
	}
	transformer, err := github.NewHistoryNormalizer()
	if err != nil {
		return fmt.Errorf("configure GitHub history normalizer: %w", err)
	}
	// Normalizers are pure, so every forge's handlers stay registered for
	// already stored data. Only connectors depend on enablement.
	for _, source := range []event.Source{gitlab.Source, forgejo.Source} {
		if err := forgenorm.Register(transformer, source); err != nil {
			return fmt.Errorf("configure forge normalizer: %w", err)
		}
	}
	var connectors []collector.Connector
	if cfg.ForgeEnabled("github.com") {
		connectors = append(connectors, github.NewHistoryConnector(github.NewClient(cfg.GitHubToken, nil)))
	}
	if cfg.ForgeEnabled("gitlab.com") {
		connectors = append(connectors, gitlab.New(gitlab.Options{Token: cfg.GitLabToken}))
	}
	if cfg.ForgeEnabled("codeberg.org") {
		connectors = append(connectors, forgejo.New(forgejo.Options{Token: cfg.CodebergToken}))
	}
	if len(connectors) == 0 {
		return errors.New("no forges enabled; set UATU_FORGES")
	}
	runner, err := collector.NewRunner(db, connectors, transformer, logger, collector.RunnerOptions{
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
