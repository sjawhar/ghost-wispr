// Package dispatch reads a Dispatch server's issue record: full-text search,
// one issue with its claim, an issue's events newest first, and an issue's
// documents. It writes nothing. Every call carries the unit's bearer token
// (GHOST_WISPR_DISPATCH_TOKEN), a token with no scope, which these routes
// accept.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrBadIssueKey is a key that is not PROJECT-NUMBER. A key comes from a
// model's tool call, so it is checked before it goes into a path.
var ErrBadIssueKey = errors.New("dispatch: an issue key is a project key, a dash and a number, such as ABC-12")

// ErrUnreachable is a call that got no answer from Dispatch: no route, no
// name, a refused connection or a timeout.
var ErrUnreachable = errors.New("dispatch: no answer")

var (
	issueKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*-[0-9]+$`)
	slugPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	markTags        = strings.NewReplacer("<mark>", "", "</mark>", "")
)

// maxAnswerBytes bounds an answer the client reads. An issue with many
// documents answers some tens of kilobytes.
const maxAnswerBytes = 8 << 20

// Client calls one Dispatch server.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// NewClient returns a client for the server at baseURL (scheme and host, such
// as https://dispatch.example) with the bearer token. httpClient nil uses a
// client with a 15 s timeout.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("dispatch: the server URL %q names no scheme and host", baseURL)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("dispatch: no token")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: base, token: token, http: httpClient}, nil
}

// APIError is an answer that is no success. Code is Dispatch's error code
// (NOT_FOUND, NO_IDENTITY, ...), which callers branch on. Route names the
// call (search, issue, events, document). An error reaches the log, and both
// Dispatch's own text and the request's path can hold words said in the room
// (a search's words; a key such as LAST-24 heard in "the last 24 hours"), so
// Error gives neither.
type APIError struct {
	Status  int
	Code    string
	Message string
	Route   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("dispatch: %s answered %d %s", e.Route, e.Status, e.Code)
}

// IsNotFound reports whether err is Dispatch's 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound
}

// SearchHit is one result of a search: the issue it belongs to (empty for a
// result that belongs to none), the document slug when the hit is in a
// document, and the matched text without Dispatch's <mark> tags.
type SearchHit struct {
	Kind     string `json:"kind"`
	IssueKey string `json:"issue_key,omitempty"`
	Title    string `json:"title,omitempty"`
	Status   string `json:"status,omitempty"`
	Document string `json:"document,omitempty"`
	Snippet  string `json:"snippet,omitempty"`
	// Score is Dispatch's rank of the result, higher for a better match.
	Score float64 `json:"score,omitempty"`
}

// Projects lists Dispatch's project keys.
func (c *Client) Projects(ctx context.Context) ([]string, error) {
	var projects []struct {
		Key string `json:"key"`
	}
	if err := c.get(ctx, "projects", "/api/v1/projects", nil, &projects); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(projects))
	for _, project := range projects {
		keys = append(keys, project.Key)
	}
	return keys, nil
}

// Search runs Dispatch's normal search, words and meaning, and returns up to
// limit hits, best first. Its meaning leg sends the query to Dispatch's
// embedding model, outside Dispatch.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return c.search(ctx, query, limit, false)
}

// SearchKeywords runs Dispatch's words leg alone, which reaches no embedding
// model and answers in about 0.15 s, against 1 to 6 s for Search.
func (c *Client) SearchKeywords(ctx context.Context, query string, limit int) ([]SearchHit, error) {
	return c.search(ctx, query, limit, true)
}

func (c *Client) search(ctx context.Context, query string, limit int, keywordsOnly bool) ([]SearchHit, error) {
	params := url.Values{"q": {query}}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	route, path := "search", "/api/v1/search"
	if keywordsOnly {
		// The route that declares keywords_only: an older Dispatch answers
		// the plain route 200 with the member dropped, a full search.
		params.Set("keywords_only", "true")
		route, path = "search_keywords", "/api/v1/search/results"
	}
	var answer struct {
		Results []struct {
			Kind  string `json:"kind"`
			Issue *struct {
				Key    string `json:"key"`
				Title  string `json:"title"`
				Status string `json:"status"`
			} `json:"issue"`
			Artifact *struct {
				Slug string `json:"slug"`
			} `json:"artifact"`
			Snippet string  `json:"snippet"`
			Rank    float64 `json:"rank"`
		} `json:"results"`
	}
	if err := c.get(ctx, route, path, params, &answer); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(answer.Results))
	for _, result := range answer.Results {
		hit := SearchHit{Kind: result.Kind, Snippet: markTags.Replace(result.Snippet), Score: result.Rank}
		if result.Issue != nil {
			hit.IssueKey, hit.Title, hit.Status = result.Issue.Key, result.Issue.Title, result.Issue.Status
		}
		if result.Artifact != nil {
			hit.Document = result.Artifact.Slug
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// Actor is who made an event or holds a claim: a session (ID is its session
// id), a person (ID is their email), or another kind Dispatch names.
type Actor struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Owner  string `json:"owner,omitempty"`
	Origin struct {
		SessionTitle string `json:"session_title,omitempty"`
		Machine      string `json:"machine,omitempty"`
	} `json:"origin"`
}

// Title is a session actor's title, else its id.
func (a *Actor) Title() string {
	if a.Origin.SessionTitle != "" {
		return a.Origin.SessionTitle
	}
	return a.ID
}

// Claim is an issue's claim: who took it and when.
type Claim struct {
	Actor Actor     `json:"actor"`
	At    time.Time `json:"at"`
}

// Issue is what the Concierge reads of an issue.
type Issue struct {
	Key         string     `json:"key"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	Priority    int        `json:"priority"`
	Assignee    string     `json:"assignee"`
	Parent      string     `json:"parent"`
	UpdatedAt   time.Time  `json:"updated_at"`
	Claim       *Claim     `json:"claim"`
	HolderState string     `json:"holder_state"`
	LastSeen    *time.Time `json:"last_seen"`
	// OpenAsks counts the asks still open on the issue.
	OpenAsks          int
	PrimaryArtifactID string `json:"primary_artifact_id"`
	Artifacts         []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	} `json:"artifacts"`
}

// PrimaryDocument is the slug of the issue's primary document (its spec),
// empty when it has none.
func (i *Issue) PrimaryDocument() string {
	for _, artifact := range i.Artifacts {
		if artifact.ID == i.PrimaryArtifactID && i.PrimaryArtifactID != "" {
			return artifact.Slug
		}
	}
	return ""
}

// Issue reads one issue. The key is checked (ErrBadIssueKey) and upper-cased.
func (c *Client) Issue(ctx context.Context, key string) (*Issue, error) {
	key, err := NormalizeIssueKey(key)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Issue
		OpenAsks []json.RawMessage `json:"open_asks"`
	}
	if err := c.get(ctx, "issue", "/api/v1/issues/"+key, nil, &answer); err != nil {
		return nil, err
	}
	issue := answer.Issue
	issue.OpenAsks = len(answer.OpenAsks)
	return &issue, nil
}

// Event is one event of an issue's log.
type Event struct {
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Actor     Actor           `json:"actor"`
	Payload   json.RawMessage `json:"payload"`
}

// Events reads an issue's newest limit events, newest first.
func (c *Client) Events(ctx context.Context, key string, limit int) ([]Event, error) {
	key, err := NormalizeIssueKey(key)
	if err != nil {
		return nil, err
	}
	params := url.Values{"order": {"desc"}}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	var events []Event
	if err := c.get(ctx, "events", "/api/v1/issues/"+key+"/events", params, &events); err != nil {
		return nil, err
	}
	return events, nil
}

// DocumentText reads an issue document's markdown by its slug.
func (c *Client) DocumentText(ctx context.Context, key, slug string) (string, error) {
	key, err := NormalizeIssueKey(key)
	if err != nil {
		return "", err
	}
	if !slugPattern.MatchString(slug) || strings.Contains(slug, "..") {
		return "", fmt.Errorf("dispatch: %q is no document slug", slug)
	}
	var answer struct {
		Markdown string `json:"markdown"`
	}
	if err := c.get(ctx, "document", "/api/v1/issues/"+key+"/artifacts/"+slug+"/text", nil, &answer); err != nil {
		return "", err
	}
	return answer.Markdown, nil
}

// NormalizeIssueKey checks that key is PROJECT-NUMBER (ErrBadIssueKey) and
// upper-cases it.
func NormalizeIssueKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if !issueKeyPattern.MatchString(key) {
		return "", ErrBadIssueKey
	}
	return strings.ToUpper(key), nil
}

// get reads path into into. Its errors name route, never path.
func (c *Client) get(ctx context.Context, route, path string, params url.Values, into any) error {
	target := c.base.JoinPath(path)
	target.RawQuery = params.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return fmt.Errorf("dispatch: build the %s call: %w", route, withoutURL(err))
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnreachable, route, withoutURL(err))
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes))
	if err != nil {
		return fmt.Errorf("%w: read the %s answer: %w", ErrUnreachable, route, withoutURL(err))
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var refusal struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &refusal)
		return &APIError{Status: response.StatusCode, Code: refusal.Code, Message: refusal.Error, Route: route}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("dispatch: decode the %s answer: %w", route, err)
	}
	return nil
}

// withoutURL is err without the URL a *url.Error names: a search's URL holds
// its words, which come from the room, and an error reaches the log.
func withoutURL(err error) error {
	var withURL *url.Error
	if errors.As(err, &withURL) {
		return withURL.Err
	}
	return err
}
