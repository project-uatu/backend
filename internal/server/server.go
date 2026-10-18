// Package server exposes Uatu's stored results over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/store"
)

type historyStore interface {
	Timeline(context.Context, uuid.UUID, time.Time, time.Time, int) ([]store.TimelineEntry, error)
	CoverageForPerson(context.Context, uuid.UUID, time.Time, time.Time) ([]store.CoverageInterval, error)
	ContributionCounts(context.Context, uuid.UUID, time.Time, time.Time) ([]store.ContributionCount, error)
	Ready(context.Context) error
}

const readyTimeout = 2 * time.Second

// ReadyFunc reports whether a process can do useful work.
type ReadyFunc func(context.Context) error

// NewHandler returns the HTTP API handler. Handlers only read stored data;
// collection happens in the collector.
func NewHandler(s historyStore, logger *slog.Logger) http.Handler {
	mux := newHealthMux(s.Ready, logger)
	mux.HandleFunc("GET /v1/persons/{id}/activity", activityHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/coverage", coverageHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/counts", countsHandler(s, logger))
	return mux
}

// NewHealthHandler returns a handler serving only /healthz and /readyz, for
// processes such as the collector that have no API.
func NewHealthHandler(ready ReadyFunc, logger *slog.Logger) http.Handler {
	return newHealthMux(ready, logger)
}

// newHealthMux serves liveness and readiness. Liveness never checks the
// database, so a database outage does not restart pods; readiness does.
func newHealthMux(ready ReadyFunc, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyTimeout)
		defer cancel()
		if err := ready(ctx); err != nil {
			logger.WarnContext(r.Context(), "readiness check failed", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

type activityEntry struct {
	EventID    uuid.UUID `json:"event_id"`
	Kind       string    `json:"kind"`
	Role       string    `json:"role"`
	OccurredAt time.Time `json:"occurred_at"`
	Project    string    `json:"project"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
}

type coverageEntry struct {
	ForgeInstanceID uuid.UUID `json:"forge_instance_id"`
	Source          string    `json:"source"`
	From            time.Time `json:"from"`
	To              time.Time `json:"to"`
	Status          string    `json:"status"`
	GapReason       string    `json:"gap_reason,omitempty"`
	Scope           string    `json:"scope"`
	RecordedAt      time.Time `json:"recorded_at"`
}

type countEntry struct {
	ForgeInstanceID uuid.UUID `json:"forge_instance_id"`
	Date            time.Time `json:"date"`
	Metric          string    `json:"metric"`
	Kind            string    `json:"kind,omitempty"`
	Value           int64     `json:"value"`
	ObservedAt      time.Time `json:"observed_at"`
}

func activityHandler(s historyStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personID, from, to, ok := requestedPersonRange(w, r, logger)
		if !ok {
			return
		}
		limit := 100
		if rawLimit := r.URL.Query().Get("limit"); rawLimit != "" {
			var err error
			limit, err = strconv.Atoi(rawLimit)
			if err != nil || limit < 1 || limit > 500 {
				writeError(w, r, logger, http.StatusBadRequest, "limit must be between 1 and 500")
				return
			}
		}
		entries, err := s.Timeline(r.Context(), personID, from, to, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "read person activity", "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read activity")
			return
		}
		response := make([]activityEntry, 0, len(entries))
		for _, entry := range entries {
			response = append(response, activityEntry{
				EventID: entry.EventID, Kind: entry.Kind, Role: entry.Role,
				OccurredAt: entry.OccurredAt, Project: entry.Project,
				Title: entry.Title, URL: entry.URL,
			})
		}
		writeJSON(w, r, logger, response)
	}
}

func coverageHandler(s historyStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personID, from, to, ok := requestedPersonRange(w, r, logger)
		if !ok {
			return
		}
		intervals, err := s.CoverageForPerson(r.Context(), personID, from, to)
		if err != nil {
			logger.ErrorContext(r.Context(), "read person coverage", "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read coverage")
			return
		}
		response := make([]coverageEntry, 0, len(intervals))
		for _, interval := range intervals {
			response = append(response, coverageEntry{
				ForgeInstanceID: interval.ForgeInstanceID, Source: interval.SourceKey,
				From: interval.StartsAt, To: interval.EndsAt, Status: interval.Status,
				GapReason: interval.GapReason, Scope: interval.CoverageScope,
				RecordedAt: interval.RecordedAt,
			})
		}
		writeJSON(w, r, logger, response)
	}
}

func countsHandler(s historyStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personID, from, to, ok := requestedPersonRange(w, r, logger)
		if !ok {
			return
		}
		counts, err := s.ContributionCounts(r.Context(), personID, from, to)
		if err != nil {
			logger.ErrorContext(r.Context(), "read person contribution counts", "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read contribution counts")
			return
		}
		response := make([]countEntry, 0, len(counts))
		for _, count := range counts {
			response = append(response, countEntry{
				ForgeInstanceID: count.ForgeInstanceID, Date: count.ActivityDate,
				Metric: count.Metric, Kind: count.Kind, Value: count.Value,
				ObservedAt: count.ObservedAt,
			})
		}
		writeJSON(w, r, logger, response)
	}
}

func requestedPersonRange(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (uuid.UUID, time.Time, time.Time, bool) {
	personID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "invalid person ID")
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	from, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "from must be an RFC3339 timestamp")
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	to, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("to"))
	if err != nil || !from.Before(to) {
		writeError(w, r, logger, http.StatusBadRequest, "to must be an RFC3339 timestamp after from")
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	return personID, from, to, true
}

func writeJSON(w http.ResponseWriter, r *http.Request, logger *slog.Logger, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		logger.ErrorContext(r.Context(), "write JSON response", "err", err)
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
