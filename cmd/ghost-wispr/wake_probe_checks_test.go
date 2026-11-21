package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	"github.com/dvonthenen/websocket"
)

// probeEndpoint is a Deepgram live endpoint for the probes' own checks: it
// takes every frame and, on Finalize, sends results, as many as the test
// gives, interim ones first.
func probeEndpoint(t *testing.T, results ...string) string {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			kind, message, err := c.ReadMessage()
			if err != nil {
				return
			}
			if kind != websocket.TextMessage || !strings.Contains(string(message), "Finalize") {
				continue
			}
			for _, res := range results {
				if err := c.WriteMessage(websocket.TextMessage, []byte(res)); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws://" + srv.Listener.Addr().String()
}

func probeResultFrame(text string, final bool) string {
	return fmt.Sprintf(`{"type":"Results","channel_index":[0,1],"duration":1,"start":0,"is_final":%t,"speech_final":%t,"channel":{"alternatives":[{"transcript":%q,"confidence":0.9}]}}`, final, final, text)
}

// TestProbeTranscribeNeedsAFinal fails if a probe's socket that sends no
// final result counts as one that heard nothing: the false-wake probe then
// scored a socket that sent no results at all as hours of speech with no
// false wake (Sol's review of #135 at 21bbef7: finals 0, false_wakes 0,
// per_hour 0). A socket that sends no result, or only an interim, must fail.
// The controls send a final, which comes back marked final, and an empty
// final, which comes back too, since it closes its interims' segment (Sol's
// review of #155 at 1ce434e).
func TestProbeTranscribeNeedsAFinal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		results []string
		ok      bool
		want    []probeResult
	}{
		{"no result", nil, false, nil},
		{"only an interim", []string{probeResultFrame("Whisper, what", false)}, false, nil},
		{"an interim and a final", []string{probeResultFrame("Whisper, what", false), probeResultFrame("Whisper, what's the status?", true)}, true,
			[]probeResult{{"Whisper, what", false}, {"Whisper, what's the status?", true}}},
		{"an interim and an empty final", []string{probeResultFrame("Whisper, what", false), probeResultFrame("", true)}, true,
			[]probeResult{{"Whisper, what", false}, {"", true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := probeHost
			probeHost = probeEndpoint(t, tc.results...)
			t.Cleanup(func() { probeHost = old })
			opts := &interfaces.LiveTranscriptionOptions{Model: "nova-3", Language: "en-US", Encoding: "linear16", SampleRate: 16000, Channels: 1}
			heard, err := probeTranscribe(t.Context(), "test-key", opts, make([]byte, 16000*2/10))
			if !tc.ok {
				if err == nil {
					t.Fatalf("a socket that sent %s was measured: %+v", tc.name, heard)
				}
				return
			}
			if err != nil {
				t.Fatalf("control: %v", err)
			}
			if !slices.Equal(heard, tc.want) {
				t.Fatalf("control: heard %+v, want %+v", heard, tc.want)
			}
		})
	}
}

// TestProbeClipsKeepWhetherEachResultWasFinal fails if a short probe's clip
// keeps what it heard without saying which results were final: the saved
// clips could not show that their last text was a final (Sol's review of
// #135 at 21bbef7).
func TestProbeClipsKeepWhetherEachResultWasFinal(t *testing.T) {
	raw, err := json.Marshal(probeClip{Heard: []probeResult{{Text: "Whisper, what", Final: false}, {Text: "Whisper, what's the status?", Final: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"heard":[{"text":"Whisper, what","final":false},{"text":"Whisper, what's the status?","final":true}]`)) {
		t.Fatalf("a clip saved as %s, want each result's text and whether it was final", raw)
	}
}

// TestEmptyFinalsSeparateFalseWakeStretches fails if the false-wake probe
// counts two waking interims as one stretch when each is closed by an empty
// final: the empty finals were dropped before the stretches were cut (Sol's
// review of #155 at 1ce434e: one stretch, for both keyterm runs). The
// meeting is cached silence, and a local endpoint gives the results, so no
// request goes out.
func TestEmptyFinalsSeparateFalseWakeStretches(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "corpus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus", "m.pcm"), make([]byte, 32000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "corpus", "m.pcm.inserted"), []byte("0"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_WISPR_DEEPGRAM_API_KEY", "test-key")
	t.Setenv("GHOST_WISPR_WAKE_PROBE_DIR", dir)
	t.Setenv("GHOST_WISPR_AMI_WORDS", dir)
	t.Setenv("GHOST_WISPR_WAKE_PROBE_PROJECTS", "INFRA")
	oldHost, oldMeetings := probeHost, probeMeetings
	probeMeetings = []string{"m"}
	probeHost = probeEndpoint(t,
		probeResultFrame("Whisper, what?", false), probeResultFrame("", true),
		probeResultFrame("Whisper, who?", false), probeResultFrame("", true))
	t.Cleanup(func() { probeHost, probeMeetings = oldHost, oldMeetings })
	TestProbeWakeFalseRate(t)
	raw, err := os.ReadFile(filepath.Join(dir, "falserate-summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]json.RawMessage // the runs, and the corpus's own numbers
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"without Whisper", "with Whisper"} {
		var row struct {
			Wakes  int `json:"false_wakes"`
			Finals int `json:"finals"`
		}
		if err := json.Unmarshal(summary[name], &row); err != nil {
			t.Fatalf("the %q run in %s: %v", name, raw, err)
		}
		if row.Wakes != 2 || row.Finals != 0 {
			t.Errorf("%s: %d false-wake stretches and %d finals with text, want 2 and 0", name, row.Wakes, row.Finals)
		}
	}
}

// TestAMIUtterancesSplitOnARealPause fails if a speaker's words split into
// two utterances where the pause between them is under a second: they were
// split by the time between the words' starts, so a 1.5 s word followed 0.1 s
// later was taken for a pause (Sol's review of #135 at 21bbef7). The control
// has a real 1.2 s pause, which splits.
func TestAMIUtterancesSplitOnARealPause(t *testing.T) {
	for _, tc := range []struct {
		next string
		want int
	}{{`<w starttime="1.6" endtime="1.9">works</w>`, 1}, {`<w starttime="2.7" endtime="3.0">works</w>`, 2}} {
		dir := t.TempDir()
		xml := `<words><w starttime="0" endtime="1.5">configuration</w>` + tc.next + `</words>`
		if err := os.WriteFile(filepath.Join(dir, "m.A.words.xml"), []byte(xml), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := amiUtterances(dir, "m")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.want {
			t.Errorf("after a word ending at 1.5 s, %s gave %d utterances %+v, want %d", tc.next, len(got), got, tc.want)
		}
	}
}

// TestProbeMeetingInsertsBySpokenAudio fails if the false-wake probe puts its
// own sentence in by the words' estimated time rather than the speech's audio:
// two utterances of 300 words, each 240 s of audio, were taken for 120 s each,
// and the first sentence came at 480.6 s instead of after the first
// utterance (Sol's review of #135 at 21bbef7). The speech is cached, so no
// request goes out.
func TestProbeMeetingInsertsBySpokenAudio(t *testing.T) {
	dir := t.TempDir()
	words := filepath.Join(dir, "words")
	for _, sub := range []string{"corpus", "tts", "words"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var xml strings.Builder
	xml.WriteString("<words>")
	for i := range 300 {
		fmt.Fprintf(&xml, `<w starttime="%f" endtime="%f">word</w>`, float64(i)/2, float64(i)/2+0.4)
	}
	for i := range 300 {
		fmt.Fprintf(&xml, `<w starttime="%f" endtime="%f">other</w>`, 1000+float64(i)/2, 1000+float64(i)/2+0.4)
	}
	xml.WriteString(`<w starttime="2000" endtime="2000.4">next</w></words>`)
	if err := os.WriteFile(filepath.Join(words, "m.A.words.xml"), []byte(xml.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	voice := probeVoices[0]
	cache := func(text string, seconds int, value byte) {
		sum := sha256.Sum256([]byte(voice + "\x00" + text))
		if err := os.WriteFile(filepath.Join(dir, "tts", hex.EncodeToString(sum[:16])+".pcm"), bytes.Repeat([]byte{value}, seconds*16000*2), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cache(strings.TrimSpace(strings.Repeat("word ", 300)), 240, 0)
	cache(strings.TrimSpace(strings.Repeat("other ", 300)), 240, 0)
	cache("next", 1, 0)
	for _, s := range probeNonCalls {
		cache(s, 1, 127)
	}
	pcm, n, err := probeMeeting(t.Context(), "unused-key", words, dir, "m", 0)
	if err != nil {
		t.Fatal(err)
	}
	at := float64(bytes.IndexByte(pcm, 127)) / (16000 * 2)
	if n < 1 || at < 240 || at > 241 {
		t.Fatalf("%d sentences put in, the first at %.2f s of audio, want one right after the first 240 s utterance", n, at)
	}
}
