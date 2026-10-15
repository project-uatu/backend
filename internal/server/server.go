// Package server exposes Uatu's stored results over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/project-uatu/backend/internal/collector"
	"github.com/project-uatu/backend/internal/store"
)

type snapshotStore interface {
	Get(context.Context, store.Source, string) (store.Snapshot, bool, error)
}

type collectionService interface {
	Collect(context.Context, store.Source, string) ([]byte, error)
}

// NewHandler returns the HTTP API handler.
func NewHandler(s snapshotStore, c collectionService, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if _, err := w.Write([]byte("Hello, World!")); err != nil {
			logger.ErrorContext(r.Context(), "write root response", "err", err)
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /gh/users/{user}/contributions", storedHandler(s, c, logger, store.SourceGitHubRepos))
	mux.HandleFunc("GET /devscores/{user}", storedHandler(s, c, logger, store.SourceDevStats))
	return mux
}

func storedHandler(s snapshotStore, c collectionService, logger *slog.Logger, source store.Source) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := r.PathValue("user")
		snapshot, ok, err := s.Get(r.Context(), source, user)
		if err != nil {
			logger.ErrorContext(r.Context(), "read stored snapshot", "source", source, "user", user, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read stored data")
			return
		}
		if !ok {
			_, err := c.Collect(r.Context(), source, user)
			if err != nil {
				logger.ErrorContext(r.Context(), "collect source data", "source", source, "user", user, "err", err)
				var upstreamErr *collector.UpstreamError
				if errors.As(err, &upstreamErr) {
					writeError(w, r, logger, http.StatusBadGateway, "failed to fetch upstream data")
					return
				}
				writeError(w, r, logger, http.StatusInternalServerError, "failed to store collected data")
				return
			}
			snapshot, ok, err = s.Get(r.Context(), source, user)
			if err != nil {
				logger.ErrorContext(r.Context(), "read collected snapshot", "source", source, "user", user, "err", err)
				writeError(w, r, logger, http.StatusInternalServerError, "failed to read stored data")
				return
			}
			if !ok {
				logger.ErrorContext(r.Context(), "collected snapshot is missing", "source", source, "user", user)
				writeError(w, r, logger, http.StatusInternalServerError, "failed to read stored data")
				return
			}
		}

		if source == store.SourceDevStats {
			w.Header().Set("Content-Type", "application/json")
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		if !snapshot.FetchedAt.IsZero() {
			w.Header().Set("Last-Modified", snapshot.FetchedAt.UTC().Format(http.TimeFormat))
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(snapshot.Data); err != nil {
			logger.ErrorContext(r.Context(), "write HTTP response", "source", source, "user", user, "err", err)
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
		WriteTimeout:      45 * time.Second,
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
