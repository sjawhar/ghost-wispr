package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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
	// prefetchWait bounds how long Answer waits for a read still running.
	prefetchWait = 2 * time.Second
	// prefetchTimeout bounds one read.
	prefetchTimeout = 10 * time.Second
	// minSearchWords is the fewest words worth a search.
	minSearchWords = 3
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

// readTarget is what a turn asks the record for: "key:ABC-12" for the last
// issue it names, else "search:<words>", else nothing.
func readTarget(words string) string {
	if keys := issueKeysIn(words); len(keys) > 0 {
		return "key:" + keys[len(keys)-1]
	}
	if words = strings.Join(strings.Fields(words), " "); len(strings.Fields(words)) >= minSearchWords {
		return "search:" + words
	}
	return ""
}

// read is one read of the record for a target.
type read struct {
	done   chan struct{}
	record string
	cancel context.CancelFunc
}

// prefetch is the record read for one exchange while its asker speaks. Each
// new target replaces the read still running for an older one, which the
// words since have made stale: at most one read runs.
type prefetch struct {
	at      time.Time
	reads   map[string]*read
	current string
}

// Heard tells the Concierge the turn so far in exchange x. It returns at
// once; the read runs in the background.
func (c *Concierge) Heard(x *voice.Exchange, words string) {
	target := readTarget(words)
	if target == "" {
		return
	}
	exchange := *x
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, p := range c.prefetches {
		if now.Sub(p.at) > namedFor {
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
	ctx, cancel := context.WithTimeout(context.Background(), prefetchTimeout)
	r := &read{done: make(chan struct{}), cancel: cancel}
	p.reads[target], p.current = r, target
	go func() {
		defer cancel()
		r.record = c.readRecord(ctx, &exchange, target)
		close(r.done)
	}()
}

// readFor is the record Answer gives the model for turn: the read Heard made
// of exactly what the final turn asks, waited for at most prefetchWait, else
// a read made now.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string) string {
	target := readTarget(turn)
	if target == "" {
		return ""
	}
	c.mu.Lock()
	var pending *read
	if p, ok := c.prefetches[exchangeKey(x)]; ok {
		pending = p.reads[target]
	}
	c.mu.Unlock()
	if pending != nil {
		wait, cancel := context.WithTimeout(ctx, prefetchWait)
		defer cancel()
		select {
		case <-pending.done:
			if pending.record != "" {
				return pending.record
			}
		case <-wait.Done():
			if ctx.Err() != nil {
				return ""
			}
		}
	}
	return c.readRecord(ctx, x, target)
}

// readRecord reads target with the tools, as the model would call them, and
// writes each call and its result on a line. A read of an issue that fails
// (a number said that names no issue) is left out, so the model is not told
// of an issue nobody asked about. It is empty when nothing was found.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string) string {
	tools := &runner{c: c, x: x}
	var lines []string
	call := func(name string, args map[string]any) map[string]any {
		result, err := tools.call(ctx, name, args)
		if err != nil {
			return nil
		}
		encodedArgs, _ := json.Marshal(args)
		encoded, _ := json.Marshal(result)
		lines = append(lines, fmt.Sprintf("%s(%s) = %s", name, encodedArgs, encoded))
		return result
	}
	key := ""
	switch {
	case strings.HasPrefix(target, "key:"):
		key = strings.TrimPrefix(target, "key:")
	case strings.HasPrefix(target, "search:"):
		found := call("search_issues", map[string]any{"query": strings.TrimPrefix(target, "search:")})
		if issues, ok := found["issues"].([]map[string]any); ok && len(issues) > 0 {
			key, _ = issues[0]["key"].(string)
		}
	}
	searched := strings.Join(lines, "\n")
	if key == "" {
		return searched
	}
	issue := call("read_issue", map[string]any{"key": key})
	if issue == nil || issue["error"] != nil {
		return searched
	}
	call("issue_holder", map[string]any{"key": key})
	return strings.Join(lines, "\n")
}
