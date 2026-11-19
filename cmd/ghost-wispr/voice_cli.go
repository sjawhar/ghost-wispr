package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/logging"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/tts"
	"github.com/sjawhar/ghost-wispr/internal/voice"
	voicetts "github.com/sjawhar/ghost-wispr/internal/voice/tts"
)

const ttsFirstAudioUsage = "usage: ghost-wispr tts-first-audio --engine gemini|aura|aura2|gemini-clip [--location LOCATION]\n" +
	"  Reads sentences from stdin, one a line, and prints for each {\"ms_to_first_frame\":N,\"mip_opt_out\":true|false|null}.\n" +
	"  --location is Gemini's Vertex AI location (default: voice.tts_location)."

// runTTSFirstAudioCommand is ghost-wispr tts-first-audio: for each non-empty
// line of stdin, the time from the start of an engine's request to the first
// PCM frame it gives, printed as {"ms_to_first_frame":N,"mip_opt_out":...}.
// gemini and aura (or aura2) are voice A's streaming engines, built and
// spoken through as the voice does, in one Session; gemini-clip is today's
// whole-clip Gemini provider, timed to its whole clip, which is its first
// byte of audio. mip_opt_out says whether the Aura socket that carried the
// sentence asked with mip_opt_out=true, as Aura dialled it; Gemini has none
// (null). The audio goes to a sink that drops it: no microphone, speaker or
// Jabra opens. vendors replaces the vendors' addresses, for tests; main
// passes nil. Wrong arguments exit 2, a failure 1 with its error on stderr.
func runTTSFirstAudioCommand(ctx context.Context, args []string, configPath string, vendors *voiceAOptions, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tts-first-audio", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	engine := flags.String("engine", "", "")
	location := flags.String("location", "", "")
	usage := func(problem string) int {
		_, _ = fmt.Fprintf(stderr, "tts-first-audio: %s\n%s\n", problem, ttsFirstAudioUsage)
		return 2
	}
	if err := flags.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	switch {
	case flags.NArg() > 0:
		return usage(fmt.Sprintf("unexpected arguments %q", flags.Args()))
	case *engine == "":
		return usage("--engine is required")
	case !slices.Contains([]string{"gemini", "aura", "aura2", "gemini-clip"}, *engine):
		return usage(fmt.Sprintf("no engine %q", *engine))
	case *location != "" && *engine != "gemini":
		return usage("--location is for --engine gemini")
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "tts-first-audio %s: %v\n", *engine, err)
		return 1
	}
	cfgStore, _, err := loadRuntimeConfig(configPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}
	cfg := cfgStore.Get()
	opts := voiceAOptions{}
	if vendors != nil {
		opts = *vendors
	}
	var timer firstAudioTimer
	switch *engine {
	case "gemini-clip":
		timer, err = newClipTimer(ctx, &cfg, &opts)
	case "gemini":
		timer, err = newVoiceATimer(ctx, &cfg, config.TTSEngineGemini, *location, &opts)
	default:
		timer, err = newVoiceATimer(ctx, &cfg, config.TTSEngineAura, "", &opts)
	}
	if err != nil {
		return fail(err)
	}
	defer timer.close()

	lines := bufio.NewScanner(stdin)
	lines.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for lines.Scan() {
		text := strings.TrimSpace(lines.Text())
		if text == "" {
			continue
		}
		took, optedOut, err := timer.firstFrame(ctx, text)
		if err != nil {
			return fail(err)
		}
		line, err := json.Marshal(firstAudioLine{
			MS:       float64(took.Microseconds()) / 1000,
			OptedOut: optedOut,
		})
		if err != nil {
			return fail(err)
		}
		if _, err := fmt.Fprintf(stdout, "%s\n", line); err != nil {
			return fail(err)
		}
	}
	if err := lines.Err(); err != nil {
		return fail(fmt.Errorf("read the sentences: %w", err))
	}
	return 0
}

// firstAudioLine is what tts-first-audio prints for one sentence.
type firstAudioLine struct {
	MS       float64 `json:"ms_to_first_frame"`
	OptedOut *bool   `json:"mip_opt_out"` // null for an engine that has no such parameter
}

// firstAudioTimer times one engine's way to its first frame.
type firstAudioTimer interface {
	// firstFrame speaks text and returns the time from the start of its
	// request to its first PCM frame and, for Aura, whether the socket that
	// carried it asked with mip_opt_out=true (nil for Gemini).
	firstFrame(ctx context.Context, text string) (time.Duration, *bool, error)
	close()
}

// voiceATimer speaks through voice A in one Session, as an exchange does,
// into a sink that drops the audio.
type voiceATimer struct {
	session voice.Session
	sink    *firstFrameSink
	aura    bool

	mu     sync.Mutex
	dialed *url.URL // the URL of the Aura socket dialled last
}

// newVoiceATimer opens voice A on engine, built from cfg as the unit builds
// it. The model and voice the unit names belong to its own engine; the other
// engine runs with its defaults. A location replaces voice.tts_location.
func newVoiceATimer(ctx context.Context, cfg *config.Config, engine, location string, opts *voiceAOptions) (*voiceATimer, error) {
	if cfg.Voice.TTSEngine != engine {
		cfg.Voice.TTSModel, cfg.Voice.TTSVoice = "", ""
	}
	cfg.Voice.TTSEngine = engine
	if location != "" {
		cfg.Voice.TTSLocation = location
	}
	t := &voiceATimer{sink: &firstFrameSink{}, aura: engine == config.TTSEngineAura}
	if t.aura {
		opts.auraDialed = func(u *url.URL) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.dialed = u
		}
	}
	built, err := voiceAEngine(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	t.session, err = voicetts.New(built).Open(ctx, &voice.Exchange{Out: t.sink})
	if err != nil {
		return nil, err
	}
	return t, nil
}

func (t *voiceATimer) firstFrame(ctx context.Context, text string) (time.Duration, *bool, error) {
	t.sink.arm()
	start := time.Now()
	if err := t.session.Say(ctx, text, voice.Fact); err != nil {
		return 0, nil, err
	}
	first, ok := t.sink.firstWrite(ctx)
	if !ok {
		return 0, nil, errors.New("the engine gave no audio")
	}
	if !t.aura {
		return first.Sub(start), nil, nil
	}
	t.mu.Lock()
	dialed := t.dialed
	t.mu.Unlock()
	if dialed == nil {
		return 0, nil, errors.New("aura spoke on no socket it reported dialling")
	}
	optedOut := slices.Equal(dialed.Query()["mip_opt_out"], []string{"true"})
	return first.Sub(start), &optedOut, nil
}

func (t *voiceATimer) close() { _ = t.session.Close() }

// firstFrameSink is the Output tts-first-audio speaks into: it plays nothing,
// and keeps the time of the first frame written since it was last armed.
type firstFrameSink struct {
	mu    sync.Mutex
	first time.Time
	got   chan struct{} // closed at the first frame since arm
}

func (s *firstFrameSink) Begin() voice.Playback { return s }

func (s *firstFrameSink) Write(context.Context, []byte, audio.AudioFormat) error {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.first.IsZero() {
		s.first = now
		if s.got != nil {
			close(s.got)
		}
	}
	return nil
}

func (*firstFrameSink) Drain(context.Context) error { return nil }

func (s *firstFrameSink) arm() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.first, s.got = time.Time{}, make(chan struct{})
}

// firstFrameWait bounds how long after its Say returns a text's first frame
// may take: voice A's Say returns once the text is synthesised, and its
// playback stage writes the first frame just after.
const firstFrameWait = 5 * time.Second

// firstWrite waits for the first frame since arm, up to firstFrameWait.
func (s *firstFrameSink) firstWrite(ctx context.Context) (time.Time, bool) {
	s.mu.Lock()
	got := s.got
	s.mu.Unlock()
	select {
	case <-got:
	case <-ctx.Done():
	case <-time.After(firstFrameWait):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first, !s.first.IsZero()
}

// clipTimer times today's whole-clip Gemini provider: its first byte of
// audio comes with the whole clip.
type clipTimer struct{ provider tts.Provider }

// newClipTimer builds the Gemini provider as the unit builds its TTS
// provider. The model and voice the unit names for another provider are not
// Gemini's; Gemini then runs with its defaults.
func newClipTimer(ctx context.Context, cfg *config.Config, opts *voiceAOptions) (clipTimer, error) {
	if cfg.TTSProvider != "gemini" {
		cfg.TTSModel, cfg.TTSVoice = "", ""
	}
	cfg.TTSProvider = "gemini"
	var geminiOptions []tts.GeminiOption
	if opts.vertexBaseURL != "" {
		geminiOptions = append(geminiOptions, tts.WithGeminiBaseURL(opts.vertexBaseURL))
	}
	provider, err := newTTSProvider(ctx, cfg, geminiOptions...)
	if err != nil {
		return clipTimer{}, err
	}
	return clipTimer{provider: provider}, nil
}

func (c clipTimer) firstFrame(ctx context.Context, text string) (time.Duration, *bool, error) {
	start := time.Now()
	speech, err := c.provider.Synthesize(ctx, tts.SpeechRequest{Text: text})
	took := time.Since(start)
	if err != nil {
		return 0, nil, err
	}
	if len(speech.Audio) == 0 {
		return 0, nil, errors.New("the engine gave no audio")
	}
	return took, nil, nil
}

func (clipTimer) close() {}

const issueHolderUsage = "usage: ghost-wispr issue-holder KEY"

// runIssueHolderCommand is ghost-wispr issue-holder KEY: the Concierge's
// issue_holder tool for KEY, run as the voice runs it, with the unit's
// Dispatch token and Envoy's registry, for the acceptance driver. It prints
// the tool's answer and a newline. An answer that is an error, such as a
// token Dispatch refuses, exits 1 with it on stderr, after the Concierge's
// log of the cause; a wrong argument count exits 2.
//
// It runs beside the running unit and changes nothing of it. It opens no
// Envoy session, since the unit holds the unit's, and only reads the unit's
// database, for the unit's own session id. The Concierge's question ledger,
// which issue_holder does not write, is a scratch database of its own.
func runIssueHolderCommand(ctx context.Context, args []string, configPath string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		_, _ = fmt.Fprintln(stderr, issueHolderUsage)
		return 2
	}
	key := args[1]
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "issue-holder %s: %v\n", key, err)
		return 1
	}
	cfgStore, _, err := loadRuntimeConfig(configPath)
	if err != nil {
		return fail(fmt.Errorf("config: %w", err))
	}
	cfg := cfgStore.Get()
	scratch, err := os.MkdirTemp("", "ghost-wispr-issue-holder-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	ledger, err := storage.NewSQLiteStore(filepath.Join(scratch, "ledger.db"))
	if err != nil {
		return fail(fmt.Errorf("open a scratch ledger: %w", err))
	}
	defer func() { _ = ledger.Close() }()
	logger := logging.New(stderr, "warn")
	c, err := newConcierge(ctx, &cfg, ledger, logger, nil)
	if err != nil {
		return fail(err)
	}
	defer c.Stop()
	// Without an Envoy URL, the tool answers that it cannot reach the agents.
	if cfg.Concierge.EnvoyURL != "" {
		listener, err := envoy.NewListener(cfg.Concierge.EnvoyURL, nil)
		if err != nil {
			return fail(err)
		}
		dbPath, err := databasePath(cfg.DBPath)
		if err != nil {
			return fail(err)
		}
		own, err := unitEnvoySession(dbPath)
		if err != nil {
			return fail(fmt.Errorf("read the unit's Envoy session id: %w", err))
		}
		c.SetAgents(envoyRegistry{Listener: listener, id: own})
	}
	answer, err := c.IssueHolder(ctx, key)
	if err != nil {
		return fail(err)
	}
	var refused struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(answer, &refused) == nil && refused.Error != "" {
		return fail(errors.New(refused.Error))
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", answer); err != nil {
		return fail(err)
	}
	return 0
}

// envoyRegistry is the unit's Envoy identity as issue-holder uses it: Envoy's
// registry, for whether a holder is live, and the unit's own session id, so
// the unit's own events name no holder. It is no session, and asks no agent.
type envoyRegistry struct {
	*envoy.Listener
	id string
}

func (r envoyRegistry) ID() string { return r.id }

func (envoyRegistry) Send(context.Context, *envoy.SendRequest) (envoy.SendResult, error) {
	return envoy.SendResult{}, errors.New("issue-holder asks no agent")
}

// unitEnvoySession reads the unit's own Envoy session id from its database
// without writing to it: the running unit owns the database, and opening it
// as the unit does backs it up and finishes the unit's erases. A unit with no
// database, or no session id yet, has none (""), and no event is its own.
func unitEnvoySession(dbPath string) (string, error) {
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	dsn := url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return "", err
	}
	defer func() { _ = db.Close() }()
	state, err := concierge.LoadEnvoyState(readOnlyAppState{db: db})
	if errors.Is(err, errReadOnlyAppState) {
		return "", nil // LoadEnvoyState would make the unit an id
	}
	if err != nil {
		return "", err
	}
	return state.SessionID, nil
}

var errReadOnlyAppState = errors.New("issue-holder writes nothing to the unit's database")

// readOnlyAppState is the unit's app_state, read through a read-only
// connection.
type readOnlyAppState struct{ db *sql.DB }

func (s readOnlyAppState) GetAppState(key string) (string, bool, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM app_state WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (readOnlyAppState) SetAppState(string, string) error { return errReadOnlyAppState }
