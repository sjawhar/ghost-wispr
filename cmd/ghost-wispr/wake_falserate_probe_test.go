package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"

	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// probeMeetings are the AMI meetings whose transcripts say "whisper" or
// "mister" (or "Mr"), about 30 times in all, in people's own words.
var probeMeetings = []string{
	"EN2004a", "IS1003d", "TS3011d", "TS3011c", "TS3011a", "TS3007d", "TS3006b", "IS1008c", "IS1008a", "IS1006b",
	"IS1004c", "IS1004a", "IS1003c", "IS1003b", "IN1013", "ES2013a", "ES2009c", "ES2007a", "EN2009b",
}

// probeInsertEvery is how often, in the synthetic meeting's audio, one of the
// probe's own sentences with "whisper" or "mister" in it (probeNonCalls) is
// said, by the speaker who speaks next.
const probeInsertEvery = 4 * time.Minute

// probeChunk is how much of a meeting one live socket hears.
const probeChunk = 10 * time.Minute

// amiWord is a word or punctuation mark of an AMI transcript.
type amiWord struct {
	start float64
	text  string
	punc  bool
}

// amiUtterance is what one speaker says without a pause of a second or more.
type amiUtterance struct {
	start   float64
	speaker string
	text    string
}

// TestProbeWakeFalseRate measures how often the live transcript wakes Wispr
// when nobody calls it, on hours of meeting speech, with the office unit's
// exact Nova-3 options, with and without "Whisper" as a keyterm. The speech is
// the AMI meetings (probeMeetings) as their transcripts say them, each
// speaker in an Aura-2 voice, with one of the probe's own sentences with
// "whisper" or "mister" in it every probeInsertEvery. A false wake is a
// stretch of results, up to and including a final, of which any wakes the
// Supervisor's detector. It runs only with GHOST_WISPR_DEEPGRAM_API_KEY,
// GHOST_WISPR_WAKE_PROBE_DIR and GHOST_WISPR_AMI_WORDS (the words folder of
// AMI's manual annotations) set, and writes the corpus, every waking result
// and a summary with each rate's 95% upper bound to the probe folder.
func TestProbeWakeFalseRate(t *testing.T) {
	key, dir, words := os.Getenv("GHOST_WISPR_DEEPGRAM_API_KEY"), os.Getenv("GHOST_WISPR_WAKE_PROBE_DIR"), os.Getenv("GHOST_WISPR_AMI_WORDS")
	if key == "" || dir == "" || words == "" {
		t.Skip("set GHOST_WISPR_DEEPGRAM_API_KEY, GHOST_WISPR_WAKE_PROBE_DIR and GHOST_WISPR_AMI_WORDS to run the probe against Deepgram")
	}
	projects := strings.Split(os.Getenv("GHOST_WISPR_WAKE_PROBE_PROJECTS"), ",")
	projects = slices.DeleteFunc(projects, func(p string) bool { return strings.TrimSpace(p) == "" })
	cfg, _, err := config.Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.SpeakerEnabled = true
	with := liveTranscriptionOptions(&cfg, 16000, projects)
	if !slices.Contains(with.Keyterm, "Whisper") {
		t.Fatalf("control: the unit's keyterms %q hold no Whisper", with.Keyterm)
	}
	without := *with
	without.Keyterm = slices.DeleteFunc(slices.Clone(with.Keyterm), func(k string) bool { return k == "Whisper" })

	ctx := t.Context()
	corpus := filepath.Join(dir, "corpus")
	for _, d := range []string{corpus, filepath.Join(dir, "tts")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var chunks [][]byte
	var seconds float64
	inserted := 0
	for i, m := range probeMeetings {
		pcm, n, err := probeMeeting(ctx, key, words, dir, m, i)
		if err != nil {
			t.Fatalf("meeting %s: %v", m, err)
		}
		inserted += n
		seconds += float64(len(pcm)) / (16000 * 2)
		step := int(probeChunk.Seconds()) * 16000 * 2
		for off := 0; off < len(pcm); off += step {
			chunks = append(chunks, pcm[off:min(off+step, len(pcm))])
		}
	}
	t.Logf("corpus: %d meetings, %.2f h of speech, %d sentences of the probe's own, %d sockets per run", len(probeMeetings), seconds/3600, inserted, len(chunks))

	detector := transcript.New(cfg.Voice.WakePhrases)
	mentions := regexp.MustCompile(`(?i)\b(whisper\w*|mister|mr)\b`)
	summary := map[string]any{"meetings": probeMeetings, "hours": seconds / 3600, "probe_sentences": inserted}
	for _, run := range []struct {
		name string
		opts *interfaces.LiveTranscriptionOptions
	}{{"without Whisper", &without}, {"with Whisper", with}} {
		type wake struct {
			Chunk   int           `json:"chunk"`
			Results []probeResult `json:"results"`
		}
		var (
			mu      sync.Mutex
			wakes   []wake
			finals  int
			mention int
			wg      sync.WaitGroup
			errs    = make(chan error, len(chunks))
			sem     = make(chan struct{}, 24)
		)
		for c := range chunks {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				heard, err := probeTranscribe(ctx, key, run.opts, chunks[c])
				if err != nil {
					errs <- fmt.Errorf("chunk %d: %w", c, err)
					return
				}
				var stretch []probeResult
				woke := false
				mu.Lock()
				defer mu.Unlock()
				for _, h := range heard {
					stretch = append(stretch, h)
					if _, ok := detector.Find(h.Text); ok {
						woke = true
					}
					if !h.Final {
						continue
					}
					finals++
					if mentions.MatchString(h.Text) {
						mention++
					}
					if woke {
						wakes = append(wakes, wake{Chunk: c, Results: stretch})
					}
					stretch, woke = nil, false
				}
				if woke {
					wakes = append(wakes, wake{Chunk: c, Results: stretch})
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
		hours := seconds / 3600
		upper := poissonUpper95(len(wakes)) / hours
		summary[run.name] = map[string]any{
			"keyterms": run.opts.Keyterm, "false_wakes": len(wakes), "per_hour": float64(len(wakes)) / hours,
			"per_hour_95_upper": upper, "finals": finals, "finals_with_whisper_or_mister": mention,
		}
		t.Logf("%s: %d false wakes in %.2f h: %.2f per hour, 95%% upper bound %.2f per hour; %d of %d finals say whisper or mister", run.name, len(wakes), hours, float64(len(wakes))/hours, upper, mention, finals)
		raw, _ := json.MarshalIndent(wakes, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "wakes-"+strings.ReplaceAll(run.name, " ", "-")+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "falserate-summary.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// poissonUpper95 is the one-sided 95% upper bound of a Poisson mean from k
// events: the mean at which k or fewer events come with probability 0.05.
func poissonUpper95(k int) float64 {
	cdf := func(mean float64) float64 {
		sum, term := 0.0, math.Exp(-mean)
		for i := 0; i <= k; i++ {
			if i > 0 {
				term *= mean / float64(i)
			}
			sum += term
		}
		return sum
	}
	lo, hi := 0.0, float64(k)+20
	for range 200 {
		mid := (lo + hi) / 2
		if cdf(mid) > 0.05 {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi
}

// TestPoissonUpper95 fails if the bound is not the known one: 3.00 for no
// events, 4.74 for one.
func TestPoissonUpper95(t *testing.T) {
	for k, want := range map[int]float64{0: 2.9957, 1: 4.7439, 5: 10.5130} {
		if got := poissonUpper95(k); math.Abs(got-want) > 0.001 {
			t.Errorf("poissonUpper95(%d) = %.4f, want %.4f", k, got, want)
		}
	}
}

// probeMeeting returns AMI meeting m as synthetic speech, 16 kHz PCM16: its
// utterances in order, each speaker in its own Aura-2 voice, 300 ms apart,
// with one of the probe's own sentences every probeInsertEvery, and how many
// of those it holds. It keeps the speech in dir, and speaks only what it has
// not kept.
func probeMeeting(ctx context.Context, key, words, dir, m string, index int) ([]byte, int, error) {
	path := filepath.Join(dir, "corpus", m+".pcm")
	countPath := path + ".inserted"
	if pcm, err := os.ReadFile(path); err == nil {
		raw, _ := os.ReadFile(countPath)
		n, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
		return pcm, n, nil
	}
	utterances, err := amiUtterances(words, m)
	if err != nil {
		return nil, 0, err
	}
	voices := map[string]string{}
	type line struct{ voice, text string }
	var lines []line
	spoken, next, inserted := 0.0, probeInsertEvery.Seconds(), 0
	for _, u := range utterances {
		v, ok := voices[u.speaker]
		if !ok {
			v = probeVoices[(index*4+len(voices))%len(probeVoices)]
			voices[u.speaker] = v
		}
		if spoken >= next {
			lines = append(lines, line{v, probeNonCalls[(index*7+inserted)%len(probeNonCalls)]})
			inserted++
			next += probeInsertEvery.Seconds()
		}
		lines = append(lines, line{v, u.text})
		spoken += float64(len(strings.Fields(u.text))) / 2.5 // about 150 words a minute
	}
	pcms := make([][]byte, len(lines))
	var wg sync.WaitGroup
	errs := make(chan error, len(lines))
	sem := make(chan struct{}, 8)
	for i, l := range lines {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			sum := sha256.Sum256([]byte(l.voice + "\x00" + l.text))
			cache := filepath.Join(dir, "tts", hex.EncodeToString(sum[:16])+".pcm")
			if pcm, err := os.ReadFile(cache); err == nil {
				pcms[i] = pcm
				return
			}
			var pcm []byte
			var err error
			for attempt := range 4 {
				if pcm, err = probeSpeak(ctx, key, l.voice, l.text); err == nil {
					break
				}
				time.Sleep(time.Duration(attempt+1) * time.Second)
			}
			if err != nil {
				errs <- fmt.Errorf("Aura %s, %d chars: %w", l.voice, len(l.text), err)
				return
			}
			pcms[i] = pcm
			_ = os.WriteFile(cache, pcm, 0o600)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return nil, 0, err
	}
	gap := make([]byte, 16000*2*3/10)
	var out bytes.Buffer
	for _, pcm := range pcms {
		out.Write(pcm)
		out.Write(gap)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		return nil, 0, err
	}
	_ = os.WriteFile(countPath, []byte(strconv.Itoa(inserted)), 0o600)
	return out.Bytes(), inserted, nil
}

// amiUtterances reads AMI meeting m's words, every speaker's, from the words
// folder, as utterances in the order they start: what one speaker says with
// no pause of a second or more, cut after a sentence's end past 30 words.
func amiUtterances(dir, m string) ([]amiUtterance, error) {
	files, err := filepath.Glob(filepath.Join(dir, m+".*.words.xml"))
	if err != nil || len(files) == 0 {
		return nil, fmt.Errorf("no words for %s in %s", m, dir)
	}
	var all []amiUtterance
	for _, f := range files {
		speaker := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), m+"."), ".words.xml")
		ws, err := amiWords(f)
		if err != nil {
			return nil, err
		}
		var cur []amiWord
		last := -1.0
		flush := func() {
			var b strings.Builder
			for _, w := range cur {
				if !w.punc && b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(w.text)
			}
			if text := strings.TrimSpace(b.String()); len(strings.Fields(text)) > 0 {
				all = append(all, amiUtterance{start: cur[0].start, speaker: speaker, text: text})
			}
			cur = nil
		}
		for _, w := range ws {
			if len(cur) > 0 && !w.punc && w.start-last >= 1.0 {
				flush()
			}
			cur = append(cur, w)
			if !w.punc {
				last = w.start
			}
			if w.punc && (w.text == "." || w.text == "?") && len(cur) > 30 {
				flush()
			}
		}
		if len(cur) > 0 {
			flush()
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].start < all[j].start })
	return all, nil
}

// amiWords reads the words and punctuation of one AMI words file, which is
// ISO-8859-1.
func amiWords(path string) ([]amiWord, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) {
		b, err := io.ReadAll(in)
		if err != nil {
			return nil, err
		}
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		return strings.NewReader(string(r)), nil
	}
	var out []amiWord
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "w" {
			continue
		}
		var w amiWord
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "starttime":
				w.start, _ = strconv.ParseFloat(a.Value, 64)
			case "punc":
				w.punc = a.Value == "true"
			}
		}
		var text string
		if err := dec.DecodeElement(&text, &se); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if w.text = strings.TrimSpace(text); w.text != "" {
			out = append(out, w)
		}
	}
}
