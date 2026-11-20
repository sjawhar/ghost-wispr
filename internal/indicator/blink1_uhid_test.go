//go:build linux && (amd64 || arm64) && uhid

package indicator

// These tests run the driver against a real hidraw node: a blink(1) that the
// kernel's uhid driver makes for this process, whose one feature-report buffer
// is the device's, shared by every handle as the real blink(1)'s is. They show
// that a flock on a hidraw node excludes the other opens of it, in this process
// and in another one, and that a read is not answered by another process's
// command. They need root, for /dev/uhid and for the device node they make,
// and build only with -tags uhid:
//
//	docker run --rm --init --privileged -e GODEBUG=asyncpreemptoff=1 \
//	  -v "$PWD":/src -w /src golang:1.25 \
//	  go test -tags uhid -run Uhid -count=1 -v ./internal/indicator/

import (
	"bufio"
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
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func init() {
	otherProcesses["light-state-sim"] = lightStateSim
	otherProcesses["try-lock"] = tryLock
}

// uhid event types and the size of struct uhid_event (linux/uhid.h).
const (
	uhidDestroy        = 1
	uhidGetReport      = 9
	uhidGetReportReply = 10
	uhidCreate2        = 11
	uhidSetReport      = 13
	uhidSetReportReply = 14
	uhidEventSize      = 4 + 4372
)

// blink1Descriptor is a blink(1)'s HID report descriptor: one vendor-defined
// feature report, ID 1, of eight bytes.
var blink1Descriptor = []byte{
	0x06, 0xab, 0xff, // Usage Page (Vendor 0xFFAB)
	0x0a, 0x00, 0x20, // Usage (0x2000)
	0xa1, 0x01, //       Collection (Application)
	0x15, 0x00, //         Logical Minimum (0)
	0x26, 0xff, 0x00, //   Logical Maximum (255)
	0x75, 0x08, //         Report Size (8)
	0x85, 0x01, //         Report ID (1)
	0x95, 0x08, //         Report Count (8)
	0x09, 0x00, //         Usage (0)
	0xb2, 0x02, 0x01, //   Feature (Data, Variable, Absolute, Buffered Bytes)
	0xc0, //             End Collection
}

// uhidBlink1 answers the kernel's feature-report requests for its blink(1) as
// the device does: a SET_REPORT runs the command and leaves its reply in the
// one buffer; a GET_REPORT returns the buffer. Each transfer takes delay, as
// a USB control transfer takes milliseconds.
type uhidBlink1 struct {
	file  *os.File
	uniq  string
	delay time.Duration
	done  chan struct{}

	mu     sync.Mutex
	colour [3]byte
	buffer [8]byte
}

func newUhidBlink1(t *testing.T) *uhidBlink1 {
	t.Helper()
	return newUhidBlink1Delay(t, 2*time.Millisecond)
}

// newUhidBlink1Delay makes a uhid blink(1) whose transfers each take delay.
func newUhidBlink1Delay(t *testing.T, delay time.Duration) *uhidBlink1 {
	t.Helper()
	file, err := os.OpenFile("/dev/uhid", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open /dev/uhid: %v (run as root with -tags uhid, as this file's comment says)", err)
	}
	d := &uhidBlink1{file: file, uniq: fmt.Sprintf("ghost-wispr-uhid-test-%d", os.Getpid()), delay: delay, done: make(chan struct{})}
	event := make([]byte, uhidEventSize)
	binary.NativeEndian.PutUint32(event, uhidCreate2)
	request := event[4:]
	copy(request[0:128], "ThingM blink(1) mk3 (ghost-wispr uhid test)")
	copy(request[192:256], d.uniq)
	binary.NativeEndian.PutUint16(request[256:], uint16(len(blink1Descriptor)))
	binary.NativeEndian.PutUint16(request[258:], 0x03) // BUS_USB
	binary.NativeEndian.PutUint32(request[260:], 0x27b8)
	binary.NativeEndian.PutUint32(request[264:], 0x01ed)
	copy(request[276:], blink1Descriptor)
	if _, err := file.Write(event); err != nil {
		_ = file.Close()
		t.Fatalf("create the uhid blink(1): %v", err)
	}
	go d.serve()
	t.Cleanup(func() {
		destroy := make([]byte, uhidEventSize)
		binary.NativeEndian.PutUint32(destroy, uhidDestroy)
		_, _ = file.Write(destroy)
		_ = file.Close()
		<-d.done
	})
	return d
}

func (d *uhidBlink1) serve() {
	defer close(d.done)
	event := make([]byte, uhidEventSize)
	for {
		n, err := d.file.Read(event)
		if err != nil {
			return
		}
		if n < 10 {
			continue
		}
		id := binary.NativeEndian.Uint32(event[4:])
		switch binary.NativeEndian.Uint32(event) {
		case uhidSetReport:
			var report [8]byte
			copy(report[:], event[12:12+min(int(binary.NativeEndian.Uint16(event[10:])), 8)])
			time.Sleep(d.delay)
			d.set(report)
			reply := make([]byte, uhidEventSize)
			binary.NativeEndian.PutUint32(reply, uhidSetReportReply)
			binary.NativeEndian.PutUint32(reply[4:], id)
			_, _ = d.file.Write(reply)
		case uhidGetReport:
			time.Sleep(d.delay)
			d.mu.Lock()
			buffer := d.buffer
			d.mu.Unlock()
			reply := make([]byte, uhidEventSize)
			binary.NativeEndian.PutUint32(reply, uhidGetReportReply)
			binary.NativeEndian.PutUint32(reply[4:], id)
			binary.NativeEndian.PutUint16(reply[10:], uint16(len(buffer)))
			copy(reply[12:], buffer[:])
			_, _ = d.file.Write(reply)
		}
	}
}

// set runs a command as the blink(1) does, by the replies usbmon showed on
// the office Pi: 'v' answers the version, 'r' the colour, and any other
// command leaves its own report as the reply.
func (d *uhidBlink1) set(report [8]byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buffer = report
	switch report[1] {
	case 'n':
		copy(d.colour[:], report[2:5])
	case 'r':
		d.buffer = [8]byte{0x01, 'r', d.colour[0], d.colour[1], d.colour[2]}
	case 'v':
		d.buffer = [8]byte{0x01, 'v', 0, '3', '3'}
	}
}

// node waits for the kernel's hidraw device for d and makes its node in a
// directory of its own, as /dev holds it. It fails unless d is the only
// blink(1) the kernel lists, so OpenBlink1 finds it and no other.
func (d *uhidBlink1) node(t *testing.T) (devRoot, path string) {
	t.Helper()
	var name string
	for deadline := time.Now().Add(5 * time.Second); name == "" && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		entries, _ := filepath.Glob("/sys/class/hidraw/*/device/uevent")
		blink1s := 0
		for _, entry := range entries {
			uevent, err := os.ReadFile(entry)
			if err != nil || !strings.Contains(string(uevent), "HID_ID=0003:000027B8:000001ED") {
				continue
			}
			blink1s++
			if strings.Contains(string(uevent), "HID_UNIQ="+d.uniq+"\n") {
				name = filepath.Base(filepath.Dir(filepath.Dir(entry)))
			}
		}
		if name != "" && blink1s != 1 {
			t.Fatalf("the kernel lists %d blink(1)s; OpenBlink1 could open another than the test's", blink1s)
		}
	}
	if name == "" {
		t.Fatal("the kernel made no hidraw device for the uhid blink(1) within 5 s")
	}
	numbers, err := os.ReadFile("/sys/class/hidraw/" + name + "/dev")
	if err != nil {
		t.Fatal(err)
	}
	major, minor, ok := strings.Cut(strings.TrimSpace(string(numbers)), ":")
	majorNumber, majorErr := strconv.ParseUint(major, 10, 32)
	minorNumber, minorErr := strconv.ParseUint(minor, 10, 32)
	if !ok || majorErr != nil || minorErr != nil {
		t.Fatalf("hidraw device numbers %q", numbers)
	}
	devRoot = t.TempDir()
	path = filepath.Join(devRoot, name)
	if err := unix.Mknod(path, unix.S_IFCHR|0o600, int(unix.Mkdev(uint32(majorNumber), uint32(minorNumber)))); err != nil {
		t.Fatalf("make the node of %s: %v", name, err)
	}
	t.Logf("the uhid blink(1) is %s, character device %d:%d, node %s", name, majorNumber, minorNumber, path)
	return devRoot, path
}

// tryLock is another process that tries once to take the flock on the node.
// It exits 0 if it got it, 3 if another open of the node holds it.
func tryLock() int {
	file, err := os.Open(os.Getenv("GHOST_WISPR_TEST_HIDRAW"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = file.Close() }()
	err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, unix.EWOULDBLOCK):
		return 3
	default:
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
}

func runTryLock(t *testing.T, path string) int {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), otherProcessEnv+"=try-lock", "GHOST_WISPR_TEST_HIDRAW="+path)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("try-lock: %v: %s", err, output)
	}
	return 0
}

// TestUhidFlockOnAHidrawNodeExcludesItsOtherOpens fails if flock(LOCK_EX) on
// a hidraw node does not exclude another open of that node, in this process
// or in another one, or if a free node cannot be locked.
func TestUhidFlockOnAHidrawNodeExcludesItsOtherOpens(t *testing.T) {
	device := newUhidBlink1(t)
	_, path := device.node(t)
	first, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if code := runTryLock(t, path); code != 0 {
		t.Fatalf("control: another process could not lock the free node (exit %d)", code)
	}
	if err := unix.Flock(int(first.Fd()), unix.LOCK_EX); err != nil {
		t.Fatalf("lock the node: %v", err)
	}
	if err := unix.Flock(int(second.Fd()), unix.LOCK_EX|unix.LOCK_NB); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("a second open in this process locked the held node: %v, want EWOULDBLOCK", err)
	}
	if code := runTryLock(t, path); code != 3 {
		t.Fatalf("another process trying the held node exited %d, want 3 (EWOULDBLOCK)", code)
	}
	if err := unix.Flock(int(first.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(second.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("control: the second open could not lock the node once it was free: %v", err)
	}
}

// pipeClock is a lock simulation's clock (lockSim) in another process: Sleep
// asks the simulation, on file 4, to let the process sleep, and waits on file
// 3 until the simulation wakes it at an instant of its clock, which Now
// returns until the next wake. A process on it runs only while the
// simulation lets it, as an actor of the test's own process does.
type pipeClock struct {
	wakes    *bufio.Reader
	requests *os.File
	now      time.Time
}

// newPipeClock waits for the simulation to start this process.
func newPipeClock() (*pipeClock, error) {
	c := &pipeClock{wakes: bufio.NewReader(os.NewFile(3, "wakes")), requests: os.NewFile(4, "requests")}
	if err := c.waitWake(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *pipeClock) Now() time.Time { return c.now }

func (c *pipeClock) Sleep(d time.Duration) {
	if _, err := fmt.Fprintf(c.requests, "sleep %d\n", d); err != nil {
		panic(err)
	}
	if err := c.waitWake(); err != nil {
		panic(err)
	}
}

func (c *pipeClock) waitWake() error {
	line, err := c.wakes.ReadString('\n')
	if err != nil {
		return err
	}
	var nanos int64
	if _, err := fmt.Sscanf(line, "wake %d\n", &nanos); err != nil {
		return fmt.Errorf("wake %q: %w", line, err)
	}
	c.now = time.Unix(0, nanos)
	return nil
}

// done tells the simulation the process has ended its part.
func (c *pipeClock) done() {
	_, _ = fmt.Fprintln(c.requests, "done")
}

// Remote starts command, which runs on a pipeClock, as an actor in another
// process, offset after the clock's start, and returns the actor.
func (s *lockSim) Remote(name string, loses bool, offset time.Duration, command *exec.Cmd) (*simActor, error) {
	wakesRead, wakesWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	requestsRead, requestsWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s.t.Cleanup(func() { _, _ = wakesWrite.Close(), requestsRead.Close() })
	command.ExtraFiles = []*os.File{wakesRead, requestsWrite}
	err = command.Start()
	_, _ = wakesRead.Close(), requestsWrite.Close()
	if err != nil {
		return nil, err
	}
	a := s.add(name, loses, offset)
	a.resume = func(now time.Time) error {
		_, err := fmt.Fprintf(wakesWrite, "wake %d\n", now.UnixNano())
		return err
	}
	go func() {
		defer s.finish(a)
		requests := bufio.NewScanner(requestsRead)
		for requests.Scan() {
			var nanos int64
			switch _, err := fmt.Sscanf(requests.Text(), "sleep %d", &nanos); {
			case requests.Text() == "done":
				return
			case err != nil:
				s.fail("actor %s asked %q", name, requests.Text())
				return
			}
			s.mu.Lock()
			if s.running != a {
				s.mu.Unlock()
				s.fail("actor %s slept while it did not run", name)
				return
			}
			s.sleep(a, time.Duration(nanos))
			s.mu.Unlock()
		}
		s.fail("actor %s ended without saying it was done", name)
	}()
	return a, nil
}

// Ended reports whether every one of actors has ended.
func (s *lockSim) Ended(actors []*simActor) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range actors {
		if !a.done {
			return false
		}
	}
	return true
}

// lightStateSim is `ghost-wispr light-state` run again and again on a lock
// simulation's clock (pipeClock): it opens the blink(1) (a version query),
// reads its colour and closes it, GHOST_WISPR_TEST_ITERATIONS times, back to
// back. Each transfer takes 2 ms on the clock, with the node's lock held.
// With GHOST_WISPR_TEST_AS_RECORDER set, it opens the light as the recorder
// does (OpenBlink1). It prints how many reads it made, how many were not red
// and how many could not read the light (unknown: ErrBusy or ErrOtherReply),
// tells the simulation it is done, then waits for its input to end before it
// exits, so its exit signals no process that is in a request.
func lightStateSim() int {
	iterations, err := strconv.Atoi(os.Getenv("GHOST_WISPR_TEST_ITERATIONS"))
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	clock, err := newPipeClock()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "start:", err)
		return 1
	}
	lockClock = clock
	transfer := featureIOCTL
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		lockClock.Sleep(2 * time.Millisecond)
		return transfer(fd, request, report)
	}
	open := OpenBlink1ForCommand
	if os.Getenv("GHOST_WISPR_TEST_AS_RECORDER") != "" {
		open = OpenBlink1
	}
	reads, notRed, unknown := 0, 0, 0
	for range iterations {
		light, err := open("/sys", os.Getenv("GHOST_WISPR_TEST_DEV_ROOT"))
		if Contended(err) {
			unknown++
			continue
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "open:", err)
			return 1
		}
		r, g, b, err := light.ReadRGB()
		_ = light.Close()
		if Contended(err) {
			unknown++
			continue
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "read:", err)
			return 1
		}
		reads++
		if r != 255 || g != 0 || b != 0 {
			notRed++
			_, _ = fmt.Fprintf(os.Stderr, "light-state read %d,%d,%d\n", r, g, b)
		}
	}
	fmt.Println(reads, notRed, unknown)
	clock.done()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// uhidLockOrderRun is what the recorder and light-state processes did on a
// uhid blink(1), on a lock simulation's clock.
type uhidLockOrderRun struct {
	reads, notRed, unknown int    // the recorder's reads
	first                  string // the recorder's first read that was not red
	held                   int    // the recorder's polls: times it found the node's lock held
	loops                  []string
}

// uhidLockOrderLoops is how many light-state processes a uhid lock order test
// runs, and uhidLockOrderIterations how many times each runs light-state.
const (
	uhidLockOrderLoops      = 4
	uhidLockOrderIterations = 30
)

// runUhidLockOrder runs, on a lock simulation from seed, a recorder that
// tickles the watchdog of a uhid blink(1) and reads its red light back, over
// and over, while uhidLockOrderLoops other processes run light-state
// (lightStateSim) uhidLockOrderIterations times each, back to back, started
// at instants in the first 5 ms. Each transfer takes 2 ms on the clock, with
// the node's lock held; the uhid blink(1) answers at once.
func runUhidLockOrder(t *testing.T, seed uint64, asRecorder bool) uhidLockOrderRun {
	t.Helper()
	if !strings.Contains(os.Getenv("GODEBUG"), "asyncpreemptoff=1") {
		t.Fatal("run with GODEBUG=asyncpreemptoff=1: a preemption signal fails a uhid request with EIO")
	}
	device := newUhidBlink1Delay(t, 0)
	// The light is red from the start, so a read that is not red can only be
	// another command's reply.
	device.mu.Lock()
	device.colour = [3]byte{255, 0, 0}
	device.mu.Unlock()
	devRoot, _ := device.node(t)
	transfer := featureIOCTL
	t.Cleanup(func() { featureIOCTL = transfer })
	featureIOCTL = func(fd uintptr, request uintptr, report *[8]byte) error {
		lockClock.Sleep(2 * time.Millisecond) // a USB control transfer: the sender holds the lock meanwhile
		return transfer(fd, request, report)
	}
	sim := newLockSim(t, seed, lockOrderLateness)

	type loop struct {
		command *exec.Cmd
		stdin   io.Closer
		stdout  *bufio.Reader
		stderr  *strings.Builder
	}
	var loops []loop
	var actors []*simActor
	t.Cleanup(func() {
		for _, l := range loops {
			_ = l.command.Process.Kill()
			_ = l.command.Wait()
		}
	})
	for i := range uhidLockOrderLoops {
		command := exec.Command(os.Args[0], "-test.run=^$")
		command.Env = append(os.Environ(), otherProcessEnv+"=light-state-sim", "GHOST_WISPR_TEST_DEV_ROOT="+devRoot, fmt.Sprint("GHOST_WISPR_TEST_ITERATIONS=", uhidLockOrderIterations))
		if asRecorder {
			command.Env = append(command.Env, "GHOST_WISPR_TEST_AS_RECORDER=1")
		}
		stdin, err := command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := command.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stderr := &strings.Builder{}
		command.Stderr = stderr
		actor, err := sim.Remote(fmt.Sprintf("light-state %d", i+1), false, sim.Offset(5*time.Millisecond), command)
		if err != nil {
			t.Fatal(err)
		}
		loops = append(loops, loop{command, stdin, bufio.NewReader(stdout), stderr})
		actors = append(actors, actor)
	}

	var run uhidLockOrderRun
	recorderActor := sim.Go("recorder", true, 0, func() {
		// Opened here, its exchanges run on the simulation's clock only.
		opened, err := OpenBlink1("/sys", devRoot)
		if err != nil {
			sim.fail("open the uhid blink(1): %v", err)
			return
		}
		recorder := opened.(*blink1)
		defer func() { _ = recorder.Close() }()
		// The recorder lights red as it starts recording; a write that does
		// not get the light is tried again.
		for err := recorder.SetRGB(255, 0, 0); err != nil; err = recorder.SetRGB(255, 0, 0) {
			if !Contended(err) {
				sim.fail("the recorder's red write: %v", err)
				return
			}
		}
		for !sim.Ended(actors) {
			if err := recorder.ServerDown(3 * time.Second); err != nil {
				sim.fail("tickle: %v", err)
				return
			}
			r, g, b, err := recorder.ReadRGB()
			if Contended(err) {
				run.unknown++
				continue
			}
			if err != nil {
				sim.fail("the recorder's read: %v", err)
				return
			}
			run.reads++
			if r != 255 || g != 0 || b != 0 {
				if run.notRed++; run.first == "" {
					run.first = fmt.Sprintf("%d,%d,%d", r, g, b)
				}
			}
		}
	})
	sim.Run(time.Minute)
	run.held = sim.Sleeps(recorderActor, recorderPoll)
	for _, l := range loops {
		line, _ := l.stdout.ReadString('\n')
		_ = l.stdin.Close()
		if err := l.command.Wait(); err != nil {
			t.Fatalf("light-state process: %v: %s", err, l.stderr)
		}
		run.loops = append(run.loops, strings.TrimSpace(line))
	}
	return run
}

// TestUhidARecorderReadIsNotAnsweredByLightState fails if the recorder's read
// of its red light, or a `ghost-wispr light-state` run meanwhile, is answered
// by the other's command on a real hidraw node: on the office Pi the recorder
// read 0,51,51, light-state's version reply, and light-state read 1,1,44, the
// recorder's watchdog tickle. Here the recorder tickles the watchdog and reads
// the light back, over and over, while four other processes run light-state
// 30 times each, back to back. A light-state read may then not get the light
// within its wait: it reports unknown, which is allowed. A colour other than
// red is not, and the recorder, which goes first, must get the light each time:
// an unknown read drops a buffer of audio (review of #79).
//
// Run on the real clock, the recorder's order was only as good as the
// machine's timers: at load 690 to 2,240 on sami-devbox the recorder's read
// got no light 4 of 383 times, as on main. So the recorder and the
// light-state processes run on a lock simulation (lockSim, pipeClock), which
// wakes them in the order their waits give; the flock and the transfers are
// still the kernel's. Control: with light-state processes that wait and poll
// as the recorder does (OpenBlink1), the recorder's reads lose the light.
//
// uhid fails a request with EIO when a signal ends its wait for the reply,
// and Go's runtime preempts goroutines with signals, so the test needs
// GODEBUG=asyncpreemptoff=1. A USB blink(1)'s requests do not wait on signals.
func TestUhidARecorderReadIsNotAnsweredByLightState(t *testing.T) {
	for _, seed := range lockOrderSeeds[:2] {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			run := runUhidLockOrder(t, seed, false)
			t.Logf("the recorder read its red light %d times, %d not red, %d unknown, found it held %d times; light-state (reads, not red, unknown): %s", run.reads, run.notRed, run.unknown, run.held, strings.Join(run.loops, "; "))
			if run.notRed > 0 {
				t.Errorf("the recorder read its red light as not red %d of %d times, first %s", run.notRed, run.reads, run.first)
			}
			if run.unknown > 0 {
				t.Errorf("the recorder's read got no light %d of %d times while light-state ran, want 0", run.unknown, run.reads+run.unknown)
			}
			for _, output := range run.loops {
				var reads, notRed, unknown int
				if _, err := fmt.Sscan(output, &reads, &notRed, &unknown); err != nil || reads+unknown != uhidLockOrderIterations || reads < 20 {
					t.Errorf("light-state process printed %q, want %d runs, at least 20 of them reads", output, uhidLockOrderIterations)
				} else if notRed > 0 {
					t.Errorf("light-state read the red light as not red %d of %d times", notRed, reads)
				}
			}
			if !t.Failed() && (run.reads < 20 || run.held == 0) {
				t.Fatalf("control: the recorder read the light %d times and found it held %d times while light-state ran, want at least 20 and once: too few to show anything", run.reads, run.held)
			}
		})
		t.Run(fmt.Sprint("control: light-state polling as the recorder does, seed ", seed), func(t *testing.T) {
			run := runUhidLockOrder(t, seed, true)
			t.Logf("the recorder read its red light %d times, %d not red, %d unknown, found it held %d times; light-state (reads, not red, unknown): %s", run.reads, run.notRed, run.unknown, run.held, strings.Join(run.loops, "; "))
			if run.unknown == 0 {
				t.Fatal("with light-state processes that poll as the recorder does, every read of the recorder got the light: the test cannot see a lost lock order")
			}
		})
	}
}
