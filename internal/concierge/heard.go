package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// Reading the record while the asker speaks (plan stage 4): the Supervisor
// calls Heard with the turn so far on every interim and final result, and the
// issue it names, or the best hit of a search for its words, is read in the
// background. Answer then hands the model what was read with the turn, so the
// first fact takes one model call and no tool round.

const (
	// prefetchTimeout bounds one read.
	prefetchTimeout = 10 * time.Second
	// searchSettle is how long a turn's search words must stay the same
	// before Heard searches them: each new word changes them, and every
	// search reaches Dispatch and its embedding model (R6).
	searchSettle = 400 * time.Millisecond
	// maxKeys is how many of a turn's keys a read tries.
	maxKeys = 3
)

// spokenKey is an issue key as a transcript writes it: ABC-12, or "infra
// 4592" as said aloud. A single digit is too often a plain number.
var spokenKey = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*)[- ](\d{2,})\b`)

// issueKeysIn lists the issue keys words name, in order, upper-cased.
func issueKeysIn(words string) []string {
	var keys []string
	for _, match := range spokenKey.FindAllStringSubmatch(words, -1) {
		if key, err := dispatch.NormalizeIssueKey(match[1] + "-" + match[2]); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

// read is one read of the record for a target. complete is set when it ran
// to its end, not stopped or replaced; found is then what it found. seen is
// closed once its issue's holder is known, which early then holds. A search
// read waits for its words to settle, or for start, which an answer closes
// (begin) when it needs the read now.
type read struct {
	done      chan struct{}
	found     *reading
	complete  bool
	cancel    context.CancelFunc
	seen      chan struct{}
	early     *reading
	once      sync.Once
	start     chan struct{}
	startOnce sync.Once
}

// begin starts r now if it waits for its words to settle.
func (r *read) begin() {
	r.startOnce.Do(func() { close(r.start) })
}

// reading is what a read of the record found for a turn.
type reading struct {
	// text is each call and its result on a line: the record the model gets,
	// and the source R8's check holds a fast answer to.
	text string
	// searched: the issue came from a search, not a key the turn named.
	searched bool
	// noResult: a search found no issue.
	noResult bool
	// clear: the search had one clear match (R5).
	clear bool
	// key, title and status name the chosen issue; empty when none was read.
	key, title, status string
	// candidates are a search's top two issues, best first.
	candidates []candidate
	// holder is issue_holder's answer for it; nil when not read.
	holder map[string]any
	// newest is the chosen issue's newest event; zero when it has none.
	newest time.Time
	// unreachable: Dispatch did not answer.
	unreachable bool
	// ahead: the read-ahead made this reading while the asker spoke.
	ahead bool
}

// candidate is an issue a search offers, by key and title.
type candidate struct{ key, title string }

// prefetch is the record read for one exchange while its asker speaks. Each
// new target replaces the read still running for an older one, which the
// words since have made stale: at most one read runs. searches is how many
// searches the read-ahead made since the exchange's last answer: at most one.
type prefetch struct {
	at       time.Time
	reads    map[string]*read
	current  string
	searches int
}

// cancelAll ends every read of p still running.
func (p *prefetch) cancelAll() {
	for _, r := range p.reads {
		r.cancel()
	}
}

// readTarget is what a turn asks the record for:
//   - "keys:K1,K2|terms" when it names issue keys of Dispatch's projects, the
//     last first, which a read tries in that order, then searches terms when
//     none names an issue;
//   - "search:terms" when it names none;
//   - nothing when its words name no work either.
//
// terms is searchTerms's, the fewest words. With wait, Dispatch's projects
// are read first when they are not known; Heard, which must not wait, uses
// what is known.
func (c *Concierge) readTarget(ctx context.Context, words string, wait bool) string {
	var keys []string
	for _, key := range issueKeysIn(words) {
		project, _, _ := strings.Cut(key, "-")
		if c.isProject(ctx, project, wait) {
			keys = append(keys, key)
		}
	}
	terms := searchTerms(words)
	if len(keys) > 0 {
		slices.Reverse(keys)
		keys = slices.Compact(keys)
		return "keys:" + strings.Join(keys[:min(len(keys), maxKeys)], ",") + "|" + terms
	}
	if terms != "" {
		return "search:" + terms
	}
	return ""
}

// Heard tells the Concierge the turn so far in exchange x. It returns at
// once; the read runs in the background and ends when ctx, the exchange's
// context, ends with a stop. With no Dispatch it reads nothing.
func (c *Concierge) Heard(ctx context.Context, x *voice.Exchange, words string) {
	if ctx.Err() != nil || c.records == nil {
		return
	}
	target := c.readTarget(ctx, words, false)
	if target == "" {
		return
	}
	exchange := *x
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if closed, _ := c.closedLocked(exchange.SessionID); closed {
		return
	}
	for key, p := range c.prefetches {
		if now.Sub(p.at) > namedFor {
			p.cancelAll()
			delete(c.prefetches, key)
		}
	}
	key := exchangeKey(&exchange)
	p := c.prefetchLocked(&exchange)
	if p == nil {
		p = &prefetch{at: now, reads: map[string]*read{}}
		c.prefetches[key] = p
	}
	if _, read := p.reads[target]; read {
		return
	}
	// A search waits for its words to settle, and the read-ahead makes one
	// a turn: every search reaches Dispatch and its embedding model (R6).
	search := strings.HasPrefix(target, "search:")
	if search && p.searches > 0 {
		return
	}
	if running, ok := p.reads[p.current]; ok {
		select {
		case <-running.done:
		default:
			running.cancel()
			delete(p.reads, p.current)
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, prefetchTimeout)
	r := &read{done: make(chan struct{}), seen: make(chan struct{}), cancel: cancel, start: make(chan struct{})}
	p.reads[target], p.current = r, target
	settle := c.searchSettle
	go func() {
		defer cancel()
		defer close(r.done)
		if search {
			select {
			case <-time.After(settle):
			case <-r.start:
			case <-readCtx.Done():
				return
			}
			c.mu.Lock()
			counted := p.searches == 0 && readCtx.Err() == nil
			if counted {
				p.searches++
			}
			c.mu.Unlock()
			if !counted {
				return
			}
		}
		found := c.readRecord(readCtx, &exchange, target, r.heldBy)
		if readCtx.Err() == nil {
			found.ahead = true
			r.found, r.complete = found, true
		}
	}()
}

// heldBy keeps the issue and holder a read learned before it ended, for an
// "ask" turn that need not wait for the rest.
func (r *read) heldBy(early *reading) {
	r.once.Do(func() {
		r.early = early
		close(r.seen)
	})
}

// readFor is what the record holds for turn: the read Heard made of exactly
// what the final turn asks, which is final once it ran to its end, else a
// read made now. ctx, the answer's, bounds both.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string, onHolder func(*reading)) *reading {
	target := c.readTarget(ctx, turn, true)
	if target == "" {
		// A key-shaped word with no list of projects to tell it by: Dispatch
		// did not answer (R10).
		if len(issueKeysIn(turn)) > 0 && c.projectsDownNow() {
			return &reading{unreachable: true}
		}
		return &reading{}
	}
	c.mu.Lock()
	var pending *read
	if p := c.prefetchLocked(x); p != nil {
		pending = p.reads[target]
	}
	c.mu.Unlock()
	if pending != nil {
		// The answer needs the read now: a search still settling starts.
		pending.begin()
		if onHolder != nil {
			go func() {
				select {
				case <-pending.seen:
					onHolder(pending.early)
				case <-pending.done:
					select {
					case <-pending.seen:
						onHolder(pending.early)
					default:
					}
				case <-ctx.Done():
				}
			}()
		}
		select {
		case <-pending.done:
			if pending.complete {
				return pending.found
			}
		case <-ctx.Done():
			return &reading{}
		}
	}
	return c.readRecord(ctx, x, target, onHolder)
}

// prefetchLocked is exchange x's read-ahead. An exchange whose wake came
// before the recording session existed got its id only later, so its reads
// from before then are under no session id: they move to the id it has now.
// c.mu is held.
func (c *Concierge) prefetchLocked(x *voice.Exchange) *prefetch {
	key := exchangeKey(x)
	if p, ok := c.prefetches[key]; ok || x.SessionID == "" {
		return p
	}
	early := exchangeKey(&voice.Exchange{WakeAt: x.WakeAt})
	p, ok := c.prefetches[early]
	if !ok {
		return nil
	}
	delete(c.prefetches, early)
	c.prefetches[key] = p
	return p
}

// takeAheadSearches is how many searches x's read-ahead made since its last
// answer, which count toward this answer's two (R6); the count starts over.
func (c *Concierge) takeAheadSearches(x *voice.Exchange) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.prefetchLocked(x)
	if p == nil {
		return 0
	}
	n := p.searches
	p.searches = 0
	return n
}

// readRecord reads target with the tools, as the model would call them. For
// keys, the first key that names an issue is read with its holder, and the
// reads of the others are left out, so the model is not told of an issue
// nobody asked about; when none names one, the terms are searched. For a
// search, the top hit's issue is read with its holder, and whether it is one
// clear match (R5) is kept: the voice names the issue it took when it is not,
// and asks its holder nothing unasked. A failure other than not-found, such as
// Dispatch out of reach, ends the read.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string, onHolder func(*reading)) *reading {
	tools := &runner{c: c, x: x}
	found := &reading{}
	var lines []string
	// record keeps name's call and result for the model and R8's check.
	record := func(name string, args, result map[string]any, keepErrors bool) map[string]any {
		if result["error"] == cannotReachDispatch {
			found.unreachable = true
		}
		if !keepErrors && result["error"] != nil {
			return result
		}
		encodedArgs, _ := json.Marshal(args)
		encoded, _ := json.Marshal(result)
		lines = append(lines, fmt.Sprintf("%s(%s) = %s", name, encodedArgs, encoded))
		return result
	}
	call := func(name string, args map[string]any, keepErrors bool) map[string]any {
		result, err := tools.call(ctx, name, args)
		if err != nil {
			return nil
		}
		return record(name, args, result, keepErrors)
	}
	// readIssue reads key and its holder, and reports whether key names an
	// issue; a failure other than not-found ends the read. The holder needs
	// only the key, so it is read beside the issue, and onHolder hears it
	// at once: a slow spec read holds no question to the holder back.
	readIssue := func(key string) (named, stop bool) {
		holderArgs := map[string]any{"key": key}
		searched, clear := found.searched, found.clear
		held := make(chan map[string]any, 1)
		go func() {
			holder, err := (&runner{c: c, x: x}).call(ctx, "issue_holder", holderArgs)
			if err != nil {
				holder = nil
			}
			if holder != nil && holder["error"] == nil && onHolder != nil {
				onHolder(&reading{key: key, holder: holder, searched: searched, clear: clear})
			}
			held <- holder
		}()
		issue := call("read_issue", map[string]any{"key": key}, false)
		holder := <-held
		switch {
		case issue == nil:
			return false, true
		case issue["error"] == nil:
			found.key, _ = issue["key"].(string)
			found.title, _ = issue["title"].(string)
			found.status, _ = issue["status"].(string)
			found.newest = tools.newest[found.key]
			if holder != nil && record("issue_holder", holderArgs, holder, true)["error"] == nil {
				found.holder = holder
			}
			return true, true
		default:
			return false, issue["error"] != noSuchIssue
		}
	}
	search := func(terms string) {
		found.searched = true
		hits := call("search_issues", map[string]any{"query": terms}, true)
		issues, _ := hits["issues"].([]map[string]any)
		if hits == nil || hits["error"] != nil {
			return
		}
		if len(issues) == 0 {
			found.noResult = true
			return
		}
		found.clear, _ = hits["clear_match"].(bool)
		for _, issue := range issues[:min(2, len(issues))] {
			key, _ := issue["key"].(string)
			title, _ := issue["title"].(string)
			found.candidates = append(found.candidates, candidate{key: key, title: title})
		}
		if key, ok := issues[0]["key"].(string); ok && ctx.Err() == nil {
			readIssue(key)
		}
	}
	switch {
	case strings.HasPrefix(target, "keys:"):
		list, terms, _ := strings.Cut(strings.TrimPrefix(target, "keys:"), "|")
		for _, key := range strings.Split(list, ",") {
			if ctx.Err() != nil {
				break
			}
			if named, stop := readIssue(key); named || stop {
				found.text = strings.Join(lines, "\n")
				return found
			}
		}
		if terms != "" && ctx.Err() == nil {
			search(terms)
		}
	case strings.HasPrefix(target, "search:"):
		search(strings.TrimPrefix(target, "search:"))
	}
	found.text = strings.Join(lines, "\n")
	return found
}
