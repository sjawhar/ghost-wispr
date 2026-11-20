package dgagent

// The probe against Deepgram's real Voice Agent. It runs only when
// GHOST_WISPR_DGAGENT_PROBE_DIR names a directory for its output and
// GHOST_WISPR_DEEPGRAM_API_KEY holds a key; CI has neither, so there it skips.
//
// It synthesises the question with Aura-2, plays it into the session through
// a fake tap at real time in the mic's 250 ms buffers, answers the answer
// calls from a stand-in Concierge that returns fixed AnswerResults, the
// shape the Supervisor's tools return, and records the voice's speech to a
// WAV with the time from the end of the question to the first audio. The
// default run then talks over the answer and interrupts it as the Supervisor
// would, to see that Deepgram hears the words that interrupted and asks for
// their answer, and last speaks a fact with Say. The WAV is transcribed back
// with Deepgram, and each answer's words must be in it: they reach the room
// only by injection, so a voice that does not inject them fails.
//
// GHOST_WISPR_DGAGENT_PROBE_MODELS lists provider/model names (comma
// separated) to compare, each alone and without the interruption; by
// default it runs the default model once.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

const (
	probeQuestion  = "Wispr, what is happening with the issue about realtime spoken conversation?"
	probeInterject = "Wait, who holds that issue?"
	// The answers the stand-in Concierge gives, the routing line the
	// Supervisor says while it works, and the fact Say speaks. Each has
	// words the room hears from nothing else, checked in the transcript of
	// what the voice said.
	probeStatus  = "That issue is in progress. Its pull request for the Deepgram voice opened this morning, and three voices are being compared."
	probeHolder  = "Ada Lovelace holds that issue."
	probeRouting = "Reading the record now."
	probeFact    = "The room test is today, and the Deepgram voice is ready for it."
)

// Words that only each injected text puts in the transcript. The status's
// first sentence plays before the default run talks over it. They avoid
// the words the transcriber of the recording mishears: at de10564 it heard
// "Ada Lovelace" as "Ehalovelace" and "Reading" as "Running".
const (
	statusWords  = "that issue is in progress"
	holderWords  = "holds that issue"
	routingWords = "the record now"
	factWords    = "the room test is today"
)

// The stand-in Concierge takes probeAnswerDelay, about as long as the real
// one's record lookups, and its routing line comes at probeRoutingAfter
// (the Concierge's routingAfter), which the Supervisor says through Say
// while the answer call is pending.
const (
	probeAnswerDelay  = 2500 * time.Millisecond
	probeRoutingAfter = 1500 * time.Millisecond
)

// probeConcierge answers each question as the Supervisor's tools do: an
// AnswerResult whose Say is the Concierge's words, after route is called
// at the routing line's time, as the Supervisor forwards it to Say.
func probeConcierge(log func(string, ...any), questions chan<- string, route func()) tools {
	return func(ctx context.Context, c voice.ToolCall) (json.RawMessage, error) {
		var args voice.AnswerArgs
		if c.Name != voice.AnswerTool || json.Unmarshal(c.Args, &args) != nil {
			return nil, fmt.Errorf("the probe answers only %s, got %s %s", voice.AnswerTool, c.Name, c.Args)
		}
		log("concierge answers %q", args.Question)
		select {
		case questions <- args.Question:
		default:
		}
		routing := time.AfterFunc(probeRoutingAfter, route)
		defer routing.Stop()
		select {
		case <-time.After(probeAnswerDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		say := probeStatus
		if strings.Contains(strings.ToLower(args.Question), "hold") {
			say = probeHolder
		}
		return json.Marshal(voice.AnswerResult{Say: say})
	}
}

// recordingOutput records every frame written to it, and when each
// playback's first frame came, playing at real time as the speaker would.
type recordingOutput struct {
	mu     sync.Mutex
	pcm    []byte
	begins []time.Time
}

func (o *recordingOutput) Begin() voice.Playback { return &recordingPlayback{out: o} }

func (o *recordingOutput) firstAfter(t time.Time) time.Time {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, b := range o.begins {
		if b.After(t) {
			return b
		}
	}
	return time.Time{}
}

type recordingPlayback struct {
	out     *recordingOutput
	started bool
}

func (p *recordingPlayback) Write(ctx context.Context, frame []byte, format audio.AudioFormat) error {
	if format.SampleRate != OutputRate || format.Channels != 1 {
		return fmt.Errorf("probe output takes %d Hz mono, got %+v", OutputRate, format)
	}
	p.out.mu.Lock()
	if !p.started {
		p.started = true
		p.out.begins = append(p.out.begins, time.Now())
	}
	p.out.pcm = append(p.out.pcm, frame...)
	p.out.mu.Unlock()
	select {
	case <-time.After(time.Duration(len(frame)/2) * time.Second / OutputRate):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *recordingPlayback) Drain(context.Context) error { return nil }

// feeder is the fake tap: it delivers what the room says, then silence, in
// the mic's 250 ms buffers at real time.
type feeder struct {
	mu      sync.Mutex
	start   time.Time
	sent    int // samples delivered
	pending []int16
}

const feedBuffer = InputRate / 4 // 250 ms

// say queues pcm to be heard next, and returns when its last voiced sample
// is spoken: sample n of the feed is spoken at start + n/16 kHz.
func (f *feeder) say(pcm []byte) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	at := f.sent + len(f.pending)
	last := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		s := int16(binary.LittleEndian.Uint16(pcm[i:]))
		f.pending = append(f.pending, s)
		if s > 500 || s < -500 {
			last = i / 2
		}
	}
	return f.start.Add(time.Duration(at+last) * time.Second / InputRate)
}

func (f *feeder) run(ctx context.Context, tap chan<- audio.Frame) {
	defer close(tap)
	for k := 1; ; k++ {
		select {
		case <-time.After(time.Until(f.start.Add(time.Duration(k) * 250 * time.Millisecond))):
		case <-ctx.Done():
			return
		}
		frame := make([]int16, feedBuffer)
		f.mu.Lock()
		n := copy(frame, f.pending)
		f.pending = f.pending[n:]
		f.sent += feedBuffer
		f.mu.Unlock()
		select {
		case tap <- audio.Frame{PCM: frame, Rate: InputRate, Channels: 1}:
		case <-ctx.Done():
			return
		}
	}
}

type probeRun struct {
	name  string
	think string // DefaultThinkModel when empty
	barge bool
	// unheard plays the room nothing: the question is only the Exchange's
	// AskerTurn, as when the live transcript lags the mic by more than the
	// tap holds.
	unheard bool
}

func TestProbeRealVoiceAgent(t *testing.T) {
	dir := os.Getenv("GHOST_WISPR_DGAGENT_PROBE_DIR")
	key := os.Getenv("GHOST_WISPR_DEEPGRAM_API_KEY")
	if dir == "" || key == "" {
		t.Skip("set GHOST_WISPR_DGAGENT_PROBE_DIR and GHOST_WISPR_DEEPGRAM_API_KEY to probe the real Voice Agent")
	}
	question := synthesize(t, key, probeQuestion)
	interject := synthesize(t, key, probeInterject)
	writeWAV(t, filepath.Join(dir, "question-16k.wav"), question, InputRate)
	writeWAV(t, filepath.Join(dir, "interjection-16k.wav"), interject, InputRate)
	runs := []probeRun{{name: "default", barge: true}, {name: "unheard", unheard: true}}
	if list := os.Getenv("GHOST_WISPR_DGAGENT_PROBE_MODELS"); list != "" {
		runs = nil
		for _, m := range strings.Split(list, ",") {
			m = strings.TrimSpace(m)
			runs = append(runs, probeRun{name: strings.ReplaceAll(m, "/", "_"), think: m})
		}
	}
	for _, r := range runs {
		t.Run(r.name, func(t *testing.T) { probeOnce(t, key, dir, r, question, interject) })
	}
}

// heardWords is text as words, lower case, one space apart, to find a
// phrase in a transcript whatever its punctuation.
func heardWords(text string) string { return " " + strings.Join(wordsOf(text), " ") + " " }

// routingHeard says what is wrong with the routing lines in transcript, the
// heardWords of the recording, when said of them were spoken through Say
// and notSaid were refused; "" when nothing is.
func routingHeard(transcript string, said, notSaid int32) string {
	// Each routing line, said while an answer call was pending, is heard
	// once: Deepgram speaks an injection then, and a line forwarded twice
	// or replayed is heard more often than Say spoke one.
	if said == 0 {
		return fmt.Sprintf("no routing line was spoken through Say while an answer was pending (%d refused)", notSaid)
	}
	if n := timesHeard(transcript, routingWords); n != int(said) {
		return fmt.Sprintf("the recorded speech has the routing line %d times, want once for each of the %d spoken through Say (%d refused)", n, said, notSaid)
	}
	return ""
}

// timesHeard is how many times words is heard in transcript, the heardWords
// of a recording; a phrase said twice in a row counts twice. A phrase of
// four words or more is heard with one word in it misheard, as the
// transcriber of the recording slips ("groom" for "room").
func timesHeard(transcript, words string) int {
	heard, want := strings.Fields(transcript), strings.Fields(words)
	slips := 0
	if len(want) >= 4 {
		slips = 1
	}
	n := 0
	for i := 0; i+len(want) <= len(heard); {
		miss := 0
		for j, w := range want {
			if heard[i+j] != w {
				miss++
			}
		}
		if miss <= slips {
			n++
			i += len(want)
			continue
		}
		i++
	}
	return n
}

// TestTheProbeHearsEachRoutingLineOnce fails if the probe's routing check
// passes a recording that holds a routing line more or fewer times than
// Say spoke one, so a line forwarded twice or replayed after a refused
// injection passes, or passes a run where no routing line was said.
func TestTheProbeHearsEachRoutingLineOnce(t *testing.T) {
	line := heardWords(probeRouting)
	answer := heardWords(probeStatus)
	cases := []struct {
		name       string
		transcript string
		said       int32
		ok         bool
	}{
		{"one said, heard once", line + answer, 1, true},
		{"two said, heard twice", line + answer + line + answer, 2, true},
		{"one said, heard twice", line + line + answer, 1, false},
		{"one said, heard twice apart", line + answer + line, 1, false},
		{"two said, heard once", line + answer + answer, 2, false},
		{"one said, not heard", answer, 1, false},
		{"none said", line + answer, 0, false},
	}
	for _, c := range cases {
		got := routingHeard(heardWords(c.transcript), c.said, 0)
		if (got == "") != c.ok {
			t.Errorf("%s: routingHeard = %q, want ok %v", c.name, got, c.ok)
		}
	}
	// The answers' check counts the same way: an answer said twice in a row
	// is heard twice, also when the two share the space between them.
	if n := timesHeard(heardWords(probeStatus+" "+probeStatus), statusWords); n != 2 {
		t.Errorf("an answer said twice in a row is heard %d times, want 2", n)
	}
	if n := timesHeard(heardWords(routingWords+" "+routingWords), routingWords); n != 2 {
		t.Errorf("a phrase said twice in a row is heard %d times, want 2", n)
	}
	// One slip of the recording's transcriber in a phrase of four words or
	// more still counts: at 66b984b it heard "The groom test is today" for
	// what the voice spoke as "The room test is today". Two slips, or one in
	// a shorter phrase, do not.
	for _, c := range []struct {
		heard, words string
		want         int
	}{
		{"The groom test is today, and the voice is ready.", factWords, 1},
		{"The groom test was today.", factWords, 0},
		{"Reading the records now.", routingWords, 0},
		{"The groom test is today. The room test is today.", factWords, 2},
	} {
		if n := timesHeard(heardWords(c.heard), c.words); n != c.want {
			t.Errorf("%q is heard %d times in %q, want %d", c.words, n, c.heard, c.want)
		}
	}
}

//nolint:gocritic // One run's settings, read once.
func probeOnce(t *testing.T, key, dir string, r probeRun, question, interject []byte) {
	var (
		logMu sync.Mutex
		lines []string
		ended bool // past the end of the test: t.Log would panic
	)
	start := time.Now()
	log := func(format string, args ...any) {
		line := fmt.Sprintf("%7.3fs ", time.Since(start).Seconds()) + fmt.Sprintf(format, args...)
		logMu.Lock()
		defer logMu.Unlock()
		if ended {
			return
		}
		lines = append(lines, line)
		t.Log(line)
	}
	defer func() {
		logMu.Lock()
		ended = true
		logMu.Unlock()
	}()
	logger := slog.New(slog.NewTextHandler(logWriter(func(p []byte) { log("%s", bytes.TrimSpace(p)) }), &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay := probeRelay(t, filepath.Join(dir, "frames-"+r.name+".jsonl"))
	agent, err := New(&Config{APIKey: key, URL: relay, ThinkModel: r.think, Keyterms: []string{"Wispr", "Ghost Wispr"}, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	out := &recordingOutput{}
	tap := make(chan audio.Frame, 64)
	questions := make(chan string, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	// The routing line, said through Say while an answer call is pending,
	// as the Supervisor forwards the Concierge's Ack line.
	var (
		liveMu    sync.Mutex
		live      voice.Session
		routedOK  atomic.Int32
		routedErr atomic.Int32
	)
	route := func() {
		liveMu.Lock()
		sess := live
		liveMu.Unlock()
		if sess == nil {
			return
		}
		err := sess.Say(ctx, probeRouting, voice.Ack)
		log("routing line Say returned %v", err)
		if err == nil {
			routedOK.Add(1)
		} else {
			routedErr.Add(1)
		}
	}
	x := &voice.Exchange{SessionID: "probe", WakeAt: time.Now(), Tap: tap, Out: out, Tools: probeConcierge(log, questions, route)}
	if r.unheard {
		_, x.AskerTurn, _ = strings.Cut(probeQuestion, ", ")
	}
	sess, err := agent.Open(ctx, x)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	liveMu.Lock()
	live = sess
	liveMu.Unlock()
	log("session open after %v", time.Since(start))
	feed := &feeder{start: time.Now()}
	questionEnd := time.Now() // the unheard run's question ended before the session opened
	if !r.unheard {
		questionEnd = feed.say(question)
	}
	go feed.run(ctx, tap)

	statusFirst := heardWords(strings.SplitN(probeStatus, ".", 2)[0])
	var (
		answers                   int
		speaking, sayDone         bool
		lastEnd, sayAt            time.Time
		answerStart               time.Time
		interjectAt, interjectEnd time.Time
		interruptAt, stoppedAt    time.Time
		followUpAt                time.Time
		followUpText              string
		holderAsked               bool
		sayErr                    error
		sayResult                 = make(chan error, 1)
		tick                      = time.NewTicker(100 * time.Millisecond)
		failed                    string
		agentTexts                []string
		sinceEnd                  = func() time.Duration { return time.Since(lastEnd) }
		quiet                     = func() bool { return !speaking && !lastEnd.IsZero() }
		bargeDone                 = func() bool { return !r.barge || (holderAsked && lastEnd.After(followUpAt)) }
		readyToSay                = func() bool {
			return sayAt.IsZero() && !answerStart.IsZero() && quiet() && sinceEnd() > 2500*time.Millisecond && bargeDone()
		}
		finished                   = func() bool { return sayDone && quiet() && lastEnd.After(sayAt) && sinceEnd() > 1500*time.Millisecond }
		interjectDue, interruptDue = func() bool {
			return r.barge && interjectAt.IsZero() && !answerStart.IsZero() && time.Since(answerStart) > time.Second
		}, func() bool {
			return !interjectAt.IsZero() && interruptAt.IsZero() && time.Since(interjectAt) > 1200*time.Millisecond
		}
	)
	defer tick.Stop()
loop:
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				failed = "the events closed"
				break loop
			}
			switch ev.Kind {
			case voice.EventUserTurn:
				log("user turn %q", ev.Text)
				if !interruptAt.IsZero() && followUpAt.IsZero() {
					followUpAt, followUpText = time.Now(), ev.Text
				}
			case voice.EventAgentText:
				log("agent text %q", ev.Text)
				agentTexts = append(agentTexts, ev.Text)
				if answerStart.IsZero() && strings.HasPrefix(heardWords(ev.Text), statusFirst) {
					answerStart = time.Now()
				}
			case voice.EventToolCall:
				answers++
				log("tool call %s %s", ev.Call.Name, ev.Call.Args)
			case voice.EventSpeechStarted:
				speaking = true
				log("speech started")
			case voice.EventSpeechEnded:
				speaking, lastEnd = false, time.Now()
				if !interruptAt.IsZero() && stoppedAt.IsZero() {
					stoppedAt = lastEnd
				}
				log("speech ended")
			case voice.EventErr:
				log("error %v", ev.Err)
			}
		case q := <-questions:
			if !interruptAt.IsZero() && strings.Contains(strings.ToLower(q), "hold") {
				holderAsked = true
			}
		case err := <-sayResult:
			sayDone, sayErr = true, err
			log("Say returned %v after %v", err, time.Since(sayAt))
		case <-tick.C:
			switch {
			case interjectDue():
				interjectAt = time.Now()
				interjectEnd = feed.say(interject)
				log("the room talks over the voice: %q", probeInterject)
			case interruptDue():
				interruptAt = time.Now()
				log("Interrupt, as the Supervisor would after two words")
				sess.Interrupt()
			case readyToSay():
				sayAt = time.Now()
				log("Say %q", probeFact)
				go func() { sayResult <- sess.Say(ctx, probeFact, voice.Fact) }()
			case finished():
				break loop
			}
		case <-ctx.Done():
			failed = "the probe did not finish within 75 s"
			break loop
		}
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	out.mu.Lock()
	pcm := append([]byte(nil), out.pcm...)
	out.mu.Unlock()
	wav := filepath.Join(dir, "answer-"+r.name+".wav")
	writeWAV(t, wav, pcm, OutputRate)
	heard := "" // nothing played: the word checks below say so
	if len(pcm) > 0 {
		heard = transcribe(t, key, wav)
	}
	log("Deepgram transcribes the recorded WAV as %q", heard)
	between := func(from, to time.Time) any {
		if from.IsZero() || to.IsZero() {
			return nil
		}
		return to.Sub(from).Milliseconds()
	}
	// The model's own words that played, and the Think Live fillers dropped:
	// the session logs each agent text's kind.
	logMu.Lock()
	var modelPlayed, fillersDropped int
	for _, line := range lines {
		switch {
		case strings.Contains(line, `msg="agent words" `) && strings.Contains(line, "kind=model"):
			modelPlayed++
		case strings.Contains(line, `msg="agent words" `) && strings.Contains(line, "kind=filler"):
			fillersDropped++
		}
	}
	logMu.Unlock()
	summary := map[string]any{
		"run": r.name, "think": agent.think, "wav": wav, "failed": failed,
		"question_end_to_first_audio_ms":  between(questionEnd, out.firstAfter(questionEnd)),
		"question_end_to_answer_words_ms": between(questionEnd, answerStart),
		"answer_calls":                    answers,
		"agent_texts":                     agentTexts,
		"model_own_texts_played":          modelPlayed,
		"deepgram_fillers_dropped":        fillersDropped,
		"routing_says_spoken":             routedOK.Load(),
		"routing_says_not_spoken":         routedErr.Load(),
		"output_seconds":                  float64(len(pcm)/2) / OutputRate,
		"wav_transcript":                  heard,
		"log":                             lines,
	}
	if r.barge {
		summary["interrupt_to_speech_ended_ms"] = between(interruptAt, stoppedAt)
		summary["follow_up_user_turn"] = followUpText
		summary["follow_up_answered"] = holderAsked
		summary["interjection_end_to_follow_up_audio_ms"] = between(interjectEnd, out.firstAfter(followUpAt))
	}
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summary-"+r.name+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if failed != "" {
		t.Errorf("%s; WAV %s", failed, wav)
	}
	if sayErr != nil {
		t.Errorf("Say of a fact = %v, want nil", sayErr)
	}
	transcript := heardWords(heard)
	want := []string{statusWords, factWords}
	if r.barge {
		want = append(want, holderWords)
	}
	for _, words := range want {
		// Each answer reaches the room once, word for word: a model that
		// repeats or rewords one is heard here.
		if n := timesHeard(transcript, words); n != 1 {
			t.Errorf("the recorded speech has the injected %q %d times, want once: %q", words, n, heard)
		}
	}
	if modelPlayed > 0 {
		t.Errorf("%d texts of the model's own played, want none: it keeps the turn, and only the session's own injections play", modelPlayed)
	}
	if problem := routingHeard(transcript, routedOK.Load(), routedErr.Load()); problem != "" {
		t.Errorf("%s: %q", problem, heard)
	}
	if r.unheard {
		// A run Deepgram heard a turn in did not take the path it exists for.
		logMu.Lock()
		sent := false
		for _, l := range lines {
			sent = sent || strings.Contains(l, "the asker's words at the wake go to it as text")
		}
		logMu.Unlock()
		if !sent {
			t.Error("the unheard run never sent the asker's words as text, so it did not test that path")
		}
	}
	if r.barge {
		if took := stoppedAt.Sub(interruptAt); stoppedAt.IsZero() || took > 100*time.Millisecond {
			t.Errorf("Interrupt to SpeechEnded = %v, want at most 100 ms", took)
		}
		if !strings.Contains(strings.ToLower(followUpText), "hold") {
			t.Errorf("after the Interrupt Deepgram heard %q, want the words that interrupted", followUpText)
		}
		if !holderAsked {
			t.Error("after the Interrupt the voice did not ask for the answer to the question that interrupted it")
		}
	}
	t.Logf("question end to first audio: %v ms, to the answer's words: %v ms; model's own texts played %d, Deepgram fillers dropped %d; WAV %s",
		summary["question_end_to_first_audio_ms"], summary["question_end_to_answer_words_ms"], modelPlayed, fillersDropped, wav)
}

// logWriter is an io.Writer that hands each write to a function.
type logWriter func([]byte)

func (w logWriter) Write(p []byte) (int, error) {
	w(p)
	return len(p), nil
}

// synthesize speaks text with Aura-2 as 16 kHz PCM16, as the room mic would
// hear it.
func synthesize(t *testing.T, key, text string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text})
	return deepgramPost(t, key,
		"https://api.deepgram.com/v1/speak?model=aura-2-apollo-en&encoding=linear16&sample_rate=16000&container=none&mip_opt_out=true",
		"application/json", body)
}

// transcribe returns what Deepgram hears in a WAV file.
func transcribe(t *testing.T, key, path string) string {
	t.Helper()
	wav, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data := deepgramPost(t, key, "https://api.deepgram.com/v1/listen?model=nova-3&smart_format=true&mip_opt_out=true", "audio/wav", wav)
	var result struct {
		Results struct {
			Channels []struct {
				Alternatives []struct {
					Transcript string `json:"transcript"`
				} `json:"alternatives"`
			} `json:"channels"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Results.Channels) == 0 || len(result.Results.Channels[0].Alternatives) == 0 {
		t.Fatalf("transcribe %s: %v %.200s", path, err, data)
	}
	return result.Results.Channels[0].Alternatives[0].Transcript
}

func deepgramPost(t *testing.T, key, url, contentType string, body []byte) []byte {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "token "+key)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", req.URL.Path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("POST %s: %s %v %.200s", req.URL.Path, resp.Status, err, data)
	}
	return data
}

// writeWAV writes mono PCM16 at rate as a WAV file.
func writeWAV(t *testing.T, path string, pcm []byte, rate int) {
	t.Helper()
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	w(uint32(36 + len(pcm)))
	b.WriteString("WAVEfmt ")
	w(uint32(16))
	w(uint16(1)) // PCM
	w(uint16(1)) // mono
	w(uint32(rate))
	w(uint32(rate * 2))
	w(uint16(2))
	w(uint16(16))
	b.WriteString("data")
	w(uint32(len(pcm)))
	b.Write(pcm)
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
