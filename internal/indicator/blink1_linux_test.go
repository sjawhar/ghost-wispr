//go:build linux && (amd64 || arm64)

package indicator

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenBlink1FindsTheDeviceBySysfs(t *testing.T) {
	sysfsRoot := t.TempDir()
	devRoot := t.TempDir()
	for _, name := range []string{"hidraw0", "hidraw3"} {
		if err := os.MkdirAll(filepath.Join(sysfsRoot, "class", "hidraw", name, "device"), 0o755); err != nil {
			t.Fatalf("create sysfs entry: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device", "uevent"), []byte("HID_ID=0003:00001234:00005678\n"), 0o644); err != nil {
		t.Fatalf("write unrelated uevent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw3", "device", "uevent"), []byte("HID_ID=0003:000027B8:000001ED\n"), 0o644); err != nil {
		t.Fatalf("write blink uevent: %v", err)
	}
	if err := os.WriteFile(filepath.Join(devRoot, "hidraw3"), nil, 0o600); err != nil {
		t.Fatalf("create fake hidraw device: %v", err)
	}
	original := featureIOCTL
	featureIOCTL = func(_ uintptr, _ uintptr, report *[8]byte) error {
		if report[1] == 'v' {
			report[3] = '3'
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	light, err := OpenBlink1(sysfsRoot, devRoot)
	if err != nil {
		t.Fatalf("open blink(1): %v", err)
	}
	t.Cleanup(func() { _ = light.Close() })
	if got := light.(*blink1).file.Name(); got != filepath.Join(devRoot, "hidraw3") {
		t.Fatalf("opened device = %q, want %q", got, filepath.Join(devRoot, "hidraw3"))
	}
	if got := light.(*blink1).patternLines; got != 32 {
		t.Fatalf("mk3 pattern lines = %d, want 32", got)
	}
}

func TestReadRGBSendsThenGets(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "hidraw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	light := &blink1{file: file, patternLines: 32}
	original := featureIOCTL
	var requests []uintptr
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		requests = append(requests, request)
		if request == hidIOCGetFeature {
			report[2], report[3], report[4] = 255, 0, 0
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	r, g, b, err := light.ReadRGB()
	if err != nil {
		t.Fatalf("read rgb: %v", err)
	}
	if got, want := [3]uint8{r, g, b}, [3]uint8{255, 0, 0}; got != want {
		t.Fatalf("colour = %v, want %v", got, want)
	}
	if got, want := requests, []uintptr{hidIOCSetFeature, hidIOCGetFeature}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("ioctl requests = %v, want %v", got, want)
	}
}

func TestBlink1BlackPatternAndWatchdogReports(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "hidraw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	light := &blink1{file: file, patternLines: 32}
	original := featureIOCTL
	var reports [][8]byte
	patterns := make(map[byte][8]byte)
	var patternReads []byte
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		if request == hidIOCSetFeature {
			reports = append(reports, *report)
			if report[1] == 'P' {
				patterns[report[7]] = *report
			}
			return nil
		}
		if report[1] == 'R' {
			line := report[7]
			patternReads = append(patternReads, line)
			pattern, ok := patterns[line]
			if !ok || pattern[2] != 0 || pattern[3] != 0 || pattern[4] != 0 || (pattern[5] == 0 && pattern[6] == 0) {
				report[2] = 1
			} else {
				report[6] = pattern[6]
			}
		}
		return nil
	}
	t.Cleanup(func() { featureIOCTL = original })

	if err := light.BlackPattern(); err != nil {
		t.Fatalf("black pattern: %v", err)
	}
	if err := light.ServerDown(3 * time.Second); err != nil {
		t.Fatalf("serverdown: %v", err)
	}
	if got, want := reports[1], Blink1Report('l', [6]byte{}); got != want {
		t.Fatalf("all-LED report = %v, want %v", got, want)
	}
	var patternsWritten [][8]byte
	lastPattern := -1
	saveIndex := -1
	for i, report := range reports {
		switch report[1] {
		case 'P':
			patternsWritten = append(patternsWritten, report)
			lastPattern = i
		case 'W':
			saveIndex = i
		}
	}
	if got, want := len(patternsWritten), 32; got != want {
		t.Fatalf("saved pattern lines = %d, want %d", got, want)
	}
	for line := range 32 {
		if got, want := patternsWritten[line], Blink1Report('P', [6]byte{0, 0, 0, 0, 10, byte(line)}); got != want {
			t.Fatalf("pattern line %d = %v, want %v", line, got, want)
		}
	}
	if saveIndex != lastPattern+1 || reports[saveIndex] != Blink1Report('W', [6]byte{0xbe, 0xef, 0xca, 0xfe}) {
		t.Fatalf("save report at %d after final pattern %d = %v, want immediate save", saveIndex, lastPattern, reports[saveIndex])
	}
	if got, want := len(patternReads), 33; got != want || patternReads[0] != 0 {
		t.Fatalf("pattern reads = %v, want initial line 0 then 32 verification reads", patternReads)
	}
	for line := range 32 {
		if got := patternReads[line+1]; got != byte(line) {
			t.Fatalf("verification read %d = line %d, want line %d", line, got, line)
		}
	}
	if got, want := reports[len(reports)-1], Blink1Report('D', [6]byte{1, 0x01, 0x2c, 1, 0, 31}); got != want {
		t.Fatalf("watchdog report = %v, want %v", got, want)
	}
}
