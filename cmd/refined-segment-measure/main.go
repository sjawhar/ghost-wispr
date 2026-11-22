// refined-segment-measure measures direct text deletion on every timed segment
// of one recording. Run with `go run ./cmd/refined-segment-measure [-config
// PATH] RECORDING`; it prints no transcript text. It transcribes the recording
// as the recorder refines a session: with batch_transcription.model ("" is
// transcription.model), at that provider's url, with that provider's key. The
// config is -config, by default $GHOST_WISPR_CONFIG, else ghost-wispr.yaml;
// the key is read from the variable the provider's auth.env names, in the
// environment or in the .env file beside the config. It exits 0 when it
// measured every segment, and 2 when it could not measure: a bad argument or
// config, a recording it could not read or transcribe, or one with no
// segment to measure.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// exitCannotMeasure is the exit status of a run that measured nothing.
const exitCannotMeasure = 2

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the tool: it transcribes the recording args name, deletes the text
// of each of its segments on a scratch store, and prints the count of
// deletions verified and refused, with each refusal's reason.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("refined-segment-measure", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: refined-segment-measure [-config PATH] RECORDING")
		flags.PrintDefaults()
	}
	configPath := flags.String("config", config.PathFromEnvironment(), "the unit's config file; by default $"+config.EnvPrefix+"CONFIG, else ghost-wispr.yaml, as the recorder reads it")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitCannotMeasure
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return exitCannotMeasure
	}
	transcriber, err := newTranscriber(*configPath)
	if err != nil {
		return complain(stderr, "%v", err)
	}
	recording := flags.Arg(0)
	hash, err := recordingname.SHA256(recording)
	if err != nil {
		return complain(stderr, "read the recording: %v", err)
	}
	result, err := transcriber.Transcribe(context.Background(), recording)
	if err != nil {
		return complain(stderr, "transcribe the recording: %v", err)
	}
	m := newMeasurement(recording, hash, result)
	// A recording with no segment gives no rate: counting no deletions is
	// not a measurement.
	if len(m.segments) == 0 {
		return complain(stderr, "refined_segments=0: nothing measured: Deepgram heard no word in the recording")
	}
	refusals := map[string]int{}
	for index := range m.segments {
		if reason := m.measureOnScratch(index); reason != "" {
			refusals[reason]++
		}
	}
	keys := make([]string, 0, len(refusals))
	refused := 0
	for reason, count := range refusals {
		keys = append(keys, reason)
		refused += count
	}
	sort.Strings(keys)
	segments := len(m.segments)
	_, _ = fmt.Fprintf(stdout, "refined_segments=%d attempted=%d verified=%d refused=%d refusal_rate=%.4f\n", segments, segments, segments-refused, refused, float64(refused)/float64(segments))
	for _, reason := range keys {
		_, _ = fmt.Fprintf(stdout, "reason=%q count=%d examples=none\n", reason, refusals[reason])
	}
	return 0
}

// complain writes "refined-segment-measure: " and the message to w, and
// returns exitCannotMeasure.
func complain(w io.Writer, format string, args ...any) int {
	_, _ = fmt.Fprintf(w, "refined-segment-measure: "+format+"\n", args...)
	return exitCannotMeasure
}

// newTranscriber builds the batch transcriber of the unit's config at
// configPath, read with the .env file beside it, as the recorder's
// makeBatchTranscriber builds it: batch_transcription.model at its
// provider's url, with the provider's key.
func newTranscriber(configPath string) (transcribe.BatchTranscriber, error) {
	// The config's warnings are about the recorder's other settings; one that
	// stops batch transcription shows as an error below.
	store, _, err := config.NewStoreWithEnv(configPath, filepath.Join(filepath.Dir(configPath), ".env"))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg := store.Get()
	p, model, err := providers.New(&cfg, providers.Options{}).Model(&cfg, config.RoleBatchTranscription, cfg.BatchTranscription.Model)
	if err != nil {
		return nil, err
	}
	key, err := p.Credential(context.Background())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", config.RoleBatchTranscription.Key, err)
	}
	// The provider's client: the key and the recording go only to its origin.
	return transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{APIKey: key, Model: model, BaseURL: p.BaseURL(), HTTPClient: p.HTTPClient(nil)}), nil
}

// measurement is a recording's refinement as the recorder publishes one: the
// words of its batch transcription, the segments built from them, and the
// model that transcribed them.
type measurement struct {
	recording string
	model     string
	words     []storage.RefinedWord
	segments  []storage.RefinedSegment
}

// newMeasurement is the refinement of recording, whose SHA-256 is hash, from
// its batch transcription result: its words with their times and speakers,
// and the model Deepgram reported.
func newMeasurement(recording, hash string, result transcribe.BatchResult) *measurement {
	words := make([]storage.RefinedWord, 0, len(result.Words))
	for ordinal := range result.Words {
		word := result.Words[ordinal]
		words = append(words, storage.RefinedWord{RecordingPath: recording, RecordingSHA256: hash, Ordinal: ordinal, Text: word.Text, Speaker: word.Speaker, Start: word.Start, End: word.End})
	}
	return &measurement{recording: recording, model: result.Model, words: words, segments: storage.BuildRefinedSegments(words)}
}

// measureOnScratch is measureOne on a scratch store of its own, removed
// after, with the store's logs discarded.
func (m *measurement) measureOnScratch(index int) string {
	dir, err := os.MkdirTemp("", "ghost-wispr-refusal-")
	if err != nil {
		return "make scratch store"
	}
	defer func() { _ = os.RemoveAll(dir) }()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	store, err := storage.NewSQLiteStore(filepath.Join(dir, "measure.db"))
	if err != nil {
		return "open scratch store"
	}
	defer func() { _ = store.Close() }()
	return m.measureOne(store, index)
}

// scratchStore is what measureOne asks of a store: the calls that publish a
// session's refinement and delete a segment's text, and the reads that check
// the deletion. *storage.SQLiteStore is one.
type scratchStore interface {
	CreateSession(id string, startedAt time.Time) error
	EndSession(id string, endedAt time.Time, audioPath, queueEvent string) error
	ClaimRefinement(id, audioPath string) (generation int, claimed bool, err error)
	PublishRefinementAtGeneration(sessionID, transcript, model string, words []storage.RefinedWord, segments []storage.RefinedSegment, generation int) (int, error)
	StoredSegments(sessionID string) ([]storage.StoredSegment, error)
	RefinedWords(sessionID string, revision int) ([]storage.RefinedWord, error)
	DeleteRefinedSegmentText(sessionID string, segmentID int64, revision int) error
	GetSession(id string) (storage.Session, error)
}

// measureOne publishes the refinement on store, which holds no session yet,
// as the recorder publishes a session's; deletes the text of segment index;
// and returns why the deletion was refused or went wrong, or "" when exactly
// that segment's words went.
func (m *measurement) measureOne(store scratchStore, index int) string {
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("measurement", started); err != nil {
		return "create session"
	}
	if err := store.EndSession("measurement", started.Add(time.Hour), m.recording, ""); err != nil {
		return "end session"
	}
	generation, claimed, err := store.ClaimRefinement("measurement", m.recording)
	if err != nil || !claimed {
		return "claim refinement"
	}
	revision, err := store.PublishRefinementAtGeneration("measurement", storage.RefinedTranscript(m.words), m.model, m.words, m.segments, generation)
	if err != nil {
		return "publish refinement"
	}
	// Read the current ids and revision immediately before every deletion.
	current, err := store.StoredSegments("measurement")
	if err != nil || len(current) != len(m.segments) {
		return "read refined segments"
	}
	selected := current[index]
	before, err := store.RefinedWords("measurement", revision)
	if err != nil {
		return "read refined words"
	}
	if err := store.DeleteRefinedSegmentText("measurement", selected.ID, revision); err != nil {
		return err.Error()
	}
	afterSession, err := store.GetSession("measurement")
	if err != nil {
		return "read post-delete session"
	}
	after, err := store.RefinedWords("measurement", afterSession.RefinedRevision)
	if err != nil {
		return "read post-delete words"
	}
	kept := make(map[int]bool, len(after))
	for _, word := range after {
		kept[word.Ordinal] = true
	}
	for _, word := range before {
		selectedWord := word.RecordingPath == selected.RecordingPath && word.RecordingSHA256 == selected.RecordingSHA256 && word.Ordinal >= selected.FirstWordOrdinal && word.Ordinal <= selected.LastWordOrdinal
		if selectedWord && kept[word.Ordinal] {
			return "selected occurrence remained"
		}
		if !selectedWord && !kept[word.Ordinal] {
			return "nonselected occurrence disappeared"
		}
	}
	return ""
}
