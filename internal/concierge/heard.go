package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
// to its end, not stopped or replaced; found is then what it found.
type read struct {
	done     chan struct{}
	found    *reading
	complete bool
	cancel   context.CancelFunc
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
	// key and title name the chosen issue; empty when none was read.
	key, title string
	// holder is issue_holder's answer for it; nil when not read.
	holder map[string]any
	// newest is the chosen issue's newest event; zero when it has none.
	newest time.Time
	// unreachable: Dispatch did not answer.
	unreachable bool
}

// prefetch is the record read for one exchange while its asker speaks. Each
// new target replaces the read still running for an older one, which the
// words since have made stale: at most one read runs.
type prefetch struct {
	at      time.Time
	reads   map[string]*read
	current string
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
	p, ok := c.prefetches[key]
	if !ok {
		p = &prefetch{at: now, reads: map[string]*read{}}
		c.prefetches[key] = p
	}
	if _, read := p.reads[target]; read {
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
	r := &read{done: make(chan struct{}), cancel: cancel}
	p.reads[target], p.current = r, target
	go func() {
		defer cancel()
		found := c.readRecord(readCtx, &exchange, target)
		if readCtx.Err() == nil {
			r.found, r.complete = found, true
		}
		close(r.done)
	}()
}

// readFor is what the record holds for turn: the read Heard made of exactly
// what the final turn asks, which is final once it ran to its end, else a
// read made now, bounded as Heard's are.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string) *reading {
	target := c.readTarget(ctx, turn, true)
	if target == "" {
		return &reading{}
	}
	c.mu.Lock()
	var pending *read
	if p, ok := c.prefetches[exchangeKey(x)]; ok {
		pending = p.reads[target]
	}
	c.mu.Unlock()
	if pending != nil {
		select {
		case <-pending.done:
			if pending.complete {
				return pending.found
			}
		case <-ctx.Done():
			return &reading{}
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, prefetchTimeout)
	defer cancel()
	return c.readRecord(readCtx, x, target)
}

// readRecord reads target with the tools, as the model would call them. For
// keys, the first key that names an issue is read with its holder, and the
// reads of the others are left out, so the model is not told of an issue
// nobody asked about; when none names one, the terms are searched. For a
// search, the top hit's issue is read with its holder, and whether it is one
// clear match (R5) is kept: the voice names the issue it took when it is not,
// and asks its holder nothing unasked. A failure other than not-found, such as
// Dispatch out of reach, ends the read.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string) *reading {
	tools := &runner{c: c, x: x}
	found := &reading{}
	var lines []string
	call := func(name string, args map[string]any, keepErrors bool) map[string]any {
		result, err := tools.call(ctx, name, args)
		if err != nil {
			return nil
		}
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
	// readIssue reads key and its holder, and reports whether key names an
	// issue; a failure other than not-found ends the read.
	readIssue := func(key string) (named, stop bool) {
		issue := call("read_issue", map[string]any{"key": key}, false)
		switch {
		case issue == nil:
			return false, true
		case issue["error"] == nil:
			found.key, _ = issue["key"].(string)
			found.title, _ = issue["title"].(string)
			found.newest = tools.newest[found.key]
			if holder := call("issue_holder", map[string]any{"key": key}, true); holder != nil && holder["error"] == nil {
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
