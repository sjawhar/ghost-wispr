package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/erase"
	"github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

const (
	// defaultSampleRate is the rate the recorder takes before the microphone
	// opens.
	defaultSampleRate = 16000
	pcmChannels       = 1
	pcmBitDepth       = 16
	// prerollSeconds is how much audio from before a session's start, or from
	// before the first words Deepgram heard for it (HoldPreroll), leads its
	// recording.
	prerollSeconds = 5
	// maxHeldSeconds bounds the audio a hold (HoldPreroll) keeps for the next
	// session's start: far longer than any stretch of speech without the end
	// of speech or the utterance end that starts a session.
	maxHeldSeconds = 300
)

// pcmBytesPerSecond is the byte rate of the recorder's PCM at sampleRate.
func pcmBytesPerSecond(sampleRate int) int {
	return sampleRate * pcmChannels * pcmBitDepth / 8
}

// pcmBytes is the size of d of the recorder's PCM at sampleRate, in whole
// samples. It splits d at the second, so a long d does not overflow it.
func pcmBytes(d time.Duration, sampleRate int) int {
	rate := int64(sampleRate)
	samples := int64(d/time.Second)*rate + int64(d%time.Second)*rate/int64(time.Second)
	return int(samples) * pcmChannels * pcmBitDepth / 8
}

// pcmDuration is how long n bytes of the recorder's PCM at sampleRate last.
func pcmDuration(n int, sampleRate int) time.Duration {
	perSecond := pcmBytesPerSecond(sampleRate)
	return time.Duration(n/perSecond)*time.Second + time.Duration(n%perSecond)*time.Second/time.Duration(perSecond)
}

// encodeTimeout is how long the MP3 encoders (ffmpeg, then lame) together may
// take for a recording of pcmBytes at sampleRate before the recording is kept
// as WAV instead: a minute plus a tenth of the recording's length, far slower
// than either encoder runs. A wedged encoder then cannot hold a session end,
// or a capture pause waiting on it, open for good.
var encodeTimeout = func(pcmBytes int64, sampleRate int) time.Duration {
	bytesPerSecond := int64(pcmBytesPerSecond(sampleRate))
	length := time.Duration(pcmBytes/bytesPerSecond) * time.Second
	return time.Minute + length/10
}

type Recorder struct {
	audioDir string

	mu         sync.Mutex
	sessionID  string
	rawPath    string
	rawFile    *os.File
	sampleRate int
	// history keeps the audio taken while no recording is open: the last
	// prerollSeconds plus lookback of it. The last prerollSeconds lead the
	// next recording, or, during a hold, as much as the hold asks.
	history  *transcribe.RingBuf
	lookback time.Duration
	// holding is set while words Deepgram heard may still open a session
	// (HoldPreroll). history then stops taking audio, and the audio taken goes
	// to held instead; the last holdBack bytes of history and all of held lead
	// the next recording. holdBack never exceeds history.Len(): whatever
	// shrinks history during a hold (SetLookback) shrinks holdBack with it.
	holding  bool
	holdBack int
	held     []byte
	// trimWarned is set once the hold under way has dropped its oldest
	// audio past maxHeldSeconds and said so in the log.
	trimWarned bool
	// sinceClose counts the bytes taken since the last recording closed
	// (SinceClose).
	sinceClose int
	// sessionRate is the rate the recording under way is captured at, as
	// StartSession recorded it.
	sessionRate int
	// unsavedID and unsavedPath name a recording EndSession closed but could
	// not save, captured at unsavedRate; EndSession for that session saves it,
	// unless DiscardUnsaved gave up on it first.
	unsavedID   string
	unsavedPath string
	unsavedRate int
	// savingPath is the raw recording EndSession is saving now.
	savingPath string
	// rates keeps the rate of each session's raw recording, which the raw
	// file does not (UseSessionRates); nil keeps none.
	rates SessionRates

	// encode saves the raw recording at rawPath, captured at sampleRate,
	// under name (without its extension) in the audio directory and returns
	// the saved file (defaultEncode).
	encode func(ctx context.Context, rawPath, name string, sampleRate int) (string, error)
	// createRaw creates a session's raw recording, refusing one that exists
	// (createRawFile).
	createRaw func(path string) (*os.File, error)
}

func NewRecorder(audioDir string) *Recorder {
	if audioDir == "" {
		audioDir = filepath.Join("data", "audio")
	}

	r := &Recorder{
		audioDir:   audioDir,
		sampleRate: defaultSampleRate,
	}
	r.history = r.newHistoryLocked()
	r.encode = r.defaultEncode
	r.createRaw = createRawFile
	return r
}

// newHistoryLocked returns an empty history for the recorder's rate and
// lookback. It must be called with r.mu held, or before r is shared.
func (r *Recorder) newHistoryLocked() *transcribe.RingBuf {
	return transcribe.NewRingBuf(prerollSeconds*pcmBytesPerSecond(r.sampleRate) + pcmBytes(r.lookback, r.sampleRate))
}

// SetLookback sets how far behind the microphone the words that open a
// session can be heard: the most audio live transcription holds unsent while
// Deepgram is not connected. The recorder then keeps that much audio from
// before a session besides the preroll, so a hold (HoldPreroll) can reach
// back to the first words Deepgram hears late. The audio kept stays.
func (r *Recorder) SetLookback(lookback time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	lookback = max(lookback, 0)
	if lookback == r.lookback {
		return
	}
	r.lookback = lookback
	kept := make([]byte, r.history.Len())
	_, _ = r.history.Read(kept)
	r.history = r.newHistoryLocked()
	_, _ = r.history.Write(kept)
	r.holdBack = min(r.holdBack, r.history.Len())
}

// SetSampleRate sets the rate the microphone opened at, which the audio the
// recorder takes from now on has. The history then holds prerollSeconds and
// the lookback at that rate, and drops audio it held at another rate, with
// any audio a hold kept: that cannot lead a recording at this one. A
// recording already open takes the new rate too, and with session rates kept
// (UseSessionRates) its session's row does: a session can start before the
// microphone first opens (a manual start during the start-up retry, or while
// the light is not confirmed red), and the rate changes only before audio
// first flows, so all of its audio is at the new rate. It returns the error
// of that row's write; the recording itself still saves at the new rate when
// its session ends.
func (r *Recorder) SetSampleRate(sampleRate int) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sampleRate <= 0 || sampleRate == r.sampleRate {
		return nil
	}
	r.sampleRate = sampleRate
	r.history = r.newHistoryLocked()
	r.holding, r.holdBack, r.held = false, 0, nil
	if r.rawFile == nil {
		return nil
	}
	r.sessionRate = sampleRate
	// The row is written outside the lock, as StartSession writes it, and
	// again while the recording's rate changed meanwhile.
	for id := r.sessionID; r.rates != nil; {
		rates, rate := r.rates, r.sessionRate
		r.mu.Unlock()
		err := rates.SetSessionSampleRate(id, rate)
		r.mu.Lock()
		if err != nil {
			return fmt.Errorf("record the sample rate of session %s: %w", id, err)
		}
		if r.sessionID != id || r.sessionRate == rate {
			return nil
		}
	}
	return nil
}

// SessionRates keeps the sample rate each session's raw recording is captured
// at, which a raw .pcm does not store (storage.SQLiteStore). A session from a
// build that kept none reads as 0, and its raw recording is taken as captured
// at the rate the recorder has now (SaveKept).
type SessionRates interface {
	SetSessionSampleRate(id string, rate int) error
	SessionSampleRate(id string) (int, error)
}

// UseSessionRates makes the recorder record the rate of each session it
// starts in rates, and encode each kept raw recording (SaveKept) at the rate
// rates has for its session.
func (r *Recorder) UseSessionRates(rates SessionRates) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rates = rates
}

func (r *Recorder) Writer(dst io.Writer) io.Writer {
	return &teeWriter{recorder: r, dst: dst}
}

// createRawFile creates the raw recording at path, and refuses, with an
// error that wraps os.ErrExist, a file that is already there.
func createRawFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

// StartSession starts recording session sessionID into its raw recording. It
// never reuses the id of a recording on disk, raw or saved as MP3 or WAV:
// that is another session's audio (a clock set back can name a new session
// as an earlier one), so it refuses with an error that wraps os.ErrExist,
// leaving the file as it is and starting nothing. With session rates kept
// (UseSessionRates), it first records the rate the recording is captured at,
// and starts nothing if it cannot: a raw recording whose rate is not kept
// would be recovered at the wrong one.
func (r *Recorder) StartSession(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rate := r.sampleRate
	for r.rates != nil {
		// The lock is released for the write, which can wait on the
		// database, and the rate read again after it: the microphone can
		// open at another meanwhile.
		rates := r.rates
		r.mu.Unlock()
		err := rates.SetSessionSampleRate(sessionID, rate)
		r.mu.Lock()
		if err != nil {
			return fmt.Errorf("record the sample rate of session %s: %w", sessionID, err)
		}
		if r.sampleRate == rate {
			break
		}
		rate = r.sampleRate
	}

	if err := os.MkdirAll(r.audioDir, 0o755); err != nil {
		return fmt.Errorf("create audio directory: %w", err)
	}

	for _, ext := range []string{".mp3", ".wav"} {
		saved := filepath.Join(r.audioDir, sessionID+ext)
		if _, err := os.Lstat(saved); err == nil {
			return fmt.Errorf("the recording of session %s is on disk (%s): %w", sessionID, saved, os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check for a recording of session %s: %w", sessionID, err)
		}
	}

	if r.rawFile != nil {
		_ = r.rawFile.Close()
		r.sessionID, r.rawPath, r.rawFile = "", "", nil
	}

	rawPath := recording.RawPath(r.audioDir, sessionID)
	rawFile, err := r.createRaw(rawPath)
	if err != nil {
		return fmt.Errorf("open raw pcm file: %w", err)
	}

	r.sessionID = sessionID
	r.rawPath = rawPath
	r.rawFile = rawFile
	r.sessionRate = rate

	// The audio buffered for the session's start leads its recording.
	before, held := r.takeLeadLocked()
	for _, lead := range [][]byte{before, held} {
		if _, err := r.rawFile.Write(lead); err != nil {
			// Leave no session open: the caller rolls the session back, and
			// later audio must not be appended to its file.
			_ = r.rawFile.Close()
			r.sessionID, r.rawPath, r.rawFile = "", "", nil
			return fmt.Errorf("flush preroll buffer: %w", err)
		}
	}

	return nil
}

// takeLeadLocked returns the audio buffered for the next session's start,
// oldest first, and empties the buffer: during a hold (HoldPreroll), the last
// holdBack bytes of history and all the audio held; else the last
// prerollSeconds of history. It must be called with r.mu held.
func (r *Recorder) takeLeadLocked() (before, held []byte) {
	n := min(prerollSeconds*pcmBytesPerSecond(r.sampleRate), r.history.Len())
	if r.holding {
		n = r.holdBack
	}
	r.history.Discard(r.history.Len() - n)
	before = make([]byte, n)
	_, _ = r.history.Read(before) // all that is left: history is empty after
	held = r.held
	r.holding, r.holdBack, r.held = false, 0, nil
	return before, held
}

// EndSession closes session sessionID's recording and saves it, returning the
// saved file. If that session's recording was closed earlier but could not be
// saved, it tries to save it again, so a session end that is retried keeps its
// recording. For any other session it returns nothing: it never saves or
// returns another session's recording.
func (r *Recorder) EndSession(sessionID string) (string, error) {
	r.mu.Lock()
	if sessionID == "" || sessionID != r.sessionID || r.rawFile == nil {
		retry := sessionID != "" && sessionID == r.unsavedID
		rawPath, rate := r.unsavedPath, r.unsavedRate
		r.mu.Unlock()
		if !retry {
			return "", nil
		}
		return r.save(sessionID, rawPath, rate)
	}

	rawPath, rawFile, rate := r.rawPath, r.rawFile, r.sessionRate
	r.sessionID = ""
	r.rawPath = ""
	r.rawFile = nil
	r.savingPath = rawPath
	r.sinceClose = 0
	r.mu.Unlock()

	if err := rawFile.Close(); err != nil {
		r.mu.Lock()
		r.unsavedID, r.unsavedPath, r.unsavedRate = sessionID, rawPath, rate
		r.savingPath = ""
		r.mu.Unlock()
		return "", fmt.Errorf("close raw pcm file: %w", err)
	}
	return r.save(sessionID, rawPath, rate)
}

// save encodes the closed recording of sessionID, captured at sampleRate, and
// removes its raw file. If it cannot, the recording is kept for that
// session's next EndSession to save.
func (r *Recorder) save(sessionID, rawPath string, sampleRate int) (string, error) {
	audioPath, err := r.encode(context.Background(), rawPath, sessionID, sampleRate)
	r.mu.Lock()
	if r.savingPath == rawPath {
		r.savingPath = ""
	}
	if err != nil {
		r.unsavedID, r.unsavedPath, r.unsavedRate = sessionID, rawPath, sampleRate
	} else if r.unsavedID == sessionID {
		r.unsavedID, r.unsavedPath, r.unsavedRate = "", "", 0
	}
	r.mu.Unlock()
	if err != nil {
		return "", err
	}

	// Removing a whole audio file overwrites its bytes before the file system
	// frees them (Sami, 2026-11-12, "Zero every audio removal",
	// dispatch://INFRA-2961/ask/172349fa-0019-488c-9d3b-9d6b7af26a81).
	_ = erase.Remove(rawPath)
	return audioPath, nil
}

func (r *Recorder) writePCM(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rawFile == nil {
		r.sinceClose += len(data)
		if !r.holding {
			_, _ = r.history.Write(data)
			return nil
		}
		r.held = append(r.held, data...)
		// Past maxHeldSeconds, the oldest audio goes: first what history
		// keeps for the hold, then what the hold took.
		if over := r.holdBack + len(r.held) - maxHeldSeconds*pcmBytesPerSecond(r.sampleRate); over > 0 {
			back := min(over, r.holdBack)
			r.holdBack -= back
			r.held = r.held[over-back:]
			if !r.trimWarned {
				r.trimWarned = true
				log.Printf("warning: the audio kept for the next session's start reached %d s with no session started; its oldest audio is dropped from now on, and with it maybe the first words", maxHeldSeconds)
			}
		}
		return nil
	}

	if _, err := r.rawFile.Write(data); err != nil {
		return fmt.Errorf("write raw pcm bytes: %w", err)
	}
	return nil
}

// KeptRawSessions returns the sessions whose raw recordings are in the audio
// directory and that the recorder is not using: not the recording under way,
// not one being saved, and not one waiting for its retry. A raw recording
// left beside its saved recording is one of them.
func (r *Recorder) KeptRawSessions() []string {
	entries, err := os.ReadDir(r.audioDir)
	if err != nil {
		return nil
	}
	// Read after listing, as KeptRawRecordings does.
	r.mu.Lock()
	live, saving, unsaved := r.rawPath, r.savingPath, r.unsavedPath
	r.mu.Unlock()

	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || filepath.Ext(name) != ".pcm" || strings.HasPrefix(name, ".") {
			continue
		}
		if path := filepath.Join(r.audioDir, name); path == live || path == saving || path == unsaved {
			continue
		}
		ids = append(ids, strings.TrimSuffix(name, ".pcm"))
	}
	return ids
}

// SaveKept saves the raw recording kept in the audio directory for session
// sessionID as EndSession saves a session's recording (MP3, else WAV), and
// returns the saved file. It encodes it at the rate the session rates have
// for it (UseSessionRates). A session from a build that kept no rate has
// none, and its raw recording is encoded at the rate the recorder has now,
// the rate this run's microphone opened at (else defaultSampleRate), as
// those builds encoded it. It saves it under a temporary
// name (recording.KeptTempPaths), never the session's own, and keeps the raw
// file: its caller renames the saved file into place only as the session
// takes it, and erases the raw file once the saved recording is safe. So a
// session deleted while this runs, then a stop, leaves no <id>.mp3 or
// <id>.wav of it. It refuses a raw recording the recorder is using: the
// recording under way, one being saved, or one waiting for its retry. Once
// ctx is cancelled it saves nothing: the encoder is killed.
func (r *Recorder) SaveKept(ctx context.Context, sessionID string) (string, error) {
	rawPath := recording.RawPath(r.audioDir, sessionID)
	r.mu.Lock()
	inUse := rawPath == r.rawPath || rawPath == r.savingPath || rawPath == r.unsavedPath
	rates, rate := r.rates, r.sampleRate
	r.mu.Unlock()
	if inUse {
		return "", fmt.Errorf("raw recording %s is in use by the recorder", rawPath)
	}
	info, err := os.Stat(rawPath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("raw recording %s is not a regular file", rawPath)
	}
	if rates != nil {
		recorded, err := rates.SessionSampleRate(sessionID)
		if err != nil {
			return "", fmt.Errorf("read the sample rate of the raw recording %s: %w", rawPath, err)
		}
		if recorded > 0 {
			rate = recorded
		}
	}
	return r.encode(ctx, rawPath, recording.KeptTempName(sessionID), rate)
}

// DiscardUnsaved gives up on a recording EndSession closed but could not save:
// later EndSession calls no longer try to save it. Its raw audio stays on
// disk, a kept raw recording that SaveKept can save later; DiscardUnsaved
// returns its path, or "" if it is gone.
func (r *Recorder) DiscardUnsaved() string {
	r.mu.Lock()
	rawPath := r.unsavedPath
	r.unsavedID, r.unsavedPath, r.unsavedRate = "", "", 0
	r.mu.Unlock()
	if rawPath == "" {
		return ""
	}
	if _, err := os.Stat(rawPath); err != nil {
		return ""
	}
	return rawPath
}

// DiscardPreroll drops the audio buffered for the next session's start, the
// history and any hold's audio, and ends the hold. A capture resume calls it,
// so a session after the pause does not open with audio from before it.
func (r *Recorder) DiscardPreroll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.history.Reset()
	r.holding, r.holdBack, r.held = false, 0, nil
}

// HoldPreroll makes the next session's recording start prerollSeconds before
// the audio taken ago, or at the oldest audio history keeps: it keeps that
// audio, and all the audio taken from now on, until that session starts
// (StartSession), a ReleasePreroll, or a DiscardPreroll; up to
// maxHeldSeconds in all. Words Deepgram hears open a session only at the end
// of their speech, and it can hear them late (SetLookback), so the session
// manager calls it at each result with words while no session is open, ago
// being how long before now their first word was captured: the recording
// then holds them from the first, however long they run before the session
// starts. A later call that reaches further back moves the start back. While
// a recording is open it does nothing.
func (r *Recorder) HoldPreroll(ago time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rawFile != nil {
		return
	}
	if !r.holding {
		r.holding, r.holdBack, r.held, r.trimWarned = true, 0, nil, false
	}
	reach := pcmBytes(max(ago, 0), r.sampleRate) + prerollSeconds*pcmBytesPerSecond(r.sampleRate)
	// No further back than maxHeldSeconds: a hold at its bound has dropped
	// its oldest audio (writePCM), and history no longer joins what it holds.
	bound := maxHeldSeconds*pcmBytesPerSecond(r.sampleRate) - len(r.held)
	r.holdBack = max(r.holdBack, min(reach-len(r.held), r.history.Len(), bound))
}

// ReleasePreroll ends a hold (HoldPreroll) whose words opened no session,
// because Deepgram took them back: history takes the audio the hold took, as
// if there had been no hold.
func (r *Recorder) ReleasePreroll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.holding {
		return
	}
	_, _ = r.history.Write(r.held)
	r.holding, r.holdBack, r.held = false, 0, nil
}

// Pending returns how much audio the next session's recording would open
// with if it started now: the last prerollSeconds, or all that a hold keeps
// (HoldPreroll). While a recording is open it returns 0. The session manager
// starts a session at the time its recording starts: now minus Pending.
func (r *Recorder) Pending() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rawFile != nil {
		return 0
	}
	n := min(prerollSeconds*pcmBytesPerSecond(r.sampleRate), r.history.Len())
	if r.holding {
		n = r.holdBack + len(r.held)
	}
	return pcmDuration(n, r.sampleRate)
}

// SinceClose returns how much audio has been taken since the last recording
// closed, and true; while a recording is open it returns false. Words
// Deepgram hears while a session ends, captured longer ago than that, are in
// that session's recording; the rest are not, and open the next session.
func (r *Recorder) SinceClose() (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rawFile != nil {
		return 0, false
	}
	return pcmDuration(r.sinceClose, r.sampleRate), true
}

// defaultEncode saves the raw recording at rawPath, captured at sampleRate,
// as <name>.mp3 in the audio directory, else, once the MP3 encoders failed or
// ran out of time, as <name>.wav, and returns the saved file. A failed encoder
// or WAV write leaves no partial file. Once ctx is cancelled it saves nothing
// more: no WAV fallback runs after a cancelled encoder.
func (r *Recorder) defaultEncode(ctx context.Context, rawPath, name string, sampleRate int) (string, error) {
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}

	mp3Path := filepath.Join(r.audioDir, name+".mp3")

	var pcmBytes int64
	if info, err := os.Stat(rawPath); err == nil {
		pcmBytes = info.Size()
	}
	encodeCtx, cancel := context.WithTimeout(ctx, encodeTimeout(pcmBytes, sampleRate))
	defer cancel()

	if err := encodeWithFFmpeg(encodeCtx, rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}

	if err := encodeWithLame(encodeCtx, rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}
	_ = erase.Remove(mp3Path) // a partial file from an encoder that failed or ran out of time
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("save %s: %w", rawPath, err)
	}

	wavPath := filepath.Join(r.audioDir, name+".wav")
	if err := pcmToWav(rawPath, wavPath, sampleRate); err != nil {
		_ = erase.Remove(wavPath) // a partial file the failed write left
		return "", fmt.Errorf("encode wav fallback: %w", err)
	}

	return wavPath, nil
}

func encodeWithFFmpeg(ctx context.Context, rawPath, outputPath string, sampleRate int) error {
	cmd := exec.CommandContext(
		ctx,
		"ffmpeg",
		"-y",
		"-f", "s16le",
		"-ar", strconv.Itoa(sampleRate),
		"-ac", "1",
		"-i", rawPath,
		outputPath,
	)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func encodeWithLame(ctx context.Context, rawPath, outputPath string, sampleRate int) error {
	khz := float64(sampleRate) / 1000.0
	formatted := strconv.FormatFloat(khz, 'f', -1, 64)
	cmd := exec.CommandContext(
		ctx,
		"lame",
		"-r",
		"-s", formatted,
		"--bitwidth", "16",
		"-m", "m",
		rawPath,
		outputPath,
	)
	if err := cmd.Run(); err != nil {
		return err
	}
	return nil
}

func pcmToWav(rawPath, wavPath string, sampleRate int) error {
	pcmData, err := os.ReadFile(rawPath)
	if err != nil {
		return fmt.Errorf("read raw pcm data: %w", err)
	}

	out, err := os.OpenFile(wavPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open wav output: %w", err)
	}
	defer func() { _ = out.Close() }()

	header, err := wavHeader(len(pcmData), sampleRate, pcmChannels, pcmBitDepth)
	if err != nil {
		return fmt.Errorf("build wav header: %w", err)
	}

	if _, err := out.Write(header); err != nil {
		return fmt.Errorf("write wav header: %w", err)
	}
	if _, err := out.Write(pcmData); err != nil {
		return fmt.Errorf("write wav payload: %w", err)
	}

	return nil
}

func wavHeader(dataSize, sampleRate, channels, bitDepth int) ([]byte, error) {
	byteRate := sampleRate * channels * bitDepth / 8
	blockAlign := channels * bitDepth / 8
	chunkSize := 36 + dataSize

	buf := bytes.NewBuffer(make([]byte, 0, 44))
	if _, err := buf.WriteString("RIFF"); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(chunkSize)); err != nil {
		return nil, err
	}
	if _, err := buf.WriteString("WAVE"); err != nil {
		return nil, err
	}
	if _, err := buf.WriteString("fmt "); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(16)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(1)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(channels)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(sampleRate)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(byteRate)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(blockAlign)); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(bitDepth)); err != nil {
		return nil, err
	}
	if _, err := buf.WriteString("data"); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint32(dataSize)); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

type teeWriter struct {
	recorder *Recorder
	dst      io.Writer
}

// Write records p, then hands it to dst, live transcription: the recording
// never waits on it.
func (w *teeWriter) Write(p []byte) (int, error) {
	if err := w.recorder.writePCM(p); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
}
