package transcribe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUntimedSegmentJSONCarriesZeroRevision(t *testing.T) {
	timed, err := json.Marshal(Segment{ID: 1, Text: "timed", TranscriptRevision: 2})
	if err != nil {
		t.Fatal(err)
	}
	untimed, err := json.Marshal(Segment{ID: 2, Text: "untimed", Untimed: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(timed), `"transcript_revision":2`) {
		t.Fatalf("timed segment JSON = %s", timed)
	}
	if !strings.Contains(string(untimed), `"transcript_revision":0`) {
		t.Fatalf("untimed segment JSON = %s, want revision 0", untimed)
	}
}
