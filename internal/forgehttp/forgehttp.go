// Package forgehttp provides the HTTP transport shared by forge connectors:
// a descriptive User-Agent, bounded responses, rate-limit header parsing and
// error values that never include response content.
package forgehttp

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
)

// MaxBody is the largest response body a connector will read.
const MaxBody = 4 << 20

// UserAgent identifies Uatu to upstream forges.
func UserAgent() string {
	version := "dev"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
	return "uatu/" + version + " (+https://github.com/project-uatu/backend)"
}

// StatusError reports a non-success HTTP status without response content.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string { return fmt.Sprintf("forge returned HTTP %d", e.StatusCode) }

// Options configures a Client.
type Options struct {
	// BaseURL is the API root, for example https://gitlab.com/api/v4.
	BaseURL string
	// Token is an optional read-only credential sent as a Bearer token.
	Token      string
	HTTPClient *http.Client
}

// Client performs bounded GET requests against one forge API root.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New creates a client. A nil HTTP client uses a 30-second timeout.
func New(options Options) *Client {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimSuffix(options.BaseURL, "/"), token: options.Token, http: httpClient}
}

// BaseURL returns the API root without a trailing slash.
func (c *Client) BaseURL() string { return c.baseURL }

// Response is a successful, fully read response.
type Response struct {
	Body   []byte
	Header http.Header
}

// Get requests baseURL+path with query parameters. It returns the rate-limit
// state observed on the response, including for error statuses.
func (c *Client) Get(ctx context.Context, path string, query url.Values, bucket string) (*Response, event.FetchReport, error) {
	report := event.FetchReport{RateLimitBucket: bucket}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, report, fmt.Errorf("build forge request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent())
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := c.http.Do(req)
	if err != nil {
		// The URL error can contain the request URL, which is not secret, but
		// keep only the cause class to avoid leaking query values.
		if ctx.Err() != nil {
			return nil, report, ctx.Err()
		}
		return nil, report, fmt.Errorf("request forge resource: %w", urlCause(err))
	}
	ParseRateLimit(res.Header, time.Now().UTC(), &report)
	body, readErr := io.ReadAll(io.LimitReader(res.Body, MaxBody+1))
	closeErr := res.Body.Close()
	if readErr != nil {
		return nil, report, fmt.Errorf("read forge response: %w", readErr)
	}
	if closeErr != nil {
		return nil, report, fmt.Errorf("close forge response: %w", closeErr)
	}
	if len(body) > MaxBody {
		return nil, report, fmt.Errorf("forge response exceeded %d bytes", MaxBody)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, report, &StatusError{StatusCode: res.StatusCode}
	}
	return &Response{Body: body, Header: res.Header}, report, nil
}

func urlCause(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

var rateLimitParam = regexp.MustCompile(`;\s*([a-z])=(\d+)`)

// ParseRateLimit records limit, remaining and reset information from the
// header conventions used by GitLab (RateLimit-Limit/Remaining/Reset as Unix
// seconds), Forgejo (the IETF draft RateLimit and RateLimit-Policy fields) and
// X-RateLimit-* headers. A Retry-After header marks the bucket exhausted.
func ParseRateLimit(header http.Header, now time.Time, report *event.FetchReport) {
	for _, prefix := range []string{"RateLimit-", "X-RateLimit-"} {
		if v := atoi(header.Get(prefix + "Limit")); v != nil {
			report.RateLimitLimit = v
		}
		if v := atoi(header.Get(prefix + "Remaining")); v != nil {
			report.RateLimitRemaining = v
		}
		if v := atoi(header.Get(prefix + "Reset")); v != nil {
			resetAt := time.Unix(int64(*v), 0).UTC()
			report.RateLimitResetAt = &resetAt
		}
	}
	// IETF draft: RateLimit: "name";r=<remaining>;t=<seconds until reset>
	// and RateLimit-Policy: "name";q=<quota>;w=<window seconds>.
	if value := header.Get("RateLimit"); strings.Contains(value, ";") {
		for _, match := range rateLimitParam.FindAllStringSubmatch(value, -1) {
			n := atoi(match[2])
			if n == nil {
				continue
			}
			switch match[1] {
			case "r":
				report.RateLimitRemaining = n
			case "t":
				resetAt := now.Add(time.Duration(*n) * time.Second)
				report.RateLimitResetAt = &resetAt
			}
		}
	}
	if value := header.Get("RateLimit-Policy"); strings.Contains(value, ";") {
		for _, match := range rateLimitParam.FindAllStringSubmatch(value, -1) {
			if match[1] == "q" {
				report.RateLimitLimit = atoi(match[2])
			}
		}
	}
	if retryAfter := header.Get("Retry-After"); retryAfter != "" {
		var resetAt time.Time
		if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds >= 0 {
			resetAt = now.Add(time.Duration(seconds) * time.Second)
		} else if at, err := http.ParseTime(retryAfter); err == nil {
			resetAt = at.UTC()
		}
		if !resetAt.IsZero() {
			zero := 0
			report.RateLimitRemaining = &zero
			report.RateLimitResetAt = &resetAt
		}
	}
}

func atoi(value string) *int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

// HasNextPage reports whether a response advertises a following page through
// a Link header with rel="next".
func HasNextPage(header http.Header) bool {
	for _, link := range header.Values("Link") {
		for _, part := range strings.Split(link, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}
