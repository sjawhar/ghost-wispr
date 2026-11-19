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
	// minSearchWords is the fewest words worth a search.
	minSearchWords = 3
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

// readTarget is what a turn asks the record for: "keys:" and the keys it
// names, the last first, which a read tries in that order ("the last 30
// days" reads as LAST-30 too, so a key that names no issue gives way to the
// one before it); else "search:<words>"; else nothing.
func readTarget(words string) string {
	if keys := issueKeysIn(words); len(keys) > 0 {
		slices.Reverse(keys)
		keys = slices.Compact(keys)
		return "keys:" + strings.Join(keys[:min(len(keys), maxKeys)], ",")
	}
	if words = strings.Join(strings.Fields(words), " "); len(strings.Fields(words)) >= minSearchWords {
		return "search:" + words
	}
	return ""
}

// read is one read of the record for a target. complete is set when it ran
// to its end, not stopped or replaced; record is then what it found.
type read struct {
	done     chan struct{}
	record   string
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

// Heard tells the Concierge the turn so far in exchange x. It returns at
// once; the read runs in the background and ends when ctx, the exchange's
// context, ends with a stop. With no Dispatch it reads nothing.
func (c *Concierge) Heard(ctx context.Context, x *voice.Exchange, words string) {
	target := readTarget(words)
	if target == "" || ctx.Err() != nil || c.records == nil {
		return
	}
	exchange := *x
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closedLocked(exchange.SessionID) {
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
		record := c.readRecord(readCtx, &exchange, target)
		if readCtx.Err() == nil {
			r.record, r.complete = record, true
		}
		close(r.done)
	}()
}

// readFor is the record Answer gives the model for turn, and whether it came
// from a search rather than a key the turn named: the read Heard made of
// exactly what the final turn asks, which is final once it ran to its end,
// else a read made now.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string) (record string, searched bool) {
	target := readTarget(turn)
	if target == "" {
		return "", false
	}
	searched = strings.HasPrefix(target, "search:")
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
				return pending.record, searched
			}
		case <-ctx.Done():
			return "", searched
		}
	}
	return c.readRecord(ctx, x, target), searched
}

// readRecord reads target with the tools, as the model would call them, and
// writes each call and its result on a line. For keys, the first key that
// names an issue is read with its holder, and the reads of the others are
// left out, so the model is not told of an issue nobody asked about. For a
// search, the hits and the top hit's issue are read, and no holder: the hit
// may not be what the question is about, so it makes no session askable. It
// is empty when nothing was found.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string) string {
	tools := &runner{c: c, x: x}
	var lines []string
	call := func(name string, args map[string]any, keepErrors bool) map[string]any {
		result, err := tools.call(ctx, name, args)
		if err != nil || (!keepErrors && result["error"] != nil) {
			return nil
		}
		encodedArgs, _ := json.Marshal(args)
		encoded, _ := json.Marshal(result)
		lines = append(lines, fmt.Sprintf("%s(%s) = %s", name, encodedArgs, encoded))
		return result
	}
	switch {
	case strings.HasPrefix(target, "keys:"):
		for _, key := range strings.Split(strings.TrimPrefix(target, "keys:"), ",") {
			if ctx.Err() != nil {
				break
			}
			if call("read_issue", map[string]any{"key": key}, false) != nil {
				call("issue_holder", map[string]any{"key": key}, true)
				break
			}
		}
	case strings.HasPrefix(target, "search:"):
		found := call("search_issues", map[string]any{"query": strings.TrimPrefix(target, "search:")}, true)
		if issues, ok := found["issues"].([]map[string]any); ok && len(issues) > 0 {
			if key, ok := issues[0]["key"].(string); ok {
				call("read_issue", map[string]any{"key": key}, false)
			}
		}
	}
	return strings.Join(lines, "\n")
}
