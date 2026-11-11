package audio

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var ffmpegBinary = "ffmpeg"

// Splice cuts [start,end) seconds out of path, atomically replacing it with
// the cut recording and returning the replacement's SHA-256.
func Splice(ctx context.Context, path string, start, end float64) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("splice recording: path is required")
	}
	if start < 0 || end <= start {
		return "", fmt.Errorf("splice recording: invalid span [%.3f,%.3f)", start, end)
	}

	ext := filepath.Ext(path)
	if ext == "" {
		return "", fmt.Errorf("splice recording %s: file extension is required", path)
	}
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".splice-*"+ext)
	if err != nil {
		return "", fmt.Errorf("create splice output beside %s: %w", path, err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("close splice output %s: %w", tempPath, err)
	}
	keepTemp := false
	defer func() {
		if !keepTemp {
			_ = os.Remove(tempPath)
		}
	}()

	startText := strconv.FormatFloat(start, 'f', -1, 64)
	endText := strconv.FormatFloat(end, 'f', -1, 64)
	filter := "[0:a]atrim=start=0:end=" + startText + ",asetpts=PTS-STARTPTS[before];" +
		"[0:a]atrim=start=" + endText + ",asetpts=PTS-STARTPTS[after];" +
		"[before][after]concat=n=2:v=0:a=1[out]"
	cmd := exec.CommandContext(ctx, ffmpegBinary,
		"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-i", path,
		"-filter_complex", filter,
		"-map", "[out]",
		tempPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("splice recording %s: %w: %s", path, err, strings.TrimSpace(string(output)))
	}
	if err := syncFile(tempPath); err != nil {
		return "", fmt.Errorf("flush spliced recording %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return "", fmt.Errorf("replace recording %s: %w", path, err)
	}
	keepTemp = true
	if err := syncDirectory(dir); err != nil {
		return "", fmt.Errorf("flush recording directory %s: %w", dir, err)
	}
	return fileSHA256(path)
}

func syncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return file.Sync()
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
