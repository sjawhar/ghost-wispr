package concierge

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
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

// spokenKey is an issue key as a transcript writes it: ABC-12, "infra 4592"
// as said aloud, or "infra 10/27", a number said in two groups that smart
// formatting writes as a date. A single digit is too often a plain number.
var spokenKey = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*)[- ](\d+(?:/\d+)+|\d{2,})\b`)

// keytermKey is an issue key as Deepgram writes one whose project is a
// keyterm, so in capitals, or is said letter by letter ("A B C", "A. B.
// C."), as the voice says a project that reads as no word: its number after a
// pause ("INFRA? 4592."), split by a thousands comma, a decimal point or a
// date's slash ("INFRA 4,592", "INFRA 45.92", "INFRA 10/27"), or in words
// ("INFRA four thousand five hundred and ninety two", "INFRA forty-five
// ninety-two", "A B C thirteen"). Group 2 is the number in digits, group 3 in
// words.
var keytermKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+|[A-Z](?:\.?\s+[A-Z])+\.?)[?.,:;!]?[\s-]+(?:(\d+(?:[.,/]\d+)+|\d{2,})\b|((?:(?i:zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand)\b[\s-]*(?:(?i:and)\s+)?)+))`)

// spelledProject is what a project said letter by letter leaves between its
// letters.
var spelledProject = strings.NewReplacer(" ", "", ".", "", "\t", "")

// keyGroups joins the groups of an issue number Deepgram wrote split.
var keyGroups = strings.NewReplacer(",", "", ".", "", "/", "")

// spokenKeys rewrites each key keytermKey reads to its written form,
// "INFRA-4592" or "ABC-13", so the rest of the turn reads as before. A
// number of one digit stays as said, so "X V six" is no key.
func spokenKeys(words string) string {
	return keytermKey.ReplaceAllStringFunc(words, func(match string) string {
		parts := keytermKey.FindStringSubmatch(match)
		digits := keyGroups.Replace(parts[2])
		trail := ""
		if parts[3] != "" {
			digits = keyDigits(parts[3])
			if strings.HasSuffix(parts[3], " ") {
				trail = " "
			}
		}
		if len(digits) < 2 {
			return match
		}
		return spelledProject.Replace(parts[1]) + "-" + digits + trail
	})
}

// keyDigits is an issue number said in words, in digits: a number with a
// scale is its value ("four thousand five hundred and ninety two" is 4592);
// one without is its groups in order ("forty-five ninety-two" and "four five
// nine two" are 4592).
func keyDigits(said string) string {
	words := strings.FieldsFunc(strings.ToLower(said), func(r rune) bool { return r == ' ' || r == '-' })
	if slices.Contains(words, "hundred") || slices.Contains(words, "thousand") {
		total, current := 0, 0
		for _, word := range words {
			switch word {
			case "and":
			case "hundred":
				current = max(current, 1) * 100
			case "thousand":
				total, current = total+max(current, 1)*1000, 0
			default:
				current += numberUnits[word]
			}
		}
		return strconv.Itoa(total + current)
	}
	var digits strings.Builder
	for i := 0; i < len(words); i++ {
		value, ok := numberUnits[words[i]]
		if !ok {
			continue // "and"
		}
		if value >= 20 && i+1 < len(words) {
			if unit, ok := numberUnits[words[i+1]]; ok && unit > 0 && unit < 10 {
				value += unit
				i++
			}
		}
		digits.WriteString(strconv.Itoa(value))
	}
	return digits.String()
}

// issueKeysIn lists the issue keys words name, in order, upper-cased.
func issueKeysIn(words string) []string {
	var keys []string
	for _, match := range spokenKey.FindAllStringSubmatch(spokenKeys(words), -1) {
		if key, err := dispatch.NormalizeIssueKey(match[1] + "-" + keyGroups.Replace(match[2])); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

// read is one read of the record for a target. complete is set when it ran
// to its end, not stopped or replaced; found is then what it found. seen is
// closed once its issue's holder is known, which early then holds. A search
// read waits for its words to settle, or for start, which an answer closes
// (begin) when it needs the read now; cut closes (cutNow) when the answer
// stops waiting for its full search.
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
	cut       chan struct{}
	cutOnce   sync.Once
}

// begin starts r now if it waits for its words to settle.
func (r *read) begin() {
	r.startOnce.Do(func() { close(r.start) })
}

// cutNow stops r waiting for its full search (searchBoth).
func (r *read) cutNow() {
	r.cutOnce.Do(func() { close(r.cut) })
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
	if len(keys) == 0 {
		// A number alone names the issue of the agent that joined on it
		// ("ask the 1027 agent", R17).
		if key := c.joinedNumber(words); key != "" {
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
	// A number-only turn (no key, no work words) skips the read-ahead: it
	// goes straight to the bare-number candidate lookup. A turn with words
	// beyond the number keeps its search.
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
	// a turn: every search reaches Dispatch and its embedding model (R6). A
	// reply to "Did you mean" picks; it is not searched.
	search := strings.HasPrefix(target, "search:")
	if _, offered := c.picks[wakeKey(&exchange)]; search && (p.searches > 0 || offered) {
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
	r := &read{done: make(chan struct{}), seen: make(chan struct{}), cancel: cancel, start: make(chan struct{}), cut: make(chan struct{})}
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
		found := c.readRecord(readCtx, &exchange, target, r.heldBy, r.cut)
		if readCtx.Err() == nil {
			// Only a counted search is the read-ahead's: a key that names no
			// issue searches too, uncounted, and the answer counts it.
			found.ahead = search
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
// what the final turn asks, or of a search whose words the turn's all hold,
// which is final once it ran to its end, else a read made now. ctx, the
// answer's, bounds both; cut, when it closes, ends the wait for a full
// search (searchBoth).
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, turn string, onHolder func(*reading), cut <-chan struct{}) *reading {
	target := c.readTarget(ctx, turn, true)
	if target == "" {
		// A key-shaped word with no list of projects to tell it by: Dispatch
		// did not answer (R10).
		if len(issueKeysIn(turn)) > 0 && c.projectsDownNow() {
			return &reading{unreachable: true}
		}
		return &reading{noResult: len(bareIssueNumber.FindAllString(turn, -1)) > 0}
	}
	c.mu.Lock()
	var pending *read
	searchAgain := false
	aheadWords, held := 0, 0
	if p := c.prefetchLocked(x); p != nil {
		pending = p.reads[target]
		switch {
		case pending != nil:
		case wordsHeldBy(p.current, target):
			// The read-ahead searched words the final turn holds: its search,
			// keyword and full, is this answer's (Main's rule).
			pending = p.reads[p.current]
		case strings.HasPrefix(p.current, "search:") && strings.HasPrefix(target, "search:"):
			// Counts only, never the words: why the answer searches again.
			ahead := strings.Fields(strings.TrimPrefix(p.current, "search:"))
			heldWords := set(strings.Fields(strings.TrimPrefix(target, "search:")))
			for _, word := range ahead {
				if heldWords[word] {
					held++
				}
			}
			searchAgain, aheadWords = true, len(ahead)
		}
	}
	c.mu.Unlock()
	if searchAgain { // logged without c.mu: a log write can wait
		c.logger.Info("concierge searches again: the turn holds not all of the read-ahead's words", "ahead_words", aheadWords, "held", held)
	}
	if pending != nil {
		// The answer needs the read now: a search still settling starts, and
		// stops waiting for its full search when the answer does.
		pending.begin()
		go func() {
			select {
			case <-cut:
				pending.cutNow()
			case <-pending.done:
			}
		}()
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
	return c.readRecord(ctx, x, target, onHolder, cut)
}

// wordsHeldBy reports whether ahead, a read-ahead's search target, searched
// words that target, the final turn's search, all holds.
func wordsHeldBy(ahead, target string) bool {
	aheadTerms, ok := strings.CutPrefix(ahead, "search:")
	finalTerms, final := strings.CutPrefix(target, "search:")
	if !ok || !final || aheadTerms == "" {
		return false
	}
	held := set(strings.Fields(finalTerms))
	for _, word := range strings.Fields(aheadTerms) {
		if !held[word] {
			return false
		}
	}
	return true
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
// answer, which count toward this answer's two (R6). The count starts over,
// and a read still waiting or running ends: the answer has its reading.
func (c *Concierge) takeAheadSearches(x *voice.Exchange) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.prefetchLocked(x)
	if p == nil {
		return 0
	}
	n := p.searches
	p.searches = 0
	p.cancelAll()
	// A later answer reuses only a read made since this one.
	p.current = ""
	return n
}

// readRecord reads target with the tools, as the model would call them. For
// keys, the first key that names an issue is read with its holder, and the
// reads of the others are left out, so the model is not told of an issue
// nobody asked about; when none names one, the terms are searched. For a
// search, the top hit's issue is read with its holder, and whether it is one
// clear match (R5) is kept: with none, the voice asks which issue the person
// meant, and asks its holder nothing. cut, when it closes, ends the search's
// wait for its full search. A failure other than not-found, such as
// Dispatch out of reach, ends the read.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, target string, onHolder func(*reading), cut <-chan struct{}) *reading {
	tools := &runner{c: c, x: x, fullCut: cut}
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
		// With no one clear match the voice asks which issue (R5): the top
		// hit's record would go unused.
		if !found.clear {
			return
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
