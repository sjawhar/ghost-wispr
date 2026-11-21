package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/websocket/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// The probe's synthetic speech: calls of Wispr, spelled as people say the
// name, and meeting sentences with "whisper" or "mister" in them, which must
// never wake it.
var (
	probeVoices = []string{
		"aura-2-thalia-en", "aura-2-andromeda-en", "aura-2-helena-en", "aura-2-apollo-en", "aura-2-arcas-en",
		"aura-2-aries-en", "aura-2-asteria-en", "aura-2-athena-en", "aura-2-orion-en", "aura-2-zeus-en",
	}
	probeCalls = []string{
		"Whisper, what's the status of INFRA ten twenty seven?",
		"Hey Whisper, who holds that issue?",
		"Whisper, what is happening with the voice work?",
		"Whisper, go ahead.",
	}
	probeNonCalls = []string{
		"Can you whisper the password to Dana after the meeting?",
		"Mister Chen said the review is done.",
		"I heard Mister Patel ask about the budget last week.",
		"Please don't whisper during the demo, it distracts people.",
		"We talked to Mister Lopez yesterday about the rollout.",
		"She would whisper the numbers so the client could not hear.",
		"Mister Okafor will join the call at three.",
		"Let's not whisper about the deadline, just say it.",
		"Ask Mister Rivera whether the contract is signed.",
		"The kids whisper in the back of the room all the time.",
		"I think Mister Grant already sent the slides.",
		"You can whisper it to me later if it's private.",
		"Did Mister Novak approve the design?",
		"They whisper that the launch slipped again.",
		"Mister Haddad owns the database migration.",
		"Nobody needs to whisper, this room is soundproof.",
		"Our customer, Mister Silva, wants a demo on Friday.",
		"Even a whisper carries in this office.",
		"Mister Kim asked what is happening with the voice work.",
		"I'll whisper the answer, and then you can say it out loud.",
	}
)

// probeClip is one synthetic sentence and what the live transcript made of it.
type probeClip struct {
	Voice   string        `json:"voice"`
	Text    string        `json:"text"`
	Call    bool          `json:"call"`
	Seconds float64       `json:"seconds"`
	Woke    bool          `json:"woke"`
	Heard   []probeResult `json:"heard"` // every result with text, interim and final, in order
}

// TestProbeWakeKeyterm measures how often the live transcript wakes Wispr on
// synthetic speech, with the office unit's exact Nova-3 options (a speaker,
// the default configuration, and the given Dispatch project keys), with and
// without "Whisper" as a keyterm: on each call, and on each sentence that is
// no call. Wake detection is the Supervisor's, on every interim and final
// result. It runs only with GHOST_WISPR_DEEPGRAM_API_KEY and
// GHOST_WISPR_WAKE_PROBE_DIR set: Deepgram's Aura-2 voices speak the
// sentences, and Deepgram's live endpoint transcribes them. It writes the
// clips' results and a summary to that directory.
func TestProbeWakeKeyterm(t *testing.T) {
	key, dir := os.Getenv("GHOST_WISPR_DEEPGRAM_API_KEY"), os.Getenv("GHOST_WISPR_WAKE_PROBE_DIR")
	if key == "" || dir == "" {
		t.Skip("set GHOST_WISPR_DEEPGRAM_API_KEY and GHOST_WISPR_WAKE_PROBE_DIR to run the probe against Deepgram")
	}
	projects := strings.Split(os.Getenv("GHOST_WISPR_WAKE_PROBE_PROJECTS"), ",")
	projects = slices.DeleteFunc(projects, func(p string) bool { return strings.TrimSpace(p) == "" })
	cfg, _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpeakerEnabled = true // the office unit has a speaker: its live request carries the keyterms
	with := liveTranscriptionOptions(&cfg, 16000, projects)
	if !slices.Contains(with.Keyterm, "Whisper") {
		t.Fatalf("control: the unit's keyterms %q hold no Whisper", with.Keyterm)
	}
	without := *with
	without.Keyterm = slices.DeleteFunc(slices.Clone(with.Keyterm), func(k string) bool { return k == "Whisper" })

	var clips []probeClip
	for _, v := range probeVoices {
		for _, text := range probeCalls {
			clips = append(clips, probeClip{Voice: v, Text: text, Call: true})
		}
	}
	// Each sentence that is no call, in two voices, every voice as often.
	for j, text := range probeNonCalls {
		for _, v := range []string{probeVoices[(2*j)%len(probeVoices)], probeVoices[(2*j+1)%len(probeVoices)]} {
			clips = append(clips, probeClip{Voice: v, Text: text})
		}
	}
	audio := map[int][]byte{}
	for i := range clips {
		pcm, err := probeSpeak(t.Context(), key, clips[i].Voice, clips[i].Text)
		if err != nil {
			t.Fatalf("Aura %s: %v", clips[i].Voice, err)
		}
		audio[i], clips[i].Seconds = pcm, float64(len(pcm))/(16000*2)
	}
	detector := transcript.New(cfg.Voice.WakePhrases)
	summary := map[string]any{"voices": len(probeVoices), "calls": len(probeVoices) * len(probeCalls), "sentences_no_call": len(probeNonCalls)}
	for _, run := range []struct {
		name string
		opts *interfaces.LiveTranscriptionOptions
	}{{"without Whisper", &without}, {"with Whisper", with}} {
		results := slices.Clone(clips)
		var wg sync.WaitGroup
		sem := make(chan struct{}, 6)
		errs := make(chan error, len(results))
		for i := range results {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				heard, err := probeTranscribe(t.Context(), key, run.opts, audio[i])
				if err != nil {
					errs <- fmt.Errorf("%s %q: %w", results[i].Voice, results[i].Text, err)
					return
				}
				for _, h := range heard {
					if h.Text == "" {
						continue // an empty final: no text to keep
					}
					results[i].Heard = append(results[i].Heard, h)
					if _, ok := detector.Find(h.Text); ok {
						results[i].Woke = true
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		woke, calls, falseWakes, noCalls, noCallSeconds := 0, 0, 0, 0, 0.0
		for _, c := range results {
			if c.Call {
				calls++
				if c.Woke {
					woke++
				}
				continue
			}
			noCalls++
			noCallSeconds += c.Seconds
			if c.Woke {
				falseWakes++
			}
		}
		summary[run.name] = map[string]any{
			"keyterms": run.opts.Keyterm, "calls_woke": woke, "calls": calls,
			"false_wakes": falseWakes, "sentences_no_call": noCalls, "no_call_seconds": noCallSeconds,
		}
		t.Logf("%s: woke on %d of %d calls; woke falsely on %d of %d sentences that are no call (%.0f s of speech)", run.name, woke, calls, falseWakes, noCalls, noCallSeconds)
		raw, _ := json.MarshalIndent(results, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, strings.ReplaceAll(run.name, " ", "-")+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "summary.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// probeSpeak has an Aura-2 voice speak text as 16 kHz mono PCM16.
func probeSpeak(ctx context.Context, key, voice, text string) ([]byte, error) {
	body, _ := json.Marshal(map[string]string{"text": text})
	q := url.Values{"model": {voice}, "encoding": {"linear16"}, "sample_rate": {"16000"}, "container": {"none"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.deepgram.com/v1/speak?"+q.Encode(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Token "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	pcm, err := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || err != nil {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return pcm, nil
}

// probeResult is one result a live socket handed on: its text, and whether
// it was final.
type probeResult struct {
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// probeResults collects every result a live socket hands on that has text
// or is final: an empty final closes the segment its interims belonged to.
type probeResults struct {
	transcriptCallback
	mu    sync.Mutex
	heard []probeResult
}

func (p *probeResults) Message(mr *api.MessageResponse) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	text := ""
	if len(mr.Channel.Alternatives) > 0 {
		text = strings.TrimSpace(mr.Channel.Alternatives[0].Transcript)
	}
	if text != "" || mr.IsFinal {
		p.heard = append(p.heard, probeResult{Text: text, Final: mr.IsFinal})
	}
	return nil
}

// probeHost is the Deepgram host the probes dial; empty is Deepgram's own.
var probeHost string

// probeTranscribe streams pcm to Deepgram's live endpoint with opts, as the
// microphone does, in 100 ms buffers at the rate they are captured, then 1.5 s
// of silence, and returns every result with text, and every final, empty
// ones included. A socket that sent no final result is an error: what it
// heard cannot be read, and must not count as having heard nothing. Its last
// result can still be an interim, which the caller sees marked.
func probeTranscribe(ctx context.Context, key string, opts *interfaces.LiveTranscriptionOptions, pcm []byte) ([]probeResult, error) {
	results := &probeResults{}
	socket, err := dialDeepgramLive(ctx, key, &interfaces.ClientOptions{Host: probeHost}, opts, results, deepgramWriteTimeout)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer func() { _ = socket.Close() }()
	const buffer = 16000 * 2 / 10 // 100 ms
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	stream := append(slices.Clone(pcm), make([]byte, 15*buffer)...)
	for off := 0; off < len(stream); off += buffer {
		if _, err := socket.Write(stream[off:min(off+buffer, len(stream))]); err != nil {
			return nil, fmt.Errorf("write: %w", err)
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := socket.Finalize(); err != nil {
		return nil, fmt.Errorf("finalize: %w", err)
	}
	time.Sleep(2 * time.Second) // the last results
	results.mu.Lock()
	defer results.mu.Unlock()
	if !slices.ContainsFunc(results.heard, func(r probeResult) bool { return r.Final }) {
		return nil, fmt.Errorf("no final result in %d results: the socket's hearing cannot be read", len(results.heard))
	}
	return slices.Clone(results.heard), nil
}
