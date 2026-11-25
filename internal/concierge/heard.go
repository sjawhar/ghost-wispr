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
	"github.com/sjawhar/ghost-wispr/internal/wake/transcript"
)

// Reading the record while the asker speaks (plan stage 4): the Supervisor
// calls Heard with the turn so far on every interim and final result, and the
// issue it names, or the best hit of a search for its words, is read in the
// background. Answer then hands the model what was read with the turn, so the
// first fact takes one model call and no tool round.

const (
	// prefetchTimeout bounds one read.
	prefetchTimeout = 10 * time.Second
	// searchSettle is how long a turn's question must stay the same before
	// Heard searches it: each new word changes it, and every search reaches
	// Dispatch and its embedding model (R6).
	searchSettle = 400 * time.Millisecond
	// maxKeys is how many of a turn's keys a read tries.
	maxKeys = 3
)

// spokenKey is an issue key as a transcript writes it: ABC-12, "infra 4592"
// as said aloud, or "infra 10/27", a number said in two groups that smart
// formatting writes as a date. A single digit is too often a plain number
// ("room 2 is free"); only the turn's view reads one (issueKeys).
var spokenKey = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*)[- ](\d+(?:/\d+)+|\d{2,})\b`)

// keyNumber is an issue number after its project: after a pause ("INFRA?
// 4592."), split by a thousands comma, a decimal point or a date's slash
// ("INFRA 4,592", "INFRA 45.92", "INFRA 10/27"), or in words ("INFRA four
// thousand five hundred and ninety two", "INFRA forty-five ninety-two"). Its
// group 1 is the number in digits, group 2 in words. A number of one digit
// is a key only of a project the turn's view holds (writtenKey).
const keyNumber = `[?.,:;!]?[\s-]+(?:(\d+(?:[.,/]\d+)+|\d+)\b|((?:(?i:zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand)\b[\s-]*(?:(?i:and)\s+)?)+))`

// keytermKey is an issue key as Deepgram writes one whose project is a
// keyterm, so in capitals, with its number as keyNumber reads it.
var keytermKey = regexp.MustCompile(`\b([A-Z][A-Z0-9]+)` + keyNumber)

// spelledKey is an issue key whose project is said letter by letter ("A B C
// thirteen", "A. B. C. 12"), as the voice says a project that reads as no
// word. It is a key only when the letters name a project Dispatch knows
// (readSpelledKeys): "I O U twenty dollars" is speech.
var spelledKey = regexp.MustCompile(`\b([A-Z](?:\.?\s+[A-Z])+\.?)` + keyNumber)

// spelledProject is what a project said letter by letter leaves between its
// letters.
var spelledProject = strings.NewReplacer(" ", "", ".", "", "\t", "")

// keyGroups joins the groups of an issue number Deepgram wrote split.
var keyGroups = strings.NewReplacer(",", "", ".", "", "/", "")

// writtenKey is the key a match of re (keytermKey or spelledKey) reads, in
// its written form, "INFRA-4592", and the space the match took after it;
// before and after are the turn's text around the match. ok is false for a
// number of one digit unless held reports true for the key's project and the
// key is written as one ("INFRA-7") or said as a bare issue reference
// (transcript.BareIssueReference: "what about INFRA seven", not "I BET 5
// dollars"); and for one said in words that may be no number: "one", as
// often a pronoun ("the INFRA one"), or a word before "oh", the start of a
// longer number ("INFRA one oh five").
func writtenKey(re *regexp.Regexp, before, match, after string, held func(string) bool) (key, trail string, ok bool) {
	at := re.FindStringSubmatchIndex(match)
	group := func(i int) string {
		if at[2*i] < 0 {
			return ""
		}
		return match[at[2*i]:at[2*i+1]]
	}
	digits, said, numberAt := keyGroups.Replace(group(2)), group(3), at[4]
	if said != "" {
		digits, numberAt = keyDigits(said), at[6]
		if strings.HasSuffix(said, " ") {
			trail = " "
		}
	}
	project := spelledProject.Replace(group(1))
	written := match[at[3]:numberAt] == "-"
	next, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(after)), " ")
	switch {
	case digits == "":
		return "", "", false
	case len(digits) >= 2:
	case !held(project),
		!written && !transcript.BareIssueReference(before, after),
		said != "" && (strings.EqualFold(strings.TrimSpace(said), "one") || strings.Trim(next, ",.;:?!") == "oh"):
		return "", "", false
	}
	return project + "-" + digits, trail, true
}

// rewriteKeys rewrites each match of re in words with write, which gets the
// text before the match, the match and the text after it.
func rewriteKeys(re *regexp.Regexp, words string, write func(before, match, after string) string) string {
	var b strings.Builder
	last := 0
	for _, at := range re.FindAllStringIndex(words, -1) {
		b.WriteString(words[last:at[0]])
		b.WriteString(write(words[:at[0]], words[at[0]:at[1]], words[at[1]:]))
		last = at[1]
	}
	b.WriteString(words[last:])
	return b.String()
}

// spokenKeys rewrites each key keytermKey reads to its written form,
// "INFRA-4592", so the rest of the turn reads as before. A number of one
// digit is a key only when the view holds its project and the turn names it
// as a bare issue reference ("what about INFRA seven" is INFRA-7); else it
// stays as said. The zero view holds none.
func (v projectsView) spokenKeys(words string) string {
	return rewriteKeys(keytermKey, words, func(before, match, after string) string {
		if key, trail, ok := writtenKey(keytermKey, before, match, after, v.holds); ok {
			return key + trail
		}
		return match
	})
}

// readSpelledKeys rewrites each key of words whose project is said letter by
// letter (spelledKey) to its written form, "ABC-13", when the view holds the
// project; other letters stay as said. unread reports a run of letters that
// could be a key while the view cannot tell (the list never read, or its read
// failed): such a turn is neither a pick nor words to search. Letters before
// a number of one digit are such a run only when the view holds their
// project and the turn names it as a bare issue reference: "I O U 5
// dollars" is speech. A turn goes through it once, where it enters the
// Concierge (Heard, answer), so every reading of the turn after sees the
// key.
func (v projectsView) readSpelledKeys(words string) (read string, unread bool) {
	read = rewriteKeys(spelledKey, words, func(before, match, after string) string {
		key, trail, ok := writtenKey(spelledKey, before, match, after, v.holds)
		if !ok {
			return match
		}
		project, _, _ := strings.Cut(key, "-")
		switch {
		case !v.sure:
			unread = true
		case v.holds(project):
			return key + trail
		}
		return match
	})
	return read, unread
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

// issueKeysIn lists the issue keys of two or more digits words name, in
// order, upper-cased, as a reading with no view of the project list does
// (issueKeys reads one of one digit too).
func issueKeysIn(words string) []string {
	var keys []string
	for _, match := range spokenKey.FindAllStringSubmatch(projectsView{}.spokenKeys(words), -1) {
		if key, err := dispatch.NormalizeIssueKey(match[1] + "-" + keyGroups.Replace(match[2])); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

// anyDigitsKey is spokenKey with a number of any length, "INFRA-7" and
// "infra 7" too. Group 3 is a decimal or thousands part after the number
// ("6.8", "7,500"): a number so written is no key of one digit.
var anyDigitsKey = regexp.MustCompile(`\b([A-Za-z][A-Za-z0-9]*)[- ](\d+(?:/\d+)+|\d+)((?:[.,]\d+)*)\b`)

// oneDigitKey reports whether m, a match of anyDigitsKey in text, is a key
// of one digit the view reads: its number one digit with no decimal or
// thousands part, its project held, and the key written as one ("INFRA-7")
// or said as a bare issue reference (transcript.BareIssueReference): "what
// about infra 7", not "the infra 3 times".
func (v projectsView) oneDigitKey(text string, m []int) bool {
	if len(keyGroups.Replace(text[m[4]:m[5]])) != 1 || m[6] != m[7] || !v.holds(text[m[2]:m[3]]) {
		return false
	}
	return text[m[3]] == '-' || transcript.BareIssueReference(text[:m[0]], text[m[1]:])
}

// issueKeys is the issue keys words name as the turn's view reads them, in
// order, upper-cased: every key issueKeysIn reads, and a key of one digit
// the view reads (oneDigitKey: "INFRA-7", "what about infra 7", "what about
// INFRA seven"). A number of one digit after any other word is speech ("the
// last 7 days", "room 2 is free"), as is one with a decimal or thousands
// part ("Linux 6.8", "infra 7,500") and one inside other speech ("I bet 5").
func (v projectsView) issueKeys(words string) []string {
	var keys []string
	text := v.spokenKeys(words)
	for _, m := range anyDigitsKey.FindAllStringSubmatchIndex(text, -1) {
		number := keyGroups.Replace(text[m[4]:m[5]])
		if len(number) < 2 && !v.oneDigitKey(text, m) {
			continue
		}
		if key, err := dispatch.NormalizeIssueKey(text[m[2]:m[3]] + "-" + number); err == nil {
			keys = append(keys, key)
		}
	}
	return keys
}

// oneDigitKeys is the keys of one digit the view reads in words
// (oneDigitKey), upper-cased.
func (v projectsView) oneDigitKeys(words string) map[string]bool {
	keys := map[string]bool{}
	text := v.spokenKeys(words)
	for _, m := range anyDigitsKey.FindAllStringSubmatchIndex(text, -1) {
		if !v.oneDigitKey(text, m) {
			continue
		}
		if key, err := dispatch.NormalizeIssueKey(text[m[2]:m[3]] + "-" + keyGroups.Replace(text[m[4]:m[5]])); err == nil {
			keys[key] = true
		}
	}
	return keys
}

// withoutOneDigitKeys is words with each key of one digit the view reads
// (oneDigitKey) taken out, with the possessive said after it ("INFRA-7's"),
// so the turn's search words hold no such key's project, as they hold none
// of a longer key's (searchTerms, possessiveKey). A project word and one
// digit that are speech stay: "I bet 5" searches "bet".
func (v projectsView) withoutOneDigitKeys(words string) string {
	text := v.spokenKeys(words)
	var b strings.Builder
	last := 0
	for _, m := range anyDigitsKey.FindAllStringSubmatchIndex(text, -1) {
		if v.oneDigitKey(text, m) {
			b.WriteString(text[last:m[0]])
			b.WriteString(" ")
			last = m[1] + len(possessiveTail.FindString(text[m[1]:]))
		}
	}
	b.WriteString(text[last:])
	return b.String()
}

// possessiveTail is the possessive said after a key ("INFRA-7's").
var possessiveTail = regexp.MustCompile(`^['’]s\b`)

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
	// slow: a search ran past its bound (searchBound). Neither an issue nor
	// a Dispatch outage; the room hears that the search is slow.
	slow bool
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
	// noKey: each key the read tried named no issue; a read that failed
	// does not say so.
	noKey bool
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
//   - "keys:K1,K2|question|terms" when it names issue keys of Dispatch's
//     projects, the last first, which a read tries in that order, then
//     searches when none names an issue;
//   - "search:question|terms" when it names none;
//   - nothing when its words name no work either.
//
// question and terms are what the search sends Dispatch, read with the
// turn's view of the projects: the question for its full search
// (searchQuestion), the words that name work for its keyword search
// (searchTerms); both empty when it sends nothing. A read-ahead's search is
// reused for the same target alone, so for the same two.
func (c *Concierge) readTarget(v projectsView, words string) string {
	var keys []string
	for _, key := range v.issueKeys(words) {
		project, _, _ := strings.Cut(key, "-")
		if v.holds(project) {
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
	// A key of one digit the view reads is read, not searched: the turn's
	// search holds no such key's words, as it holds none of a longer key's.
	rest := v.withoutOneDigitKeys(words)
	question := v.searchQuestion(rest)
	search := question + "|" + searchTerms(rest)
	if len(keys) > 0 {
		slices.Reverse(keys)
		keys = slices.Compact(keys)
		return "keys:" + strings.Join(keys[:min(len(keys), maxKeys)], ",") + "|" + search
	}
	if question != "" {
		return "search:" + search
	}
	return ""
}

// aheadTarget is what the read-ahead reads of words, the turn so far: what
// readTarget reads, but no key of one digit (Main's rule after Sol's review
// of #194 at e5cf4d6). Every read ahead of one, held for the turn's end or
// cancelled and read again for a late transcript, read an issue the room did
// not mean (Sol's R2 to R5); main reads none at all. A key of one digit is
// read only by the answer, from the turn's final words (readTarget). Words
// that hold one are read ahead for their other keys alone, as main reads
// them, and are not searched, since the answer reads the key first; with no
// other key nothing is read ahead. Words with no key of one digit are read
// as main reads them.
func (c *Concierge) aheadTarget(v projectsView, words string) string {
	target := c.readTarget(v, words)
	oneDigit := v.oneDigitKeys(words)
	if len(oneDigit) == 0 {
		return target
	}
	var keys []string
	for _, key := range strings.Split(keysOf(target), ",") {
		if key != "" && !oneDigit[key] {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	return "keys:" + strings.Join(keys, ",")
}

// searchOf is what target's search sends Dispatch: the question for its
// full search and the words for its keyword search; both empty when target
// searches nothing. Neither keys nor the words that name work hold a "|".
func searchOf(target string) (question, terms string) {
	rest, ok := strings.CutPrefix(target, "search:")
	if !ok {
		if rest, ok = strings.CutPrefix(target, "keys:"); ok {
			_, rest, ok = strings.Cut(rest, "|")
		}
	}
	i := strings.LastIndex(rest, "|")
	if !ok || i < 0 {
		return "", ""
	}
	return rest[:i], rest[i+1:]
}

// keysOf is the keys a keys target reads, "K1,K2"; "" for any other target.
func keysOf(target string) string {
	keys, ok := strings.CutPrefix(target, "keys:")
	if !ok {
		return ""
	}
	keys, _, _ = strings.Cut(keys, "|")
	return keys
}

// searchTarget is a keys target's search alone, "search:question|terms", for
// a turn whose keys name no issue.
func searchTarget(target string) string {
	_, search, _ := strings.Cut(strings.TrimPrefix(target, "keys:"), "|")
	return "search:" + search
}

// keyRead is p's read ahead of keys, which reads them alone, and how it
// ended: named, one of them named an issue; none, each named none. Both are
// false while it runs, and when it failed, was stopped or was taken by an
// answer (spent): then the answer reads the keys again. c.mu is held.
func (p *prefetch) keyRead(keys string) (r *read, named, none bool) {
	r = p.reads["keys:"+keys]
	if r == nil || !ended(r) {
		return r, false, false
	}
	return r, r.complete && r.found.key != "" && !r.found.searched, r.complete && r.found.noKey
}

// ended reports whether r has ended.
func ended(r *read) bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// spent stands in for each read an answer took: ended and not complete, so
// a later answer reads afresh, and words already read start no read ahead.
// Voices B and C tell the Concierge the unit's final words while it answers
// (found in review on 2026-11-24).
var spent = func() *read {
	r := &read{done: make(chan struct{}), seen: make(chan struct{}), start: make(chan struct{}), cut: make(chan struct{}), cancel: func() {}}
	close(r.done)
	return r
}()

// Heard tells the Concierge the turn so far in exchange x. It returns at
// once; the read runs in the background and ends when ctx, the exchange's
// context, ends with a stop. With no Dispatch it reads nothing.
func (c *Concierge) Heard(ctx context.Context, x *voice.Exchange, words string) {
	if ctx.Err() != nil || c.records == nil {
		return
	}
	// One reading of the project list decides the turn (projectsView). A run
	// of letters that could be a key the list cannot tell yet is neither read
	// nor searched (readSpelledKeys).
	view := c.viewProjects()
	words, unread := view.readSpelledKeys(words)
	if unread {
		return
	}
	target := c.aheadTarget(view, words)
	// A number-only turn (no key, no work words) skips the read-ahead: it
	// goes straight to the bare-number candidate lookup. A turn with words
	// beyond the number keeps its search. A turn whose only key has one
	// digit skips it too: the answer reads that key (aheadTarget).
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
	// The read ahead of keys reads them alone, once (found in review on
	// 2026-11-24): the target holds the turn's search, which each word
	// changes, and a key read's search of the words said so far went
	// uncounted. Once the keys were read, under way, named an issue, failed
	// (the answer reads them again) or were taken by an answer, later words
	// start no read of them. Once each named none, the later words are the
	// turn's search alone, which waits for its words to settle and counts as
	// the read-ahead's one.
	if keys := keysOf(target); keys != "" {
		r, _, none := p.keyRead(keys)
		question, _ := searchOf(target)
		switch {
		case none && question != "":
			target = searchTarget(target)
		case r != nil:
			return
		default:
			target = "keys:" + keys
		}
	}
	if _, read := p.reads[target]; read && !strings.HasPrefix(target, "keys:") {
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
		found := c.readRecord(readCtx, &exchange, view, words, target, r.heldBy, r.cut)
		if readCtx.Err() == nil {
			// Only a counted search is the read-ahead's: a read of keys
			// alone searches nothing.
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
// what the final turn asks (aheadFor), which is final once it ran to its
// end, else a read made now. ctx, the answer's, bounds both; cut, when it
// closes, ends the wait for a full search (searchBoth). view is the turn's
// one reading of the project list.
func (c *Concierge) readFor(ctx context.Context, x *voice.Exchange, view projectsView, turn string, onHolder func(*reading), cut <-chan struct{}) *reading {
	target := c.readTarget(view, turn)
	if target == "" {
		// A key-shaped word the project list cannot tell, with no other
		// words to search: Dispatch did not answer (R10). A written "word
		// NN" among other words may be speech ("the last 30 days"), so
		// those words are searched.
		if view.keyUntold(turn) {
			return &reading{unreachable: true}
		}
		return &reading{noResult: len(bareIssueNumber.FindAllString(turn, -1)) > 0}
	}
	// Voices B and C answer a question in their vendor's words, while the
	// read-ahead read the unit's own transcript of the turn, AskerTurn
	// (found in review on 2026-11-24): its read is looked up as the
	// read-ahead made it (aheadTarget), with no key of one digit.
	asked := ""
	if x.AskerTurn != "" && x.AskerTurn != turn {
		if words, unread := view.readSpelledKeys(x.AskerTurn); !unread {
			asked = c.aheadTarget(view, words)
		}
	}
	c.mu.Lock()
	var pending *read
	searchAgain, sameTerms := false, false
	aheadWords, finalWords, held := 0, 0, 0
	if p := c.prefetchLocked(x); p != nil {
		keys := keysOf(target)
		pending, target = p.aheadFor(target)
		// The unit's own transcript stands in for the vendor's wording when
		// both name the same keys, or the wording names none: a key the
		// vendor heard otherwise is read as it heard it.
		if pending == nil && asked != "" && (keys == "" || keysOf(asked) == keys) {
			pending, _ = p.aheadFor(asked)
		}
		if pending == nil {
			// The answer reads its own target: a read-ahead of other words
			// still waiting or running would search a question no one uses
			// and spend one of the answer's searches.
			if r := p.reads[p.current]; r != nil {
				r.cancel()
			}
		}
		if pending == nil && strings.HasPrefix(p.current, "search:") && strings.HasPrefix(target, "search:") {
			// Counts only, never the words: why the answer searches again.
			// A final turn whose search is not the one the read-ahead sent,
			// by a word, the order of its words or the words that name
			// work, is searched as it is: the read-ahead's search is reused
			// only for what it sent (Main's rule). same_keyword_words says
			// whether the rule before, which compared those words alone in
			// any order, could have reused it.
			aheadQuestion, aheadTerms := searchOf(p.current)
			finalQuestion, finalTerms := searchOf(target)
			ahead, final := strings.Fields(aheadQuestion), strings.Fields(finalQuestion)
			heldWords := set(final)
			for _, word := range ahead {
				if heldWords[word] {
					held++
				}
			}
			aheadSorted, finalSorted := strings.Fields(aheadTerms), strings.Fields(finalTerms)
			slices.Sort(aheadSorted)
			slices.Sort(finalSorted)
			searchAgain, sameTerms, aheadWords, finalWords = true, slices.Equal(aheadSorted, finalSorted), len(ahead), len(final)
		}
	}
	c.mu.Unlock()
	if searchAgain { // logged without c.mu: a log write can wait
		c.logger.Info("concierge searches again: the turn's question is not the read-ahead's", "ahead_words", aheadWords, "final_words", finalWords, "held", held, "same_keyword_words", sameTerms)
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
			switch {
			case !pending.complete:
			case pending.found.noKey:
				// Each key the read ahead tried named none: what is left is
				// the turn's search, made now.
				if question, _ := searchOf(target); question == "" {
					return pending.found
				}
				if keysOf(target) != "" {
					target = searchTarget(target)
				}
			default:
				return pending.found
			}
		case <-ctx.Done():
			return &reading{}
		}
	}
	return c.readRecord(ctx, x, view, turn, target, onHolder, cut)
}

// aheadFor is p's read ahead of what target asks, and the target the answer
// reads if it reads afresh. For keys that is the read of the keys alone,
// running or one that named an issue, or, once each named none, the read of
// the turn's search alone; a read of them that failed is read again. c.mu
// is held.
func (p *prefetch) aheadFor(target string) (*read, string) {
	keys := keysOf(target)
	if keys == "" {
		return p.reads[target], target
	}
	r, named, none := p.keyRead(keys)
	question, _ := searchOf(target)
	switch {
	case named, none && question == "", r != nil && !ended(r):
		return r, target
	case none:
		search := searchTarget(target)
		return p.reads[search], search
	}
	return nil, target
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
	// A later answer reuses only a read made since this one: a reading of
	// the same words this answer took would be the record as it was then.
	for target := range p.reads {
		p.reads[target] = spent
	}
	p.current = ""
	return n
}

// readRecord reads target with the tools, as the model would call them. For
// keys, the first key that names an issue is read with its holder, and the
// reads of the others are left out, so the model is not told of an issue
// nobody asked about; when none names one, target's search, if it has one,
// is made. For a search, the top hit's issue is read with its holder, and
// whether it is one clear match (R5) is kept: with none, the voice asks
// which issue the person meant, and asks its holder nothing. cut, when it
// closes, ends the search's wait for its full search. A failure other than
// not-found, such as Dispatch out of reach, ends the read. view is the
// turn's one reading of the project list, which the search reads its
// question with.
//
// words are the turn's, which target was read from (readTarget), and the
// search is sent them, as the model would send them: search_issues reads
// them once. An offered issue picked is read by its key alone and never
// searched, so its words, the question first asked, are not sent. Read
// again from the question in target, which has lost its punctuation, the
// keyword search lost the end of a description ("the issue about the
// realtime voice, is it close?" searched "realtime voice close"), and the
// full search was not sent the question target holds ("X. V. 6" went as
// "xv6"). Found in review on 2026-11-24.
func (c *Concierge) readRecord(ctx context.Context, x *voice.Exchange, view projectsView, words, target string, onHolder func(*reading), cut <-chan struct{}) *reading {
	tools := &runner{c: c, x: x, view: &view, fullCut: cut}
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
	search := func() {
		found.searched = true
		// The record's line holds the words searched by keyword (searchTerms),
		// not the question: the record is R8's source, where a figure or key
		// of the asker's is no fact (earCheckAsked). With the question in it,
		// "what changed in the last 45 days" let the voice say "45" as the
		// record's. Found in review on 2026-11-24.
		var hits map[string]any
		if result, err := tools.call(ctx, "search_issues", map[string]any{"query": words}); err == nil {
			hits = record("search_issues", map[string]any{"query": searchTerms(view.withoutOneDigitKeys(words))}, result, true)
		}
		issues, _ := hits["issues"].([]map[string]any)
		if hits == nil || hits["error"] != nil {
			return
		}
		if slow, _ := hits["slow"].(bool); slow {
			// A search that ran past its bound is slow, not an outage: the
			// voice says the search is slow (searchIsSlow).
			found.slow = true
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
		for _, key := range strings.Split(keysOf(target), ",") {
			if ctx.Err() != nil {
				break
			}
			if named, stop := readIssue(key); named || stop {
				found.text = strings.Join(lines, "\n")
				return found
			}
		}
		found.noKey = ctx.Err() == nil
		if _, terms := searchOf(target); terms != "" && ctx.Err() == nil {
			search()
		}
	case strings.HasPrefix(target, "search:"):
		search()
	}
	found.text = strings.Join(lines, "\n")
	return found
}
