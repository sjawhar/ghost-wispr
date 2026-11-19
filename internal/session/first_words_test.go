package session

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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
	return timedResult(t, isFinal, speechFinal, 0, words...)
}

// timedResult is liveResult with its words a second apart from first into
// the live stream.
func timedResult(t *testing.T, isFinal, speechFinal bool, first time.Duration, words ...string) *api.MessageResponse {
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
		at := first.Seconds() + float64(i)
		timed[i] = word{Word: w, PunctuatedWord: w, Start: at, End: at + 0.5}
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
// said is how much audio it has captured, which a clockStream times
// Deepgram's words against.
type capture struct {
	t        *testing.T
	dir      string
	store    *storeMock
	recorder *audio.Recorder
	manager  *Manager
	mic      io.Writer
	said     time.Duration
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	return newCaptureOn(t, func(r *audio.Recorder) Recorder { return r })
}

// newCaptureOn is newCapture with the manager on wrap(recorder).
func newCaptureOn(t *testing.T, wrap func(*audio.Recorder) Recorder) *capture {
	t.Helper()
	dir := t.TempDir()
	recorder := audio.NewRecorder(dir)
	store := newStoreMock()
	manager := NewManager(store, wrap(recorder), nil, nil, NewDetector(time.Hour), 0)
	return &capture{t: t, dir: dir, store: store, recorder: recorder, manager: manager, mic: recorder.Writer(io.Discard)}
}

// say captures one labelled second of audio per label.
func (c *capture) say(labels ...uint16) {
	c.t.Helper()
	for _, label := range labels {
		if _, err := c.mic.Write(labelledSecond(label)); err != nil {
			c.t.Fatalf("capture second %d: %v", label, err)
		}
		c.said += time.Second
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
	return c.secondsOf(sessionID)
}

// secondsOf returns the labels of the seconds session id's raw recording
// holds so far, in order.
func (c *capture) secondsOf(id string) []uint16 {
	c.t.Helper()
	raw, err := os.ReadFile(recording.RawPath(c.dir, id))
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

// textOf returns the text of session id's segments, in order.
func (c *capture) textOf(id string) []string {
	c.t.Helper()
	segments, err := c.store.GetSegments(id)
	if err != nil {
		c.t.Fatal(err)
	}
	var text []string
	for _, segment := range segments {
		text = append(text, segment.Text)
	}
	return text
}

// clockStream is a live stream with no lag: a word at into the stream was
// captured at into the capture, so it was captured said minus at ago.
type clockStream struct{ c *capture }

func (s clockStream) CapturedSince(at time.Duration) time.Duration { return max(s.c.said-at, 0) }

func (s clockStream) Captured() time.Duration { return s.c.said }

func (clockStream) CapturedBefore(at time.Duration) time.Duration { return at }

func (clockStream) MaxBehind() time.Duration { return time.Minute }

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
		t.Fatalf("the recording holds seconds %v; want it to end with the whole first utterance %v, whose words the live transcript holds", got, utterance)
	}
}

// TestWordsDeepgramTookBackDoNotLeadTheNextSession fails if words Deepgram
// showed only in interim results, then finalized as no words, still hold the
// audio from before them for the next session: it opens with the last 5 s
// before its own first words, as with no taken-back words.
func TestWordsDeepgramTookBackDoNotLeadTheNextSession(t *testing.T) {
	c := newCapture(t)
	c.say(1, 2, 3)
	c.hear(liveResult(t, false, false, "Um"))
	c.say(4, 5, 6, 7, 8, 9, 10, 11, 12)
	c.hear(liveResult(t, true, false)) // Deepgram took "Um" back
	c.say(13)
	c.hear(liveResult(t, true, true, "Hello."))

	if got, want := c.recordedSeconds(), []uint16{9, 10, 11, 12, 13}; !slices.Equal(got, want) {
		t.Fatalf("the recording holds seconds %v; want %v, the last 5 s before its first words", got, want)
	}
}

// fixedStream is a live stream that says every word was captured ago before
// its result, at the start of the capture, and that can fall behind by
// behind.
type fixedStream struct{ ago, behind time.Duration }

func (s fixedStream) CapturedSince(time.Duration) time.Duration { return s.ago }

func (fixedStream) Captured() time.Duration { return 0 }

func (fixedStream) CapturedBefore(time.Duration) time.Duration { return 0 }

func (s fixedStream) MaxBehind() time.Duration { return s.behind }

// TestAnUnsetLiveStreamTakesWordsAsCapturedNow fails if unsetting the live
// stream (SetLiveStream with nil) panics, or leaves the old stream timing the
// words: they are then taken as captured when their result comes, and the
// recording opens the usual 5 s before that.
func TestAnUnsetLiveStreamTakesWordsAsCapturedNow(t *testing.T) {
	c := newCapture(t)
	c.manager.SetLiveStream(fixedStream{ago: 20 * time.Second, behind: 30 * time.Second})
	c.manager.SetLiveStream(nil)
	c.say(1, 2, 3, 4, 5, 6, 7, 8)
	c.say(10)
	c.hear(liveResult(t, false, false, "Good", "morning,"))
	c.say(11, 12)
	c.hear(liveResult(t, true, true, "Good", "morning,", "everyone."))

	if got, want := c.recordedSeconds(), []uint16{5, 6, 7, 8, 10, 11, 12}; !slices.Equal(got, want) {
		t.Fatalf("the recording holds seconds %v; want %v, from 5 s before the words' first result", got, want)
	}
}
