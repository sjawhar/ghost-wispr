package audio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
)

const (
	defaultSampleRate = 16000
	pcmChannels       = 1
	pcmBitDepth       = 16
)

type Recorder struct {
	audioDir string

	mu         sync.Mutex
	sessionID  string
	rawPath    string
	rawFile    *os.File
	sampleRate int

	encode func(rawPath, sessionID string) (string, error)
}

func NewRecorder(audioDir string) *Recorder {
	if audioDir == "" {
		audioDir = filepath.Join("data", "audio")
	}

	r := &Recorder{audioDir: audioDir, sampleRate: defaultSampleRate}
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

	return nil
}

func (r *Recorder) EndSession() (string, error) {
	r.mu.Lock()
	if r.sessionID == "" || r.rawFile == nil {
		r.mu.Unlock()
		return "", nil
	}

	sessionID := r.sessionID
	rawPath := r.rawPath
	rawFile := r.rawFile

	r.sessionID = ""
	r.rawPath = ""
	r.rawFile = nil
	r.mu.Unlock()

	if err := rawFile.Close(); err != nil {
		return "", fmt.Errorf("close raw pcm file: %w", err)
	}

	audioPath, err := r.encode(rawPath, sessionID)
	if err != nil {
		return "", err
	}

	_ = os.Remove(rawPath)
	return audioPath, nil
}

func (r *Recorder) writePCM(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.rawFile == nil {
		return nil
	}

	if _, err := r.rawFile.Write(data); err != nil {
		return fmt.Errorf("write raw pcm bytes: %w", err)
	}
	return nil
}

func (r *Recorder) defaultEncode(rawPath, sessionID string) (string, error) {
	r.mu.Lock()
	sampleRate := r.sampleRate
	r.mu.Unlock()
	if sampleRate <= 0 {
		sampleRate = defaultSampleRate
	}

	mp3Path := filepath.Join(r.audioDir, sessionID+".mp3")

	if err := encodeWithFFmpeg(rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}

	if err := encodeWithLame(rawPath, mp3Path, sampleRate); err == nil {
		return mp3Path, nil
	}

	wavPath := filepath.Join(r.audioDir, sessionID+".wav")
	if err := pcmToWav(rawPath, wavPath, sampleRate); err != nil {
		return "", fmt.Errorf("encode wav fallback: %w", err)
	}

	return wavPath, nil
}

func encodeWithFFmpeg(rawPath, outputPath string, sampleRate int) error {
	cmd := exec.Command(
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

func encodeWithLame(rawPath, outputPath string, sampleRate int) error {
	khz := float64(sampleRate) / 1000.0
	formatted := strconv.FormatFloat(khz, 'f', -1, 64)
	cmd := exec.Command(
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
	defer out.Close()

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
