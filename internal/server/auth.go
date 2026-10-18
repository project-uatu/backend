// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// Options configures access to the versioned API.
type Options struct {
	APIKey      string
	CORSOrigins []string
}

type apiLogger interface {
	ErrorContext(context.Context, string, ...any)
	Error(string, ...any)
}

func secureAPI(next http.Handler, options Options, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.NewString()
		w.Header().Set("X-Request-ID", requestID)
		r.Header.Set("X-Request-ID", requestID)
		if strings.HasPrefix(r.URL.Path, "/v1") {
			w.Header().Set("Cache-Control", "no-store")
		}

		origins := r.Header.Values("Origin")
		if len(origins) > 1 {
			writeAuthError(w, logger, requestID, http.StatusForbidden, "origin_not_allowed", "origin not allowed")
			return
		}
		origin := r.Header.Get("Origin")
		if origin != "" {
			if !slices.Contains(options.CORSOrigins, origin) {
				writeAuthError(w, logger, requestID, http.StatusForbidden, "origin_not_allowed", "origin not allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Expose-Headers", "Location, X-Request-ID")
			w.Header().Add("Vary", "Origin")
			if r.Method == http.MethodOptions {
				allowPreflight(w, r, logger, requestID)
				return
			}
		}

		if strings.HasPrefix(r.URL.Path, "/v1") {
			if r.Method == http.MethodOptions {
				writeAuthError(w, logger, requestID, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
				return
			}
			authorization := r.Header.Values("Authorization")
			if len(authorization) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="uatu"`)
				writeAuthError(w, logger, requestID, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
				return
			}
			scheme, provided, found := strings.Cut(authorization[0], " ")
			if !found || !strings.EqualFold(scheme, "Bearer") ||
				provided == "" || strings.ContainsAny(provided, " \t\r\n") {
				w.Header().Set("WWW-Authenticate", `Bearer realm="uatu"`)
				writeAuthError(w, logger, requestID, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
				return
			}
			if !hmac.Equal([]byte(provided), []byte(options.APIKey)) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="uatu"`)
				writeAuthError(w, logger, requestID, http.StatusUnauthorized, "unauthorized", "valid bearer token required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func allowPreflight(w http.ResponseWriter, r *http.Request, logger apiLogger, requestID string) {
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	methods := r.Header.Values("Access-Control-Request-Method")
	if len(methods) != 1 {
		writeAuthError(w, logger, requestID, http.StatusForbidden, "method_not_allowed", "cross-origin method not allowed")
		return
	}
	method := methods[0]
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodPatch {
		writeAuthError(w, logger, requestID, http.StatusForbidden, "method_not_allowed", "cross-origin method not allowed")
		return
	}
	requestHeaders := r.Header.Values("Access-Control-Request-Headers")
	if len(requestHeaders) > 1 {
		writeAuthError(w, logger, requestID, http.StatusForbidden, "header_not_allowed", "cross-origin header not allowed")
		return
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "", "authorization", "content-type", "idempotency-key":
		default:
			writeAuthError(w, logger, requestID, http.StatusForbidden, "header_not_allowed", "cross-origin header not allowed")
			return
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func writeAuthError(w http.ResponseWriter, logger apiLogger, requestID string, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}{Error: struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}{Code: code, Message: message, RequestID: requestID}}); err != nil {
		logger.Error("write API authentication response", "request_id", requestID, "err", err)
	}
}
