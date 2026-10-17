// Package github retrieves public person history from GitHub.
package github

import (
	"net/http"
	"time"
)

const responseLimit = 4 << 20

// Client retrieves public person history from the GitHub APIs.
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewClient creates a GitHub client. A nil HTTP client uses a 30-second
// timeout.
func NewClient(token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{
		httpClient: httpClient,
		baseURL:    "https://api.github.com",
		token:      token,
	}
}

// ValidLogin reports whether user is a syntactically valid GitHub login.
func ValidLogin(user string) bool {
	if len(user) == 0 || len(user) > 39 {
		return false
	}
	for i, char := range user {
		valid := char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '-' && i > 0 && i < len(user)-1
		if !valid {
			return false
		}
	}
	return true
}
