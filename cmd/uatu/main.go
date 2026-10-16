// Command uatu collects open source activity and serves stored results.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/project-uatu/backend/internal/collector"
	"github.com/project-uatu/backend/internal/config"
	"github.com/project-uatu/backend/internal/github"
	"github.com/project-uatu/backend/internal/server"
	"github.com/project-uatu/backend/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], config.Load(), logger); err != nil {
		logger.Error("uatu stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, cfg config.Config, logger *slog.Logger) (runErr error) {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}
	if command != "serve" && command != "collect" {
		return fmt.Errorf("unknown command %q (want serve or collect)", command)
	}

	db, err := store.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() {
		runErr = errors.Join(runErr, db.Close())
	}()

	activityCollector := collector.New(db, github.NewClient(cfg.GitHubToken, nil))

	switch command {
	case "serve":
		handler := server.NewHandler(db, activityCollector, logger)
		return server.Run(ctx, cfg.Address, handler, logger)
	case "collect":
		users, err := config.ParseUsers(args, cfg.Users)
		if err != nil {
			return err
		}
		var errs []error
		for _, user := range users {
			for _, source := range collector.Sources() {
				if err := ctx.Err(); err != nil {
					return errors.Join(append(errs, err)...)
				}
				data, err := activityCollector.Collect(ctx, source, user)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s/%s: %w", source, user, err))
					continue
				}
				logger.Info("collection stored", "source", source, "user", user, "bytes", len(data))
			}
		}
		return errors.Join(errs...)
	}
	return nil
}
