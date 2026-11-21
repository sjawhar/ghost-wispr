package speakertrace

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"runtime/trace"
	"strings"
	"testing"
	"time"
)

// TestTheReaderReadsWhatTheRunningGoWrites fails if the reader cannot read an
// execution trace the Go running the tests writes, as the speaker writes them
// on the unit: a Go release that changes the trace format then needs a newer
// golang.org/x/exp. CI runs it with the module's Go and with the newest
// stable Go (.github/workflows/ci.yml), so a new format is found before a
// unit's toolchain moves to it.
func TestTheReaderReadsWhatTheRunningGoWrites(t *testing.T) {
	buf := traced(t, func() {
		ctx := context.Background()
		for range 3 {
			trace.WithRegion(ctx, RegionWrite, func() { time.Sleep(5 * time.Millisecond) })
		}
	})
	format := formatOf(buf.Bytes())
	if format == 0 {
		t.Fatalf("control: the trace %s wrote begins %q, not a trace's header", runtime.Version(), buf.Bytes()[:min(16, buf.Len())])
	}
	if _, err := Read(buf, time.Hour); err != nil {
		t.Fatalf("%s writes traces in Go 1.%d's format; the reader reads up to Go 1.%d's and cannot read it: %v", runtime.Version(), format, newestFormat(), err)
	}
}

// TestATraceInANewerFormatSaysWhichFormats fails if a trace in a format newer
// than the reader reads fails with anything but an error that names the
// trace's format and the newest the reader reads, or if a trace that is not
// a trace's header at all is taken for one.
func TestATraceInANewerFormatSaysWhichFormats(t *testing.T) {
	newest := newestFormat()
	if newest < 25 {
		t.Fatalf("control: the reader reads formats up to Go 1.%d's, want at least Go 1.25's, whose clock snapshots it uses", newest)
	}
	tooNew := newest + 1
	_, err := Read(bytes.NewBufferString(fmt.Sprintf(traceHeader, tooNew)), time.Hour)
	if want := fmt.Sprintf("the trace is in Go 1.%d's format, and this reader reads formats up to Go 1.%d's", tooNew, newest); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Read of a Go 1.%d trace = %v, want an error saying %q", tooNew, err, want)
	}
	_, err = Read(bytes.NewBufferString("not a trace at all"), time.Hour)
	if err == nil || strings.Contains(err.Error(), "this reader reads formats up to") {
		t.Fatalf("Read of a file that is no trace = %v, want the reader's own error, not one about formats", err)
	}
}
