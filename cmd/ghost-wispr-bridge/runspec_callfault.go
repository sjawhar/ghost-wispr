//go:build callfault

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

// The bridge's faults of slice 3 (plan C8).
const (
	faultFakePi                = "fake_pi"
	faultProbeNetwork          = "probe_network"
	faultExitBridge            = "exit_bridge"
	faultChatCheckAlwaysPasses = "chat_check_always_passes"
	faultNoticeJoinOnly        = "notice_join_only"
	faultLongLines             = "long_lines"
)

var knownFaults = []string{
	faultFakePi, faultProbeNetwork, faultExitBridge,
	faultChatCheckAlwaysPasses, faultNoticeJoinOnly, faultLongLines,
}

// runSpecLongest is the longest run: the end time is at most this long
// after the bridge's start.
const runSpecLongest = 2 * time.Hour

// runSpecLogLevel is the only log level of the test pool's worker in a run
// without fake_pi: a run that carries no Pi's audio may log more.
const runSpecLogLevel = "ERROR"

// runSpec is the test pool's run spec (plan C8), which the bridge reads once
// at its start from BRIDGE_RUN_SPEC_FILE. The bridge refuses a field it does
// not know at the top level; worker and upkeep belong to the other
// containers, and the bridge reads only the worker's log level.
type runSpec struct {
	MeetURL     string          `json:"meet_url"`
	EndTime     time.Time       `json:"end_time"`
	Fault       faultNames      `json:"fault"`
	FaultAfterS int             `json:"fault_after_s"`
	Worker      json.RawMessage `json:"worker"`
	Upkeep      json.RawMessage `json:"upkeep"`
}

// faultNames is the run spec's fault: one name, or a list of names, so one
// run can name several faults (contract A4).
type faultNames []string

func (f *faultNames) UnmarshalJSON(b []byte) error {
	var one *string
	if err := json.Unmarshal(b, &one); err == nil {
		*f = nil
		if one != nil && *one != "" {
			*f = faultNames{*one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("fault is neither a name nor a list of names")
	}
	*f = many
	return nil
}

func (r *runSpec) has(fault string) bool { return slices.Contains(r.Fault, fault) }

// runSpecRefusal is why the bridge refuses its run spec: a fixed reason for
// the log line, never a value the run spec or its path supplied (C9).
type runSpecRefusal struct{ reason string }

func refuse(reason string) *runSpecRefusal { return &runSpecRefusal{reason: reason} }

// readRunSpec reads the run spec at path for a bridge that starts at now,
// and refuses one that is missing, unreadable or malformed, one whose end
// time has passed or is more than 2 hours away, and one that asks for a
// fault this build lacks or a log level that only fake_pi may have.
func readRunSpec(path string, now time.Time) (runSpec, *runSpecRefusal) {
	var spec runSpec
	if path == "" {
		return spec, refuse("missing") // BRIDGE_RUN_SPEC_FILE is not set
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return spec, refuse("missing")
	}
	if err != nil {
		return spec, refuse("unreadable")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, refuse("malformed") // not one valid JSON object of the run spec's fields
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return spec, refuse("malformed") // more than one JSON value
	}
	if !isMeetLink(spec.MeetURL) {
		return spec, refuse("meet_url") // not an https://meet.google.com/ link
	}
	switch {
	case spec.EndTime.IsZero():
		return spec, refuse("end_time")
	case !now.Before(spec.EndTime):
		return spec, refuse("expired")
	case spec.EndTime.Sub(now) > runSpecLongest:
		return spec, refuse("too_long")
	}
	for _, f := range spec.Fault {
		if !slices.Contains(knownFaults, f) {
			return spec, refuse("unknown_fault") // this build does not support a requested fault
		}
	}
	if spec.has(faultExitBridge) && (spec.FaultAfterS <= 0 || spec.FaultAfterS > int(spec.EndTime.Sub(now)/time.Second)) {
		return spec, refuse("fault_after_s") // exit_bridge needs fault_after_s above 0 and within the run
	}
	level, err := spec.workerLogLevel()
	if err != nil {
		return spec, refuse("malformed")
	}
	if level != runSpecLogLevel && !spec.has(faultFakePi) {
		return spec, refuse("log_level") // a log_level other than ERROR needs fake_pi
	}
	return spec, nil
}

// workerLogLevel is the worker's log_level, ERROR when unset.
func (r *runSpec) workerLogLevel() (string, error) {
	var w struct {
		LogLevel *string `json:"log_level"`
	}
	if len(r.Worker) > 0 {
		if err := json.Unmarshal(r.Worker, &w); err != nil {
			return "", fmt.Errorf("worker: %w", err)
		}
	}
	if w.LogLevel == nil {
		return runSpecLogLevel, nil
	}
	return *w.LogLevel, nil
}

// isMeetLink reports whether s is a Google Meet link over HTTPS.
func isMeetLink(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host == "meet.google.com" && len(u.Path) > 1
}

// runSpecCalendar is the test bridge's calendar: one event, the run spec's
// Meet link from the bridge's start to the run spec's end time, so the
// bridge's own join rules apply to it. Its record (done, joins) lives in the
// bridge's memory.
type runSpecCalendar struct {
	mu    sync.Mutex
	event calendar.Event
}

// newRunSpecCalendar holds spec's event from start. The event's id comes
// from the end time, so a restarted bridge of the same run finds its bot's
// event again.
func newRunSpecCalendar(spec *runSpec, start time.Time) *runSpecCalendar {
	return &runSpecCalendar{event: calendar.Event{
		ID:      "run-spec-" + strconv.FormatInt(spec.EndTime.Unix(), 10),
		MeetURL: spec.MeetURL,
		Start:   start,
		End:     spec.EndTime,
	}}
}

// Events gives the event when it overlaps [from, to), as the Calendar API's
// timeMin and timeMax do.
func (c *runSpecCalendar) Events(_ context.Context, from, to time.Time) ([]calendar.Event, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.event.End.After(from) && c.event.Start.Before(to) {
		return []calendar.Event{c.event}, nil
	}
	return nil, nil
}

// MarkDone marks the run's event done. It has no other event: the event of
// an earlier run's bot is gone, so that bot's end is done at once.
func (c *runSpecCalendar) MarkDone(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.event.ID {
		return errEventGone
	}
	c.event.Done = true
	return nil
}

func (c *runSpecCalendar) AddJoin(_ context.Context, id string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if id != c.event.ID {
		return 0, errEventGone
	}
	c.event.Joins++
	return c.event.Joins, nil
}
