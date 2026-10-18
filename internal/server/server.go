// SPDX-License-Identifier: Apache-2.0

// Package server exposes Uatu's stored results over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/store"
)

type historyStore interface {
	CoverageForPerson(context.Context, uuid.UUID, time.Time, time.Time) ([]store.CoverageInterval, error)
	ContributionCounts(context.Context, uuid.UUID, time.Time, time.Time) ([]store.ContributionCount, error)
	Ready(context.Context) error
}

const readyTimeout = 2 * time.Second

// ReadyFunc reports whether a process can do useful work.
type ReadyFunc func(context.Context) error

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

type pageMetadata struct {
	NextCursor string `json:"next_cursor,omitempty"`
	HasMore    bool   `json:"has_more"`
}

// NewHandler returns the HTTP handler for health and authenticated API routes.
func NewHandler(s apiStore, logger *slog.Logger, options Options) http.Handler {
	mux := newHealthMux(s.Ready, logger)
	registerAPI(mux, s, logger)
	mux.HandleFunc("GET /v1/persons/{id}/coverage", coverageHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/counts", countsHandler(s, logger))
	for _, pattern := range []string{
		"/v1/persons",
		"/v1/persons/{id}",
		"/v1/persons/{id}/sync",
		"/v1/persons/{id}/activity",
		"/v1/persons/{id}/summary",
		"/v1/persons/{id}/projects",
		"/v1/persons/{id}/coverage",
		"/v1/persons/{id}/counts",
		"/v1/jobs/{id}",
	} {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, logger, http.StatusMethodNotAllowed, "method not allowed")
		})
	}
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, logger, http.StatusNotFound, "API route not found")
	})
	return secureAPI(mux, options, logger)
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

func coverageHandler(s apiStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personID, from, to, ok := requestedPersonRange(w, r, logger)
		if !ok {
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		intervals, err := s.CoverageForPerson(r.Context(), personID, from, to)
		if err != nil {
			logger.ErrorContext(r.Context(), "read person coverage", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
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

func countsHandler(s apiStore, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		personID, from, to, ok := requestedPersonRange(w, r, logger)
		if !ok {
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		counts, err := s.ContributionCounts(r.Context(), personID, from, to)
		if err != nil {
			logger.ErrorContext(r.Context(), "read person contribution counts", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
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

func requirePerson(w http.ResponseWriter, r *http.Request, s apiStore, logger apiLogger, personID uuid.UUID) (store.APIPerson, bool) {
	person, err := s.APIPersonByID(r.Context(), personID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, r, logger, http.StatusNotFound, "person not found")
		return store.APIPerson{}, false
	}
	if err != nil {
		logger.ErrorContext(r.Context(), "check API person", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
		writeError(w, r, logger, http.StatusInternalServerError, "failed to read person")
		return store.APIPerson{}, false
	}
	return person, true
}

func requestedPersonRange(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (uuid.UUID, time.Time, time.Time, bool) {
	if !allowQueryKeys(w, r, logger, "from", "to") {
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	personID, ok := requestedPersonID(w, r, logger)
	if !ok {
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	from, to, ok := requestedTimeRange(w, r, logger, true)
	if !ok {
		return uuid.Nil, time.Time{}, time.Time{}, false
	}
	return personID, from, to, true
}

func writeJSON(w http.ResponseWriter, r *http.Request, logger apiLogger, value any) {
	writeResponse(w, r, logger, value, nil)
}

func writeResponse(w http.ResponseWriter, r *http.Request, logger apiLogger, value any, page *pageMetadata) {
	writeResponseStatus(w, r, logger, http.StatusOK, value, page)
}

func writeResponseStatus(w http.ResponseWriter, r *http.Request, logger apiLogger, status int, value any, page *pageMetadata) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Data any           `json:"data"`
		Page *pageMetadata `json:"page,omitempty"`
	}{Data: value, Page: page}); err != nil {
		logger.ErrorContext(r.Context(), "write JSON response", "request_id", r.Header.Get("X-Request-ID"), "err", err)
	}
}

func writeError(w http.ResponseWriter, r *http.Request, logger apiLogger, status int, message string) {
	code := "internal_error"
	switch status {
	case http.StatusBadRequest:
		code = "invalid_request"
	case http.StatusUnauthorized:
		code = "unauthorized"
	case http.StatusForbidden:
		code = "forbidden"
	case http.StatusNotFound:
		code = "not_found"
	case http.StatusConflict:
		code = "conflict"
	case http.StatusRequestEntityTooLarge:
		code = "request_too_large"
	case http.StatusUnsupportedMediaType:
		code = "unsupported_media_type"
	case http.StatusMethodNotAllowed:
		code = "method_not_allowed"
	case http.StatusTooManyRequests:
		code = "rate_limited"
	}
	writeAuthError(w, logger, r.Header.Get("X-Request-ID"), status, code, message)
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
