package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	calendarapi "google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	runapi "google.golang.org/api/run/v2"

	"github.com/sjawhar/ghost-wispr/internal/calendar"
)

// The watcher (mode watch) keeps the call pool's instance count at 1 while an
// event of the bot user's calendar keeps the pool up, and at 0 while none
// does (spec, "The calendar: the watcher and the bridge": "Starting" and "An
// event that got no bot"). Its log lines are plan C9's.
const (
	watchEvery       = 30 * time.Second // a pass every 30 s on the clock
	watchAhead       = 24 * time.Hour   // a pass reads a day ahead, so next_start shows the next day's first event
	watchPassTimeout = 25 * time.Second // a pass's reads, its count change and its read-back end before the next pass is due
)

// watchSettings are the watcher's settings: BRIDGE_ORG_DOMAINS,
// BRIDGE_TESTERS, BRIDGE_CALENDAR_USER and BRIDGE_POOL (the call pool's
// resource name).
type watchSettings struct {
	OrgDomains, Testers []string
	CalendarUser, Pool  string
}

// poolName is the form of a Cloud Run worker pool's resource name.
var poolName = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/workerPools/[^/]+$`)

// watchSettingsFromEnv reads the watch mode's settings. BRIDGE_TESTERS may be
// empty (after the tester period); the others are required. Its error names
// each refused setting as <KEY>=unset or <KEY>=malformed, and never a
// setting's value, so the watch mode logs it whole.
func watchSettingsFromEnv(getenv func(string) string) (watchSettings, error) {
	s := watchSettings{
		OrgDomains:   list(getenv("BRIDGE_ORG_DOMAINS")),
		Testers:      list(getenv("BRIDGE_TESTERS")),
		CalendarUser: strings.TrimSpace(getenv("BRIDGE_CALENDAR_USER")),
		Pool:         strings.TrimSpace(getenv("BRIDGE_POOL")),
	}
	var refused []string
	if len(s.OrgDomains) == 0 {
		refused = append(refused, "BRIDGE_ORG_DOMAINS=unset")
	}
	if s.CalendarUser == "" {
		refused = append(refused, "BRIDGE_CALENDAR_USER=unset")
	}
	switch {
	case s.Pool == "":
		refused = append(refused, "BRIDGE_POOL=unset")
	case !poolName.MatchString(s.Pool):
		refused = append(refused, "BRIDGE_POOL=malformed") // not projects/<project>/locations/<region>/workerPools/<pool>
	}
	if len(refused) > 0 {
		return watchSettings{}, errors.New(strings.Join(refused, " "))
	}
	return s, nil
}

// googleClients is what reaches Google: the metadata server's client and the
// client options of the delegated sign-in, the Calendar API and Cloud Run's
// Admin API. The zero value is production's: Google's own endpoints, with the
// instance's credentials.
type googleClients struct {
	metadata              *metadata.Client
	signIn, calendar, run []option.ClientOption
}

// watchMode runs the watcher until ctx ends: exit 2 for a settings error, 1
// for a failure after the start. Every line it writes is a marked line on
// stdout (plan C9), a refused setting's included; it writes nothing on
// stderr.
func watchMode(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer) int {
	return watchModeWith(ctx, getenv, stdout, stderr, &googleClients{})
}

func watchModeWith(ctx context.Context, getenv func(string) string, stdout, _ io.Writer, g *googleClients) int {
	log := newLogger(stdout, "watcher", slog.LevelInfo)
	s, err := watchSettingsFromEnv(getenv)
	if err != nil {
		log.Error("SETTINGS-REFUSED " + err.Error()) // setting names and fixed reasons alone
		return 2
	}
	deps, step, err := g.watcherDeps(ctx, &s, log)
	if err == nil {
		step, err = "watch", runWatch(ctx, s, deps)
	}
	if err != nil {
		log.Error("WATCHER-FAILED", "step", step, "error", errorClass(err))
		return 1
	}
	return 0
}

// watcherDeps gives the watcher's real deps: the bot user's calendar, read
// only (calendar.readonly) by domain-wide delegation and kept to a call's
// events by the settings, and the call pool through Cloud Run's Admin API
// with the instance's own credentials. On a failure it names the step that
// failed: calendar or cloud_run.
func (g *googleClients) watcherDeps(ctx context.Context, s *watchSettings, log *slog.Logger) (watchDeps, string, error) {
	md := g.metadata
	if md == nil {
		md = metadata.NewClient(nil)
	}
	cal, err := delegatedCalendarVia(ctx, md, calendarapi.CalendarReadonlyScope, s.CalendarUser, g.signIn, g.calendar)
	if err != nil {
		return watchDeps{}, "calendar", err
	}
	run, err := runapi.NewService(ctx, g.run...)
	if err != nil {
		return watchDeps{}, "cloud_run", err
	}
	return watchDeps{
		Calendar: calendar.NewReader(cal, calendar.Filter{OrgDomains: s.OrgDomains, Testers: s.Testers, BotUser: s.CalendarUser}),
		Pool:     newRunPool(run, s.Pool),
		Clock:    realClock{},
		Log:      log,
	}, "", nil
}

// watchDeps is what runWatch works with, so each test gives it fakes.
type watchDeps struct {
	Calendar calendar.Reader
	Pool     poolAPI
	Clock    clock
	Log      *slog.Logger
}

// runWatch makes a pass at once and then one at each watchEvery mark of the
// clock from its start, until ctx ends. A pass's own time does not move the
// marks; a pass that runs past a mark is followed at the next one.
//
//nolint:gocritic // hugeParam: the contract passes watchSettings by value
func runWatch(ctx context.Context, s watchSettings, deps watchDeps) error {
	deps.Log.Info(fmt.Sprintf("WATCHER-START pool=%s org_domains=%d testers=%d", s.Pool, len(s.OrgDomains), len(s.Testers)))
	w := &watcher{deps: &deps, unserved: map[string]bool{}}
	next := deps.Clock.Now()
	for {
		w.pass(ctx)
		now := deps.Clock.Now()
		next = next.Add(watchEvery)
		if next.Before(now) {
			next = next.Add(now.Sub(next).Truncate(watchEvery) + watchEvery)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deps.Clock.After(next.Sub(now)):
		}
	}
}

type watcher struct {
	deps *watchDeps
	// unserved holds the events of the last read that EVENT-UNSERVED named,
	// so each is named once.
	unserved map[string]bool
}

// pass reads the calendar and the pool's count, sets the count the events
// want, names each event that ended unserved, and logs WATCHER-PASS with the
// count Cloud Run reports, read back after a change, whether the change was
// made or not, and the number of done events that keep the pool up. A read
// that fails, of the calendar or of the pool's count, before or after a
// change, logs WATCHER-READ-FAILED and no pass; one before a change changes
// no count, so an outage never stops a call, and "Watcher silent" opens while
// it lasts.
func (w *watcher) pass(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, watchPassTimeout)
	defer cancel()
	now := w.deps.Clock.Now()
	evs, err := w.deps.Calendar.Events(ctx, now.Add(-calendar.PoolHold), now.Add(watchAhead))
	if err != nil {
		w.deps.Log.Warn("WATCHER-READ-FAILED source=calendar status=" + answerStatus(err))
		return
	}
	count, err := w.deps.Pool.Count(ctx)
	if err != nil {
		w.deps.Log.Warn("WATCHER-READ-FAILED source=pool status=" + answerStatus(err))
		return
	}
	if want := wantedCount(evs, now); want != count {
		if err := w.deps.Pool.SetCount(ctx, want); err != nil {
			var permission string
			var perr *poolError
			if errors.As(err, &perr) {
				permission = perr.Permission
			}
			w.deps.Log.Error(fmt.Sprintf("POOL-PATCH-FAILED count=%d status=%s permission=%s", want, answerStatus(err), permission))
		}
		if count, err = w.deps.Pool.Count(ctx); err != nil {
			w.deps.Log.Warn("WATCHER-READ-FAILED source=pool status=" + answerStatus(err))
			return
		}
	}
	w.nameUnserved(evs, now)
	w.deps.Log.Log(ctx, LevelNotice, fmt.Sprintf("WATCHER-PASS count=%d owed=%d next_start=%s events=%s", count, owedCount(evs, now), nextStart(evs, now), eventList(evs)))
}

// wantedCount is 1 when an event keeps the pool up at now, else 0.
func wantedCount(evs []calendar.Event, now time.Time) int64 {
	for i := range evs {
		if calendar.KeepsPoolUp(evs[i], now) {
			return 1
		}
	}
	return 0
}

// owedCount gives the number of done events that keep the pool up at now:
// each still owes the unit a message (a busy, or a failed chat_check, the
// link has not taken), the reason the pool is up with every event done.
func owedCount(evs []calendar.Event, now time.Time) int {
	n := 0
	for i := range evs {
		if evs[i].Done && calendar.KeepsPoolUp(evs[i], now) {
			n++
		}
	}
	return n
}

// nameUnserved logs EVENT-UNSERVED, once, for each event whose end has
// passed with no bot made for it (ghostWisprJoins unset) and not done.
func (w *watcher) nameUnserved(evs []calendar.Event, now time.Time) {
	named := make(map[string]bool, len(w.unserved))
	for i := range evs {
		e := &evs[i]
		switch {
		case w.unserved[e.ID]:
			named[e.ID] = true
		case !now.Before(e.End) && e.Joins == 0 && !e.Done:
			w.deps.Log.Warn("EVENT-UNSERVED " + e.ID)
			named[e.ID] = true
		}
	}
	w.unserved = named
}

// nextStart gives the start of the first event that has not ended, an event
// under way included, or "none".
func nextStart(evs []calendar.Event, now time.Time) string {
	for i := range evs {
		if now.Before(evs[i].End) {
			return evs[i].Start.UTC().Format(time.RFC3339)
		}
	}
	return "none"
}

// eventList gives each event as <id>:<done>:<joins>, comma-separated.
func eventList(evs []calendar.Event) string {
	parts := make([]string, len(evs))
	for i := range evs {
		parts[i] = fmt.Sprintf("%s:%t:%d", evs[i].ID, evs[i].Done, evs[i].Joins)
	}
	return strings.Join(parts, ",")
}

// answerStatus gives the HTTP status Google answered a failed call with, or
// "none" when no answer came (a timeout, a failed sign-in). It is the only
// part of the error a log line carries.
func answerStatus(err error) string {
	var perr *poolError
	if errors.As(err, &perr) {
		return strconv.Itoa(perr.Status)
	}
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return strconv.Itoa(gerr.Code)
	}
	return "none"
}
