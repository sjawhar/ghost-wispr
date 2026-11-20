package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// deepgramAnswer is the least answer the batch transcriber accepts: one
// transcription model, and a transcript of no words.
const deepgramAnswer = `{"metadata":{"models":["m"],"model_info":{"m":{"name":"nova-2-example"}}},"results":{"channels":[{"alternatives":[{"transcript":"","words":[]}]}]}}`

// writeUnitConfig writes yaml as a unit's config file and, when env is not
// empty, env as the .env file beside it, and returns the config's path. It
// clears the unit's environment for the test (clearUnitEnvironment), so the
// config the tool loads is the one written here.
func writeUnitConfig(t *testing.T, yaml, env string) string {
	t.Helper()
	clearUnitEnvironment(t)
	dir := t.TempDir()
	if env != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// clearUnitEnvironment sets every GHOST_WISPR_ variable the environment holds
// to "" for the test, which config reads as unset: a role-model override
// such as GHOST_WISPR_TRANSCRIPTION_MODEL, a provider's key, or any other
// setting would otherwise replace the test's own.
func clearUnitEnvironment(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		if name, _, _ := strings.Cut(entry, "="); strings.HasPrefix(name, config.EnvPrefix) {
			t.Setenv(name, "")
		}
	}
}

// TestTranscriberUsesTheConfiguredProviderModelAndKey: the tool transcribes
// with batch_transcription.model, or transcription.model when that is unset,
// at its provider's url, with the provider's key from the .env file beside
// the config, as the recorder's batch transcription does.
func TestTranscriberUsesTheConfiguredProviderModelAndKey(t *testing.T) {
	for name, roles := range map[string]string{
		"batch_transcription.model":                 "transcription: {model: deepgram/nova-3}\nbatch_transcription: {model: deepgram/nova-2-example}\n",
		"transcription.model with no batch setting": "transcription: {model: deepgram/nova-2-example}\n",
	} {
		t.Run(name, func(t *testing.T) {
			type request struct{ path, model, auth string }
			var mu sync.Mutex
			var got []request
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				got = append(got, request{r.URL.Path, r.URL.Query().Get("model"), r.Header.Get("Authorization")})
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(deepgramAnswer))
			}))
			defer server.Close()
			path := writeUnitConfig(t,
				"providers:\n  deepgram: {api: deepgram, url: \""+server.URL+"/deepgram-example\", auth: {kind: api_key, env: GHOST_WISPR_MEASURE_TEST_KEY}}\n"+roles,
				"GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n")
			audio := filepath.Join(t.TempDir(), "recording.wav")
			if err := os.WriteFile(audio, []byte("fake-audio"), 0o600); err != nil {
				t.Fatal(err)
			}

			transcriber, err := newTranscriber(path)
			if err != nil {
				t.Fatalf("newTranscriber: %v", err)
			}
			if _, err := transcriber.Transcribe(context.Background(), audio); err != nil {
				t.Fatalf("Transcribe: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := request{path: "/deepgram-example/v1/listen", model: "nova-2-example", auth: "Token dg-1"}
			if len(got) != 1 || got[0] != want {
				t.Fatalf("the fake Deepgram got %+v, want one request %+v", got, want)
			}
		})
	}
}

// TestTranscriberWithoutAKeyNamesItsVariable: a provider whose key is set
// neither in the environment nor in .env stops the tool with an error that
// names batch_transcription.model and the variable its key is read from.
func TestTranscriberWithoutAKeyNamesItsVariable(t *testing.T) {
	path := writeUnitConfig(t,
		"providers:\n  deepgram: {api: deepgram, url: \"https://deepgram.example.test\", auth: {kind: api_key, env: GHOST_WISPR_MEASURE_TEST_KEY}}\ntranscription: {model: deepgram/nova-2-example}\n",
		"")

	transcriber, err := newTranscriber(path)
	if err == nil {
		t.Fatalf("newTranscriber = %v with no key, want an error", transcriber)
	}
	if !errors.Is(err, providers.ErrNoKey) || !strings.Contains(err.Error(), "batch_transcription.model") || !strings.Contains(err.Error(), "GHOST_WISPR_MEASURE_TEST_KEY") {
		t.Fatalf("newTranscriber error %q, want providers.ErrNoKey naming batch_transcription.model and GHOST_WISPR_MEASURE_TEST_KEY", err)
	}
}

// TestTranscriberWithAnUnresolvableModelNamesTheSetting: a
// batch_transcription.model that names no Deepgram provider stops the tool
// with an error that names the setting.
func TestTranscriberWithAnUnresolvableModelNamesTheSetting(t *testing.T) {
	for name, model := range map[string]string{
		"no such provider":        "elsewhere/nova-2-example",
		"not a Deepgram provider": "openai/whisper-example",
		"not provider/model":      "nova-2-example",
	} {
		t.Run(name, func(t *testing.T) {
			path := writeUnitConfig(t,
				"providers:\n"+
					"  deepgram: {api: deepgram, auth: {kind: api_key, env: GHOST_WISPR_MEASURE_TEST_KEY}}\n"+
					"  openai: {api: openai, auth: {kind: api_key, env: GHOST_WISPR_MEASURE_TEST_OPENAI_KEY}}\n"+
					"transcription: {model: deepgram/nova-3}\nbatch_transcription: {model: "+model+"}\n",
				"GHOST_WISPR_MEASURE_TEST_KEY=dg-1\nGHOST_WISPR_MEASURE_TEST_OPENAI_KEY=sk-1\n")

			transcriber, err := newTranscriber(path)
			if err == nil || !strings.Contains(err.Error(), "batch_transcription.model") {
				t.Fatalf("newTranscriber = %v, %v; want an error naming batch_transcription.model", transcriber, err)
			}
		})
	}
}

// twoSentences is a Deepgram answer of two sentences by two speakers, which
// the recorder splits into two segments, transcribed by nova-2-example.
const twoSentences = `{"metadata":{"models":["m"],"model_info":{"m":{"name":"nova-2-example"}}},"results":{"channels":[{"alternatives":[{"transcript":"Hello there. Second one.","words":[` +
	`{"word":"hello","punctuated_word":"Hello","start":0.1,"end":0.4,"speaker":0},` +
	`{"word":"there","punctuated_word":"there.","start":0.5,"end":0.9,"speaker":0},` +
	`{"word":"second","punctuated_word":"Second","start":2.0,"end":2.4,"speaker":1},` +
	`{"word":"one","punctuated_word":"one.","start":2.5,"end":2.9,"speaker":1}]}]}]}}`

// fakeDeepgram is a Deepgram that answers every request with status and
// body, and counts the requests it got.
func fakeDeepgram(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// deepgramUnit is a unit config whose transcription runs on the Deepgram at
// url, which signs in with the key GHOST_WISPR_MEASURE_TEST_KEY holds.
func deepgramUnit(url string) string {
	return "providers:\n  deepgram: {api: deepgram, url: \"" + url + "\", auth: {kind: api_key, env: GHOST_WISPR_MEASURE_TEST_KEY}}\ntranscription: {model: deepgram/nova-2-example}\n"
}

// writeRecording writes a recording of no real audio, which a fake Deepgram
// transcribes as it is told to, and returns its path.
func writeRecording(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recording.wav")
	if err := os.WriteFile(path, []byte("fake-audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// runTool runs the tool with args and returns its exit status and output.
func runTool(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestRunMeasuresEverySegment: the tool transcribes the recording with the
// unit's Deepgram, deletes each segment's text on a scratch store, and prints
// the count of segments it verified.
func TestRunMeasuresEverySegment(t *testing.T) {
	server, requests := fakeDeepgram(t, http.StatusOK, twoSentences)
	path := writeUnitConfig(t, deepgramUnit(server.URL), "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n")

	code, stdout, stderr := runTool("-config", path, writeRecording(t))
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if want := "refined_segments=2 attempted=2 verified=2 refused=0 refusal_rate=0.0000\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("the fake Deepgram got %d requests, want 1", n)
	}
}

// TestRunWithNoSegmentsSaysItMeasuredNothing: a recording Deepgram hears no
// word in has no segment to delete, so the tool says it measured nothing and
// exits 2, rather than print a rate of no deletions.
func TestRunWithNoSegmentsSaysItMeasuredNothing(t *testing.T) {
	server, _ := fakeDeepgram(t, http.StatusOK, deepgramAnswer)
	path := writeUnitConfig(t, deepgramUnit(server.URL), "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n")

	code, stdout, stderr := runTool("-config", path, writeRecording(t))
	if code != 2 || stdout != "" || !strings.Contains(stderr, "refined_segments=0: nothing measured") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 2, no result, and refined_segments=0: nothing measured", code, stdout, stderr)
	}
}

// TestRunReadsTheConfigGhostWisprConfigNames: with no -config, the tool reads
// the unit's config where the recorder does, GHOST_WISPR_CONFIG.
func TestRunReadsTheConfigGhostWisprConfigNames(t *testing.T) {
	server, requests := fakeDeepgram(t, http.StatusOK, twoSentences)
	t.Setenv(config.EnvPrefix+"CONFIG", writeUnitConfig(t, deepgramUnit(server.URL), "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n"))

	if code, _, stderr := runTool(writeRecording(t)); code != 0 || requests.Load() != 1 {
		t.Fatalf("exit %d after %d requests to the Deepgram GHOST_WISPR_CONFIG names; want 0 after 1; stderr:\n%s", code, requests.Load(), stderr)
	}
}

// TestRunRefusesABadConfigWithExit2NamingTheSetting: a config that cannot
// transcribe stops the tool with exit 2 before it calls Deepgram, and its
// message names the setting to fix.
func TestRunRefusesABadConfigWithExit2NamingTheSetting(t *testing.T) {
	server, requests := fakeDeepgram(t, http.StatusOK, twoSentences)
	for name, c := range map[string]struct {
		yaml, env string
		want      []string
	}{
		"no such provider": {
			yaml: deepgramUnit(server.URL) + "batch_transcription: {model: elsewhere/nova-2-example}\n",
			env:  "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n",
			want: []string{"batch_transcription.model", `"elsewhere"`},
		},
		"no key": {
			yaml: deepgramUnit(server.URL),
			want: []string{"batch_transcription.model", "GHOST_WISPR_MEASURE_TEST_KEY"},
		},
		"not YAML": {
			yaml: "providers: [\n",
			want: []string{"config: parse config file"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runTool("-config", writeUnitConfig(t, c.yaml, c.env), writeRecording(t))
			if code != 2 || stdout != "" {
				t.Errorf("exit %d, stdout %q; want 2 and no result", code, stdout)
			}
			for _, want := range c.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not name %s", stderr, want)
				}
			}
		})
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("the fake Deepgram got %d requests from configs that cannot transcribe, want 0", n)
	}
}

// TestRunExits2WhenItCannotMeasure: a bad argument, a recording the tool
// cannot read, and a recording Deepgram will not transcribe each stop the
// tool with exit 2 and no result.
func TestRunExits2WhenItCannotMeasure(t *testing.T) {
	refusing, _ := fakeDeepgram(t, http.StatusUnauthorized, `{"err_code":"INVALID_AUTH"}`)
	answering, _ := fakeDeepgram(t, http.StatusOK, twoSentences)
	refused := writeUnitConfig(t, deepgramUnit(refusing.URL), "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n")
	good := writeUnitConfig(t, deepgramUnit(answering.URL), "GHOST_WISPR_MEASURE_TEST_KEY=dg-1\n")
	recording := writeRecording(t)
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"no recording":                  {args: []string{"-config", good}, want: "usage: refined-segment-measure"},
		"two recordings":                {args: []string{"-config", good, recording, recording}, want: "usage: refined-segment-measure"},
		"an unknown flag":               {args: []string{"-config", good, "-model", "nova-3", recording}, want: "flag provided but not defined: -model"},
		"a recording that is not there": {args: []string{"-config", good, filepath.Join(t.TempDir(), "missing.wav")}, want: "read the recording"},
		"Deepgram refuses":              {args: []string{"-config", refused, recording}, want: "transcribe the recording"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := runTool(c.args...)
			if code != 2 || stdout != "" || !strings.Contains(stderr, c.want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want 2, no result, and a message with %q", code, stdout, stderr, c.want)
			}
		})
	}
}

// TestRunHelpExits0: -h prints the usage and its -config flag, measures
// nothing, and exits 0.
func TestRunHelpExits0(t *testing.T) {
	code, stdout, stderr := runTool("-h")
	if code != 0 || stdout != "" || !strings.Contains(stderr, "usage: refined-segment-measure") || !strings.Contains(stderr, "-config") {
		t.Errorf("exit %d, stdout %q, stderr %q; want 0, no result, and the usage with -config", code, stdout, stderr)
	}
}

// twoSegments is the refinement of a recording of two sentences by two
// speakers, transcribed by nova-2-example: segment 0 holds words 0 and 1,
// segment 1 words 2 and 3.
func twoSegments() *measurement {
	return newMeasurement("recording.wav", "sha-1", transcribe.BatchResult{
		Model: "nova-2-example",
		Words: []transcribe.TimestampedWord{
			{Text: "Hello", Start: 0.1, End: 0.4},
			{Text: "there.", Start: 0.5, End: 0.9},
			{Text: "Second", Start: 2.0, End: 2.4, Speaker: 1},
			{Text: "one.", Start: 2.5, End: 2.9, Speaker: 1},
		},
	})
}

// newScratchStore opens an empty store of the test's own.
func newScratchStore(t *testing.T) *storage.SQLiteStore {
	t.Helper()
	store, err := storage.NewSQLiteStore(filepath.Join(t.TempDir(), "measure.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestMeasureOneStoresTheModelDeepgramReported: the scratch session's
// refinement is by the model the transcription result names, as the recorder
// publishes a session's refinement with the model Deepgram reported.
func TestMeasureOneStoresTheModelDeepgramReported(t *testing.T) {
	store := newScratchStore(t)
	if reason := twoSegments().measureOne(store, 0); reason != "" {
		t.Fatalf("measureOne refused: %s", reason)
	}
	session, err := store.GetSession("measurement")
	if err != nil {
		t.Fatal(err)
	}
	if session.RefinementModel != "nova-2-example" {
		t.Errorf("scratch session's refinement model %q, want nova-2-example, the model the result names", session.RefinementModel)
	}
}

// plantedStore is a scratch store whose Delete text is deleteText, run in
// place of the store's own.
type plantedStore struct {
	*storage.SQLiteStore
	deleteText func(store *storage.SQLiteStore, sessionID string, segmentID int64, revision int) error
}

func (p plantedStore) DeleteRefinedSegmentText(sessionID string, segmentID int64, revision int) error {
	return p.deleteText(p.SQLiteStore, sessionID, segmentID, revision)
}

// republishWithout is a Delete text that removes the words drop picks, given
// the ordinals of the segment asked for: it publishes the session's words at
// revision again without them, as a new revision.
func republishWithout(drop func(ordinal, first, last int) bool) func(*storage.SQLiteStore, string, int64, int) error {
	return func(store *storage.SQLiteStore, sessionID string, segmentID int64, revision int) error {
		segments, err := store.StoredSegments(sessionID)
		if err != nil {
			return err
		}
		first, last := -1, -1
		for i := range segments {
			if segments[i].ID == segmentID {
				first, last = segments[i].FirstWordOrdinal, segments[i].LastWordOrdinal
			}
		}
		words, err := store.RefinedWords(sessionID, revision)
		if err != nil {
			return err
		}
		var kept []storage.RefinedWord
		for _, word := range words {
			if !drop(word.Ordinal, first, last) {
				kept = append(kept, word)
			}
		}
		session, err := store.GetSession(sessionID)
		if err != nil {
			return err
		}
		generation, claimed, err := store.ClaimRefinement(sessionID, session.AudioPath)
		if err != nil || !claimed {
			return errors.Join(errors.New("planted deletion: claim refinement"), err)
		}
		_, err = store.PublishRefinementAtGeneration(sessionID, storage.RefinedTranscript(kept), session.RefinementModel, kept, storage.BuildRefinedSegments(kept), generation)
		return err
	}
}

// TestMeasureOneReportsWhatTheDeletionDid: measureOne passes a deletion that
// removed exactly the segment's words, and otherwise names what went wrong:
// the store's refusal, a selected word left behind, or a word of another
// segment taken with it.
func TestMeasureOneReportsWhatTheDeletionDid(t *testing.T) {
	for name, c := range map[string]struct {
		deleteText func(*storage.SQLiteStore, string, int64, int) error
		want       string
	}{
		"the store's own deletion": {deleteText: (*storage.SQLiteStore).DeleteRefinedSegmentText, want: ""},
		"exactly the segment's words, planted": {
			deleteText: republishWithout(func(ordinal, first, last int) bool { return ordinal >= first && ordinal <= last }),
			want:       "",
		},
		"refused": {
			deleteText: func(*storage.SQLiteStore, string, int64, int) error { return storage.ErrStaleTranscriptRevision },
			want:       storage.ErrStaleTranscriptRevision.Error(),
		},
		"a selected word left behind": {
			deleteText: republishWithout(func(ordinal, first, last int) bool { return ordinal >= first && ordinal < last }),
			want:       "selected occurrence remained",
		},
		"a word of the next segment taken too": {
			deleteText: republishWithout(func(ordinal, first, last int) bool { return ordinal >= first && ordinal <= last+1 }),
			want:       "nonselected occurrence disappeared",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := plantedStore{SQLiteStore: newScratchStore(t), deleteText: c.deleteText}
			if got := twoSegments().measureOne(store, 0); got != c.want {
				t.Errorf("measureOne = %q, want %q", got, c.want)
			}
		})
	}
}
