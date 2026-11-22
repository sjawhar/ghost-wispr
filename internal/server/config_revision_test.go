package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
)

// revisionOf decodes the revision of a config answer.
func revisionOf(t *testing.T, body []byte) config.Revision {
	t.Helper()
	var answer struct {
		Revision *config.Revision `json:"revision"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Revision == nil {
		t.Fatalf("config answer with no revision (%v): %s", err, body)
	}
	return *answer.Revision
}

// TestConfigAnswersCarryTheRevisionTheyHold fails if GET or PATCH
// /api/config answers without a revision, if a change does not raise its
// count by one, if a refused patch or a read raises it, or if the run changes
// within one store. A page with two saves on their way keeps the answer with
// the higher count: before, it kept the answer to the save it sent last, and
// an earlier save that reached the unit later held the newer state.
func TestConfigAnswersCarryTheRevisionTheyHold(t *testing.T) {
	cfgStore, _ := newProvidersConfigStore(t)
	h := newConfigHandler(t, cfgStore, &ControlHooks{})

	var ignored map[string]any
	start := revisionOf(t, getJSON(t, h, "/api/config", &ignored).Body.Bytes())
	if start.Run == "" {
		t.Fatalf("revision %+v names no run", start)
	}
	if again := revisionOf(t, getJSON(t, h, "/api/config", &ignored).Body.Bytes()); again != start {
		t.Errorf("a second read has revision %+v, want %+v", again, start)
	}

	rr := patchConfig(h, `{"silence_timeout":"45s"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rr.Code, rr.Body.String())
	}
	first := revisionOf(t, rr.Body.Bytes())
	if first.Run != start.Run || first.Count != start.Count+1 {
		t.Errorf("after a change the revision is %+v, want run %s count %d", first, start.Run, start.Count+1)
	}

	if rr := patchConfig(h, `{"silence_timeout":"not a duration"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("a bad silence_timeout = %d %s, want 400", rr.Code, rr.Body.String())
	}
	if after := revisionOf(t, getJSON(t, h, "/api/config", &ignored).Body.Bytes()); after != first {
		t.Errorf("after a refused patch the revision is %+v, want %+v", after, first)
	}

	rr = patchConfig(h, `{"tts":{"voice":"Puck"}}`)
	if second := revisionOf(t, rr.Body.Bytes()); second.Run != start.Run || second.Count != first.Count+1 {
		t.Errorf("after a second change the revision is %+v, want count %d", second, first.Count+1)
	}

	// A new run of the unit names another run.
	other, _ := newProvidersConfigStore(t)
	if _, revision := other.GetRevision(); revision.Run == start.Run {
		t.Errorf("two stores share the run %s", revision.Run)
	}
}
