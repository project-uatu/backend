// Package gitlab collects public GitLab.com person activity.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/project-uatu/backend/internal/event"
	"github.com/project-uatu/backend/internal/forgehttp"
	"github.com/project-uatu/backend/internal/forgenorm"
	"github.com/project-uatu/backend/internal/identity"
)

// Source is the stored activity stream of this connector.
const Source event.Source = "gitlab_history"

const (
	// DefaultAPIURL is the GitLab.com REST API root.
	DefaultAPIURL = "https://gitlab.com/api/v4"
	quotaBucket   = "gitlab/gitlab.com/api"
	pageSize      = 50
	// incrementalOverlap re-reads recently updated records so late edits are
	// not missed; raw events are idempotent by external ID.
	incrementalOverlap = 24 * time.Hour
	coverageScope      = "authored issues and merge requests: public projects; releases: known public projects only; reviews: recent approval events only"
)

// floorDate is used when GitLab does not expose the account creation time;
// GitLab.com launched in 2011.
var floorDate = time.Date(2011, 1, 1, 0, 0, 0, 0, time.UTC)

// Options configures a Connector.
type Options struct {
	// APIURL overrides the API root, for tests.
	APIURL string
	// Token is an optional read-only token.
	Token      string
	HTTPClient *http.Client
}

// Connector pages through public GitLab activity for one person.
type Connector struct {
	client *forgehttp.Client
}

// New creates a GitLab.com connector.
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

// QuotaBucket names the GitLab.com API quota.
func (c *Connector) QuotaBucket() string { return quotaBucket }

type task struct {
	Kind    string            `json:"kind"`
	Start   string            `json:"start,omitempty"`
	End     string            `json:"end,omitempty"`
	Since   string            `json:"since,omitempty"`
	Page    int               `json:"page,omitempty"`
	Project string            `json:"project,omitempty"`
	Type    string            `json:"type,omitempty"`
	Records []json.RawMessage `json:"records,omitempty"`
}

type projectInfo struct {
	Path   string `json:"path"`
	WebURL string `json:"web_url"`
	Public bool   `json:"public"`
}

type scanState struct {
	Login     string                 `json:"login"`
	AccountID string                 `json:"account_id"`
	Tasks     []task                 `json:"tasks"`
	Projects  map[string]projectInfo `json:"projects,omitempty"`
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
			return scanState{}, errors.New("invalid GitLab history cursor")
		}
		if stored.AccountID != "" {
			state.AccountID = stored.AccountID
		}
		if stored.Login != "" {
			state.Login = stored.Login
		}
		state.Tasks, state.Projects = stored.Tasks, stored.Projects
	}
	if state.Projects == nil {
		state.Projects = map[string]projectInfo{}
	}
	return state, nil
}

// FetchPage performs one bounded upstream request.
func (c *Connector) FetchPage(ctx context.Context, request event.PageRequest) (event.Page, event.FetchReport, error) {
	state, err := decodeState(request)
	if err != nil {
		return event.Page{}, event.FetchReport{}, err
	}
	report := event.FetchReport{RateLimitBucket: quotaBucket, CoverageScope: coverageScope}
	if request.Cursor == "" {
		if state.AccountID == "" {
			return c.resolve(ctx, request, state, report)
		}
		c.plan(request, &state, time.Time{})
	}
	if len(state.Tasks) == 0 {
		return event.Page{Done: true}, report, nil
	}
	var events []event.RawEvent
	var runErr error
	switch state.Tasks[0].Kind {
	case "issues", "merge_requests":
		runErr = c.list(ctx, &state, &report)
	case "approvals":
		runErr = c.approvals(ctx, &state, &report)
	case "process":
		events, runErr = c.process(ctx, request, &state, &report)
	case "releases":
		events, runErr = c.releases(ctx, request, &state, &report)
	default:
		runErr = fmt.Errorf("unknown GitLab history cursor task %q", state.Tasks[0].Kind)
	}
	if runErr != nil {
		return event.Page{}, report, runErr
	}
	next, err := json.Marshal(state)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("encode GitLab history cursor: %w", err)
	}
	return event.Page{Events: events, NextCursor: string(next), Done: len(state.Tasks) == 0}, report, nil
}

func (c *Connector) resolve(ctx context.Context, request event.PageRequest, state scanState, report event.FetchReport) (event.Page, event.FetchReport, error) {
	if !identity.ValidHandle(state.Login) {
		return event.Page{}, report, errors.New("GitLab history requires a valid login identity")
	}
	res, upstream, err := c.client.Get(ctx, "/users", url.Values{"username": {state.Login}}, quotaBucket)
	mergeReport(&report, upstream)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("resolve GitLab account: %w", err)
	}
	var users []struct {
		ID        int64      `json:"id"`
		Username  string     `json:"username"`
		CreatedAt *time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(res.Body, &users); err != nil {
		return event.Page{}, report, fmt.Errorf("decode GitLab account: %w", err)
	}
	if len(users) != 1 || users[0].ID <= 0 || !identity.ValidHandle(users[0].Username) {
		return event.Page{}, report, errors.New("GitLab did not return exactly one valid account")
	}
	user := users[0]
	resolved := &event.ResolvedAccount{AccountID: strconv.FormatInt(user.ID, 10), Login: user.Username}
	var created time.Time
	if user.CreatedAt != nil {
		created = user.CreatedAt.UTC()
		resolved.CreatedAt = created
	}
	state.AccountID, state.Login = resolved.AccountID, user.Username
	c.plan(request, &state, created)
	next, err := json.Marshal(state)
	if err != nil {
		return event.Page{}, report, fmt.Errorf("encode resolved GitLab cursor: %w", err)
	}
	return event.Page{NextCursor: string(next), ResolvedAccount: resolved}, report, nil
}

// plan builds the task queue for a new scan.
func (c *Connector) plan(request event.PageRequest, state *scanState, created time.Time) {
	now := time.Now().UTC()
	incremental := request.ScanMode == "incremental" && !request.Window.From.IsZero()
	for _, repo := range request.KnownRepositories {
		state.Tasks = append(state.Tasks, task{Kind: "releases", Project: repo, Page: 1})
	}
	if incremental {
		since := request.Window.From.UTC().Add(-incrementalOverlap).Format(time.RFC3339)
		state.Tasks = append(state.Tasks,
			task{Kind: "issues", Since: since, Page: 1},
			task{Kind: "merge_requests", Since: since, Page: 1},
			task{Kind: "approvals", Start: since, Page: 1})
		return
	}
	start, end := request.Window.From.UTC(), request.Window.To.UTC()
	if request.Window.From.IsZero() {
		start = created
		if start.IsZero() {
			start = floorDate
		}
	}
	if request.Window.To.IsZero() {
		end = now
	}
	for from := start; from.Before(end); {
		to := time.Date(from.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
		if to.After(end) {
			to = end
		}
		for _, kind := range []string{"issues", "merge_requests"} {
			state.Tasks = append(state.Tasks, task{
				Kind: kind, Start: from.Format(time.RFC3339), End: to.Format(time.RFC3339), Page: 1,
			})
		}
		from = to
	}
	state.Tasks = append(state.Tasks, task{Kind: "approvals", Start: start.Format(time.RFC3339), Page: 1})
}

func pop(state *scanState) { state.Tasks = state.Tasks[1:] }

func advance(state *scanState, got int) {
	if got < pageSize {
		pop(state)
		return
	}
	state.Tasks[0].Page++
}

func (c *Connector) list(ctx context.Context, state *scanState, report *event.FetchReport) error {
	t := state.Tasks[0]
	query := url.Values{
		"scope": {"all"}, "author_id": {state.AccountID}, "state": {"all"},
		"order_by": {"created_at"}, "sort": {"asc"},
		"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(t.Page)},
	}
	if t.Since != "" {
		query.Set("updated_after", t.Since)
	} else {
		query.Set("created_after", t.Start)
		query.Set("created_before", t.End)
	}
	res, upstream, err := c.client.Get(ctx, "/"+t.Kind, query, quotaBucket)
	mergeReport(report, upstream)
	if err != nil {
		return fmt.Errorf("list GitLab %s: %w", t.Kind, err)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(res.Body, &records); err != nil {
		return fmt.Errorf("decode GitLab %s: %w", t.Kind, err)
	}
	advance(state, len(records))
	if len(records) > 0 {
		state.Tasks = append([]task{{Kind: "process", Type: t.Kind, Records: records}}, state.Tasks...)
	}
	return nil
}

func (c *Connector) approvals(ctx context.Context, state *scanState, report *event.FetchReport) error {
	t := state.Tasks[0]
	start, err := time.Parse(time.RFC3339, t.Start)
	if err != nil {
		return errors.New("invalid GitLab approvals cursor")
	}
	query := url.Values{
		"action": {"approved"}, "target_type": {"merge_request"}, "sort": {"asc"},
		"after":    {start.AddDate(0, 0, -1).Format("2006-01-02")},
		"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(t.Page)},
	}
	res, upstream, err := c.client.Get(ctx, "/users/"+url.PathEscape(state.AccountID)+"/events", query, quotaBucket)
	mergeReport(report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 &&
		statusErr.StatusCode != http.StatusTooManyRequests {
		report.Gaps = append(report.Gaps, event.Gap{
			From: start, To: time.Now().UTC(), Reason: "GitLab approval events are unavailable",
		})
		pop(state)
		return nil
	}
	if err != nil {
		return fmt.Errorf("list GitLab approval events: %w", err)
	}
	var records []json.RawMessage
	if err := json.Unmarshal(res.Body, &records); err != nil {
		return fmt.Errorf("decode GitLab approval events: %w", err)
	}
	advance(state, len(records))
	if len(records) > 0 {
		state.Tasks = append([]task{{Kind: "process", Type: "approvals", Records: records}}, state.Tasks...)
	}
	return nil
}

type listed struct {
	ID        int64      `json:"id"`
	IID       int64      `json:"iid"`
	ProjectID int64      `json:"project_id"`
	Title     string     `json:"title"`
	State     string     `json:"state"`
	WebURL    string     `json:"web_url"`
	CreatedAt *time.Time `json:"created_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	MergedAt  *time.Time `json:"merged_at"`
	TargetIID int64      `json:"target_iid"`
	Target    string     `json:"target_title"`
}

// process verifies project visibility one project per request, then emits the
// events whose projects are public. Records from private or unreadable
// projects are dropped.
func (c *Connector) process(ctx context.Context, request event.PageRequest, state *scanState, report *event.FetchReport) ([]event.RawEvent, error) {
	t := state.Tasks[0]
	items := make([]listed, len(t.Records))
	for i, record := range t.Records {
		if err := json.Unmarshal(record, &items[i]); err != nil {
			return nil, fmt.Errorf("decode GitLab %s record: %w", t.Type, err)
		}
		if items[i].ProjectID <= 0 {
			continue
		}
		id := strconv.FormatInt(items[i].ProjectID, 10)
		if _, known := state.Projects[id]; !known {
			if err := c.verifyProject(ctx, state, id, report); err != nil {
				return nil, err
			}
			return nil, nil
		}
	}
	pop(state)
	forgeID := request.ForgeInstanceID
	var events []event.RawEvent
	for _, item := range items {
		project := state.Projects[strconv.FormatInt(item.ProjectID, 10)]
		if !project.Public {
			continue
		}
		raw, ok, err := buildEvent(forgeID, request.Source, state.AccountID, t.Type, item, project)
		if err != nil {
			return nil, err
		}
		if ok {
			events = append(events, raw)
			addKnown(report, project.Path)
		}
	}
	return events, nil
}

func addKnown(report *event.FetchReport, path string) {
	for _, known := range report.KnownRepositories {
		if known == path {
			return
		}
	}
	report.KnownRepositories = append(report.KnownRepositories, path)
}

func (c *Connector) verifyProject(ctx context.Context, state *scanState, idOrPath string, report *event.FetchReport) error {
	res, upstream, err := c.client.Get(ctx, "/projects/"+url.PathEscape(idOrPath), nil, quotaBucket)
	mergeReport(report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusNotFound || statusErr.StatusCode == http.StatusForbidden) {
		state.Projects[idOrPath] = projectInfo{}
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify GitLab project: %w", err)
	}
	var project struct {
		ID         int64  `json:"id"`
		Path       string `json:"path_with_namespace"`
		WebURL     string `json:"web_url"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal(res.Body, &project); err != nil {
		return fmt.Errorf("decode GitLab project: %w", err)
	}
	info := projectInfo{Path: project.Path, WebURL: project.WebURL, Public: project.Visibility == "public" && project.Path != ""}
	state.Projects[idOrPath] = info
	if project.ID > 0 {
		state.Projects[strconv.FormatInt(project.ID, 10)] = info
	}
	return nil
}

func buildEvent(forgeID uuid.UUID, source event.Source, accountID, recordType string, item listed, project projectInfo) (event.RawEvent, bool, error) {
	var (
		kind       event.Kind
		externalID string
		role       = "author"
		payload    forgenorm.Payload
		at         *time.Time
	)
	payload.Project = project.Path
	switch recordType {
	case "issues", "merge_requests":
		if item.IID <= 0 || item.CreatedAt == nil {
			return event.RawEvent{}, false, nil
		}
		kind, payload.Title, payload.URL, payload.State = forgenorm.KindIssue, item.Title, item.WebURL, item.State
		externalID = fmt.Sprintf("issue:%d:%d", item.ProjectID, item.IID)
		if recordType == "merge_requests" {
			kind = forgenorm.KindChangeRequest
			externalID = fmt.Sprintf("merge_request:%d:%d", item.ProjectID, item.IID)
		}
		payload.CreatedAt, payload.ClosedAt, payload.MergedAt = item.CreatedAt, item.ClosedAt, item.MergedAt
		at = item.CreatedAt
	case "approvals":
		if item.ID <= 0 || item.TargetIID <= 0 || item.CreatedAt == nil {
			return event.RawEvent{}, false, nil
		}
		kind, role, externalID = forgenorm.KindReview, "reviewer", fmt.Sprintf("approval:%d", item.ID)
		payload.Title, payload.State = item.Target, "approved"
		payload.URL = fmt.Sprintf("%s/-/merge_requests/%d", project.WebURL, item.TargetIID)
		payload.OccurredAt, at = item.CreatedAt, item.CreatedAt
	default:
		return event.RawEvent{}, false, fmt.Errorf("unknown GitLab record type %q", recordType)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return event.RawEvent{}, false, fmt.Errorf("encode GitLab payload: %w", err)
	}
	occurred := at.UTC()
	return event.RawEvent{
		ForgeInstanceID: forgeID, Source: source, ExternalID: externalID, Kind: kind,
		OccurredAt: &occurred, Payload: body, PayloadSchemaVersion: 1,
		Actors: []event.Actor{{Key: event.ActorKey{
			IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: accountID},
			ForgeInstanceID: &forgeID,
		}, Role: role}},
	}, true, nil
}

func (c *Connector) releases(ctx context.Context, request event.PageRequest, state *scanState, report *event.FetchReport) ([]event.RawEvent, error) {
	t := state.Tasks[0]
	if _, known := state.Projects[t.Project]; !known {
		if err := c.verifyProject(ctx, state, t.Project, report); err != nil {
			return nil, err
		}
		return nil, nil
	}
	project := state.Projects[t.Project]
	if !project.Public {
		pop(state)
		return nil, nil
	}
	query := url.Values{"per_page": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(t.Page)}}
	res, upstream, err := c.client.Get(ctx, "/projects/"+url.PathEscape(t.Project)+"/releases", query, quotaBucket)
	mergeReport(report, upstream)
	var statusErr *forgehttp.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		pop(state)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list GitLab releases: %w", err)
	}
	var releases []struct {
		TagName    string     `json:"tag_name"`
		Name       string     `json:"name"`
		ReleasedAt *time.Time `json:"released_at"`
		Author     struct {
			ID int64 `json:"id"`
		} `json:"author"`
	}
	if err := json.Unmarshal(res.Body, &releases); err != nil {
		return nil, fmt.Errorf("decode GitLab releases: %w", err)
	}
	advance(state, len(releases))
	forgeID := request.ForgeInstanceID
	var events []event.RawEvent
	for _, release := range releases {
		if strconv.FormatInt(release.Author.ID, 10) != state.AccountID || release.ReleasedAt == nil || release.TagName == "" {
			continue
		}
		title := release.Name
		if title == "" {
			title = release.TagName
		}
		occurred := release.ReleasedAt.UTC()
		body, err := json.Marshal(forgenorm.Payload{
			Title: title, Project: project.Path, OccurredAt: &occurred,
			URL: project.WebURL + "/-/releases/" + url.PathEscape(release.TagName),
		})
		if err != nil {
			return nil, fmt.Errorf("encode GitLab release payload: %w", err)
		}
		events = append(events, event.RawEvent{
			ForgeInstanceID: forgeID, Source: request.Source,
			ExternalID: "release:" + project.Path + ":" + release.TagName, Kind: forgenorm.KindRelease,
			OccurredAt: &occurred, Payload: body, PayloadSchemaVersion: 1,
			Actors: []event.Actor{{Key: event.ActorKey{
				IdentityKey:     identity.Key{Kind: identity.KindAccountID, Value: state.AccountID},
				ForgeInstanceID: &forgeID,
			}, Role: "author"}},
		})
	}
	return events, nil
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
