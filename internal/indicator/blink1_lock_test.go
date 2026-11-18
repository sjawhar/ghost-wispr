//go:build linux && (amd64 || arm64)

package indicator

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// otherProcessEnv names, in the environment of this test binary, the second
// program it runs as instead of the tests: one of otherProcesses. A test
// starts it to send commands to a blink(1) from another process, as
// `ghost-wispr light-state` does while the recorder runs.
const otherProcessEnv = "GHOST_WISPR_TEST_OTHER_PROCESS"

// otherProcesses are the programs a test can run in another process. Each
// returns its exit code.
var otherProcesses = map[string]func() int{
	"light-red-and-read": lightRedAndRead,
}

func TestMain(m *testing.M) {
	if name := os.Getenv(otherProcessEnv); name != "" {
		run, ok := otherProcesses[name]
		if !ok {
			_, _ = fmt.Fprintf(os.Stderr, "no other process %q\n", name)
			os.Exit(2)
		}
		os.Exit(run())
	}
	os.Exit(m.Run())
}

// startOtherProcess runs the other process name against the hidraw node
// hidraw and the shared blink(1) device, and returns once it has the node
// open. The process then reads one line from the returned writer before it
// sends anything.
func startOtherProcess(t *testing.T, name, hidraw string, device fileBlink1) (start io.Writer, done <-chan error, stderr *bytes.Buffer) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), otherProcessEnv+"="+name, "GHOST_WISPR_TEST_HIDRAW="+hidraw, "GHOST_WISPR_TEST_DEVICE="+device.path)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	ready, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ready.Close() })
	command.Stdout = readyWriter
	stderr = &bytes.Buffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = readyWriter.Close()
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-exited
	})

	started := make(chan error, 1)
	go func() {
		_, err := bufio.NewReader(ready).ReadString('\n')
		started <- err
	}()
	select {
	case err = <-started:
	case <-time.After(10 * time.Second):
		err = errors.New("no ready line within 10 s")
	}
	if err != nil {
		_ = command.Process.Kill()
		<-exited
		t.Fatalf("the other process did not start: %v: %s", err, stderr)
	}

	wait := make(chan error, 1)
	go func() {
		select {
		case <-exited:
			wait <- waitErr
		case <-time.After(10 * time.Second):
			wait <- errors.New("the other process did not exit within 10 s")
		}
	}()
	return stdin, wait, stderr
}

// fileBlink1 is one blink(1) that several processes share, as they share the
// real one: its LED colour and its single feature-report buffer are in a
// file. A SET_FEATURE runs its command and leaves the reply in the buffer; a
// GET_FEATURE returns the buffer, whoever sent the last SET. The device serves
// one transfer at a time: a flock on its state file, not on the hidraw node
// the driver locks.
//
// State: bytes 0-7 are the report buffer, 8-10 the LED colour, 11 the number
// of SET_FEATUREs so far.
type fileBlink1 struct{ path string }

type fileBlink1State [12]byte

func newFileBlink1(t *testing.T, dir string) fileBlink1 {
	t.Helper()
	device := fileBlink1{path: filepath.Join(dir, "blink1-state")}
	if err := os.WriteFile(device.path, make([]byte, len(fileBlink1State{})), 0o600); err != nil {
		t.Fatal(err)
	}
	return device
}

// transfer runs change on the device's state as one transfer.
func (d fileBlink1) transfer(change func(*fileBlink1State)) error {
	file, err := os.OpenFile(d.path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	var state fileBlink1State
	if _, err := file.ReadAt(state[:], 0); err != nil {
		return err
	}
	change(&state)
	_, err = file.WriteAt(state[:], 0)
	return err
}

func (d fileBlink1) ioctl(_ uintptr, request uintptr, report *[8]byte) error {
	return d.transfer(func(state *fileBlink1State) {
		if request == hidIOCGetFeature {
			copy(report[:], state[0:8])
			return
		}
		reply := *report
		switch report[1] {
		case 'n':
			copy(state[8:11], report[2:5])
		case 'r':
			reply = [8]byte{0x01, 'r', state[8], state[9], state[10]}
		case 'v':
			reply = [8]byte{0x01, 'v', 0, '3', '3'}
		}
		copy(state[0:8], reply[:])
		state[11]++
	})
}

func (d fileBlink1) read(t *testing.T) fileBlink1State {
	t.Helper()
	var state fileBlink1State
	if err := d.transfer(func(s *fileBlink1State) { state = *s }); err != nil {
		t.Fatal(err)
	}
	return state
}

// colour is the LED's colour now.
func (d fileBlink1) colour(t *testing.T) [3]byte {
	t.Helper()
	state := d.read(t)
	return [3]byte{state[8], state[9], state[10]}
}

// setColour changes the LED without a command, as the blink(1)'s watchdog
// does when it plays its pattern: the report buffer keeps what it held.
func (d fileBlink1) setColour(t *testing.T, colour [3]byte) {
	t.Helper()
	if err := d.transfer(func(s *fileBlink1State) { copy(s[8:11], colour[:]) }); err != nil {
		t.Fatal(err)
	}
}

// lightRedAndRead is another program with the blink(1) open: once told to, it
// lights it red and reads it back.
func lightRedAndRead() int {
	featureIOCTL = fileBlink1{path: os.Getenv("GHOST_WISPR_TEST_DEVICE")}.ioctl
	// It waits for the light past the read the test holds between its SET and
	// GET, so its commands land after that GET if the read holds the lock.
	exchangeWait = 5 * time.Second
	file, err := os.OpenFile(os.Getenv("GHOST_WISPR_TEST_HIDRAW"), os.O_RDWR, 0)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = file.Close() }()
	light := &blink1{file: file, patternLines: 32}
	if _, err := fmt.Println("ready"); err != nil {
		return 1
	}
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "wait for the start:", err)
		return 1
	}
	if err := light.SetRGB(255, 0, 0); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "light red:", err)
		return 1
	}
	if r, g, b, err := light.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
		_, _ = fmt.Fprintf(os.Stderr, "read back %d,%d,%d, %v, want red\n", r, g, b, err)
		return 1
	}
	return 0
}

// TestAReadDoesNotReturnAnotherProcesssRed fails if a read of a blink(1) that is
// off returns red because another process's 'r' read of red, sent between the
// read's SET_FEATURE and its GET_FEATURE, answered it. The other process lit
// the light red and read it back; then the light went off with no command, as
// the blink(1)'s watchdog turns it off, so the report buffer still held that
// red reply when the read's GET_FEATURE came. The light was off at the read's
// SET and at its GET: a red answer would be a false red, the reading that
// opens the microphone with the light off.
func TestAReadDoesNotReturnAnotherProcesssRed(t *testing.T) {
	dir := t.TempDir()
	hidraw := filepath.Join(dir, "hidraw0")
	if err := os.WriteFile(hidraw, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	device := newFileBlink1(t, dir)
	original := featureIOCTL
	t.Cleanup(func() { featureIOCTL = original })
	featureIOCTL = device.ioctl
	file, err := os.OpenFile(hidraw, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	reader := &blink1{file: file, patternLines: 32}

	// Controls: the fake reads back a red light as red and an off one as off.
	device.setColour(t, [3]byte{255, 0, 0})
	if r, g, b, err := reader.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
		t.Fatalf("control: ReadRGB of a red light = %d,%d,%d, %v, want 255,0,0", r, g, b, err)
	}
	device.setColour(t, [3]byte{})
	if r, g, b, err := reader.ReadRGB(); err != nil || r != 0 || g != 0 || b != 0 {
		t.Fatalf("control: ReadRGB of an off light = %d,%d,%d, %v, want 0,0,0", r, g, b, err)
	}

	start, done, stderr := startOtherProcess(t, "light-red-and-read", hidraw, device)
	setsBefore := device.read(t)[11]
	var atSet, atGet [3]byte
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		if request == hidIOCGetFeature {
			atGet = device.colour(t)
			return device.ioctl(fd, request, report)
		}
		atSet = device.colour(t)
		if err := device.ioctl(fd, request, report); err != nil {
			return err
		}
		// The other process's turn, right after this read's SET_FEATURE. A
		// lock held across SET and GET makes it wait; give its two commands
		// 300 ms to land, then turn the light off without a command.
		if _, err := io.WriteString(start, "go\n"); err != nil {
			return err
		}
		for deadline := time.Now().Add(300 * time.Millisecond); device.read(t)[11]-setsBefore < 3 && time.Now().Before(deadline); {
			time.Sleep(time.Millisecond)
		}
		device.setColour(t, [3]byte{})
		return nil
	}
	r, g, b, err := reader.ReadRGB()
	featureIOCTL = device.ioctl

	if err := <-done; err != nil {
		t.Fatalf("the other process: %v: %s", err, stderr)
	}
	if sets := device.read(t)[11] - setsBefore; sets != 3 {
		t.Fatalf("SET_FEATUREs during the test = %d, want 3: the read's and the other process's two", sets)
	}
	if err != nil {
		t.Fatalf("ReadRGB: %v", err)
	}
	if r != 0 || g != 0 || b != 0 {
		t.Fatalf("false red: ReadRGB = %d,%d,%d with the light %d,%d,%d at its SET and %d,%d,%d at its GET, and another process's red 'r' reply between them; want 0,0,0",
			r, g, b, atSet[0], atSet[1], atSet[2], atGet[0], atGet[1], atGet[2])
	}
}

// TestAReadTakesOnlyTheReplyToItsOwnCommand fails if ReadRGB takes the reply
// to another command for the light's colour. A program that takes no lock
// (blink1-tool, or a lone SET that Ghost Wispr sends without the lock once its
// wait has run out) writes pattern line 0 as red between the read's
// SET_FEATURE and its GET_FEATURE: the LED stays off, and the buffer holds that
// command's echo, 01 50 ff 00 00, whose bytes 2-4 read as red. The blink(1)
// answers a colour read with 'r' in byte 1 (0172ff00 on the office Pi,
// usbmon 2026-11-17), so a reply with anything else there is not red: the
// read fails with ErrOtherReply. Found by the red team on #79.
func TestAReadTakesOnlyTheReplyToItsOwnCommand(t *testing.T) {
	device := &sharedBlink1{names: map[uintptr]string{}, setFrom: map[string]chan struct{}{}}
	original := featureIOCTL
	featureIOCTL = device.ioctl
	t.Cleanup(func() { featureIOCTL = original })
	reader, _ := openTwice(t, device)

	if r, g, b, err := reader.ReadRGB(); err != nil || r != 0 || g != 0 || b != 0 {
		t.Fatalf("control: ReadRGB of an off light = %d,%d,%d, %v, want 0,0,0", r, g, b, err)
	}
	device.mu.Lock()
	device.betweenWho, device.betweenCmd, device.betweenDone = "first", 'r', false
	device.between = func() {
		line := Blink1Report('P', [6]byte{255, 0, 0, 0, 100, 0})
		_ = device.ioctl(^uintptr(0), hidIOCSetFeature, &line)
	}
	device.mu.Unlock()

	r, g, b, err := reader.ReadRGB()
	device.mu.Lock()
	led := device.colour
	device.mu.Unlock()
	if led != [3]byte{} {
		t.Fatalf("control: the LED is %v, want off", led)
	}
	if r == 255 && g == 0 && b == 0 {
		t.Fatalf("false red: ReadRGB = 255,0,0 with the LED %d,%d,%d: it took the echo of another program's 'P' for the reply to its 'r'", led[0], led[1], led[2])
	}
	if !errors.Is(err, ErrOtherReply) || r != 0 || g != 0 || b != 0 {
		t.Fatalf("ReadRGB answered by another command's echo = %d,%d,%d, %v, want no colour and ErrOtherReply", r, g, b, err)
	}
}

// TestABusyBlink1ReadsUnknownNotRed fails if a command waits without bound for
// a blink(1) another handle holds, sends anything without its lock, or if a
// read that could not get the light reports a colour: it must report
// ErrBusy (unknown), never red, though the light is red.
func TestABusyBlink1ReadsUnknownNotRed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	light := &blink1{file: file, patternLines: 32}
	var sent atomic.Int32
	original, originalWait := featureIOCTL, exchangeWait
	t.Cleanup(func() { featureIOCTL, exchangeWait = original, originalWait })
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		sent.Add(1)
		if request == hidIOCGetFeature {
			report[2], report[3], report[4] = 255, 0, 0
		}
		return nil
	}
	exchangeWait = 50 * time.Millisecond

	holder, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Close() })
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	r, g, b, err := light.ReadRGB()
	waited := time.Since(started)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("ReadRGB of a held blink(1) error = %v, want ErrBusy", err)
	}
	if r != 0 || g != 0 || b != 0 {
		t.Fatalf("ReadRGB of a held blink(1) = %d,%d,%d, want no colour", r, g, b)
	}
	if waited < exchangeWait || waited > exchangeWait+time.Second {
		t.Fatalf("ReadRGB of a held blink(1) waited %v, want %v", waited, exchangeWait)
	}
	if err := light.SetRGB(0, 0, 0); !errors.Is(err, ErrBusy) {
		t.Fatalf("SetRGB of a held blink(1) error = %v, want ErrBusy", err)
	}
	if n := sent.Load(); n != 0 {
		t.Fatalf("feature reports sent to a held blink(1) = %d, want 0", n)
	}

	// Control: once the other handle lets go, the same read gets red.
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if r, g, b, err := light.ReadRGB(); err != nil || r != 255 || g != 0 || b != 0 {
		t.Fatalf("control: ReadRGB after the other handle let go = %d,%d,%d, %v, want 255,0,0", r, g, b, err)
	}
}

// TestAHandleThatSendsWithoutPauseLetsAnotherIn fails if a handle that sends
// one command after another, as the recorder does when it writes the black
// pattern (65 commands) or checks the light (up to four), keeps another
// handle out until that one's wait runs out: a `ghost-wispr light-state` run
// then would print unknown though the light never stayed busy for long.
func TestAHandleThatSendsWithoutPauseLetsAnotherIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidraw0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := func() *blink1 {
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		return &blink1{file: file, patternLines: 32}
	}
	busy, other := open(), open()
	original, originalWait := featureIOCTL, exchangeWait
	t.Cleanup(func() { featureIOCTL, exchangeWait = original, originalWait })
	busyFd := busy.file.Fd()
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		// A USB control transfer: the sender's thread waits in the ioctl.
		if fd == busyFd {
			for started := time.Now(); time.Since(started) < 500*time.Microsecond; {
			}
		}
		if request == hidIOCGetFeature {
			report[2] = 255
		}
		return nil
	}
	exchangeWait = 250 * time.Millisecond

	stop := make(chan struct{})
	sending := make(chan struct{})
	var commands atomic.Int32
	go func() {
		defer close(sending)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, _, _, err := busy.ReadRGB(); err == nil {
				commands.Add(1)
			}
		}
	}()
	defer func() { <-sending }()
	defer close(stop)

	for deadline := time.Now().Add(time.Second); commands.Load() < 10; {
		if time.Now().After(deadline) {
			t.Fatal("control: the busy handle sent fewer than 10 commands in 1 s")
		}
		time.Sleep(time.Millisecond)
	}
	for i := range 10 {
		if _, _, _, err := other.ReadRGB(); err != nil {
			t.Fatalf("read %d of the other handle while one sends without pause: %v", i+1, err)
		}
	}
}

// TestEveryBlink1CommandHoldsTheLock fails if a command reaches the blink(1)
// while another handle could take its lock, or lands inside another command's
// exchange on the same handle. The census of the driver's commands:
//
//	command        sent by                              exchange
//	'v'            loadVersion (OpenBlink1)             SET then GET
//	'n'            SetRGB                               SET
//	'r'            ReadRGB                              SET then GET
//	'D'            ServerDown                           SET
//	'R'            patternIsBlack (BlackPattern)        SET then GET, per line
//	'l', 'P', 'W'  writeBlackPattern (BlackPattern)     SET each
//
// The commands run at once from several goroutines on one handle.
func TestEveryBlink1CommandHoldsTheLock(t *testing.T) {
	sysfsRoot, devRoot := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sysfsRoot, "class", "hidraw", "hidraw0", "device", "uevent"), []byte("HID_ID=0003:000027B8:000001ED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(devRoot, "hidraw0")
	if err := os.WriteFile(node, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	probe, err := os.Open(node)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = probe.Close() })

	var (
		mu         sync.Mutex
		violations []string
		pending    byte
		patterns   = map[byte]bool{}
		sent       = map[byte]int{}
	)
	original, originalWait := featureIOCTL, exchangeWait
	t.Cleanup(func() { featureIOCTL, exchangeWait = original, originalWait })
	// No command's wait runs out here, so every command, lone SETs too, must
	// hold the lock.
	exchangeWait = 10 * time.Second
	featureIOCTL = func(_ uintptr, request uintptr, report *[8]byte) error {
		// The sending handle holds the lock if another open of the node cannot take it.
		held := true
		if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			held = false
			_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
		} else if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		mu.Lock()
		command := report[1]
		if !held {
			violations = append(violations, fmt.Sprintf("%q sent without the lock", command))
		}
		if request == hidIOCGetFeature {
			if pending != command {
				violations = append(violations, fmt.Sprintf("GET_FEATURE for %q after the SET of %q", command, pending))
			}
			pending = 0
			switch command {
			case 'v':
				report[3] = '3'
			case 'R':
				if !patterns[report[7]] {
					report[2] = 1 // a lit line: BlackPattern writes the pattern
				} else {
					report[6] = 10
				}
			}
		} else {
			if pending != 0 {
				violations = append(violations, fmt.Sprintf("%q sent between the SET and GET of %q", command, pending))
			}
			sent[command]++
			switch command {
			case 'v', 'r', 'R':
				pending = command
			case 'P':
				patterns[report[7]] = true
			}
		}
		mu.Unlock()
		time.Sleep(200 * time.Microsecond) // room for a command from another goroutine to land
		return nil
	}

	opened, err := OpenBlink1(sysfsRoot, devRoot)
	if err != nil {
		t.Fatal(err)
	}
	light := opened.(*blink1)
	t.Cleanup(func() { _ = light.Close() })

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	run := func(times int, command func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range times {
				if err := command(); err != nil {
					errs <- err
				}
			}
		}()
	}
	run(1, light.BlackPattern)
	run(10, func() error { version := Blink1Report('v', [6]byte{}); return light.query(&version) })
	run(20, func() error { return light.SetRGB(255, 0, 0) })
	run(20, func() error { _, _, _, err := light.ReadRGB(); return err })
	run(10, func() error { return light.ServerDown(3 * time.Second) })
	run(10, func() error { return light.ServerDown(0) })
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("command: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		t.Fatalf("%d commands outside the lock, first %s", len(violations), strings.Join(violations[:min(len(violations), 5)], "; "))
	}
	for _, command := range []byte("vnrDRlPW") {
		if sent[command] == 0 {
			t.Errorf("the census sent no %q: it checked nothing for it", command)
		}
	}
}
