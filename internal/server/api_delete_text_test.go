package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deleteModeHandler serves the API with session s1 ended (when withSession)
// and both deletion hooks recording which ran.
func deleteModeHandler(t *testing.T, withSession bool, ran *[]string) http.Handler {
	t.Helper()
	sessions := map[string]storage.Session{}
	if withSession {
		sessions["s1"] = storage.Session{ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted, RefinedRevision: 1}
	}
	store := &apiStoreStub{sessionsByDate: map[string][]storage.Session{}, sessions: sessions, segments: map[string][]transcribe.Segment{}}
	hook := func(name string) func(context.Context, string, int64, int) error {
		return func(_ context.Context, sessionID string, segmentID int64, revision int) error {
			if sessionID != "s1" || segmentID != 42 || revision != 1 {
				t.Errorf("%s hook got session %q segment %d revision %d", name, sessionID, segmentID, revision)
			}
			*ran = append(*ran, name)
			return nil
		}
	}
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		DeleteSegment:     hook("text_and_audio"),
		DeleteSegmentText: hook("text"),
	}, "", nil)
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	return handler
}

// TestDeleteSegmentAPIRequiresAMode fails if a segment deletion without a
// mode, or with one that is neither "text" nor "text_and_audio", deletes
// anything or answers other than 400 naming both modes: no client gets a
// mode by default. The mode is checked before the session is read, so the
// refusal does not depend on the session or segment existing.
func TestDeleteSegmentAPIRequiresAMode(t *testing.T) {
	for _, withSession := range []bool{true, false} {
		for _, query := range []string{"", "?mode=", "?mode=audio", "?mode=TEXT", "?mode=text_and_audio&mode=text"} {
			var ran []string
			response := httptest.NewRecorder()
			deleteModeHandler(t, withSession, &ran).ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42"+query, nil))
			var body struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			if response.Code != http.StatusBadRequest || !strings.Contains(body.Error, `"text"`) || !strings.Contains(body.Error, `"text_and_audio"`) {
				t.Errorf("session %v, query %q: status %d, error %q; want 400 naming \"text\" and \"text_and_audio\"", withSession, query, response.Code, body.Error)
			}
			if len(ran) != 0 {
				t.Errorf("session %v, query %q: a deletion ran: %v", withSession, query, ran)
			}
		}
	}
}

// TestDeleteSegmentAPIRunsTheChosenMode fails if mode=text does not run
// "Delete text", or mode=text_and_audio does not run "Delete text and
// audio", or either runs the other.
func TestDeleteSegmentAPIRunsTheChosenMode(t *testing.T) {
	for _, mode := range []string{"text", "text_and_audio"} {
		var ran []string
		response := httptest.NewRecorder()
		deleteModeHandler(t, true, &ran).ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode="+mode+"&revision=1", nil))
		if response.Code != http.StatusNoContent || len(ran) != 1 || ran[0] != mode {
			t.Errorf("mode %s: status %d body %s, ran %v; want 204 and only %s", mode, response.Code, response.Body.String(), ran, mode)
		}
	}
}

func TestDeleteSegmentAPIRefusesUntilRefinementCompletes(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementPending},
		},
		segments: map[string][]transcribe.Segment{},
	}
	called := false
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		DeleteSegment: func(context.Context, string, int64, int) error {
			called = true
			return nil
		},
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			called = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text&revision=1", nil))
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusConflict || body.Error != "this conversation is still being refined; delete once it finishes" || called {
		t.Fatalf("delete before refinement = %d %q called=%v, want 409, the refinement message, and no deletion", response.Code, body.Error, called)
	}
}

func TestDeleteSegmentAPIRequiresTranscriptRevision(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted},
		},
		segments: map[string][]transcribe.Segment{},
	}
	called := false
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			called = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text", nil))
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusBadRequest || !strings.Contains(body.Error, "revision") || called {
		t.Fatalf("delete without revision = %d %q called=%v, want 400 naming revision and no deletion", response.Code, body.Error, called)
	}
}

// TestOpenAPISpecDocumentsTheDeletionModes fails if the OpenAPI document
// does not give the segment deletion its required mode, with exactly the two
// modes, and its 400 refusal.
func TestOpenAPISpecDocumentsTheDeletionModes(t *testing.T) {
	raw, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	operation := []string{"paths", "/api/sessions/{id}/segments/{segmentID}", "delete"}
	parameters, _ := dig(t, spec, append(operation, "parameters")...).([]any)
	var mode map[string]any
	for _, parameter := range parameters {
		if p, _ := parameter.(map[string]any); p["name"] == "mode" {
			mode = p
		}
	}
	if mode == nil {
		t.Fatalf("the segment deletion documents no mode parameter: %v", parameters)
	}
	if mode["in"] != "query" || mode["required"] != true {
		t.Errorf("mode is in %v, required %v; want a required query parameter", mode["in"], mode["required"])
	}
	enum, _ := dig(t, mode, "schema", "enum").([]any)
	if len(enum) != 2 || enum[0] != "text" || enum[1] != "text_and_audio" {
		t.Errorf("mode enum = %v, want [text text_and_audio]", enum)
	}
	if description, _ := dig(t, spec, append(operation, "responses", "400", "description")...).(string); !strings.Contains(description, "mode") {
		t.Errorf("400 description %q does not name the mode", description)
	}
}

func TestOpenAPISpecDocumentsTranscriptRevisionAndStaleConflict(t *testing.T) {
	raw, err := OpenAPISpecJSON()
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	operation := []string{"paths", "/api/sessions/{id}/segments/{segmentID}", "delete"}
	parameters, _ := dig(t, spec, append(operation, "parameters")...).([]any)
	var revision map[string]any
	for _, parameter := range parameters {
		if p, _ := parameter.(map[string]any); p["name"] == "revision" {
			revision = p
		}
	}
	if revision == nil || revision["in"] != "query" || revision["required"] != true {
		t.Fatalf("revision parameter = %#v, want a required query parameter", revision)
	}
	if schema, _ := revision["schema"].(map[string]any); schema["type"] != "integer" || schema["minimum"] != float64(1) {
		t.Errorf("revision schema = %#v, want positive integer", schema)
	}
	if description, _ := dig(t, spec, append(operation, "responses", "409", "description")...).(string); !strings.Contains(description, "refresh") {
		t.Errorf("409 description %q does not say to refresh a stale transcript", description)
	}
}

func TestDeleteSegmentAPIStartsRefinementForOldSegments(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted},
		},
		segments: map[string][]transcribe.Segment{},
	}
	retried := make(chan string, 1)
	deleted := false
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		RetryRefinement: func(_ context.Context, sessionID string) error {
			retried <- sessionID
			return nil
		},
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			deleted = true
			return nil
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text&revision=1", nil))
	var body struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusConflict || body.Error != "this conversation is still being refined; delete once it finishes" || deleted {
		t.Fatalf("old-session delete = %d %q deleted=%v, want 409 refinement message without deletion", response.Code, body.Error, deleted)
	}
	select {
	case sessionID := <-retried:
		if sessionID != "s1" {
			t.Fatalf("retry session = %q, want s1", sessionID)
		}
	case <-time.After(time.Second):
		t.Fatal("old-session deletion did not start refinement")
	}
}

func TestDeleteSegmentAPIRefusesStaleRevision(t *testing.T) {
	store := &apiStoreStub{
		sessionsByDate: map[string][]storage.Session{},
		sessions: map[string]storage.Session{
			"s1": {ID: "s1", Status: storage.SessionEnded, RefinementStatus: storage.RefinementCompleted, RefinedRevision: 2},
		},
		segments: map[string][]transcribe.Segment{},
	}
	handler, err := Handler(testStaticFS(t), NewHub(), store, &ControlHooks{
		DeleteSegmentText: func(context.Context, string, int64, int) error {
			return storage.ErrStaleTranscriptRevision
		},
	}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode=text&revision=1", nil))
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusConflict || body.Error != "the transcript changed; refresh and try again" {
		t.Fatalf("stale revision response = %d %q, want 409 refresh message", response.Code, body.Error)
	}
}
