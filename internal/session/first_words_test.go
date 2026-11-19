package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/recording"
)

// firstWordsBytesPerSecond is the byte rate of the audio these tests capture:
// 16-bit mono PCM at the recorder's 16000 Hz default.
const firstWordsBytesPerSecond = 16000 * 2

// labelledSecond returns one second of audio whose samples all hold label, so
// a test can tell which second of the capture a recording holds.
func labelledSecond(label uint16) []byte {
	pcm := make([]byte, firstWordsBytesPerSecond)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], label)
	}
	return pcm
}

// liveResult is a Deepgram live result: an interim (isFinal false), a final,
// or a final that ends the speech (speechFinal), with words as its transcript.
func liveResult(t *testing.T, isFinal, speechFinal bool, words ...string) *api.MessageResponse {
	t.Helper()
	type word struct {
		Speaker        int     `json:"speaker"`
		Word           string  `json:"word"`
		PunctuatedWord string  `json:"punctuated_word"`
		Start          float64 `json:"start"`
		End            float64 `json:"end"`
	}
	timed := make([]word, len(words))
	for i, w := range words {
		timed[i] = word{Word: w, PunctuatedWord: w, Start: float64(i), End: float64(i) + 0.5}
	}
	raw, err := json.Marshal(map[string]any{
		"type":         "Results",
		"is_final":     isFinal,
		"speech_final": speechFinal,
		"channel": map[string]any{"alternatives": []map[string]any{{
			"transcript": strings.Join(words, " "),
			"words":      timed,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return buildMsg(t, string(raw))
}

// capture is a manager on the real recorder, with the microphone's writer.
type capture struct {
	t        *testing.T
	dir      string
	recorder *audio.Recorder
	manager  *Manager
	mic      io.Writer
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	manager := NewManager(newStoreMock(), recorder, nil, nil, NewDetector(time.Hour), 0)
	return &capture{t: t, dir: dir, recorder: recorder, manager: manager, mic: recorder.Writer(io.Discard)}
}

// say captures one labelled second of audio per label.
func (c *capture) say(labels ...uint16) {
	c.t.Helper()
	for _, label := range labels {
		if _, err := c.mic.Write(labelledSecond(label)); err != nil {
			c.t.Fatalf("capture second %d: %v", label, err)
		}
	}
}

// hear hands the manager a result from Deepgram.
func (c *capture) hear(mr *api.MessageResponse) {
	c.t.Helper()
	if err := c.manager.Message(mr); err != nil {
		c.t.Fatalf("Message: %v", err)
	}
}

// recordedSeconds returns the labels of the seconds the session's raw
// recording holds so far, in order.
func (c *capture) recordedSeconds() []uint16 {
	c.t.Helper()
	sessionID, _ := c.manager.ActiveSession()
	if sessionID == "" {
		c.t.Fatal("no session started")
	}
	raw, err := os.ReadFile(recording.RawPath(c.dir, sessionID))
	if err != nil {
		c.t.Fatal(err)
	}
	if len(raw)%firstWordsBytesPerSecond != 0 {
		c.t.Fatalf("the recording holds %d bytes, not whole captured seconds", len(raw))
	}
	var labels []uint16
	for at := 0; at < len(raw); at += firstWordsBytesPerSecond {
		second := raw[at : at+firstWordsBytesPerSecond]
		label := binary.LittleEndian.Uint16(second)
		if !bytes.Equal(second, labelledSecond(label)) {
			c.t.Fatalf("second %d of the recording is not one captured second", at/firstWordsBytesPerSecond)
		}
		labels = append(labels, label)
	}
	return labels
}

func secondsString(labels []uint16) string {
	parts := make([]string, len(labels))
	for i, label := range labels {
		parts[i] = fmt.Sprint(label)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestALongFirstUtteranceIsAllInTheRecording fails if a session's recording
// starts after the first words its live transcript holds. A session starts at
// Deepgram's first end of speech, and its recording opened with only the last
// 5 s before that, so a first utterance longer than 5 s lost its beginning
// from the recording, and from the refined transcript that replaces the live
// one.
func TestALongFirstUtteranceIsAllInTheRecording(t *testing.T) {
	c := newCapture(t)
	c.say(1, 2) // the room before anyone speaks
	c.say(10)   // "Good morning, everyone."
	c.hear(liveResult(t, false, false, "Good", "morning,"))
	c.say(11, 12)
	c.hear(liveResult(t, false, false, "Good", "morning,", "everyone.", "This"))
	c.say(13, 14, 15)
	c.hear(liveResult(t, true, false, "Good", "morning,", "everyone.", "This", "is", "a"))
	c.say(16, 17, 18)
	c.hear(liveResult(t, false, false, "synthetic", "test", "of", "the"))
	c.hear(liveResult(t, true, true, "synthetic", "test", "of", "the", "recorder."))

	got := c.recordedSeconds()
	utterance := []uint16{10, 11, 12, 13, 14, 15, 16, 17, 18}
	if len(got) < len(utterance) || !slices.Equal(got[len(got)-len(utterance):], utterance) {
		t.Fatalf("the recording holds seconds %s; want it to end with the whole first utterance %s, whose words the live transcript holds", secondsString(got), secondsString(utterance))
	}
}
