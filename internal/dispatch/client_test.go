package dispatch

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDispatch serves one handler per path and records the requests.
type fakeDispatch struct {
	t        *testing.T
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
	requests []*http.Request
}

func newFakeDispatch(t *testing.T) (*fakeDispatch, *Client) {
	t.Helper()
	fake := &fakeDispatch{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests = append(fake.requests, r)
		handler, ok := fake.routes[r.URL.EscapedPath()]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"NOT_FOUND","error":"not found"}`))
			return
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, "test-token", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return fake, client
}

func (f *fakeDispatch) answer(path, body string) {
	f.routes[path] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

func (f *fakeDispatch) last() *http.Request {
	f.t.Helper()
	if len(f.requests) == 0 {
		f.t.Fatal("no request reached the fake Dispatch")
	}
	return f.requests[len(f.requests)-1]
}

func TestNewClientRefusesNoURLOrNoToken(t *testing.T) {
	if _, err := NewClient("", "token", nil); err == nil {
		t.Error("a client was made with no URL")
	}
	if _, err := NewClient("https://dispatch.example", " ", nil); err == nil {
		t.Error("a client was made with no token")
	}
	if _, err := NewClient("dispatch.example", "token", nil); err == nil {
		t.Error("a client was made with a URL that names no scheme")
	}
}

func TestSearchSendsTheQueryAndTheBearerToken(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/search", `{"results":[
		{"kind":"issue","issue":{"key":"ABC-12","title":"Realtime voice","status":"backlog"},"snippet":"<mark>realtime</mark> voice","href":"/issues/ABC-12","rank":0.00655},
		{"kind":"document","issue":{"key":"ABC-12","title":"Realtime voice","status":"backlog"},"artifact":{"slug":"spec"},"snippet":"the <mark>spec</mark>"},
		{"kind":"comment","snippet":"no issue at all"}
	]}`)

	hits, err := client.Search(context.Background(), "realtime voice & co", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	request := fake.last()
	if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := request.URL.Query().Get("q"); got != "realtime voice & co" {
		t.Errorf("q = %q", got)
	}
	if got := request.URL.Query().Get("limit"); got != "5" {
		t.Errorf("limit = %q", got)
	}
	// A search is Dispatch's normal one, words and meaning: the unit sends
	// it a question's few keywords, never the sentence (searchTerms).
	if _, set := request.URL.Query()["keywords_only"]; set {
		t.Errorf("keywords_only = %q, want Dispatch's normal search", request.URL.Query().Get("keywords_only"))
	}
	if len(hits) != 3 {
		t.Fatalf("hits = %+v, want 3", hits)
	}
	if hits[0].Kind != "issue" || hits[0].IssueKey != "ABC-12" || hits[0].Title != "Realtime voice" || hits[0].Status != "backlog" || hits[0].Snippet != "realtime voice" || hits[0].Score != 0.00655 {
		t.Errorf("first hit = %+v", hits[0])
	}
	if hits[1].Document != "spec" || hits[1].Snippet != "the spec" {
		t.Errorf("document hit = %+v", hits[1])
	}
	if hits[2].IssueKey != "" {
		t.Errorf("a hit with no issue names one: %+v", hits[2])
	}
}

func TestProjectsListsTheKeys(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/projects", `[{"key":"ABC","name":"A project"},{"key":"INFRA","name":"Another"}]`)
	keys, err := client.Projects(context.Background())
	if err != nil {
		t.Fatalf("Projects: %v", err)
	}
	if len(keys) != 2 || keys[0] != "ABC" || keys[1] != "INFRA" {
		t.Fatalf("keys = %q", keys)
	}
}

func TestIssueReadsItsClaimAndHolder(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/issues/ABC-12", `{"key":"ABC-12","title":"Realtime voice","status":"in_progress","priority":2,
		"assignee":"person@example.com","updated_at":"2026-11-19T05:44:31.379816Z","parent":"ABC-1","open_asks":[{"id":"x"}],
		"primary_artifact_id":"doc-1","artifacts":[{"id":"doc-1","slug":"spec","kind":"doc"},{"id":"doc-2","slug":"notes","kind":"doc"}],
		"claim":{"actor":{"kind":"session","id":"01a1f178","origin":{"session_title":"Legion architect","machine":"box"}},"at":"2026-11-19T01:45:30.270611Z"},
		"holder_state":"live","last_seen":"2026-11-19T06:10:28.392Z"}`)

	issue, err := client.Issue(context.Background(), "abc-12")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issue.Key != "ABC-12" || issue.Status != "in_progress" || issue.Assignee != "person@example.com" || issue.OpenAsks != 1 || issue.Parent != "ABC-1" {
		t.Errorf("issue = %+v", issue)
	}
	if issue.Claim == nil || issue.Claim.Actor.Kind != "session" || issue.Claim.Actor.ID != "01a1f178" || issue.Claim.Actor.Title() != "Legion architect" {
		t.Errorf("claim = %+v", issue.Claim)
	}
	if issue.HolderState != "live" || issue.LastSeen == nil || !issue.LastSeen.Equal(time.Date(2026, 11, 19, 6, 10, 28, 392000000, time.UTC)) {
		t.Errorf("holder = %q, %v", issue.HolderState, issue.LastSeen)
	}
	if issue.PrimaryDocument() != "spec" {
		t.Errorf("primary document = %q, want spec", issue.PrimaryDocument())
	}
}

func TestIssueWithNoClaimHasNoHolder(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/issues/ABC-13", `{"key":"ABC-13","status":"backlog","claim":null,"holder_state":null,"last_seen":null}`)
	issue, err := client.Issue(context.Background(), "ABC-13")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Claim != nil || issue.HolderState != "" || issue.LastSeen != nil {
		t.Errorf("issue with no claim = %+v", issue)
	}
}

func TestAnIssueKeyMustLookLikeOne(t *testing.T) {
	fake, client := newFakeDispatch(t)
	for _, key := range []string{"", "ABC", "ABC-", "../admin", "ABC-1/events", "ABC-1?x=1", "abc 12"} {
		if _, err := client.Issue(context.Background(), key); !errors.Is(err, ErrBadIssueKey) {
			t.Errorf("Issue(%q) = %v, want ErrBadIssueKey", key, err)
		}
	}
	if len(fake.requests) != 0 {
		t.Errorf("a bad key reached Dispatch: %d requests", len(fake.requests))
	}
}

func TestEventsAsksForTheNewestFirst(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/issues/ABC-12/events", `[
		{"type":"comment.created","created_at":"2026-11-19T05:44:31Z","actor":{"kind":"session","id":"s-2","origin":{"session_title":"Worker"},"owner":"person@example.com"},"payload":{"body":"Merged PR 80."}},
		{"type":"issue.updated","created_at":"2026-11-19T05:40:00Z","actor":{"kind":"user","id":"person@example.com"},"payload":{}}
	]`)

	events, err := client.Events(context.Background(), "ABC-12", 20)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	query := fake.last().URL.Query()
	if query.Get("order") != "desc" || query.Get("limit") != "20" {
		t.Errorf("events query = %v", query)
	}
	if len(events) != 2 || events[0].Type != "comment.created" || events[0].Actor.ID != "s-2" || events[0].Actor.Kind != "session" || events[1].Actor.Kind != "user" {
		t.Fatalf("events = %+v", events)
	}
	if !strings.Contains(string(events[0].Payload), "Merged PR 80.") {
		t.Errorf("payload = %s", events[0].Payload)
	}
}

func TestDocumentTextReadsTheMarkdown(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.answer("/api/v1/issues/ABC-12/artifacts/spec/text", `{"markdown":"## Summary\n\nBuild it.","token":"t","version":3}`)
	text, err := client.DocumentText(context.Background(), "ABC-12", "spec")
	if err != nil {
		t.Fatalf("DocumentText: %v", err)
	}
	if text != "## Summary\n\nBuild it." {
		t.Errorf("text = %q", text)
	}
	if _, err := client.DocumentText(context.Background(), "ABC-12", "../../me"); err == nil {
		t.Error("a slug with a path in it was sent")
	}
}

func TestAnAnswerThatIsNoSuccessCarriesDispatchsCode(t *testing.T) {
	_, client := newFakeDispatch(t)
	_, err := client.Issue(context.Background(), "ABC-404")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || apiErr.Code != "NOT_FOUND" {
		t.Fatalf("err = %v, want a 404 APIError with code NOT_FOUND", err)
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound is false for a 404")
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Error("the error carries the token")
	}
}

// TestAFailedSearchCarriesNoQueryWords: a search that never reaches Dispatch
// fails with an error that does not hold the query, which comes from the room
// and would otherwise reach the log.
func TestAFailedSearchCarriesNoQueryWords(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1", "test-token", &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Search(context.Background(), "private room words", 5)
	if err == nil {
		t.Fatal("control: a search of a closed port did not fail")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("a search of a closed port = %v, want ErrUnreachable", err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("the error holds the query: %v", err)
	}
}

// TestARefusalCarriesNoQueryWords: Dispatch's refusal text may echo the
// query; the error keeps the status and code and drops the text.
func TestARefusalCarriesNoQueryWords(t *testing.T) {
	fake, client := newFakeDispatch(t)
	fake.routes["/api/v1/search"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"BAD_QUERY","error":"cannot search for private room words"}`))
	}
	_, err := client.Search(context.Background(), "private room words", 5)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusBadRequest || apiErr.Code != "BAD_QUERY" {
		t.Fatalf("err = %v, want the 400 with its code", err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("the error holds the query: %v", err)
	}
}

// TestAnErrorNamesTheRouteNotTheKey: a key comes from words said in the
// room ("the last 24 hours" reads as LAST-24), so an error that reaches the
// log names the route it failed on, not the issue's path.
func TestAnErrorNamesTheRouteNotTheKey(t *testing.T) {
	_, client := newFakeDispatch(t)
	_, err := client.Issue(context.Background(), "LAST-24")
	if !IsNotFound(err) {
		t.Fatalf("control: Issue of an unknown key = %v, want a 404", err)
	}
	if strings.Contains(err.Error(), "LAST") || strings.Contains(err.Error(), "24") {
		t.Fatalf("the error names the key: %v", err)
	}
	unreachable, err := NewClient("http://127.0.0.1:1", "test-token", &http.Client{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = unreachable.Events(context.Background(), "LAST-24", 5)
	if !errors.Is(err, ErrUnreachable) || strings.Contains(err.Error(), "LAST") {
		t.Fatalf("an unreachable read of LAST-24 = %v", err)
	}
}
