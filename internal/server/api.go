package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/github"
	"github.com/project-uatu/backend/internal/identity"
	"github.com/project-uatu/backend/internal/store"
)

const maxRequestBody = 64 << 10

type apiStore interface {
	historyStore
	ForgeForHost(context.Context, string) (store.ForgeInstance, store.SourceProfile, error)
	CreateOnboardedPerson(context.Context, store.ForgeInstance, string, string, []string) (store.Person, store.CollectionJob, error)
	ListAPIPersons(context.Context, string, uuid.UUID, int) ([]store.APIPerson, bool, error)
	APIPersonByID(context.Context, uuid.UUID) (store.APIPerson, error)
	PersonSourceCoverage(context.Context, uuid.UUID) ([]store.APISourceCoverage, error)
	UpdatePersonIdentifiers(context.Context, uuid.UUID, uuid.UUID, []store.APIIdentity, []store.APIIdentity) error
	EnqueueHistoryWithKey(context.Context, uuid.UUID, uuid.UUID, string) (store.CollectionJob, error)
	APIJobByID(context.Context, uuid.UUID) (store.APIJob, error)
	ActivityPage(context.Context, store.ActivityFilter) (store.ActivityPage, error)
	PersonSummaryByYear(context.Context, uuid.UUID) ([]store.PersonSummary, error)
	PersonProjects(context.Context, uuid.UUID, *time.Time, *time.Time, string, int) ([]store.ProjectSummary, bool, error)
}

type personResponse struct {
	ID          uuid.UUID                `json:"id"`
	DisplayName *string                  `json:"display_name,omitempty"`
	CreatedAt   time.Time                `json:"created_at"`
	Sources     []sourceCoverageResponse `json:"sources"`
}

type personListResponse struct {
	ID          uuid.UUID `json:"id"`
	DisplayName *string   `json:"display_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type sourceCoverageResponse struct {
	Source     string     `json:"source"`
	Status     string     `json:"status"`
	From       *time.Time `json:"from,omitempty"`
	To         *time.Time `json:"to,omitempty"`
	GapCount   int64      `json:"gap_count"`
	RecordedAt time.Time  `json:"recorded_at"`
}

type jobResponse struct {
	ID          uuid.UUID `json:"id"`
	PersonID    uuid.UUID `json:"person_id"`
	Source      string    `json:"source"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	MaxAttempts int       `json:"max_attempts"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type createPersonRequest struct {
	Forge       string   `json:"forge,omitempty"`
	Login       string   `json:"login"`
	DisplayName string   `json:"display_name"`
	Emails      []string `json:"emails,omitempty"`
}

type patchPersonRequest struct {
	Forge  string              `json:"forge,omitempty"`
	Add    []identifierRequest `json:"add,omitempty"`
	Remove []identifierRequest `json:"remove,omitempty"`
}

type identifierRequest struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type summaryResponse struct {
	Year   int    `json:"year"`
	Source string `json:"source"`
	Kind   string `json:"kind"`
	Count  int64  `json:"count"`
}

type projectResponse struct {
	Project string `json:"project"`
	Count   int64  `json:"count"`
}

func registerAPI(mux *http.ServeMux, s apiStore, logger apiLogger) {
	mux.HandleFunc("GET /v1/persons", listPersonsHandler(s, logger))
	mux.HandleFunc("POST /v1/persons", createPersonHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}", personHandler(s, logger))
	mux.HandleFunc("PATCH /v1/persons/{id}", patchPersonHandler(s, logger))
	mux.HandleFunc("POST /v1/persons/{id}/sync", syncPersonHandler(s, logger))
	mux.HandleFunc("GET /v1/jobs/{id}", jobHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/activity", activityPageHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/summary", summaryHandler(s, logger))
	mux.HandleFunc("GET /v1/persons/{id}/projects", projectsHandler(s, logger))
}

func listPersonsHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger, "q", "limit", "cursor") {
			return
		}
		search, ok := oneQueryValue(w, r, logger, "q", false)
		if !ok || utf8.RuneCountInString(search) > 100 {
			if ok {
				writeError(w, r, logger, http.StatusBadRequest, "q must not exceed 100 characters")
			}
			return
		}
		limit, ok := requestedLimit(w, r, logger, 50, 100)
		if !ok {
			return
		}
		var after uuid.UUID
		if token, ok := oneQueryValue(w, r, logger, "cursor", false); !ok {
			return
		} else if token != "" {
			cursor, err := decodePageCursor(apiKeyFromOptions(r), token, "persons", search)
			if err != nil || cursor.ID == uuid.Nil {
				writeError(w, r, logger, http.StatusBadRequest, "cursor is invalid for this query")
				return
			}
			after = cursor.ID
		}
		persons, hasMore, err := s.ListAPIPersons(r.Context(), search, after, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "list API persons", "request_id", r.Header.Get("X-Request-ID"), "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to list persons")
			return
		}
		if hasMore && len(persons) == 0 {
			logger.ErrorContext(r.Context(), "API person page reported more rows without returning any", "request_id", r.Header.Get("X-Request-ID"))
			writeError(w, r, logger, http.StatusInternalServerError, "failed to list persons")
			return
		}
		response := make([]personListResponse, 0, len(persons))
		for _, person := range persons {
			response = append(response, personListResponse{
				ID: person.ID, DisplayName: person.DisplayName, CreatedAt: person.CreatedAt,
			})
		}
		page := pageMetadata{HasMore: hasMore}
		if hasMore {
			page.NextCursor, err = encodePageCursor(apiKeyFromOptions(r), pageCursor{
				Kind: "persons", Filter: search, ID: persons[len(persons)-1].ID,
			})
			if err != nil {
				writeError(w, r, logger, http.StatusInternalServerError, "failed to encode page cursor")
				return
			}
		}
		writeResponse(w, r, logger, response, &page)
	}
}

func createPersonHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger) {
			return
		}
		var input createPersonRequest
		if !decodeJSONBody(w, r, logger, &input) {
			return
		}
		input.Login = strings.TrimSpace(input.Login)
		input.DisplayName = strings.TrimSpace(input.DisplayName)
		forge, profile, ok := requestedForge(w, r, s, logger, input.Forge)
		if !ok {
			return
		}
		if !validLogin(profile, input.Login) || utf8.RuneCountInString(input.DisplayName) > 200 ||
			strings.ContainsRune(input.DisplayName, '\x00') || len(input.Emails) > 20 {
			writeError(w, r, logger, http.StatusBadRequest, "login, display_name or emails are invalid")
			return
		}
		seenEmails := make(map[string]struct{}, len(input.Emails))
		for i, email := range input.Emails {
			email = strings.TrimSpace(email)
			if len(email) > 320 {
				writeError(w, r, logger, http.StatusBadRequest, "email identifier is too long")
				return
			}
			normalized, err := normalizeEmailIdentifier(email)
			if err != nil {
				writeError(w, r, logger, http.StatusBadRequest, "email identifier is invalid")
				return
			}
			if _, exists := seenEmails[normalized]; exists {
				writeError(w, r, logger, http.StatusBadRequest, "emails must be unique")
				return
			}
			seenEmails[normalized] = struct{}{}
			input.Emails[i] = email
		}
		person, job, err := s.CreateOnboardedPerson(r.Context(), forge, input.DisplayName, input.Login, input.Emails)
		if errors.Is(err, store.ErrIdentityConflict) {
			writeError(w, r, logger, http.StatusConflict, "one or more identifiers are already assigned")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "create API person", "request_id", r.Header.Get("X-Request-ID"), "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to create person")
			return
		}
		apiJob, err := s.APIJobByID(r.Context(), job.ID)
		if err != nil {
			logger.ErrorContext(r.Context(), "read initial API collection job", "request_id", r.Header.Get("X-Request-ID"), "job_id", job.ID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read initial collection job")
			return
		}
		w.Header().Set("Location", "/v1/jobs/"+job.ID.String())
		writeResponseStatus(w, r, logger, http.StatusCreated, struct {
			Person personResponse `json:"person"`
			Job    jobResponse    `json:"job"`
		}{Person: toPersonResponse(store.APIPerson{
			ID: person.ID, DisplayName: person.DisplayName, CreatedAt: person.CreatedAt,
		}, nil), Job: toJobResponse(apiJob)}, nil)
	}
}

func personHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger) {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		person, err := s.APIPersonByID(r.Context(), personID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, logger, http.StatusNotFound, "person not found")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "read API person", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read person")
			return
		}
		coverage, err := s.PersonSourceCoverage(r.Context(), personID)
		if err != nil {
			logger.ErrorContext(r.Context(), "read API person source coverage", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read person coverage")
			return
		}
		writeJSON(w, r, logger, toPersonResponse(person, coverage))
	}
}

func patchPersonHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger) {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		var input patchPersonRequest
		if !decodeJSONBody(w, r, logger, &input) {
			return
		}
		if len(input.Add)+len(input.Remove) == 0 || len(input.Add)+len(input.Remove) > 20 {
			writeError(w, r, logger, http.StatusBadRequest, "provide 1 to 20 identifiers to add or remove")
			return
		}
		forge, profile, ok := requestedForge(w, r, s, logger, input.Forge)
		if !ok {
			return
		}
		add, err := validateIdentifiers(profile, input.Add)
		if err != nil {
			writeError(w, r, logger, http.StatusBadRequest, err.Error())
			return
		}
		remove, err := validateIdentifiers(profile, input.Remove)
		if err != nil {
			writeError(w, r, logger, http.StatusBadRequest, err.Error())
			return
		}
		added := make(map[string]struct{}, len(add))
		for _, item := range add {
			normalized, err := identity.Normalize(profile.Provider, item.Kind, item.Value)
			if err != nil {
				writeError(w, r, logger, http.StatusBadRequest, "identifier value is invalid")
				return
			}
			added[string(item.Kind)+"\x00"+normalized] = struct{}{}
		}
		for _, item := range remove {
			normalized, err := identity.Normalize(profile.Provider, item.Kind, item.Value)
			if err != nil {
				writeError(w, r, logger, http.StatusBadRequest, "identifier value is invalid")
				return
			}
			if _, exists := added[string(item.Kind)+"\x00"+normalized]; exists {
				writeError(w, r, logger, http.StatusBadRequest, "the same identifier cannot be added and removed")
				return
			}
		}
		if err := s.UpdatePersonIdentifiers(r.Context(), personID, forge.ID, add, remove); errors.Is(err, store.ErrNotFound) {
			writeError(w, r, logger, http.StatusNotFound, "person not found")
			return
		} else if errors.Is(err, store.ErrOnboardingResolutionPending) {
			writeError(w, r, logger, http.StatusConflict, "account resolution is still pending")
			return
		} else if errors.Is(err, store.ErrIdentityConflict) {
			writeError(w, r, logger, http.StatusConflict, "identifier is assigned to another person")
			return
		} else if err != nil {
			logger.ErrorContext(r.Context(), "update API person identifiers", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to update identifiers")
			return
		}
		person, err := s.APIPersonByID(r.Context(), personID)
		if err != nil {
			logger.ErrorContext(r.Context(), "read updated API person", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read updated person")
			return
		}
		coverage, err := s.PersonSourceCoverage(r.Context(), personID)
		if err != nil {
			logger.ErrorContext(r.Context(), "read updated API person source coverage", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read updated person coverage")
			return
		}
		writeJSON(w, r, logger, toPersonResponse(person, coverage))
	}
}

func syncPersonHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger, "forge") {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		keys := r.Header.Values("Idempotency-Key")
		if len(keys) != 1 {
			writeError(w, r, logger, http.StatusBadRequest, "exactly one Idempotency-Key is required")
			return
		}
		key := keys[0]
		if strings.TrimSpace(key) != key || strings.ContainsRune(key, '\x00') || len(key) < 1 || len(key) > 200 {
			writeError(w, r, logger, http.StatusBadRequest, "Idempotency-Key must contain 1 to 200 bytes")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1))
		if err != nil {
			writeError(w, r, logger, http.StatusBadRequest, "failed to read request body")
			return
		}
		if len(body) > 0 {
			writeError(w, r, logger, http.StatusBadRequest, "sync requests do not accept a body")
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		host, ok := oneQueryValue(w, r, logger, "forge", false)
		if !ok {
			return
		}
		forge, _, ok := requestedForge(w, r, s, logger, host)
		if !ok {
			return
		}
		job, err := s.EnqueueHistoryWithKey(r.Context(), personID, forge.ID, key)
		if errors.Is(err, store.ErrIdempotencyConflict) {
			writeError(w, r, logger, http.StatusConflict, "idempotency key is already assigned to another job")
			return
		}
		if errors.Is(err, store.ErrCollectionJobActive) {
			writeError(w, r, logger, http.StatusConflict, "collection work is already active")
			return
		}
		if errors.Is(err, store.ErrOnboardingTargetUnavailable) {
			writeError(w, r, logger, http.StatusConflict, "account resolution target is unavailable")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "enqueue API person sync", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to request sync")
			return
		}
		apiJob, err := s.APIJobByID(r.Context(), job.ID)
		if err != nil {
			logger.ErrorContext(r.Context(), "read requested API collection job", "request_id", r.Header.Get("X-Request-ID"), "job_id", job.ID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read collection job")
			return
		}
		w.Header().Set("Location", "/v1/jobs/"+job.ID.String())
		writeResponseStatus(w, r, logger, http.StatusAccepted, toJobResponse(apiJob), nil)
	}
}

func jobHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger) {
			return
		}
		jobID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			writeError(w, r, logger, http.StatusBadRequest, "invalid job ID")
			return
		}
		job, err := s.APIJobByID(r.Context(), jobID)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, r, logger, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			logger.ErrorContext(r.Context(), "read API job", "request_id", r.Header.Get("X-Request-ID"), "job_id", jobID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read job")
			return
		}
		writeJSON(w, r, logger, toJobResponse(job))
	}
}

func activityPageHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger, "from", "to", "source", "kind", "limit", "cursor") {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		from, to, ok := requestedTimeRange(w, r, logger, true)
		if !ok {
			return
		}
		source, ok := oneQueryValue(w, r, logger, "source", false)
		if !ok || utf8.RuneCountInString(source) > 100 {
			if ok {
				writeError(w, r, logger, http.StatusBadRequest, "source filter is too long")
			}
			return
		}
		kind, ok := oneQueryValue(w, r, logger, "kind", false)
		if !ok || utf8.RuneCountInString(kind) > 100 {
			if ok {
				writeError(w, r, logger, http.StatusBadRequest, "kind filter is too long")
			}
			return
		}
		limit, ok := requestedLimit(w, r, logger, 100, 500)
		if !ok {
			return
		}
		filterKey := fmt.Sprintf("%s|%s|%s|%d:%s|%d:%s",
			personID, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano),
			len(source), source, len(kind), kind)
		var after *store.ActivityCursor
		token, ok := oneQueryValue(w, r, logger, "cursor", false)
		if !ok {
			return
		}
		if token != "" {
			cursor, err := decodePageCursor(apiKeyFromOptions(r), token, "activity", filterKey)
			if err != nil || cursor.ID == uuid.Nil || cursor.OccurredAt.IsZero() || cursor.Role == "" {
				writeError(w, r, logger, http.StatusBadRequest, "cursor is invalid for this query")
				return
			}
			after = &store.ActivityCursor{OccurredAt: cursor.OccurredAt, EventID: cursor.ID, Role: cursor.Role}
		}
		result, err := s.ActivityPage(r.Context(), store.ActivityFilter{
			PersonID: personID, From: from, To: to, Source: source, Kind: kind, After: after, Limit: limit,
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, r, logger, http.StatusNotFound, "person not found")
				return
			}
			logger.ErrorContext(r.Context(), "query API activity page", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read activity")
			return
		}
		if result.HasMore && len(result.Entries) == 0 {
			logger.ErrorContext(r.Context(), "API activity page reported more rows without returning any", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read activity")
			return
		}
		page := pageMetadata{HasMore: result.HasMore}
		if result.HasMore {
			last := result.Entries[len(result.Entries)-1]
			page.NextCursor, err = encodePageCursor(apiKeyFromOptions(r), pageCursor{
				Kind: "activity", Filter: filterKey, OccurredAt: last.OccurredAt, ID: last.EventID, Role: last.Role,
			})
			if err != nil {
				writeError(w, r, logger, http.StatusInternalServerError, "failed to encode page cursor")
				return
			}
		}
		response := make([]activityEntry, 0, len(result.Entries))
		for _, entry := range result.Entries {
			response = append(response, activityEntry{
				EventID: entry.EventID, Kind: entry.Kind, Role: entry.Role,
				OccurredAt: entry.OccurredAt, Project: entry.Project, Title: entry.Title, URL: entry.URL,
			})
		}
		writeResponse(w, r, logger, response, &page)
	}
}

func summaryHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger) {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		summary, err := s.PersonSummaryByYear(r.Context(), personID)
		if err != nil {
			logger.ErrorContext(r.Context(), "query API person summary", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read person summary")
			return
		}
		response := make([]summaryResponse, 0, len(summary))
		for _, item := range summary {
			response = append(response, summaryResponse{Year: item.Year, Source: item.Source, Kind: item.Kind, Count: item.Count})
		}
		writeJSON(w, r, logger, response)
	}
}

func projectsHandler(s apiStore, logger apiLogger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !allowQueryKeys(w, r, logger, "from", "to", "limit", "cursor") {
			return
		}
		personID, ok := requestedPersonID(w, r, logger)
		if !ok {
			return
		}
		if _, ok := requirePerson(w, r, s, logger, personID); !ok {
			return
		}
		from, to, hasRange, ok := optionalTimeRange(w, r, logger)
		if !ok {
			return
		}
		limit, ok := requestedLimit(w, r, logger, 100, 500)
		if !ok {
			return
		}
		filterKey := personID.String()
		if hasRange {
			filterKey += "|" + from.UTC().Format(time.RFC3339Nano) + "|" + to.UTC().Format(time.RFC3339Nano)
		}
		after, ok := oneQueryValue(w, r, logger, "cursor", false)
		if !ok {
			return
		}
		if after != "" {
			cursor, err := decodePageCursor(apiKeyFromOptions(r), after, "projects", filterKey)
			if err != nil || cursor.Project == "" {
				writeError(w, r, logger, http.StatusBadRequest, "cursor is invalid for this query")
				return
			}
			after = cursor.Project
		}
		projects, hasMore, err := s.PersonProjects(r.Context(), personID, from, to, after, limit)
		if err != nil {
			logger.ErrorContext(r.Context(), "query API person projects", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID, "err", err)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read person projects")
			return
		}
		if hasMore && len(projects) == 0 {
			logger.ErrorContext(r.Context(), "API project page reported more rows without returning any", "request_id", r.Header.Get("X-Request-ID"), "person_id", personID)
			writeError(w, r, logger, http.StatusInternalServerError, "failed to read person projects")
			return
		}
		response := make([]projectResponse, 0, len(projects))
		for _, item := range projects {
			response = append(response, projectResponse{Project: item.Project, Count: item.Count})
		}
		page := pageMetadata{HasMore: hasMore}
		if hasMore {
			page.NextCursor, err = encodePageCursor(apiKeyFromOptions(r), pageCursor{
				Kind: "projects", Filter: filterKey, Project: projects[len(projects)-1].Project,
			})
			if err != nil {
				writeError(w, r, logger, http.StatusInternalServerError, "failed to encode page cursor")
				return
			}
		}
		writeResponse(w, r, logger, response, &page)
	}
}

func validateIdentifiers(profile store.SourceProfile, input []identifierRequest) ([]store.APIIdentity, error) {
	identifiers := make([]store.APIIdentity, 0, len(input))
	seen := make(map[string]struct{}, len(input))
	for _, item := range input {
		kind := identity.Kind(item.Kind)
		if kind != identity.KindLogin && kind != identity.KindEmail {
			return nil, errors.New("identifier kind must be login or email")
		}
		if kind == identity.KindLogin && !validLogin(profile, item.Value) {
			return nil, errors.New("login identifier is invalid")
		}
		value := item.Value
		if kind == identity.KindEmail {
			value = strings.TrimSpace(value)
			if len(value) > 320 {
				return nil, errors.New("email identifier is too long")
			}
		}
		var normalized string
		var err error
		if kind == identity.KindEmail {
			normalized, err = normalizeEmailIdentifier(value)
		} else {
			normalized, err = identity.Normalize(profile.Provider, kind, value)
		}
		if err != nil {
			return nil, errors.New("identifier value is invalid")
		}
		key := string(kind) + "\x00" + normalized
		if _, exists := seen[key]; exists {
			return nil, errors.New("identifiers must be unique")
		}
		seen[key] = struct{}{}
		identifiers = append(identifiers, store.APIIdentity{Kind: kind, Value: value})
	}
	return identifiers, nil
}

func normalizeEmailIdentifier(value string) (string, error) {
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Name != "" || parsed.Address != value {
		return "", errors.New("email identifier is not a bare address")
	}
	return identity.Normalize("github", identity.KindEmail, value)
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, logger apiLogger, target any) bool {
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		writeError(w, r, logger, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || mediaType != "application/json" {
		writeError(w, r, logger, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, r, logger, http.StatusRequestEntityTooLarge, "request body exceeds 64 KiB")
		} else {
			writeError(w, r, logger, http.StatusBadRequest, "failed to read request body")
		}
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "request body is invalid JSON")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, r, logger, http.StatusBadRequest, "request body must contain one JSON value")
		return false
	}
	return true
}

func allowQueryKeys(w http.ResponseWriter, r *http.Request, logger apiLogger, allowed ...string) bool {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "query string is invalid")
		return false
	}
	for key := range query {
		if !utf8.ValidString(key) || strings.ContainsRune(key, '\x00') {
			writeError(w, r, logger, http.StatusBadRequest, "query string is invalid")
			return false
		}
		for _, value := range query[key] {
			if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
				writeError(w, r, logger, http.StatusBadRequest, "query string is invalid")
				return false
			}
		}
		for _, candidate := range allowed {
			if key == candidate {
				goto next
			}
		}
		writeError(w, r, logger, http.StatusBadRequest, "unsupported query parameter")
		return false
	next:
	}
	return true
}

func oneQueryValue(w http.ResponseWriter, r *http.Request, logger apiLogger, name string, required bool) (string, bool) {
	values, exists := r.URL.Query()[name]
	if !exists {
		if required {
			writeError(w, r, logger, http.StatusBadRequest, name+" is required")
			return "", false
		}
		return "", true
	}
	if len(values) != 1 || required && values[0] == "" {
		writeError(w, r, logger, http.StatusBadRequest, name+" must be provided once")
		return "", false
	}
	if values[0] == "" {
		writeError(w, r, logger, http.StatusBadRequest, name+" must not be empty")
		return "", false
	}
	return values[0], true
}

func requestedLimit(w http.ResponseWriter, r *http.Request, logger apiLogger, fallback, maximum int) (int, bool) {
	raw, ok := oneQueryValue(w, r, logger, "limit", false)
	if !ok {
		return 0, false
	}
	if raw == "" {
		return fallback, true
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > maximum {
		writeError(w, r, logger, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maximum))
		return 0, false
	}
	return limit, true
}

func requestedPersonID(w http.ResponseWriter, r *http.Request, logger apiLogger) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "invalid person ID")
		return uuid.Nil, false
	}
	return id, true
}

func requestedTimeRange(w http.ResponseWriter, r *http.Request, logger apiLogger, required bool) (time.Time, time.Time, bool) {
	fromRaw, ok := oneQueryValue(w, r, logger, "from", required)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	toRaw, ok := oneQueryValue(w, r, logger, "to", required)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	if !required && fromRaw == "" && toRaw == "" {
		return time.Time{}, time.Time{}, true
	}
	from, err := time.Parse(time.RFC3339Nano, fromRaw)
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "from must be an RFC3339 timestamp")
		return time.Time{}, time.Time{}, false
	}
	to, err := time.Parse(time.RFC3339Nano, toRaw)
	if err != nil || !from.Before(to) {
		writeError(w, r, logger, http.StatusBadRequest, "to must be an RFC3339 timestamp after from")
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}

func optionalTimeRange(w http.ResponseWriter, r *http.Request, logger apiLogger) (*time.Time, *time.Time, bool, bool) {
	fromRaw, ok := oneQueryValue(w, r, logger, "from", false)
	if !ok {
		return nil, nil, false, false
	}
	toRaw, ok := oneQueryValue(w, r, logger, "to", false)
	if !ok {
		return nil, nil, false, false
	}
	if fromRaw == "" && toRaw == "" {
		return nil, nil, false, true
	}
	if fromRaw == "" || toRaw == "" {
		writeError(w, r, logger, http.StatusBadRequest, "from and to must be provided together")
		return nil, nil, false, false
	}
	from, err := time.Parse(time.RFC3339Nano, fromRaw)
	if err != nil {
		writeError(w, r, logger, http.StatusBadRequest, "from must be an RFC3339 timestamp")
		return nil, nil, false, false
	}
	to, err := time.Parse(time.RFC3339Nano, toRaw)
	if err != nil || !from.Before(to) {
		writeError(w, r, logger, http.StatusBadRequest, "to must be an RFC3339 timestamp after from")
		return nil, nil, false, false
	}
	return &from, &to, true, true
}

func toPersonResponse(person store.APIPerson, sourceCoverage []store.APISourceCoverage) personResponse {
	sources := make([]sourceCoverageResponse, 0)
	for _, coverage := range sourceCoverage {
		sources = append(sources, sourceCoverageResponse{
			Source: coverage.Source, Status: coverage.Status, From: coverage.From,
			To: coverage.To, GapCount: coverage.GapCount, RecordedAt: coverage.RecordedAt,
		})
	}
	return personResponse{
		ID: person.ID, DisplayName: person.DisplayName,
		CreatedAt: person.CreatedAt, Sources: sources,
	}
}

func toJobResponse(job store.APIJob) jobResponse {
	return jobResponse{
		ID: job.ID, PersonID: job.PersonID, Source: job.Source, Status: job.Status,
		Attempts: job.Attempts, MaxAttempts: job.MaxAttempts, CreatedAt: job.CreatedAt, UpdatedAt: job.UpdatedAt,
	}
}

func apiKeyFromOptions(r *http.Request) string {
	_, key, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found {
		return ""
	}
	return key
}

const defaultForgeHost = "github.com"

// requestedForge resolves an optional forge host, defaulting to GitHub.com.
func requestedForge(w http.ResponseWriter, r *http.Request, s apiStore, logger apiLogger, host string) (store.ForgeInstance, store.SourceProfile, bool) {
	if host == "" {
		host = defaultForgeHost
	}
	forge, profile, err := s.ForgeForHost(r.Context(), host)
	if errors.Is(err, store.ErrUnknownForge) {
		writeError(w, r, logger, http.StatusBadRequest, "forge is not supported")
		return store.ForgeInstance{}, store.SourceProfile{}, false
	}
	if err != nil {
		logger.ErrorContext(r.Context(), "load forge", "request_id", r.Header.Get("X-Request-ID"), "forge", host, "err", err)
		writeError(w, r, logger, http.StatusInternalServerError, "failed to load forge")
		return store.ForgeInstance{}, store.SourceProfile{}, false
	}
	return forge, profile, true
}

func validLogin(profile store.SourceProfile, login string) bool {
	if profile.Provider == "github" {
		return github.ValidLogin(login)
	}
	return identity.ValidHandle(login)
}
