package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

var voiceTaskAsked = time.Date(2026, 11, 19, 9, 30, 0, 0, time.UTC)

// voiceTasksStore holds ended sessions s1 and quiet, and merged session m
// with sources a and b; tasks are the questions each session's voice sent.
func voiceTasksStore(tasks map[string][]storage.VoiceTask) *apiStoreStub {
	sessions := map[string]storage.Session{}
	for _, id := range []string{"s1", "quiet", "m"} {
		sessions[id] = storage.Session{ID: id, Status: storage.SessionEnded}
	}
	for _, id := range []string{"a", "b"} {
		sessions[id] = storage.Session{ID: id, Status: storage.SessionMerged, MergedInto: "m"}
	}
	return &apiStoreStub{
		sessions:         sessions,
		sourceSessionIDs: map[string][]string{"m": {"a", "b"}},
		voiceTasks:       tasks,
	}
}

// getVoiceTasks serves GET /api/sessions/{sessionID}/voice-tasks with the
// handler the unit runs, guarded by token, and sends password as the basic
// auth password when it is not empty.
func getVoiceTasks(t *testing.T, store SessionStore, token, password, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, token, nil)
	if err != nil {
		t.Fatalf("Handler failed: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/"+sessionID+"/voice-tasks", nil)
	if password != "" {
		req.SetBasicAuth("ghost-wispr", password)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// decodeVoiceTasks decodes a 200 answer as JSON values, so a field that is
// missing or misnamed shows.
func decodeVoiceTasks(t *testing.T, rr *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("content type = %q, want application/json", got)
	}
	var tasks []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	return tasks
}

// TestVoiceTasksShowEachQuestionAndTheWrittenAnswer fails if GET
// /api/sessions/{id}/voice-tasks leaves out a field the session's page
// shows, names one differently, gives a spoken time to an answer that was
// not spoken, or changes the agent's written answer.
func TestVoiceTasksShowEachQuestionAndTheWrittenAnswer(t *testing.T) {
	spoken := voiceTaskAsked.Add(2 * time.Minute)
	store := voiceTasksStore(map[string][]storage.VoiceTask{"s1": {
		{ID: "evt-1", SessionID: "s1", AskedAt: voiceTaskAsked, IssueKey: "ABC-12", Holder: "session:agent-7", State: storage.VoiceTaskDone, AnswerWritten: "It merged.\n<b>x</b> & more", SpokenAt: &spoken},
		{ID: "evt-2", SessionID: "s1", AskedAt: voiceTaskAsked.Add(time.Minute), IssueKey: "ABC-13", Holder: "session:agent-8", State: storage.VoiceTaskWorking},
	}})

	got := decodeVoiceTasks(t, getVoiceTasks(t, store, "", "", "s1"))

	want := []map[string]any{
		{"id": "evt-1", "issue_key": "ABC-12", "holder": "session:agent-7", "state": "done", "asked_at": "2026-11-19T09:30:00Z", "spoken_at": "2026-11-19T09:32:00Z", "answer_written": "It merged.\n<b>x</b> & more"},
		{"id": "evt-2", "issue_key": "ABC-13", "holder": "session:agent-8", "state": "working", "asked_at": "2026-11-19T09:31:00Z", "answer_written": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("voice tasks =\n%v\nwant\n%v", got, want)
	}
}

// TestVoiceTasksOfAMergedSessionIncludeItsSources fails if a merged
// session's page leaves out the questions asked in the sessions merged into
// it, whose page it now is, or lists them out of the order they were asked.
func TestVoiceTasksOfAMergedSessionIncludeItsSources(t *testing.T) {
	task := func(id, sessionID string, minutes int) storage.VoiceTask {
		return storage.VoiceTask{ID: id, SessionID: sessionID, AskedAt: voiceTaskAsked.Add(time.Duration(minutes) * time.Minute), IssueKey: "ABC-1", Holder: "session:agent-1", State: storage.VoiceTaskCancelled}
	}
	store := voiceTasksStore(map[string][]storage.VoiceTask{
		"m": {task("m-1", "m", 3)},
		"a": {task("a-1", "a", 0), task("a-2", "a", 4)},
		"b": {task("b-1", "b", 1)},
	})

	var ids []any
	for _, got := range decodeVoiceTasks(t, getVoiceTasks(t, store, "", "", "m")) {
		ids = append(ids, got["id"])
	}

	if want := []any{"a-1", "b-1", "m-1", "a-2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("voice task ids = %v, want %v, oldest first", ids, want)
	}
}

// TestVoiceTasksOfASessionWithNoneIsAnEmptyList fails if a session the
// voice asked nothing in answers anything but an empty JSON list.
func TestVoiceTasksOfASessionWithNoneIsAnEmptyList(t *testing.T) {
	rr := getVoiceTasks(t, voiceTasksStore(nil), "", "", "quiet")

	if got := decodeVoiceTasks(t, rr); got == nil || len(got) != 0 {
		t.Fatalf("voice tasks = %v, want []", got)
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "[]" {
		t.Fatalf("body = %s, want []", body)
	}
}

// TestVoiceTasksOfAnUnknownSessionAre404 fails if a session that is not
// there, or was deleted, answers anything but the JSON 404 the other session
// routes give.
func TestVoiceTasksOfAnUnknownSessionAre404(t *testing.T) {
	rr := getVoiceTasks(t, voiceTasksStore(nil), "", "", "gone")

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || !strings.HasPrefix(body["error"], "get session: ") {
		t.Fatalf("body = %s (%v), want the JSON error of a missing session", rr.Body.String(), err)
	}
}

// TestVoiceTasksRefuseAnInvalidSessionID fails if an id that is not of the
// form of a session id is looked up, where the other session routes refuse
// it with 403.
func TestVoiceTasksRefuseAnInvalidSessionID(t *testing.T) {
	rr := getVoiceTasks(t, voiceTasksStore(nil), "", "", "bad.id")

	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "invalid session id") {
		t.Fatalf("status = %d body %s, want 403 invalid session id", rr.Code, rr.Body.String())
	}
}

// TestVoiceTasksNeedTheAuthToken fails if the written answers are served
// without the unit's auth token, or not served with it.
func TestVoiceTasksNeedTheAuthToken(t *testing.T) {
	store := voiceTasksStore(map[string][]storage.VoiceTask{"s1": {
		{ID: "evt-1", SessionID: "s1", AskedAt: voiceTaskAsked, IssueKey: "ABC-12", Holder: "session:agent-7", State: storage.VoiceTaskDone, AnswerWritten: "It merged."},
	}})

	for _, password := range []string{"", "wrong"} {
		rr := getVoiceTasks(t, store, "secret123", password, "s1")
		if rr.Code != http.StatusUnauthorized || strings.Contains(rr.Body.String(), "It merged.") {
			t.Fatalf("password %q: status = %d body %s, want 401 and no answer", password, rr.Code, rr.Body.String())
		}
	}
	got := decodeVoiceTasks(t, getVoiceTasks(t, store, "secret123", "secret123", "s1"))
	if len(got) != 1 || got[0]["answer_written"] != "It merged." {
		t.Fatalf("voice tasks with the token = %v, want the one answer", got)
	}
}

// TestOpenAPISpecDocumentsVoiceTasks fails if the route is not in the API's
// description, or the description does not say that the written answer may
// quote the asker and that only a session delete erases it.
func TestOpenAPISpecDocumentsVoiceTasks(t *testing.T) {
	get, _ := dig(t, OpenAPISpec(), "paths", "/api/sessions/{id}/voice-tasks", "get").(map[string]any)
	if get == nil {
		t.Fatal("GET /api/sessions/{id}/voice-tasks is not in the OpenAPI spec")
	}
	description, _ := get["description"].(string)
	for _, says := range []string{"quote the asker", "only deleting the session erases"} {
		if !strings.Contains(description, says) {
			t.Errorf("description %q does not say %q", description, says)
		}
	}
	responses, _ := get["responses"].(map[string]any)
	for _, code := range []string{"200", "403", "404"} {
		if _, found := responses[code]; !found {
			t.Errorf("responses = %v, want %s documented", responses, code)
		}
	}
	item, _ := dig(t, responses, "200", "content", "application/json", "schema", "items", "properties").(map[string]any)
	for _, field := range []string{"id", "issue_key", "holder", "state", "asked_at", "spoken_at", "answer_written"} {
		if _, found := item[field]; !found {
			t.Errorf("200 schema properties = %v, want %s", item, field)
		}
	}
}

// TestVoiceTasksOnTheUnitsStoreGoWithTheSessionDelete fails if, on the
// unit's SQLite store, a merged session's page does not list the questions
// asked in its sources with their written answers, or if any of them is
// still served or kept once the session is deleted.
func TestVoiceTasksOnTheUnitsStoreGoWithTheSessionDelete(t *testing.T) {
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "ghost-wispr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sources := []string{"20261119090000", "20261119093000"}
	for i, id := range sources {
		at := voiceTaskAsked.Add(time.Duration(i) * 30 * time.Minute)
		if err := store.CreateSession(id, at); err != nil {
			t.Fatal(err)
		}
		if err := store.CreateVoiceTask(&storage.VoiceTask{ID: "evt-" + id, SessionID: id, AskedAt: at.Add(time.Minute), IssueKey: "ABC-12", Holder: "session:agent-7"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AnswerVoiceTask("evt-"+id, "Answer in "+id+": <b>x</b>"); err != nil {
			t.Fatal(err)
		}
		if err := store.EndSession(id, at.Add(20*time.Minute), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkVoiceTaskSpoken("evt-"+sources[0], voiceTaskAsked.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	const merged = "20261119090000-merged"
	if err := store.MergeSessions(merged, sources, voiceTaskAsked, voiceTaskAsked.Add(50*time.Minute)); err != nil {
		t.Fatal(err)
	}
	cfgStore := newTestConfigStore(t)
	if err := cfgStore.Update(func(cfg *config.Config) { cfg.AudioDir = t.TempDir() }); err != nil {
		t.Fatal(err)
	}
	h, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{}, "", nil, cfgStore)
	if err != nil {
		t.Fatal(err)
	}
	serve := func(method string) *httptest.ResponseRecorder {
		path := "/api/sessions/" + merged
		if method == http.MethodGet {
			path += "/voice-tasks"
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		return rr
	}

	got := decodeVoiceTasks(t, serve(http.MethodGet))
	if len(got) != 2 || got[0]["id"] != "evt-"+sources[0] || got[1]["id"] != "evt-"+sources[1] {
		t.Fatalf("voice tasks of the merged session = %v, want its two sources' questions, oldest first", got)
	}
	if got[0]["answer_written"] != "Answer in "+sources[0]+": <b>x</b>" || got[0]["state"] != "done" || got[0]["spoken_at"] != "2026-11-19T09:32:00Z" {
		t.Errorf("first question = %v, want its answer as written, done, spoken at 09:32", got[0])
	}
	if _, spoken := got[1]["spoken_at"]; spoken {
		t.Errorf("second question = %v, want no spoken time", got[1])
	}

	if rr := serve(http.MethodDelete); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s", rr.Code, rr.Body.String())
	}
	if rr := serve(http.MethodGet); rr.Code != http.StatusNotFound {
		t.Fatalf("voice tasks of the deleted session: status = %d, want 404; body %s", rr.Code, rr.Body.String())
	}
	for _, id := range append([]string{merged}, sources...) {
		if left, err := store.VoiceTasks(id); err != nil || len(left) != 0 {
			t.Errorf("voice tasks of %s after the delete = %v (%v), want none", id, left, err)
		}
	}
}
