// Package devstats retrieves contribution summaries from CNCF DevStats.
package devstats

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiURL        = "https://devstats.cncf.io/api/v1"
	responseLimit = 1 << 20
)

// Client queries the CNCF DevStats API.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// Contribution contains the contribution counts returned by DevStats.
type Contribution struct {
	Contributions int `json:"contributions"`
	Issues        int `json:"issues"`
	PRs           int `json:"prs"`
}

// NewClient creates a DevStats client. A nil HTTP client uses a 30-second
// timeout.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{httpClient: httpClient, baseURL: apiURL}
}

// Contributions returns the contribution summary for username.
func (c *Client) Contributions(ctx context.Context, username string) (Contribution, error) {
	payload, err := json.Marshal(struct {
		API     string            `json:"api"`
		Payload map[string]string `json:"payload"`
	}{
		API:     "GithubIDContributions",
		Payload: map[string]string{"github_id": username},
	})
	if err != nil {
		return Contribution{}, fmt.Errorf("encode DevStats request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
	if err != nil {
		return Contribution{}, fmt.Errorf("build DevStats request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	res, err := c.httpClient.Do(req)
	if err != nil {
		return Contribution{}, fmt.Errorf("query DevStats for %s: %w", username, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(res.Body, responseLimit+1))
	closeErr := res.Body.Close()
	if readErr != nil {
		return Contribution{}, fmt.Errorf("read DevStats response: %w", readErr)
	}
	if closeErr != nil {
		return Contribution{}, fmt.Errorf("close DevStats response: %w", closeErr)
	}
	if len(body) > responseLimit {
		return Contribution{}, fmt.Errorf("DevStats response exceeded %d bytes", responseLimit)
	}
	if res.StatusCode != http.StatusOK {
		return Contribution{}, fmt.Errorf("DevStats returned %s: %s", res.Status, bytes.TrimSpace(body))
	}

	var result Contribution
	if err := json.Unmarshal(body, &result); err != nil {
		return Contribution{}, fmt.Errorf("decode DevStats response: %w", err)
	}
	return result, nil
}
