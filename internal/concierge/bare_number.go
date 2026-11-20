package concierge

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// bareIssueNumber finds a number that could be the number part of an issue
// key, but carries no project name. Three digits avoid ordinary small counts.
var bareIssueNumber = regexp.MustCompile(`\b\d{3,}\b`)

type numberedCandidate struct {
	candidate
	updated time.Time
}

// bareNumberCandidates reads the issues whose number is in turn, across the
// known Dispatch projects. It returns the two most recently active, so the
// person confirms a key rather than the Concierge guessing a project.
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
	var found []numberedCandidate
	for _, number := range numbers {
		for _, project := range projects {
			issue, err := c.records.Issue(ctx, project+"-"+number)
			if err != nil || issue == nil {
				continue
			}
			found = append(found, numberedCandidate{candidate: candidate{key: issue.Key, title: issue.Title}, updated: issue.UpdatedAt})
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

func bareNumberConfirmation(candidates []candidate) string {
	if len(candidates) == 1 {
		return "Did you mean " + candidates[0].key + "?"
	}
	return didYouMean(candidates)
}
