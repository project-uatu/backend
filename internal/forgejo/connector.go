// Package forgejo collects public Codeberg (Forgejo) person activity.
package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/forgehttp"
	"github.com/project-uatu/backend/internal/forgenorm"
	"github.com/project-uatu/backend/internal/identity"
)

// Source is the stored activity stream of this connector.
const Source event.Source = "forgejo_history"

const (
	// DefaultAPIURL is the Codeberg REST API root.
	DefaultAPIURL = "https://codeberg.org/api/v1"
	quotaBucket   = "forgejo/codeberg.org/api"
	pageSize      = 50
	// incrementalOverlap re-reads recent feed entries; raw events are
	// idempotent by external ID.
	incrementalOverlap = 24 * time.Hour
	coverageScope      = "public activity feed entries: issues, pull requests, merges, reviews and releases; heatmap counts are aggregates only"
)

// Options configures a Connector.
type Options struct {
	// APIURL overrides the API root, for tests.
	APIURL string
	// Token is an optional read-only token.
	Token      string
	HTTPClient *http.Client
}

// Connector reads a person's public Forgejo activity feed.
type Connector struct {
	client *forgehttp.Client
}

// New creates a Codeberg connector.
func New(options Options) *Connector {
	if options.APIURL == "" {
		options.APIURL = DefaultAPIURL
	}
	return &Connector{client: forgehttp.New(forgehttp.Options{
		BaseURL: options.APIURL, Token: options.Token, HTTPClient: options.HTTPClient,
	})}
}

// Source identifies this connector's stored activity stream.
func (c *Connector) Source() event.Source { return Source }

// QuotaBucket names the Codeberg API quota.
func (c *Connector) QuotaBucket() string { return quotaBucket }

type scanState struct {
	Login     string     `json:"login"`
	AccountID string     `json:"account_id"`
	Stage     string     `json:"stage"`
	Page      int        `json:"page"`
	Start     time.Time  `json:"start,omitempty"`
	Cutoff    *time.Time `json:"cutoff,omitempty"`
	Earliest  *time.Time `json:"earliest,omitempty"`
}

func decodeState(request event.PageRequest) (scanState, error) {
	state := scanState{Login: request.OnboardingLogin}
	for _, key := range request.IdentityKeys {
		switch key.IdentityKey.Kind {
		case identity.KindLogin:
			if state.Login == "" {
				state.Login = key.IdentityKey.Value
			}
		case identity.KindAccountID:
			state.AccountID = key.IdentityKey.Value
		}
	}
	if state.Login == "" {
		state.Login = request.CurrentLogin
	}
	if request.Cursor != "" {
		var stored scanState
		if err := json.Unmarshal([]byte(request.Cursor), &stored); err != nil {
			return scanState{}, errors.New("invalid Forgejo history cursor")
		}
		if stored.AccountID != "" {
			state.AccountID = stored.AccountID
		}
		if stored.Login != "" {
			state.Login = stored.Login
		}
		state.Stage, state.Page = stored.Stage, stored.Page
		state.Start, state.Cutoff, state.Earliest = stored.Start, stored.Cutoff, stored.Earliest
	}
	return state, nil
}

func (c *Connector) begin(request event.PageRequest, state *scanState, created time.Time) {
	state.Stage, state.Page = "feed", 1
	state.Start = request.Window.From.UTC()
	if request.Window.From.IsZero() {
		state.Start = created
	}
	if request.ScanMode == "incremental" && !request.Window.From.IsZero() {
		cutoff := request.Window.From.UTC().Add(-incrementalOverlap)
		state.Cutoff = &cutoff
	}
}

// FetchPage performs one bounded upstream request.
func (c *Connector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	state, err := decodeState(request)
	if err != nil {
		return event.Page{}, event.FetchReport{}, err
	}
	report := event.FetchReport{RateLimitBucket: quotaBucket, CoverageScope: coverageScope}
	if !identity.ValidHandle(state.Login) {
		return event.Page{}, report, errors.New("history requires a valid Forgejo login identity")
	}
	if request.Cursor == "" {
		if state.AccountID == "" {
			return c.resolve(ctx, request, state, report)
		}
		c.begin(request, &state, time.Time{})
	}
	var events []event.RawEvent
	switch state.Stage {
	case "feed":
		events, err = c.feed(ctx, request, &state, &report)
	case "heatmap":
		err = c.heatmap(ctx, &state, &report)
	default:
		err = fmt.Errorf("unknown Forgejo history stage %q", state.Stage)
	}
	if err != nil {
		return event.Page{}, report, err
	}
	if state.Stage == "done" {
		return event.Page{Events: events, Done: true}, report, nil
	}
	next, err := json.Marshal(state)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("encode Forgejo history cursor: %w", err)
	}
	return event.Page{Events: events, NextCursor: string(next)}, report, nil
}

func (c *Connector) resolve(ctx context.Context, request event.PageRequest, state scanState, report event.FetchReport) (event.Page, event.FetchReport, error) {
	res, upstream, err := c.client.Get(ctx, "/users/"+url.PathEscape(state.Login), nil, quotaBucket)
	mergeReport(&report, upstream)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("resolve Forgejo account: %w", err)
	}
	// Only identifiers are decoded; the profile email is never read.
	var user struct {
		ID      int64      `json:"id"`
		Login   string     `json:"login"`
		Created *time.Time `json:"created"`
	}
	if err := json.Unmarshal(res.Body, &user); err != nil {
		return event.Page{}, report, fmt.Errorf("decode Forgejo account: %w", err)
	}
	if user.ID <= 0 || !identity.ValidHandle(user.Login) {
		return event.Page{}, report, errors.New("upstream returned an invalid Forgejo account identity")
	}
	resolved := &event.ResolvedAccount{AccountID: strconv.FormatInt(user.ID, 10), Login: user.Login}
	var created time.Time
	if user.Created != nil {
		created = user.Created.UTC()
		resolved.CreatedAt = created
	}
	state.AccountID, state.Login = resolved.AccountID, user.Login
	c.begin(request, &state, created)
	next, err := json.Marshal(state)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("encode resolved Forgejo cursor: %w", err)
	}
	return event.Page{NextCursor: string(next), ResolvedAccount: resolved}, report, nil
}

type activity struct {
	ID        int64     `json:"id"`
	OpType    string    `json:"op_type"`
	ActUserID int64     `json:"act_user_id"`
	RefName   string    `json:"ref_name"`
	IsPrivate bool      `json:"is_private"`
	Content   string    `json:"content"`
	Created   time.Time `json:"created"`
	Repo      *struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		HTMLURL  string `json:"html_url"`
	} `json:"repo"`
}

func (c *Connector) feed(ctx context.Context, request event.PageRequest, state *scanState, report *event.FetchReport) ([]event.RawEvent, error) {
	query := url.Values{
		"only-performed-by": {"true"}, "limit": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(state.Page)},
	}
	res, upstream, err := c.client.Get(ctx, "/users/"+url.PathEscape(state.Login)+"/activities/feeds", query, quotaBucket)
	mergeReport(report, upstream)
	if err != nil {
		return nil, fmt.Errorf("list Forgejo activity feed: %w", err)
	}
	// Decoding into a closed struct drops embedded user and comment fields,
	// including email addresses, before anything is persisted.
	var activities []activity
	if err := json.Unmarshal(res.Body, &activities); err != nil {
		return nil, fmt.Errorf("decode Forgejo activity feed: %w", err)
	}
	var events []event.RawEvent
	reachedCutoff := false
	for _, a := range activities {
		at := a.Created.UTC()
		if state.Earliest == nil || at.Before(*state.Earliest) {
			state.Earliest = &at
		}
		if state.Cutoff != nil && at.Before(*state.Cutoff) {
			reachedCutoff = true
			continue
		}
		raw, ok, err := buildEvent(request, state.AccountID, a)
		if err != nil {
			return nil, err
		}
		if ok {
			events = append(events, raw)
			addKnown(report, a.Repo.FullName)
		}
	}
	if len(activities) == 0 || reachedCutoff {
		state.Stage, state.Page = "heatmap", 0
		return events, nil
	}
	state.Page++
	return events, nil
}

func addKnown(report *event.FetchReport, name string) {
	for _, known := range report.KnownRepositories {
		if known == name {
			return
		}
	}
	report.KnownRepositories = append(report.KnownRepositories, name)
}

func buildEvent(request event.PageRequest, accountID string, a activity) (event.RawEvent, bool, error) {
	if a.ID <= 0 || strconv.FormatInt(a.ActUserID, 10) != accountID || a.Repo == nil ||
		a.Repo.Private || a.IsPrivate || a.Repo.FullName == "" || a.Repo.ID <= 0 || a.Created.IsZero() {
		return event.RawEvent{}, false, nil
	}
	created := a.Created.UTC()
	payload := forgenorm.Payload{Project: a.Repo.FullName, OccurredAt: &created}
	var kind event.Kind
	var externalID string
	role := "author"
	index, title := parseContent(a.Content)
	switch a.OpType {
	case "create_issue":
		kind, externalID = forgenorm.KindIssue, fmt.Sprintf("issue:%d:%s", a.Repo.ID, index)
		payload.Title, payload.CreatedAt = title, &created
		payload.URL = a.Repo.HTMLURL + "/issues/" + index
	case "create_pull_request":
		kind, externalID = forgenorm.KindChangeRequest, fmt.Sprintf("pull_request:%d:%s", a.Repo.ID, index)
		payload.Title, payload.CreatedAt = title, &created
		payload.URL = a.Repo.HTMLURL + "/pulls/" + index
	case "merge_pull_request":
		kind, externalID = forgenorm.KindChangeRequestMerged, fmt.Sprintf("merge:%d:%s", a.Repo.ID, index)
		payload.Title = title
		payload.URL = a.Repo.HTMLURL + "/pulls/" + index
	case "approve_pull_request", "reject_pull_request":
		// The second content element is the review body, which is not stored.
		kind, role, externalID = forgenorm.KindReview, "reviewer", fmt.Sprintf("activity:%d", a.ID)
		payload.State = "approved"
		if a.OpType == "reject_pull_request" {
			payload.State = "changes_requested"
		}
		payload.URL = a.Repo.HTMLURL + "/pulls/" + index
	case "publish_release":
		if a.RefName == "" {
			return event.RawEvent{}, false, nil
		}
		tag := strings.TrimPrefix(a.RefName, "refs/tags/")
		kind, externalID = forgenorm.KindRelease, fmt.Sprintf("release:%d:%s", a.Repo.ID, tag)
		payload.Title = tag
		payload.URL = a.Repo.HTMLURL + "/releases/tag/" + url.PathEscape(tag)
	default:
		return event.RawEvent{}, false, nil
	}
	if strings.HasSuffix(externalID, ":") {
		return event.RawEvent{}, false, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return event.RawEvent{}, false, fmt.Errorf("encode Forgejo payload: %w", err)
	}
	forgeID := request.ForgeInstanceID
	return event.RawEvent{
		ForgeInstanceID: forgeID, Source: request.Source, ExternalID: externalID, Kind: kind,
		OccurredAt: &created, Payload: body, PayloadSchemaVersion: 1,
		Actors: []event.Actor{{Key: event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: accountID},
			ForgeInstanceID: &forgeID,
		}, Role: role}},
	}, true, nil
}

// parseContent splits the feed content, a JSON array of the issue index and a
// title or comment, into index and title. Only numeric indexes are accepted.
func parseContent(content string) (index, title string) {
	var parts []string
	if err := json.Unmarshal([]byte(content), &parts); err != nil || len(parts) == 0 {
		return "", ""
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return "", ""
	}
	if len(parts) > 1 {
		title = parts[1]
	}
	return parts[0], title
}

func (c *Connector) heatmap(ctx context.Context, state *scanState, report *event.FetchReport) error {
	res, upstream, err := c.client.Get(ctx, "/users/"+url.PathEscape(state.Login)+"/heatmap", nil, quotaBucket)
	mergeReport(report, upstream)
	if err != nil {
		return fmt.Errorf("read Forgejo heatmap: %w", err)
	}
	var points []struct {
		Timestamp     int64 `json:"timestamp"`
		Contributions int64 `json:"contributions"`
	}
	if err := json.Unmarshal(res.Body, &points); err != nil {
		return fmt.Errorf("decode Forgejo heatmap: %w", err)
	}
	byDay := map[time.Time]int64{}
	for _, point := range points {
		if point.Timestamp <= 0 || point.Contributions < 0 {
			continue
		}
		day := time.Unix(point.Timestamp, 0).UTC().Truncate(24 * time.Hour)
		byDay[day] += point.Contributions
	}
	for day, value := range byDay {
		report.ContributionCounts = append(report.ContributionCounts, event.ContributionCount{
			Date: day, Metric: "heatmap_contributions", Value: value,
		})
	}
	if state.Cutoff == nil && state.Earliest != nil && !state.Start.IsZero() &&
		state.Earliest.Sub(state.Start) > 24*time.Hour {
		report.Gaps = append(report.Gaps, event.Gap{
			From: state.Start, To: *state.Earliest, Reason: "Forgejo activity feed does not reach the start of the window",
		})
	}
	state.Stage = "done"
	return nil
}

func mergeReport(dst *event.FetchReport, src event.FetchReport) {
	if src.RateLimitLimit != nil {
		dst.RateLimitLimit = src.RateLimitLimit
	}
	if src.RateLimitRemaining != nil {
		dst.RateLimitRemaining = src.RateLimitRemaining
	}
	if src.RateLimitResetAt != nil {
		dst.RateLimitResetAt = src.RateLimitResetAt
	}
}
