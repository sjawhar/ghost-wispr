package audio

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

var ffprobeBinary = "ffprobe"

// Duration returns the recording duration reported by ffprobe.
func Duration(ctx context.Context, path string) (float64, error) {
	output, err := exec.CommandContext(ctx, ffprobeBinary,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("probe recording %s: %w", path, err)
	}
	raw := strings.TrimSpace(string(output))
	duration, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("parse recording duration %q: %w", raw, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("recording duration %q is not positive", raw)
	}
	return duration, nil
}
