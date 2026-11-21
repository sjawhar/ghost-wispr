package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/audio"
	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/embedding"
	"github.com/sjawhar/ghost-wispr/internal/llm"
	"github.com/sjawhar/ghost-wispr/internal/tts"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// liveSentence is what each text-to-speech model speaks, and what batch and
// live transcription must hear back.
const liveSentence = "The meeting chose the blue design."

// TestLiveEveryRoleReachesItsProvider calls each role of the config
// GHOST_WISPR_LIVE_CONFIG names on its real provider, through the unit's own
// wiring: the settings page's providers check, a summary (plain and JSON),
// the Concierge's stream, a pair of search embeddings, voice A's first frame
// and the speak API's clip for tts.model and for each model
// GHOST_WISPR_LIVE_TTS_MODELS adds (provider/model, comma-separated), a batch
// transcription and a live transcription of the first clip, which must each
// hear its words, and a session of voice C and of voice B, which must each
// open. Live transcription and the voices are skipped only when their
// provider is not configured (voice B: when voice.gptlive.model's provider
// does not sign in). It logs each answer and how long it took. It runs only
// when GHOST_WISPR_LIVE_PROVIDERS=1:
//
//	GHOST_WISPR_LIVE_PROVIDERS=1 GHOST_WISPR_LIVE_CONFIG=ghost-wispr.yaml go test -run TestLiveEveryRole -v ./cmd/ghost-wispr
//
// The providers sign in as the config says, with the keys the environment
// holds under each auth.env, and Google's credentials from
// google_credentials_file or Application Default Credentials.
func TestLiveEveryRoleReachesItsProvider(t *testing.T) {
	if os.Getenv("GHOST_WISPR_LIVE_PROVIDERS") != "1" {
		t.Skip("set GHOST_WISPR_LIVE_PROVIDERS=1 and GHOST_WISPR_LIVE_CONFIG to call every role's real provider")
	}
	store, warnings, err := config.NewStore(os.Getenv("GHOST_WISPR_LIVE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		t.Logf("config warning: %s", w)
	}
	cfg := store.Get()
	ctx := t.Context()

	t.Run("providers check", func(t *testing.T) {
		statuses := providerStatuses(store)(ctx, 0)
		if len(statuses) == 0 {
			t.Fatal("the config names no provider")
		}
		for _, s := range statuses {
			t.Logf("%s (%s API, %s): ok=%v: %s", s.Name, s.API, s.SignIn, s.OK, s.Message)
			if !s.OK {
				t.Errorf("%s did not sign in: %s", s.Name, s.Message)
			}
		}
	})

	if ref := cfg.Summarization.Model; ref != "" {
		t.Run("summaries", func(t *testing.T) {
			name, model, err := config.ParseModelRef(ref)
			if err != nil {
				t.Fatal(err)
			}
			client, err := summaryClients(store)(name, model)
			if err != nil {
				t.Fatal(err)
			}
			ask := []llm.Message{{Role: "user", Content: "Reply with the one word: ready"}}
			start := time.Now()
			reply, err := client.Complete(ctx, ask)
			t.Logf("%s answered %q in %v (err %v)", ref, reply, time.Since(start).Round(time.Millisecond), err)
			if err != nil || !strings.Contains(strings.ToLower(reply), "ready") {
				t.Errorf("%s: %q, %v; want ready", ref, reply, err)
			}
			start = time.Now()
			raw, err := client.CompleteJSON(ctx, ask, map[string]any{
				"type": "object", "properties": map[string]any{"word": map[string]any{"type": "string"}}, "required": []string{"word"},
			})
			t.Logf("%s answered JSON %s in %v (err %v)", ref, raw, time.Since(start).Round(time.Millisecond), err)
			if err != nil || !bytes.Contains(bytes.ToLower(raw), []byte("ready")) {
				t.Errorf("%s JSON: %s, %v; want {\"word\":\"ready\"}", ref, raw, err)
			}
		})
	}

	if ref := cfg.Concierge.Model; ref != "" {
		t.Run("concierge", func(t *testing.T) {
			p, model, err := unitProviders.of(&cfg).Model(&cfg, config.RoleConcierge, ref)
			if err != nil {
				t.Fatal(err)
			}
			generator, err := concierge.NewGenerator(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			var first time.Duration
			var text strings.Builder
			for resp, err := range generator.GenerateContentStream(ctx, model, genai.Text("Reply with the one word: ready"), nil) {
				if err != nil {
					t.Fatalf("%s streamed an error after %v: %v", ref, time.Since(start), err)
				}
				if first == 0 {
					first = time.Since(start)
				}
				text.WriteString(resp.Text())
			}
			t.Logf("%s streamed %q, first part in %v, all in %v", ref, text.String(), first.Round(time.Millisecond), time.Since(start).Round(time.Millisecond))
			if !strings.Contains(strings.ToLower(text.String()), "ready") {
				t.Errorf("%s: %q, want ready", ref, text.String())
			}
		})
	}

	if ref := cfg.Embedding.Model; ref != "" {
		t.Run("embeddings", func(t *testing.T) {
			client, _, err := newEmbeddingClient(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			docs, err := client.Embed(ctx, []string{liveSentence, "Lunch is at noon on Fridays."}, embedding.TaskTypeDocument)
			took := time.Since(start)
			if err != nil || len(docs) != 2 || len(docs[0]) == 0 {
				t.Fatalf("%s: %d vectors, %v", ref, len(docs), err)
			}
			query, err := client.Embed(ctx, []string{"Which design did the meeting choose?"}, embedding.TaskTypeQuery)
			if err != nil || len(query) != 1 {
				t.Fatalf("%s query: %d vectors, %v", ref, len(query), err)
			}
			near, far := cosine(query[0], docs[0]), cosine(query[0], docs[1])
			t.Logf("%s: %d dimensions, 2 documents in %v; the question's cosine to its answer %.3f, to the other %.3f", ref, len(docs[0]), took.Round(time.Millisecond), near, far)
			if near <= far {
				t.Errorf("%s ranks the unrelated sentence nearer the question (%.3f <= %.3f)", ref, near, far)
			}
		})
	}

	var clip []byte
	var clipFormat audio.AudioFormat
	models := []string{cfg.TTS.Model}
	if extra := os.Getenv("GHOST_WISPR_LIVE_TTS_MODELS"); extra != "" {
		models = append(models, strings.Split(extra, ",")...)
	}
	for _, ref := range models {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		t.Run("text-to-speech "+ref, func(t *testing.T) {
			c := cfg
			if ref != cfg.TTS.Model {
				c.TTS.Model, c.TTS.Voice = ref, ""
			}
			timer, err := newVoiceATimer(ctx, &c, nil)
			if err != nil {
				t.Fatal(err)
			}
			took, err := timer.firstFrame(ctx, liveSentence)
			timer.close()
			if err != nil {
				t.Fatalf("voice A: %v", err)
			}
			speech, err := newSpeechProvider(&c)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			resp, err := speech.Synthesize(ctx, tts.SpeechRequest{Text: liveSentence})
			if err != nil {
				t.Fatalf("the speak API: %v", err)
			}
			t.Logf("%s: voice A's first frame in %v; the speak API's clip of %d bytes (%d ms of audio, model %q, voice %q) in %v",
				ref, took.Round(time.Millisecond), len(resp.Audio), resp.DurationMs, resp.Model, resp.Voice, time.Since(start).Round(time.Millisecond))
			if len(resp.Audio) == 0 {
				t.Fatal("the clip has no audio")
			}
			if clip == nil {
				clip, clipFormat = resp.Audio, resp.Format
			}
		})
	}

	if ref := cfg.BatchTranscription.Model; ref != "" && clip != nil {
		t.Run("batch transcription", func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "clip.wav")
			if err := os.WriteFile(path, wav24k(clip), 0o600); err != nil {
				t.Fatal(err)
			}
			transcriber, err := makeBatchTranscriber(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			result, err := transcriber.Transcribe(ctx, path)
			t.Logf("%s heard %q (model %q) in %v (err %v)", ref, result.Transcript, result.Model, time.Since(start).Round(time.Millisecond), err)
			if err != nil || !strings.Contains(strings.ToLower(result.Transcript), "blue design") {
				t.Errorf("%s: %q, %v; want the words of %q", ref, result.Transcript, err, liveSentence)
			}
		})
	}

	if ref := cfg.Transcription.Model; ref != "" && clip != nil {
		t.Run("live transcription", func(t *testing.T) {
			key := cfg.LiveTranscriptionKey()
			if key == "" {
				t.Skipf("%s: its provider is not configured with a key", ref)
			}
			rate := clipFormat.SampleRate
			if clipFormat.Channels != 1 || clipFormat.Encoding != audio.EncodingPCM16 || rate <= 0 {
				t.Fatalf("the clip is %+v, want mono PCM16", clipFormat)
			}
			heard := &recordingManager{}
			start := time.Now()
			rc := newLiveTranscription(ctx, fixedKey(key), liveTranscriptionClientOptions(&cfg), liveTranscriptionOptions(&cfg, rate, nil),
				liveTranscriptionConfig(&cfg, rate), transcriptCallback{manager: heard}, newWarningsRegistry(), nil)
			defer func() { _ = rc.Close() }()
			// The clip, then 2 s of silence, so Deepgram ends the utterance.
			pcm := append(slices.Clone(clip), make([]byte, 2*2*rate)...)
			chunk := 2 * rate / 10 // 100 ms
			for off := 0; off < len(pcm); off += chunk {
				_, _ = rc.Write(pcm[off:min(off+chunk, len(pcm))])
				time.Sleep(20 * time.Millisecond)
			}
			finals := func() string {
				heard.mu.Lock()
				defer heard.mu.Unlock()
				return strings.Join(heard.finals, " ")
			}
			deadline := time.Now().Add(30 * time.Second)
			for time.Now().Before(deadline) && !strings.Contains(strings.ToLower(finals()), "blue design") {
				time.Sleep(100 * time.Millisecond)
			}
			t.Logf("%s heard %q live in %v", ref, finals(), time.Since(start).Round(time.Millisecond))
			if !strings.Contains(strings.ToLower(finals()), "blue design") {
				t.Errorf("%s: live transcription heard %q, want the words of %q", ref, finals(), liveSentence)
			}
		})
	}

	t.Run("voice C", func(t *testing.T) {
		m, err := cfg.ResolveProvider(config.RoleVoiceAgent, cfg.Voice.DGAgent.Provider)
		if err != nil || cfg.APIKey(m.Provider) == "" {
			t.Skipf("voice.dgagent.provider %q is not configured with a key (%v)", cfg.Voice.DGAgent.Provider, err)
		}
		v, err := newVoiceC(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		openLiveVoice(ctx, t, "voice C on "+m.Provider, v)
	})

	t.Run("voice B", func(t *testing.T) {
		ref := cfg.Voice.GPTLive.Model
		p, _, err := unitProviders.of(&cfg).Model(&cfg, config.RoleGPTLive, ref)
		if err == nil {
			_, err = p.Credential(ctx)
		}
		if err != nil {
			t.Skipf("voice.gptlive.model %q names no provider that signs in: %v", ref, err)
		}
		v, err := newVoiceB(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		openLiveVoice(ctx, t, "voice B on "+ref, v)
	})
}

// openLiveVoice opens a session of v on its real provider, which must take
// it, and closes it.
func openLiveVoice(ctx context.Context, t *testing.T, name string, v voice.Voice) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	s, err := v.Open(ctx, &voice.Exchange{SessionID: "live-providers", Out: silentOutput{}, Tools: unreachableTools{errors.New("no tools in this test")}})
	t.Logf("%s: the session opened in %v (err %v)", name, time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		t.Fatalf("%s: the provider did not take the session: %v", name, err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("%s: close: %v", name, err)
	}
}

// cosine is the cosine similarity of two vectors of one length.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// wav24k wraps 24 kHz mono PCM16 in a WAV header.
func wav24k(pcm []byte) []byte {
	var b bytes.Buffer
	le := binary.LittleEndian
	b.WriteString("RIFF")
	_ = binary.Write(&b, le, uint32(36+len(pcm)))
	b.WriteString("WAVEfmt ")
	for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(24000), uint32(48000), uint16(2), uint16(16)} {
		_ = binary.Write(&b, le, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, le, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}
