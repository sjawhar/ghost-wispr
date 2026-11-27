package concierge

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
)

// bareIssueNumber finds a number that could be the number part of an issue
// key, but carries no project name. Three digits avoid ordinary small counts.
var bareIssueNumber = regexp.MustCompile(`\b\d{3,}\b`)

// bareNumberLookupBound bounds the whole set of candidate-key reads.
const bareNumberLookupBound = 2 * time.Second

type numberedCandidate struct {
	candidate
	updated time.Time
}

// bareNumberCandidates reads the issues whose number is in turn, across the
// projects of the turn's view. It issues all reads in parallel under a shared
// deadline and returns the two most recently active, so the person confirms
// a key rather than the Concierge guessing a project. untold reports a turn
// whose number it found in no project and could not look up: the unit has
// no Dispatch client (no token or address), no list of Dispatch's projects
// was ever read, or a read failed other than with not-found, or ran past the
// deadline. Dispatch did not answer, so "no issue" is unknown (R10). Reads
// Dispatch answered with not-found, of a cached list whose last refresh
// failed included, are no issue.
func (c *Concierge) bareNumberCandidates(ctx context.Context, view projectsView, turn string) (candidates []candidate, untold bool) {
	if strings.HasPrefix(c.readTarget(view, turn), "keys:") {
		return nil, false
	}
	numbers := slices.Compact(bareIssueNumber.FindAllString(turn, -1))
	if len(numbers) == 0 {
		return nil, false
	}
	if c.records == nil {
		return nil, true
	}
	projects := make([]string, 0, len(view.known))
	for project := range view.known {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	lookupCtx, cancel := context.WithTimeout(ctx, bareNumberLookupBound)
	defer cancel()
	type result struct {
		candidate numberedCandidate
		ok        bool
		failed    bool
	}
	results := make(chan result, len(projects)*len(numbers))
	var wg sync.WaitGroup
	for _, number := range numbers {
		for _, project := range projects {
			wg.Add(1)
			go func() {
				defer wg.Done()
				issue, err := c.records.Issue(lookupCtx, project+"-"+number)
				if err != nil || issue == nil {
					results <- result{failed: err != nil && !dispatch.IsNotFound(err)}
					return
				}
				results <- result{candidate: numberedCandidate{candidate: candidate{key: issue.Key, title: issue.Title}, updated: issue.UpdatedAt}, ok: true}
			}()
		}
	}
	wg.Wait()
	close(results)
	var found []numberedCandidate
	failed := false
	for r := range results {
		if r.ok {
			found = append(found, r.candidate)
		}
		failed = failed || r.failed
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].updated.Equal(found[j].updated) {
			return found[i].updated.After(found[j].updated)
		}
		return found[i].key < found[j].key
	})
	candidates = make([]candidate, 0, min(2, len(found)))
	for _, found := range found {
		if !slices.ContainsFunc(candidates, func(candidate candidate) bool { return candidate.key == found.key }) {
			candidates = append(candidates, found.candidate)
		}
		if len(candidates) == 2 {
			break
		}
	}
	untold = len(candidates) == 0 && (view.known == nil || failed) && ctx.Err() == nil
	return candidates, untold
}
