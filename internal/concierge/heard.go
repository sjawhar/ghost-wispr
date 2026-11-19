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
// to its end, not stopped or replaced; record is then what it found, and
// searched whether it came from a search.
type read struct {
	done     chan struct{}
	record   string
	searched bool
	complete bool
	cancel   context.CancelFunc
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
		record, searched := c.readRecord(readCtx, &exchange, target)
		if readCtx.Err() == nil {
			r.record, r.searched, r.complete = record, searched, true
		}
		close(r.done)
	}()
}

// readFor is the record Answer gives the model for turn, and whether it came
// from a search rather than a key the turn named: the read Heard made of
// exactly what the final turn asks, which is final once it ran to its end,
// else a read made now, bounded as Heard's are.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string) (record string, searched bool) {
	target := c.readTarget(ctx, turn, true)
	if target == "" {
		return "", false
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
				return pending.record, pending.searched
			}
		case <-ctx.Done():
			return "", false
		}
	}
	readCtx, cancel := context.WithTimeout(ctx, prefetchTimeout)
	defer cancel()
	return c.readRecord(readCtx, x, target)
}

// readRecord reads target with the tools, as the model would call them, and
// writes each call and its result on a line, and reports whether it searched.
// For keys, the first key that names an issue is read with its holder, and
// the reads of the others are left out, so the model is not told of an issue
// nobody asked about; when none names one, the terms are searched. A failure
// other than not-found, such as Dispatch out of reach, ends the read. For a
// search, the hits and the top hit's issue are read, and no holder: the hit
// may not be what the question is about, so it makes no session askable. It
// is empty when nothing was found.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string) (string, bool) {
	tools := &runner{c: c, x: x}
	var lines []string
	call := func(name string, args map[string]any, keepErrors bool) map[string]any {
		result, err := tools.call(ctx, name, args)
		if err != nil || (!keepErrors && result["error"] != nil) {
			return result
		}
		encodedArgs, _ := json.Marshal(args)
		encoded, _ := json.Marshal(result)
		lines = append(lines, fmt.Sprintf("%s(%s) = %s", name, encodedArgs, encoded))
		return result
	}
	search := func(terms string) {
		found := call("search_issues", map[string]any{"query": terms}, true)
		if issues, ok := found["issues"].([]map[string]any); ok && len(issues) > 0 {
			if key, ok := issues[0]["key"].(string); ok {
				call("read_issue", map[string]any{"key": key}, false)
			}
		}
	}
	switch {
	case strings.HasPrefix(target, "keys:"):
		list, terms, _ := strings.Cut(strings.TrimPrefix(target, "keys:"), "|")
		for _, key := range strings.Split(list, ",") {
			if ctx.Err() != nil {
				return strings.Join(lines, "\n"), false
			}
			read := call("read_issue", map[string]any{"key": key}, false)
			switch {
			case read == nil:
				return strings.Join(lines, "\n"), false
			case read["error"] == nil:
				call("issue_holder", map[string]any{"key": key}, true)
				return strings.Join(lines, "\n"), false
			case read["error"] != noSuchIssue:
				return strings.Join(lines, "\n"), false
			}
		}
		if terms == "" || ctx.Err() != nil {
			return strings.Join(lines, "\n"), false
		}
		search(terms)
		return strings.Join(lines, "\n"), true
	case strings.HasPrefix(target, "search:"):
		search(strings.TrimPrefix(target, "search:"))
		return strings.Join(lines, "\n"), true
	}
	return "", false
}
