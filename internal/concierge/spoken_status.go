package concierge

import (
	"regexp"
	"strings"
)

// spokenStatus is a Dispatch status as the voice says it: an underscore is
// said as the space it stands for, so "needs_review" is "needs review" and
// "in_progress" is "in progress". read_issue and search_issues give the
// model the status in this form, and the voice's own status lines (known,
// agedStatus) say it the same way.
func spokenStatus(status string) string {
	return strings.ReplaceAll(status, "_", " ")
}

// statusToken is a Dispatch status whose name has an underscore, as a whole
// word of a text. Dispatch's other statuses (triage, icebox, backlog, todo,
// testing, retro, done) are words already.
var statusToken = regexp.MustCompile(`\b(?:in_progress|needs_review)\b`)

// statusesInWords is text with each status token in it said in words
// (spokenStatus): "moved to needs_review" is "moved to needs review". The
// texts the tools give the model, and an agent's answer the voice speaks,
// go through it, so no status token reaches the room.
func statusesInWords(text string) string {
	return statusToken.ReplaceAllStringFunc(text, spokenStatus)
}
