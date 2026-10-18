// SPDX-License-Identifier: Apache-2.0

// Package identity normalizes forge identifiers without guessing person names.
package identity

import (
	"errors"
	"fmt"
	"strings"
)

// Kind identifies the type of an external identity key.
type Kind string

// Key is a typed identifier observed on a source event.
type Key struct {
	Kind  Kind   `json:"kind"`
	Value string `json:"value"`
}

// Validate checks that a typed identity key contains a supported kind and value.
func (k Key) Validate() error {
	if k.Value == "" {
		return errors.New("identity key value is required")
	}
	switch k.Kind {
	case KindEmail, KindAccountID, KindLogin:
		return nil
	default:
		return fmt.Errorf("unsupported identity kind %q", k.Kind)
	}
}

const (
	KindEmail     Kind = "email"
	KindAccountID Kind = "account_id"
	KindLogin     Kind = "login"
)

// Normalize returns the comparison form for a source identity key.
func Normalize(provider string, kind Kind, value string) (string, error) {
	switch kind {
	case KindEmail:
		return normalizeEmail(value)
	case KindAccountID:
		if value == "" || strings.TrimSpace(value) != value {
			return "", errors.New("account ID must be non-empty and have no surrounding whitespace")
		}
		return value, nil
	case KindLogin:
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
			return "", errors.New("login must be non-empty and contain no whitespace")
		}
		switch strings.ToLower(provider) {
		case "github", "gitlab", "forgejo":
			return strings.ToLower(value), nil
		default:
			return "", fmt.Errorf("unsupported login provider %q", provider)
		}
	default:
		return "", fmt.Errorf("unsupported identity kind %q", kind)
	}
}

func normalizeEmail(value string) (string, error) {
	value = strings.TrimSpace(value)
	local, domain, ok := strings.Cut(value, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", errors.New("email identity must contain one non-empty local part and domain")
	}
	return local + "@" + strings.ToLower(domain), nil
}

// ValidHandle reports whether login is a plausible GitLab or Forgejo username:
// 1 to 255 ASCII letters, digits, underscores, dots or hyphens that does not
// start with a dot or hyphen.
func ValidHandle(login string) bool {
	if len(login) == 0 || len(login) > 255 {
		return false
	}
	for i := 0; i < len(login); i++ {
		c := login[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
		if !alnum && (i == 0 || c != '.' && c != '-') {
			return false
		}
	}
	return true
}
