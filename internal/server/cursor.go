// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pageCursor struct {
	Version    int       `json:"v"`
	Kind       string    `json:"k"`
	Filter     string    `json:"f"`
	ID         uuid.UUID `json:"id,omitempty"`
	OccurredAt time.Time `json:"t,omitempty"`
	Role       string    `json:"r,omitempty"`
	Project    string    `json:"p,omitempty"`
}

func encodePageCursor(secret string, cursor pageCursor) (string, error) {
	cursor.Version = 1
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := signCursor(secret, encoded)
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func decodePageCursor(secret, token, kind, filter string) (pageCursor, error) {
	encoded, suppliedSignature, found := strings.Cut(token, ".")
	if !found || len(token) > 2048 {
		return pageCursor{}, errors.New("invalid cursor")
	}
	signature, err := base64.RawURLEncoding.DecodeString(suppliedSignature)
	if err != nil || !hmac.Equal(signature, signCursor(secret, encoded)) {
		return pageCursor{}, errors.New("invalid cursor")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return pageCursor{}, errors.New("invalid cursor")
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil ||
		cursor.Version != 1 || cursor.Kind != kind || cursor.Filter != filter {
		return pageCursor{}, errors.New("cursor does not match this request")
	}
	return cursor, nil
}

func signCursor(secret, payload string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}
