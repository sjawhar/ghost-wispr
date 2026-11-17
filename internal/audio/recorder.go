package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	defaultSampleRate = 16000
	pcmChannels       = 1
	pcmBitDepth       = 16
)

// encodeTimeout is how long the MP3 encoders (ffmpeg, then lame) together may
// take for a recording of pcmBytes at sampleRate before the recording is kept
// as WAV instead: a minute plus a tenth of the recording's length, far slower
// than either encoder runs. A wedged encoder then cannot hold a session end,
// or a capture pause waiting on it, open for good.
var encodeTimeout = func(pcmBytes int64, sampleRate int) time.Duration {
	bytesPerSecond := int64(sampleRate * pcmChannels * pcmBitDepth / 8)
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
	preroll    *transcribe.RingBuf
	// unsavedID and unsavedPath name a recording EndSession closed but could
	// not save; EndSession for that session saves it, unless DiscardUnsaved
	// gave up on it first.
	unsavedID   string
	unsavedPath string
	// savingPath is the raw recording EndSession is saving now.
	savingPath string

	// encode saves the raw recording at rawPath under name (without its
	// extension) in the audio directory and returns the saved file
	// (defaultEncode).
	encode func(ctx context.Context, rawPath, name string) (string, error)
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
		preroll:    transcribe.NewRingBuf(5 * defaultSampleRate * pcmBitDepth / 8 * pcmChannels),
	}
	r.encode = r.defaultEncode
	r.createRaw = createRawFile
	return r
}

func (r *Recorder) SetSampleRate(sampleRate int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if sampleRate > 0 {
		r.sampleRate = sampleRate
	}
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
// leaving the file as it is and starting nothing.
func (r *Recorder) StartSession(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

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

	// Flush preroll buffer into the new file
	if n := r.preroll.Len(); n > 0 {
		buf := make([]byte, n)
		_, _ = r.preroll.Read(buf)
		r.preroll.Reset()
		if _, err := r.rawFile.Write(buf); err != nil {
			// Leave no session open: the caller rolls the session back, and
			// later audio must not be appended to its file.
			_ = r.rawFile.Close()
			r.sessionID, r.rawPath, r.rawFile = "", "", nil
			return fmt.Errorf("flush preroll buffer: %w", err)
		}
	}

	return nil
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
		rawPath := r.unsavedPath
		r.mu.Unlock()
		if !retry {
			return "", nil
		}
		return r.save(sessionID, rawPath)
	}

	rawPath, rawFile := r.rawPath, r.rawFile
	r.sessionID = ""
	r.rawPath = ""
	r.rawFile = nil
	r.savingPath = rawPath
	r.mu.Unlock()

	if err := rawFile.Close(); err != nil {
		r.mu.Lock()
		r.unsavedID, r.unsavedPath = sessionID, rawPath
		r.savingPath = ""
		r.mu.Unlock()
		return "", fmt.Errorf("close raw pcm file: %w", err)
	}
	return r.save(sessionID, rawPath)
}

// save encodes the closed recording of sessionID and removes its raw file. If
// it cannot, the recording is kept for that session's next EndSession to save.
func (r *Recorder) save(sessionID, rawPath string) (string, error) {
	audioPath, err := r.encode(context.Background(), rawPath, sessionID)
	r.mu.Lock()
	if r.savingPath == rawPath {
		r.savingPath = ""
	}
	if err != nil {
		r.unsavedID, r.unsavedPath = sessionID, rawPath
	} else if r.unsavedID == sessionID {
		r.unsavedID, r.unsavedPath = "", ""
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
		_, _ = r.preroll.Write(data)
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
// returns the saved file. It saves it under a temporary name
// (recording.KeptTempPaths), never the session's own, and keeps the raw
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
	return r.encode(ctx, rawPath, recording.KeptTempName(sessionID))
}

// DiscardUnsaved gives up on a recording EndSession closed but could not save:
// later EndSession calls no longer try to save it. Its raw audio stays on
// disk, a kept raw recording that SaveKept can save later; DiscardUnsaved
// returns its path, or "" if it is gone.
func (r *Recorder) DiscardUnsaved() string {
	r.mu.Lock()
	rawPath := r.unsavedPath
	r.unsavedID, r.unsavedPath = "", ""
	r.mu.Unlock()
	if rawPath == "" {
		return ""
	}
	if _, err := os.Stat(rawPath); err != nil {
		return ""
	}
	return rawPath
}

// DiscardPreroll drops the audio buffered for the next session's start. A
// capture resume calls it, so a session after the pause does not open with
// audio from before it.
func (r *Recorder) DiscardPreroll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.preroll.Reset()
}

// defaultEncode saves the raw recording at rawPath as <name>.mp3 in the audio
// directory, else, once the MP3 encoders failed or ran out of time, as
// <name>.wav, and returns the saved file. A failed encoder or WAV write leaves
// no partial file. Once ctx is cancelled it saves nothing more: no WAV
// fallback runs after a cancelled encoder.
func (r *Recorder) defaultEncode(ctx context.Context, rawPath, name string) (string, error) {
	r.mu.Lock()
	sampleRate := r.sampleRate
	r.mu.Unlock()
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
