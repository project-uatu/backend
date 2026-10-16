// Package server exposes Uatu's stored results over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/project-uatu/backend/internal/github"
	"github.com/project-uatu/backend/internal/store"
)

type snapshotStore interface {
	Get(context.Context, store.Source, string) (store.Snapshot, bool, error)
}

// NewHandler returns the HTTP API handler. Handlers only read stored data;
// collection happens in the collector.
func NewHandler(s snapshotStore, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /github/users/{user}/repositories", repositoriesHandler(s, logger))
	return mux
}

func repositoriesHandler(s snapshotStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := r.PathValue("user")
		if !github.ValidLogin(user) {
			writeError(w, r, logger, http.StatusBadRequest, "invalid GitHub login")
			return
		}
		snapshot, ok, err := s.Get(r.Context(), store.SourceGitHubRepositories, user)
		if err != nil {
			logger.ErrorContext(r.Context(), "read stored snapshot", "source", store.SourceGitHubRepositories, "user", user, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read stored data")
			return
		}
		if !ok {
			writeError(w, r, logger, http.StatusNotFound, "no stored data for user")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if !snapshot.FetchedAt.IsZero() {
			w.Header().Set("Last-Modified", snapshot.FetchedAt.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(snapshot.Data); err != nil {
			logger.ErrorContext(r.Context(), "write HTTP response", "user", user, "err", err)
		}
	}
}

func writeError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message}); err != nil {
		logger.ErrorContext(r.Context(), "write HTTP error response", "err", err)
	}
}

// Run serves handler until ctx is canceled or the server encounters an error.
func Run(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "HTTP server listening", "addr", addr)
		errc <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err := <-errc
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
