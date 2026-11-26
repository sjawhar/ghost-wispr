// Command gptlive-probe checks voice B's route to OpenAI from the office
// unit, once the unit's Google identity is mapped to an OpenAI service
// account. First a control: a Google ID token for another audience, which
// OpenAI must refuse as an invalid subject token, so a refusal of the real
// one reads as a missing mapping and not a bad token. Then it exchanges the
// unit's Google ID token for an OpenAI access token and opens one GPT-Live
// session. The session starts with a 2 s lead-in of silence sent at once, as
// the meeting voice's wake replay does. It sends three text turns, listens
// 10 s after each, and reports the speech that comes back and when each
// speech's first word reached the own-speech feed against its audio. With
// --ask, it then streams a spoken question from a WAV file, waits for
// GPT-Live to delegate it, answers from its own backend, and reports the
// round trip. It fails when an answer goes on after Say returned, when
// GPT-Live does not delegate the question, or when a speech's first word
// reaches the feed over 300 ms after its audio starts. It opens
// no microphone and no speaker: it sends GPT-Live silence and the question
// file, never the room, keeps GPT-Live's speech in memory, and prints no
// token.
//
// Run it on the unit as the ghost-wispr user, who owns the credential's key:
//
//	gptlive-probe --identity-provider-id idp_… --service-account-id user-… --ask question.wav
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/googleauth"
	"github.com/sjawhar/ghost-wispr/internal/openaiwif"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	"github.com/sjawhar/ghost-wispr/internal/voice/gptlive"
)

// buildCommit is set by `go build -ldflags "-X main.buildCommit=<sha>"`.
// It is the first probe output so a run proves which binary it used.
var buildCommit = "unknown"

// controlAudience is the control's audience: one OpenAI's identity
// provider does not accept.
const controlAudience = "https://example.invalid/gptlive-probe-control"

// probeSettle is how long the probe listens after Say returns. GPT-Live
// marks no end of an answer: no event closes a response, and its transcript
// has no done event. Speech in that time means Say returned before the
// answer finished, and the meeting voice, which ends an exchange when Say
// returns, would cut it off.
const probeSettle = 10 * time.Second

// probeTurns is how many text turns the probe sends: each speech's lead is
// measured.
const probeTurns = 3

// leadIn is the room audio the probe sends at once as the session opens,
// as the Supervisor's wake replay does: 2 s, in 250 ms frames at 16 kHz.
const leadIn = 8

// inputFrame is how much audio each input frame carries, the unit's and the
// probe's alike.
const inputFrame = 250 * time.Millisecond

// wordLate is the latest a speech's first word may reach the own-speech
// feed after its audio starts: the Supervisor hears its echo about that
// much later, through the room's microphone and Deepgram.
const wordLate = 300 * time.Millisecond

type probeConfig struct {
	TokenURL string // empty means openaiwif.TokenEndpoint
	LiveURL  string // empty means gptlive.DefaultURL
	// StatusURL is the local unit status endpoint. It is checked before
	// tokens and again before the probe feeds any audio.
	StatusURL          string
	IdentityProviderID string
	ServiceAccountID   string
	// Subject mints the unit's Google ID token for an audience.
	Subject  func(ctx context.Context, audience string) (string, error)
	Text     string        // the text turn GPT-Live answers
	Voice    string        // GPT-Live's voice; empty means its default
	MinAudio time.Duration // the speech that passes the probe
	WAV      string        // where to write the speech; empty writes none
	// Settle is how long to listen after Say returns; 0 means probeSettle.
	Settle time.Duration
	// Turns is how many text turns to send; 0 means probeTurns.
	Turns int
	// Ask is a spoken question, 16 kHz mono PCM16, streamed in after the
	// turns for GPT-Live to delegate; empty asks none.
	Ask []int16
	// DelegateWait is how long the probe waits for GPT-Live to delegate the
	// question; 0 means delegateWait.
	DelegateWait time.Duration
	// RoomAsker is the asker the probe's Exchange.Room returns, as the
	// Supervisor would: empty means a live room with no current question, so
	// a delegation completes with the silent ending.
	RoomAsker string
	// Observe, when set to "silent" or "spoken", makes the probe open a real
	// session, raise one delegation, answer it (silently or with a spoken
	// result), and record for observeWindow after: every event, the audio,
	// and any further function call.
	Observe string
}

// defaultStatusURL is the unit's local status endpoint. The probe refuses to
// run against a recording or active session, rather than ask a live vendor
// to hear synthetic audio alongside a person.
const defaultStatusURL = "http://localhost:8080/api/status"

// answerTailQuiet is the quiet after a delegated answer's audio stops before
// the probe calls it complete.
const answerTailQuiet = 2 * time.Second

// delegateWait is how long the probe waits, after its question ends, for
// GPT-Live to delegate it and for the answer to start playing.
const delegateWait = 20 * time.Second

// delegationEvidence is the answer's code-like fact. GPT-Live's
// instructions preserve keys and figures exactly, so the probe can require
// evidence that it spoke the backend's answer rather than merely its own
// acknowledgement.
const delegationEvidence = "PROBE-42"

// delegationAnswer is the probe's backend answer to a delegation.
const delegationAnswer = "The backend's verification key is PROBE-42."

func main() { os.Exit(run()) }

// run parses the flags, runs the probe and returns the exit code.
func run() int {
	credentials := flag.String("credentials", "/opt/ghost-wispr/google-credentials.json", "the unit's Google credential file")
	provider := flag.String("identity-provider-id", "", "OpenAI workload identity provider id (idp_…)")
	account := flag.String("service-account-id", "", "OpenAI service account id the provider maps the unit to (user-…)")
	text := flag.String("text", "Say in one short sentence that the Ghost Wispr probe reached you.", "the text turn GPT-Live answers")
	voiceName := flag.String("voice", "", "GPT-Live voice, such as marin")
	wav := flag.String("wav", "", "write GPT-Live's speech to this WAV file")
	turns := flag.Int("turns", probeTurns, "text turns to send; each speech's lead is measured")
	ask := flag.String("ask", "", "a spoken question, a 16 kHz mono 16-bit WAV, for GPT-Live to delegate to the probe's backend")
	observe := flag.String("observe-delegation", "", "instead of the strict round-trip, raise one delegation and record for 20s: \"silent\" leaves the room with no asker (the silent ending), \"spoken\" gives the room an asker (a spoken control)")
	roomAsker := flag.String("room-asker", "Wispr, what is the probe's verification key?", "the asker the probe's room reports, as the Supervisor would; the silent observe mode forces it empty")
	statusURL := flag.String("status-url", defaultStatusURL, "the office unit's local /api/status endpoint; it must say paused, not capturing and no active session")
	timeout := flag.Duration("timeout", 3*time.Minute, "give up after this long")
	flag.Parse()
	clientCertificatesOff()
	printf(os.Stdout, "build commit: %s\n", buildCommit)
	if *provider == "" || *account == "" {
		printf(os.Stderr, "gptlive-probe needs --identity-provider-id and --service-account-id\n")
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var question []int16
	if *ask != "" {
		var err error
		if question, err = readQuestion(*ask); err != nil {
			printf(os.Stderr, "gptlive-probe: %v\n", err)
			return 2
		}
	}
	asker := *roomAsker
	switch *observe {
	case "", "spoken":
	case "silent":
		asker = ""
	default:
		printf(os.Stderr, "gptlive-probe: --observe-delegation must be \"silent\" or \"spoken\"\n")
		return 2
	}
	if *observe != "" && len(question) == 0 {
		printf(os.Stderr, "gptlive-probe: --observe-delegation needs a spoken question in --ask\n")
		return 2
	}
	err := probe(ctx, &probeConfig{
		StatusURL:          *statusURL,
		IdentityProviderID: *provider,
		ServiceAccountID:   *account,
		Subject: func(ctx context.Context, audience string) (string, error) {
			return googleauth.IDToken(ctx, *credentials, audience)
		},
		Text:      *text,
		Voice:     *voiceName,
		MinAudio:  time.Second,
		Turns:     *turns,
		Ask:       question,
		WAV:       *wav,
		RoomAsker: asker,
		Observe:   *observe,
	}, os.Stdout)
	if err != nil {
		printf(os.Stdout, "result: FAIL: %v\n", err)
		return 1
	}
	return 0
}

// clientCertificateEnv turns client certificates for Google's APIs on or
// off. The unit's service sets it false for its ID-token paths, and the
// probe does the same unless its caller set it.
const clientCertificateEnv = "GOOGLE_API_USE_CLIENT_CERTIFICATE"

func clientCertificatesOff() {
	if os.Getenv(clientCertificateEnv) == "" {
		_ = os.Setenv(clientCertificateEnv, "false")
	}
}

// unitStatus is the local status values the safety guard needs. It keeps no
// room words.
type unitStatus struct {
	Paused          bool   `json:"paused"`
	Capturing       bool   `json:"capturing"`
	ActiveSessionID string `json:"active_session_id"`
}

// checkSafeUnit refuses the probe unless no person session is active and the
// unit is paused. It runs before any token or socket call, then again before
// the feeder is released to send its first audio frame.
func checkSafeUnit(ctx context.Context, url, when string) error {
	if url == "" {
		url = defaultStatusURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("probe safety guard %s: status request: %w", when, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe safety guard %s: read status: %w", when, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe safety guard %s: status HTTP %d", when, resp.StatusCode)
	}
	var status unitStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&status); err != nil {
		return fmt.Errorf("probe safety guard %s: decode status: %w", when, err)
	}
	if !status.Paused || status.Capturing || status.ActiveSessionID != "" {
		return fmt.Errorf("probe safety guard %s: the unit is not paused and idle (paused=%t, capturing=%t, active_session_id_empty=%t); refusing to send audio", when, status.Paused, status.Capturing, status.ActiveSessionID == "")
	}
	return nil
}

// printf writes one report line; a report that cannot be written changes
// nothing the probe does.
func printf(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// probe runs one check and writes its report to out.
func probe(ctx context.Context, cfg *probeConfig, out io.Writer) error {
	if err := checkSafeUnit(ctx, cfg.StatusURL, "at start"); err != nil {
		return err
	}
	if err := control(ctx, cfg); err != nil {
		return err
	}
	printf(out, "control: a token for another audience was refused (invalid_subject_token)\n")

	tokens, err := openaiwif.NewTokenSource(&openaiwif.TokenConfig{
		Endpoint:           cfg.TokenURL,
		IdentityProviderID: cfg.IdentityProviderID,
		ServiceAccountID:   cfg.ServiceAccountID,
		Subject:            cfg.Subject,
	})
	if err != nil {
		return err
	}
	begin := time.Now()
	if _, err := tokens.Token(ctx); err != nil {
		return fmt.Errorf("token exchange: %w", err)
	}
	printf(out, "token exchange: ok (%v)\n", time.Since(begin).Round(time.Millisecond))

	v, err := gptlive.New(gptlive.Config{URL: cfg.LiveURL, Voice: cfg.Voice, Token: tokens.Token})
	if err != nil {
		return err
	}
	// The feeder waits. The second safety check below must pass before the
	// lead-in is released, so Open cannot flush room-like frames before it.
	tap := make(chan audio.Frame, leadIn+4)
	in := newFeeder()
	go in.run(ctx, tap)
	rec := &recording{}
	tools := &probeTools{called: make(chan time.Time, 1)}
	begin = time.Now()
	s, err := v.Open(ctx, &voice.Exchange{SessionID: "gptlive-probe", WakeAt: begin, Tap: tap, Out: rec, Tools: tools, Room: func() voice.RoomView {
		return voice.RoomView{Live: true, Asker: cfg.RoomAsker, Revision: 1}
	}})
	if err != nil {
		return fmt.Errorf("session: %w", err)
	}
	defer func() { _ = s.Close() }()
	printf(out, "session: started (%v)\n", time.Since(begin).Round(time.Millisecond))
	if err := checkSafeUnit(ctx, cfg.StatusURL, "before audio"); err != nil {
		return err
	}
	in.start <- make([]int16, leadIn*4000)
	seen := watch(s.Events())

	settle := cfg.Settle
	if settle <= 0 {
		settle = probeSettle
	}
	turns := cfg.Turns
	if turns <= 0 {
		turns = probeTurns
	}
	for i := 0; cfg.Observe == "" && i < turns; i++ {
		// Say returns once GPT-Live has spoken the turn's answer and it
		// played.
		turn := time.Now()
		if err := s.Say(ctx, cfg.Text, voice.Ack); err != nil {
			return fmt.Errorf("text turn %d: %w", i+1, err)
		}
		if first, ok := rec.firstAt(); ok && i == 0 {
			printf(out, "first audio: %v after the text turn\n", first.Sub(turn).Round(time.Millisecond))
		}
		if err := listen(ctx, rec, settle); err != nil {
			return err
		}
	}
	if cfg.Observe != "" {
		if err := observeDelegation(ctx, cfg, in, tools, rec, seen, out); err != nil {
			return err
		}
		printf(out, "result: PASS\n")
		return nil
	}
	if len(cfg.Ask) > 0 {
		if err := delegation(ctx, cfg, in, tools, rec, seen, out); err != nil {
			return err
		}
	}
	if err := seen.failure(); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	printf(out, "audio: %.2f s at %d Hz\n", rec.duration().Seconds(), gptlive.SampleRate)
	printf(out, "words: %s\n", seen.words())
	printf(out, "spoken fact errors: %d\n", seen.factErrors())
	if rec.duration() < cfg.MinAudio {
		return fmt.Errorf("GPT-Live spoke %v, want %v or more", rec.duration(), cfg.MinAudio)
	}
	if err := reportLead(out, seen.speeches()); err != nil {
		return err
	}
	if err := s.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if cfg.WAV != "" {
		if err := os.WriteFile(cfg.WAV, rec.wav(), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", cfg.WAV, err)
		}
		printf(out, "wav: %s\n", cfg.WAV)
	}
	printf(out, "result: PASS\n")
	return nil
}

// listen waits settle after Say returned, and fails at the first audio in
// that time: Say returned before the answer finished.
func listen(ctx context.Context, rec *recording, settle time.Duration) error {
	said := rec.duration()
	timer := time.NewTimer(settle)
	defer timer.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for {
		select {
		case <-timer.C:
			return nil
		case <-poll.C:
			if more := rec.duration() - said; more > 0 {
				return fmt.Errorf("the answer was cut: Say returned before the answer finished, and GPT-Live spoke %v more after it returned; the meeting voice ends an exchange when Say returns, so the room would not hear the rest", more)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// reportLead prints, for GPT-Live's speeches, when each one's first word
// reached the own-speech feed relative to the start of its audio: negative
// when the word came first. It fails when a speech's first word came over
// wordLate after its audio started, or never, and when there is no speech
// to judge. The session releases each word as it arrives, so this lead is
// what keeps the Supervisor from taking Wispr's own voice for a person.
func reportLead(out io.Writer, speeches []spoken) error {
	if len(speeches) == 0 {
		printf(out, "speech lead: UNKNOWN: no speech of GPT-Live's to measure\n")
		return errors.New("speech lead: UNKNOWN: no speech of GPT-Live's to measure")
	}
	least, most := time.Duration(math.MaxInt64), time.Duration(math.MinInt64)
	for i, sp := range speeches {
		if sp.firstWord.IsZero() {
			printf(out, "speech lead: speech %d: no words reached the filter\n", i+1)
			return fmt.Errorf("speech lead: speech %d: no words reached the filter", i+1)
		}
		lead := sp.firstWord.Sub(sp.start).Round(time.Millisecond)
		if lead > wordLate {
			printf(out, "speech lead: speech %d's first word reached the filter %v after its audio started\n", i+1, lead)
			return fmt.Errorf("speech lead: speech %d's first word reached the filter %v after its audio started, over %v", i+1, lead, wordLate)
		}
		least, most = min(least, lead), max(most, lead)
	}
	printf(out, "speech lead: %d speeches, their first word from %v to %v after their audio started (negative: before), each within %v\n", len(speeches), least, most, wordLate)
	return nil
}

// control exchanges a Google ID token for another audience, which OpenAI
// must refuse as an invalid subject token: then a refusal of the real token
// means the mapping, not the token.
func control(ctx context.Context, cfg *probeConfig) error {
	tokens, err := openaiwif.NewTokenSource(&openaiwif.TokenConfig{
		Endpoint:           cfg.TokenURL,
		Audience:           controlAudience,
		IdentityProviderID: cfg.IdentityProviderID,
		ServiceAccountID:   cfg.ServiceAccountID,
		Subject:            cfg.Subject,
	})
	if err != nil {
		return err
	}
	_, err = tokens.Token(ctx)
	switch {
	case err == nil:
		return errors.New("control: OpenAI took a token for another audience, so this run cannot tell a refused token from a missing mapping")
	case !strings.Contains(err.Error(), "invalid_subject_token"):
		return fmt.Errorf("control: a token for another audience was refused with %w, want invalid_subject_token", err)
	}
	return nil
}

// events is what the probe saw on a session's Events, read as it comes so
// that nothing waits on the probe: GPT-Live's words, its first error, and
// each speech with when its first word reached the own-speech feed.
type events struct {
	mu       sync.Mutex
	text     strings.Builder
	first    error
	spoken   []spoken
	ended    []time.Time
	open     bool      // a speech is under way
	pending  time.Time // a word that came between speeches, for the next
	factErrs int
}

// spoken is one of GPT-Live's speeches as the probe saw it: when its audio
// started, and when its first word reached the own-speech feed (zero for
// none).
type spoken struct {
	start, firstWord time.Time
}

func watch(c <-chan voice.Event) *events {
	e := &events{}
	go func() {
		for ev := range c {
			now := time.Now()
			e.mu.Lock()
			switch ev.Kind {
			case voice.EventAgentText:
				e.text.WriteString(ev.Text)
				switch {
				case e.open && e.spoken[len(e.spoken)-1].firstWord.IsZero():
					e.spoken[len(e.spoken)-1].firstWord = now
				case !e.open && e.pending.IsZero():
					e.pending = now
				}
			case voice.EventSpeechStarted:
				e.spoken = append(e.spoken, spoken{start: now, firstWord: e.pending})
				e.open, e.pending = true, time.Time{}
			case voice.EventSpeechEnded:
				e.open = false
				e.ended = append(e.ended, now)
			case voice.EventFactCheck:
				e.factErrs += ev.Count
			case voice.EventErr:
				if e.first == nil {
					e.first = ev.Err
				}
			}
			e.mu.Unlock()
		}
	}()
	return e
}

func (e *events) words() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.TrimSpace(e.text.String())
}

func (e *events) failure() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.first
}

func (e *events) speeches() []spoken {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]spoken(nil), e.spoken...)
}

func (e *events) factErrors() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.factErrs
}

func (e *events) endedAfter(at time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ended := range e.ended {
		if ended.After(at) {
			return true
		}
	}
	return false
}

// feeder waits for the safety guard before it feeds the tap. It sends the
// lead-in at once, then 250 ms of silence or the question every 250 ms.
type feeder struct {
	start chan []int16 // lead-in, released after the second safety check
	ask   chan []int16
	ended chan time.Time // when the last of a question was sent
}

func newFeeder() *feeder {
	return &feeder{
		start: make(chan []int16, 1),
		ask:   make(chan []int16, 1),
		ended: make(chan time.Time, 1),
	}
}

func (f *feeder) run(ctx context.Context, tap chan<- audio.Frame) {
	var lead []int16
	select {
	case <-ctx.Done():
		return
	case lead = <-f.start:
	}
	for len(lead) > 0 {
		n := min(4000, len(lead))
		select {
		case tap <- audio.Frame{PCM: lead[:n], Rate: 16000, Channels: 1}:
			lead = lead[n:]
		case <-ctx.Done():
			return
		}
	}
	tick := time.NewTicker(inputFrame)
	defer tick.Stop()
	var question []int16
	for {
		select {
		case <-ctx.Done():
			return
		case q := <-f.ask:
			question = q
			continue
		case <-tick.C:
		}
		pcm := make([]int16, 4000)
		if len(question) > 0 {
			n := copy(pcm, question)
			question = question[n:]
			if len(question) == 0 {
				select {
				case f.ended <- time.Now():
				default:
				}
			}
		}
		select {
		case tap <- audio.Frame{PCM: pcm, Rate: 16000, Channels: 1}:
		case <-ctx.Done():
			return
		}
	}
}

// delegation streams the question in and reports GPT-Live's delegation of
// it: when it delegated, when the backend's answer began to play, and proof
// that GPT-Live actually spoke the backend's code-like fact.
func delegation(ctx context.Context, cfg *probeConfig, in *feeder, tools *probeTools, rec *recording, seen *events, out io.Writer) error {
	wait := cfg.DelegateWait
	if wait <= 0 {
		wait = delegateWait
	}
	in.ask <- cfg.Ask
	var ended time.Time
	select {
	case ended = <-in.ended:
	case <-ctx.Done():
		return ctx.Err()
	}
	printf(out, "question: %.2f s of audio sent\n", float64(len(cfg.Ask))/16000)
	var called time.Time
	select {
	case called = <-tools.called:
	case <-time.After(wait):
		return fmt.Errorf("delegation: GPT-Live did not delegate the question within %v of its end", wait)
	case <-ctx.Done():
		return ctx.Err()
	}
	deadline := time.Now().Add(wait)
	for {
		if at, ok := rec.firstAfter(called); ok {
			printf(out, "delegation: GPT-Live delegated %v after the question ended; the backend answered at once, and its answer began playing %v after that\n", called.Sub(ended).Round(time.Millisecond), at.Sub(called).Round(time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delegation: GPT-Live delegated %v after the question ended, but said nothing of the backend's answer within %v", called.Sub(ended).Round(time.Millisecond), wait)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	// The marker proves the backend answer reached GPT-Live, not that the
	// model finished it. Require its speech to end and its recorded audio to
	// stay unchanged after that, so a key in chunk 1 of 20 cannot pass a
	// truncated answer.
	last, quietAt := rec.duration(), time.Now()
	for {
		now := rec.duration()
		if now != last {
			last, quietAt = now, time.Now()
		}
		if evidence(seen.words(), delegationEvidence) && seen.endedAfter(called) && time.Since(quietAt) >= answerTailQuiet {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delegation: the backend answer did not finish and drain after its %s marker", delegationEvidence)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// evidence reports whether want's ordered, whole code-like tokens appear in
// text, whatever case or punctuation GPT-Live used. Searching substrings
// would take PROBE-142, PROBE-420 or "42 … probe" for PROBE-42.
func evidence(text, want string) bool {
	actual := evidenceTokens(text)
	expected := evidenceTokens(want)
	if len(expected) == 0 {
		return false
	}
	for i := 0; i+len(expected) <= len(actual); i++ {
		matched := true
		for j := range expected {
			if actual[i+j] != expected[j] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func evidenceTokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// observeWindow is how long observeDelegation watches GPT-Live after a
// delegation, for speech, a further function call, or any other event.
const observeWindow = 20 * time.Second

// observeDelegation opens one real delegation, lets the session answer it
// (silently when the room has no asker, with a spoken result when it does),
// and then reports for observeWindow every event, the audio, and any further
// function call. It asserts nothing: it records what the real vendor did so
// the silent ending can be judged against a spoken control.
func observeDelegation(ctx context.Context, cfg *probeConfig, in *feeder, tools *probeTools, rec *recording, seen *events, out io.Writer) error {
	wait := cfg.DelegateWait
	if wait <= 0 {
		wait = delegateWait
	}
	in.ask <- cfg.Ask
	var ended time.Time
	select {
	case ended = <-in.ended:
	case <-ctx.Done():
		return ctx.Err()
	}
	printf(out, "question: %.2f s of audio sent (mode: %s, room asker: %q)\n", float64(len(cfg.Ask))/16000, cfg.Observe, cfg.RoomAsker)
	// Let the delegation land: a spoken control calls the backend; a silent
	// run never does, so wait roomWait plus a margin for the session's bounded
	// wait and the silent ending before the window opens.
	landed := time.Now().Add(wait)
	for time.Now().Before(landed) {
		if n, _ := tools.calls(); n > 0 {
			break
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	startCalls, _ := tools.calls()
	startAudio := rec.duration()
	startWords := seen.words()
	begin := time.Now()
	printf(out, "observe: watching %v after the delegation (delegated %v after the question)\n", observeWindow, begin.Sub(ended).Round(time.Millisecond))
	select {
	case <-time.After(observeWindow):
	case <-ctx.Done():
		return ctx.Err()
	}
	endCalls, questions := tools.calls()
	spoke := rec.duration() - startAudio
	if spoke < 0 {
		spoke = 0
	}
	if _, ok := rec.firstAfter(begin); ok {
		printf(out, "observe: GPT-Live spoke %v of audio in the window\n", spoke)
	} else {
		printf(out, "observe: GPT-Live spoke no audio in the window\n")
	}
	printf(out, "observe: function calls %d->%d over the window; questions seen: %v\n", startCalls, endCalls, questions)
	printf(out, "observe: words before the window: %q\n", startWords)
	printf(out, "observe: words after the window:  %q\n", seen.words())
	printf(out, "observe: spoken-fact errors: %d\n", seen.factErrors())
	if err := seen.failure(); err != nil {
		printf(out, "observe: session reported: %v\n", err)
	}
	if cfg.WAV != "" {
		if err := os.WriteFile(cfg.WAV, rec.wav(), 0o600); err != nil {
			return fmt.Errorf("write %s: %w", cfg.WAV, err)
		}
		printf(out, "wav: %s\n", cfg.WAV)
	}
	return nil
}

// readQuestion reads a spoken question from a WAV file: mono PCM16 at 16 kHz.
func readQuestion(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the question: %w", err)
	}
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return nil, fmt.Errorf("the question %s is not a WAV file", path)
	}
	var pcm []byte
	okFormat := false
	for i := 12; i+8 <= len(data); {
		id, size := string(data[i:i+4]), int(binary.LittleEndian.Uint32(data[i+4:]))
		body := data[i+8 : min(len(data), i+8+size)]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, fmt.Errorf("the question %s has a short fmt chunk", path)
			}
			kind, channels := binary.LittleEndian.Uint16(body), binary.LittleEndian.Uint16(body[2:])
			rate, bits := binary.LittleEndian.Uint32(body[4:]), binary.LittleEndian.Uint16(body[14:])
			if kind != 1 || channels != 1 || rate != 16000 || bits != 16 {
				return nil, fmt.Errorf("the question %s is format %d, %d channels, %d Hz, %d bits; want PCM, 1 channel, 16000 Hz, 16 bits", path, kind, channels, rate, bits)
			}
			okFormat = true
		case "data":
			pcm = body
		}
		i += 8 + size + size%2
	}
	if !okFormat || len(pcm) == 0 {
		return nil, fmt.Errorf("the question %s has no PCM audio", path)
	}
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[2*i:]))
	}
	return out, nil
}

// probeTools is the probe's backend: it answers a delegation, counts the
// calls, and says when.
type probeTools struct {
	called    chan time.Time
	mu        sync.Mutex
	count     int
	questions []string
}

func (t *probeTools) Run(_ context.Context, c voice.ToolCall) (json.RawMessage, error) {
	if c.Name != voice.AnswerTool {
		return nil, fmt.Errorf("the probe has no tool %q", c.Name)
	}
	var args voice.AnswerArgs
	_ = json.Unmarshal(c.Args, &args)
	t.mu.Lock()
	t.count++
	t.questions = append(t.questions, args.Question)
	t.mu.Unlock()
	select {
	case t.called <- time.Now():
	default:
	}
	return json.Marshal(voice.AnswerResult{Say: delegationAnswer})
}

// calls is how many delegations the backend has answered, and their
// questions.
func (t *probeTools) calls() (int, []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.count, append([]string(nil), t.questions...)
}

// recording is the speaker output for the probe: it keeps the speech, and
// when each piece of it arrived.
type recording struct {
	mu     sync.Mutex
	pcm    []byte
	first  time.Time
	writes []time.Time
}

func (r *recording) Begin() voice.Playback { return r }

func (r *recording) Write(_ context.Context, frame []byte, format audio.AudioFormat) error {
	if format.SampleRate != gptlive.SampleRate || format.Channels != 1 {
		return fmt.Errorf("speech at %d Hz and %d channels, want %d Hz mono", format.SampleRate, format.Channels, gptlive.SampleRate)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if r.first.IsZero() {
		r.first = now
	}
	r.pcm = append(r.pcm, frame...)
	r.writes = append(r.writes, now)
	return nil
}

// firstAfter is when the first speech after t arrived.
func (r *recording) firstAfter(t time.Time) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.writes {
		if w.After(t) {
			return w, true
		}
	}
	return time.Time{}, false
}

func (r *recording) Drain(context.Context) error { return nil }

func (r *recording) duration() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Duration(len(r.pcm)/2) * time.Second / gptlive.SampleRate
}

func (r *recording) firstAt() (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first, !r.first.IsZero()
}

// wav is the speech as a WAV file: mono PCM16 at gptlive.SampleRate.
func (r *recording) wav() []byte { return r.wavAt(gptlive.SampleRate) }

// wavAt is the speech as a WAV file of mono PCM16 at rate.
func (r *recording) wavAt(rate int) []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := make([]byte, 44, 44+len(r.pcm))
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(r.pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], 1)
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(r.pcm)))
	return append(h, r.pcm...)
}
