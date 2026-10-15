// Package github retrieves public repository data from GitHub.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const responseLimit = 4 << 20

// Client retrieves repository data from the GitHub REST API.
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

type repository struct {
	Name string `json:"name"`
}

// Repositories returns the names of repositories owned by user, most recently
// updated first.
func (c *Client) Repositories(ctx context.Context, user string) ([]string, error) {
	if !validUsername(user) {
		return nil, fmt.Errorf("invalid GitHub username %q", user)
	}
	var names []string
	nextURL := c.baseURL + "/users/" + url.PathEscape(user) + "/repos?type=owner&sort=updated&direction=desc&per_page=100"

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, fmt.Errorf("build GitHub request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}

		res, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("query GitHub repositories for %s: %w", user, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
		closeErr := res.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read GitHub response: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close GitHub response: %w", closeErr)
		}
		if len(body) > responseLimit {
			return nil, fmt.Errorf("GitHub response exceeded %d bytes", responseLimit)
		}
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub returned %s: %s", res.Status, strings.TrimSpace(string(body)))
		}

		var repos []repository
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("decode GitHub repositories: %w", err)
		}
		for _, repo := range repos {
			names = append(names, repo.Name)
		}
		nextURL, err = nextPageURL(res.Header.Get("Link"), c.baseURL)
		if err != nil {
			return nil, err
		}
	}
	return names, nil
}

func validUsername(user string) bool {
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

func nextPageURL(link, baseURL string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub API base URL: %w", err)
	}
	for _, part := range strings.Split(link, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 || strings.TrimSpace(sections[1]) != `rel="next"` {
			continue
		}
		value := strings.TrimSpace(sections[0])
		if len(value) < 2 || value[0] != '<' || value[len(value)-1] != '>' {
			return "", fmt.Errorf("invalid GitHub pagination link %q", value)
		}
		nextURL := value[1 : len(value)-1]
		parsed, err := url.Parse(nextURL)
		if err != nil || parsed.Scheme != base.Scheme || parsed.Host != base.Host {
			return "", fmt.Errorf("invalid GitHub pagination URL %q", nextURL)
		}
		return nextURL, nil
	}
	return "", nil
}
