package audio

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// asoundRoot is where the kernel lists ALSA's sound cards; tests point it at
// a directory of their own.
var asoundRoot = "/proc/asound"

// CaptureDevicePresent reports whether a capture device exists, without
// opening one: ALSA lists each capture PCM of each card as
// /proc/asound/cardN/pcmMc. The microphone opens PortAudio's default input,
// which on ALSA is one of them. On a system with no /proc/asound, which is not
// ALSA, it reports true: it cannot tell.
func CaptureDevicePresent() bool {
	if _, err := os.Stat(asoundRoot); err != nil {
		return true
	}
	captures, _ := filepath.Glob(filepath.Join(asoundRoot, "card[0-9]*", "pcm[0-9]*c"))
	return len(captures) > 0
}

// cardLine is a card's first line in /proc/asound/cards, as
// " 0 [J75            ]: USB-Audio - Jabra Speak2 75": its index, id and name.
var cardLine = regexp.MustCompile(`^\s*(\d+) \[(\S+)\s*\]: (.*)$`)

// playbackDevice returns the ALSA PCM the speaker plays through for name, the
// speaker_device setting, and the ids of the sound cards it looked through:
//   - "" is ALSA's "default", and a name with a colon, as plughw:CARD=J75,
//     is a PCM, used as it is;
//   - any other name is matched, without case, against each card's id and
//     names in /proc/asound/cards, and the first card that matches and has a
//     playback PCM gives plughw:CARD=<id>,DEV=<its first playback PCM>, which
//     converts the rate and format to what the device takes. "Jabra" so finds
//     the Jabra Speak2 75 whichever card number it has;
//   - a name no such card matches is used as it is: a PCM ALSA defines, as
//     "default", "null" or "pulse", or one asound.conf defines.
func playbackDevice(name string) (device string, cards []string) {
	if name == "" {
		return "default", nil
	}
	if strings.Contains(name, ":") {
		return name, nil
	}
	listing, err := os.ReadFile(filepath.Join(asoundRoot, "cards"))
	if err != nil {
		return name, nil
	}
	want := strings.ToLower(name)
	lines := strings.Split(string(listing), "\n")
	for i, line := range lines {
		match := cardLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		index, id, names := match[1], match[2], match[3]
		if i+1 < len(lines) {
			names += " " + strings.TrimSpace(lines[i+1])
		}
		cards = append(cards, id)
		if !strings.Contains(strings.ToLower(id+" "+names), want) {
			continue
		}
		playbacks, _ := filepath.Glob(filepath.Join(asoundRoot, "card"+index, "pcm[0-9]*p"))
		if len(playbacks) == 0 {
			continue
		}
		slices.Sort(playbacks)
		dev := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(playbacks[0]), "pcm"), "p")
		return fmt.Sprintf("plughw:CARD=%s,DEV=%s", id, dev), cards
	}
	return name, cards
}
