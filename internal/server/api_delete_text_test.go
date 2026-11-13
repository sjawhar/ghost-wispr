package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deleteModeHandler serves the API with session s1 ended (when withSession)
// and both deletion hooks recording which ran.
func deleteModeHandler(t *testing.T, withSession bool, ran *[]string) http.Handler {
	t.Helper()
	sessions := map[string]storage.Session{}
	if withSession {
		sessions["s1"] = storage.Session{ID: "s1", Status: storage.SessionEnded}
	}
	store := &apiStoreStub{sessionsByDate: map[string][]storage.Session{}, sessions: sessions, segments: map[string][]transcribe.Segment{}}
	hook := func(name string) func(context.Context, string, int64) error {
		return func(_ context.Context, sessionID string, segmentID int64) error {
			if sessionID != "s1" || segmentID != 42 {
				t.Errorf("%s hook got session %q segment %d", name, sessionID, segmentID)
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
		deleteModeHandler(t, true, &ran).ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/s1/segments/42?mode="+mode, nil))
		if response.Code != http.StatusNoContent || len(ran) != 1 || ran[0] != mode {
			t.Errorf("mode %s: status %d body %s, ran %v; want 204 and only %s", mode, response.Code, response.Body.String(), ran, mode)
		}
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
