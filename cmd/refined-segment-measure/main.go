// refined-segment-measure measures direct text deletion on every timed segment
// of one recording. Run with `secrets DEEPGRAM_API_KEY -- go run
// ./cmd/refined-segment-measure RECORDING`; it prints no transcript text.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: refined-segment-measure RECORDING")
		os.Exit(2)
	}
	key := strings.TrimSpace(os.Getenv("DEEPGRAM_API_KEY"))
	if key == "" {
		fmt.Fprintln(os.Stderr, "DEEPGRAM_API_KEY is required")
		os.Exit(2)
	}
	recording := os.Args[1]
	contents, err := os.ReadFile(recording)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(contents)
	hash := hex.EncodeToString(sum[:])
	result, err := transcribe.NewDeepgramBatchTranscriber(transcribe.DeepgramBatchConfig{APIKey: key, Model: "nova-3"}).Transcribe(context.Background(), recording)
	if err != nil {
		panic(err)
	}
	words := make([]storage.RefinedWord, 0, len(result.Words))
	for ordinal := range result.Words {
		word := result.Words[ordinal]
		words = append(words, storage.RefinedWord{RecordingPath: recording, RecordingSHA256: hash, Ordinal: ordinal, Text: word.Text, Speaker: word.Speaker, Start: word.Start, End: word.End})
	}
	segments := storage.BuildRefinedSegments(words)
	refusals := map[string]int{}
	for index := range segments {
		if reason := measureOne(recording, hash, words, segments, index); reason != "" {
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
	fmt.Printf("refined_segments=%d attempted=%d verified=%d refused=%d refusal_rate=%.4f\n", len(segments), len(segments), len(segments)-refused, refused, float64(refused)/float64(len(segments)))
	for _, reason := range keys {
		fmt.Printf("reason=%q count=%d examples=none\n", reason, refusals[reason])
	}
}

func measureOne(recording, hash string, words []storage.RefinedWord, segments []storage.RefinedSegment, index int) string {
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
	started := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSession("measurement", started); err != nil {
		return "create session"
	}
	if err := store.EndSession("measurement", started.Add(time.Hour), recording); err != nil {
		return "end session"
	}
	generation, claimed, err := store.ClaimRefinement("measurement", recording)
	if err != nil || !claimed {
		return "claim refinement"
	}
	revision, err := store.PublishRefinementAtGeneration("measurement", storage.RefinedTranscript(words), "nova-3", words, segments, generation)
	if err != nil {
		return "publish refinement"
	}
	// Read the current ids and revision immediately before every deletion.
	current, err := store.GetSegments("measurement")
	if err != nil || len(current) != len(segments) {
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
