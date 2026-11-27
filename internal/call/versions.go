package call

import "slices"

// Accepted lists the link versions this build accepts. A change that breaks
// the link adds a new version here and keeps the old one, so whichever side
// updates first still links; a later release drops the old version once both
// sides run the new one.
var Accepted = []int{1}

// Choose returns the highest link version both lists hold, and false when
// they hold none in common. Neither list needs to be sorted.
func Choose(ours, theirs []int) (int, bool) {
	best, found := 0, false
	for _, v := range ours {
		if (!found || v > best) && slices.Contains(theirs, v) {
			best, found = v, true
		}
	}
	return best, found
}

// The builds a hello names: the release build, or the callfault build that
// runs only for a test run.
const (
	BuildRelease   = "release"
	BuildCallfault = "callfault"
)
