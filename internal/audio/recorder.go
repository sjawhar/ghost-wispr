package audio

import (
	"bytes"
	"context"
	"encoding/binary"
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

	encode func(rawPath, sessionID string) (string, error)
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

func (r *Recorder) StartSession(sessionID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := os.MkdirAll(r.audioDir, 0o755); err != nil {
		return fmt.Errorf("create audio directory: %w", err)
	}

	if r.rawFile != nil {
		_ = r.rawFile.Close()
	}

	rawPath := filepath.Join(r.audioDir, sessionID+".pcm")
	rawFile, err := os.OpenFile(rawPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
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
	audioPath, err := r.encode(rawPath, sessionID)
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

	// The raw recording holds every word of the session, a segment deleted
	// later included: its bytes are overwritten before the file system frees
	// them.
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

// KeptRawRecordings counts the raw recordings kept in the audio directory
// because they could not be saved, and their total size: every .pcm there
// except the recording under way, one being saved or waiting for its retry,
// and one left beside its saved recording (<id>.mp3 or <id>.wav). Nothing
// removes them; they are for recovery or deletion by hand.
func (r *Recorder) KeptRawRecordings() (count int, size int64) {
	entries, err := os.ReadDir(r.audioDir)
	if err != nil {
		return 0, 0
	}
	// Read after listing, so a session that started or ended meanwhile is
	// still recognised.
	r.mu.Lock()
	live, saving, unsaved := r.rawPath, r.savingPath, r.unsavedPath
	r.mu.Unlock()

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".pcm" {
			continue
		}
		path := filepath.Join(r.audioDir, name)
		if path == live || path == saving || path == unsaved {
			continue
		}
		id := strings.TrimSuffix(name, ".pcm")
		if isFile(filepath.Join(r.audioDir, id+".mp3")) || isFile(filepath.Join(r.audioDir, id+".wav")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		count++
		size += info.Size()
	}
	return count, size
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// DiscardUnsaved gives up on a recording EndSession closed but could not save:
// later EndSession calls no longer try to save it. Its raw audio stays on disk
// for recovery by hand; DiscardUnsaved returns its path, or "" if it is gone.
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

func (r *Recorder) defaultEncode(rawPath, sessionID string) (string, error) {
	r.mu.Lock()
	sampleRate := r.sampleRate
	r.mu.Unlock()
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}

	mp3Path := filepath.Join(r.audioDir, sessionID+".mp3")

	var pcmBytes int64
	if info, err := os.Stat(rawPath); err == nil {
		pcmBytes = info.Size()
	}
	ctx, cancel := context.WithTimeout(context.Background(), encodeTimeout(pcmBytes, sampleRate))
	defer cancel()

	if err := encodeWithFFmpeg(ctx, rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}

	if err := encodeWithLame(ctx, rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}
	_ = erase.Remove(mp3Path) // a partial file from an encoder that failed or ran out of time

	wavPath := filepath.Join(r.audioDir, sessionID+".wav")
	if err := pcmToWav(rawPath, wavPath, sampleRate); err != nil {
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

func (w *teeWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if err != nil {
		return n, err
	}

	if err := w.recorder.writePCM(p[:n]); err != nil {
		return n, err
	}

	return n, nil
}
