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

// CaptureDevicePresent reports whether the capture device the microphone
// opens exists, without opening one. ALSA lists each capture PCM of each card
// as /proc/asound/cardN/pcmMc. With device set, the mic_device setting, only
// a capture PCM of the sound card it names counts (findCard), since that is
// the card the microphone opens; empty, any capture PCM does, as PortAudio's
// default input can be any of them. On a system with no /proc/asound, which
// is not ALSA, it reports true: it cannot tell.
func CaptureDevicePresent(device string) bool {
	if _, err := os.Stat(asoundRoot); err != nil {
		return true
	}
	if device != "" {
		_, _, _, _, found := findCard(device, "c")
		return found
	}
	captures, _ := filepath.Glob(filepath.Join(asoundRoot, "card[0-9]*", "pcm[0-9]*c"))
	return len(captures) > 0
}

// cardLine is a card's first line in /proc/asound/cards, as
// " 0 [J75            ]: USB-Audio - Jabra Speak2 75": its index, id and name.
var cardLine = regexp.MustCompile(`^\s*(\d+) \[(\S+)\s*\]: (.*)$`)

// findCard looks through /proc/asound/cards for the first sound card whose id
// or names contain name, without case, and that has a PCM of direction ("p"
// for playback, "c" for capture). It returns that card's index and id, the
// number of its first such PCM, and the ids of the cards it looked through.
// "Jabra" so finds the Jabra Speak2 75 whichever card number it has.
func findCard(name, direction string) (index, id, dev string, cards []string, found bool) {
	listing, err := os.ReadFile(filepath.Join(asoundRoot, "cards"))
	if err != nil {
		return "", "", "", nil, false
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
		pcms, _ := filepath.Glob(filepath.Join(asoundRoot, "card"+index, "pcm[0-9]*"+direction))
		if len(pcms) == 0 {
			continue
		}
		slices.Sort(pcms)
		dev := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(pcms[0]), "pcm"), direction)
		return index, id, dev, cards, true
	}
	return "", "", "", cards, false
}

// playbackDevice returns the ALSA PCM the speaker plays through for name, the
// speaker_device setting, and the ids of the sound cards it looked through:
//   - "" is ALSA's "default", and a name with a colon, as plughw:CARD=J75,
//     is a PCM, used as it is;
//   - any other name names a sound card (findCard), whose first playback PCM
//     gives plughw:CARD=<id>,DEV=<n>, which converts the rate and format to
//     what the device takes;
//   - a name no card with a playback PCM matches is used as it is: a PCM ALSA
//     defines, as "default", "null" or "pulse", or one asound.conf defines.
func playbackDevice(name string) (device string, cards []string) {
	if name == "" {
		return "default", nil
	}
	if strings.Contains(name, ":") {
		return name, nil
	}
	_, id, dev, cards, found := findCard(name, "p")
	if !found {
		return name, cards
	}
	return fmt.Sprintf("plughw:CARD=%s,DEV=%s", id, dev), cards
}
