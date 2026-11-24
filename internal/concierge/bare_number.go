package concierge

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
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
// known Dispatch projects. It issues all reads in parallel under a shared
// deadline and returns the two most recently active, so the person confirms
// a key rather than the Concierge guessing a project.
func (c *Concierge) bareNumberCandidates(ctx context.Context, turn string) []candidate {
	if c.records == nil || strings.HasPrefix(c.readTarget(ctx, turn, true), "keys:") {
		return nil
	}
	numbers := slices.Compact(bareIssueNumber.FindAllString(turn, -1))
	if len(numbers) == 0 {
		return nil
	}
	c.mu.Lock()
	projects := make([]string, 0, len(c.projects))
	for project := range c.projects {
		projects = append(projects, project)
	}
	c.mu.Unlock()
	sort.Strings(projects)
	lookupCtx, cancel := context.WithTimeout(ctx, bareNumberLookupBound)
	defer cancel()
	type result struct {
		candidate numberedCandidate
		ok        bool
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
					results <- result{}
					return
				}
				results <- result{candidate: numberedCandidate{candidate: candidate{key: issue.Key, title: issue.Title}, updated: issue.UpdatedAt}, ok: true}
			}()
		}
	}
	wg.Wait()
	close(results)
	var found []numberedCandidate
	for r := range results {
		if r.ok {
			found = append(found, r.candidate)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].updated.Equal(found[j].updated) {
			return found[i].updated.After(found[j].updated)
		}
		return found[i].key < found[j].key
	})
	candidates := make([]candidate, 0, min(2, len(found)))
	for _, found := range found {
		if !slices.ContainsFunc(candidates, func(candidate candidate) bool { return candidate.key == found.key }) {
			candidates = append(candidates, found.candidate)
		}
		if len(candidates) == 2 {
			break
		}
	}
	return candidates
}
