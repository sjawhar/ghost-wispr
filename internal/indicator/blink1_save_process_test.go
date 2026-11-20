//go:build linux && (amd64 || arm64)

package indicator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func init() {
	otherProcesses["pattern-check"] = patternCheckProcess
}

// flashFileBlink1 is a blink(1) with flash whose state lives in a file, so the
// processes a test starts one after another, as a recorder restarted over and
// over, see the same unit: its RAM and flash pattern lines, how many saves
// ('W') reached it and how many it was sent.
type flashFileBlink1 struct {
	Buffer [8]byte
	RAM    [32][3]byte
	Flash  [32][3]byte
	Saves  int // saves that reached the flash
	Sent   int // saves sent, whether they reached it or not
}

func loadFlashFileBlink1(path string) (*flashFileBlink1, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var d flashFileBlink1
	return &d, json.Unmarshal(data, &d)
}

func (d *flashFileBlink1) store(path string) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// Exit codes of a pattern-check process that its fault ended.
const (
	exitCrashedBeforeSave = 3
	exitCrashedAfterSave  = 4
)

// patternCheckProcess is a recorder start, run as another process: it records
// saves where GHOST_WISPR_TEST_RECORDS says, opens the blink(1) whose state
// is in GHOST_WISPR_TEST_STATE, runs its black pattern check, and prints
// whether the unit is then known black. GHOST_WISPR_TEST_FAULT makes the save
// ('W') crash the process before it reaches the unit (crash-before-save) or
// right after (crash-after-save), or fail its transfer (save-fails).
func patternCheckProcess() int {
	state, fault := os.Getenv("GHOST_WISPR_TEST_STATE"), os.Getenv("GHOST_WISPR_TEST_FAULT")
	RecordSavesIn(os.Getenv("GHOST_WISPR_TEST_RECORDS"))
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		d, err := loadFlashFileBlink1(state)
		if err != nil {
			return err
		}
		if request == hidIOCGetFeature {
			*report = d.Buffer
			return nil
		}
		switch report[1] {
		case 'v':
			d.Buffer = [8]byte{0x01, 'v', 0, '3', '3'}
		case 'R':
			c := d.RAM[report[7]%32]
			d.Buffer = [8]byte{0x01, 'R', c[0], c[1], c[2], 0, 10, 0} // every line plays on every LED: Ghost Wispr writes them after 'l' 0
		case 'P':
			d.RAM[report[7]%32] = [3]byte{report[2], report[3], report[4]}
			d.Buffer = *report
		case 'W':
			d.Sent++
			switch fault {
			case "crash-before-save":
				_ = d.store(state)
				os.Exit(exitCrashedBeforeSave)
			case "save-fails":
				if err := d.store(state); err != nil {
					return err
				}
				return unix.EIO
			}
			d.Flash = d.RAM
			d.Saves++
			d.Buffer = *report
			if fault == "crash-after-save" {
				_ = d.store(state)
				os.Exit(exitCrashedAfterSave)
			}
		default:
			d.Buffer = *report
		}
		return d.store(state)
	}
	light, err := OpenBlink1(os.Getenv("GHOST_WISPR_TEST_SYSFS"), os.Getenv("GHOST_WISPR_TEST_DEV_ROOT"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = light.Close() }()
	if err := light.BlackPattern(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("known=%t\n", KnownBlack(light))
	return 0
}

// TestARestartingRecorderWritesTheFlashAtMostOnce fails if six recorder
// starts in a row, each a process of its own, write the blink(1)'s flash more
// than once, or if one marks the unit known black without a save that this
// process saw reach it, or a confirmed save's record. Every start checks a
// unit not known in that process, and each check of such a unit ends in a
// save; a recorder that fails restarts every 10 s. The record of a save,
// written after the save, did not bound that: a record that could not be
// written, or a process that ended between the save and its record, let every
// start write the flash again (review of #100 on sami-devbox: 6 writes in 6
// starts). So a save's attempt is recorded before the save, and spares the
// flash for saveRecordFresh whatever became of it; only the record of a save
// confirmed to reach the unit makes a later start know it. Control: with a
// record that can be written, the first start saves and the rest know the
// unit.
func TestARestartingRecorderWritesTheFlashAtMostOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// unwritable gives the records a path that cannot be made.
		unwritable bool
		fault      string
		// firstExit is the first start's exit code.
		firstExit int
		saves     int
		sent      int
		// known is whether each start reports the unit known black.
		known bool
	}{
		{name: "control: the records can be written", saves: 1, sent: 1, known: true},
		{name: "the records cannot be written", unwritable: true},
		{name: "the process ends right after the save reached the unit", fault: "crash-after-save", firstExit: exitCrashedAfterSave, saves: 1, sent: 1},
		{name: "the process ends while its save is sent", fault: "crash-before-save", firstExit: exitCrashedBeforeSave, sent: 1},
		{name: "the save's transfer fails", fault: "save-fails", sent: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sysfsRoot, devRoot, _ := fakeBlink1Sysfs(t)
			setBlink1Serial(t, sysfsRoot, "restart-test")
			dir := t.TempDir()
			records := filepath.Join(dir, "blink1-saves")
			if tc.unwritable {
				blocker := filepath.Join(dir, "not-a-directory")
				if err := os.WriteFile(blocker, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				records = filepath.Join(blocker, "blink1-saves")
			}
			// A unit saved black long ago and powered up since: RAM and flash
			// are black, and its pattern reads black at every start.
			state := filepath.Join(dir, "blink1.json")
			if err := (&flashFileBlink1{}).store(state); err != nil {
				t.Fatal(err)
			}
			// Each start's outcome: its exit code, or whether it knew the
			// unit black.
			var outcomes, want []string
			for start := range 6 {
				command := exec.Command(os.Args[0], "-test.run=^$")
				command.Env = append(os.Environ(),
					otherProcessEnv+"=pattern-check",
					"GHOST_WISPR_TEST_SYSFS="+sysfsRoot,
					"GHOST_WISPR_TEST_DEV_ROOT="+devRoot,
					"GHOST_WISPR_TEST_STATE="+state,
					"GHOST_WISPR_TEST_RECORDS="+records,
					"GHOST_WISPR_TEST_FAULT="+tc.fault,
				)
				output, err := command.Output()
				var exit *exec.ExitError
				switch {
				case errors.As(err, &exit):
					outcomes = append(outcomes, fmt.Sprintf("exit %d", exit.ExitCode()))
					if exit.ExitCode() != exitCrashedBeforeSave && exit.ExitCode() != exitCrashedAfterSave {
						t.Fatalf("control: start %d failed: %s", start+1, exitStderr(err))
					}
				case err != nil:
					t.Fatal(err)
				default:
					outcomes = append(outcomes, strings.TrimSpace(string(output)))
				}
				if start == 0 && tc.firstExit != 0 {
					want = append(want, fmt.Sprintf("exit %d", tc.firstExit))
				} else {
					want = append(want, fmt.Sprintf("known=%t", tc.known))
				}
			}
			d, err := loadFlashFileBlink1(state)
			if err != nil {
				t.Fatal(err)
			}
			if d.Sent != tc.sent || d.Saves != tc.saves || strings.Join(outcomes, ", ") != strings.Join(want, ", ") {
				t.Fatalf("6 starts sent the flash %d saves, %d of which reached it, and ended %v; want %d sent, %d reached, and %v", d.Sent, d.Saves, outcomes, tc.sent, tc.saves, want)
			}
		})
	}
}

// exitStderr is what a process that ended with an error wrote to stderr.
func exitStderr(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(string(exit.Stderr))
	}
	return ""
}
