package call

import "testing"

// TestTheHighestCommonVersionIsUsed holds R15's rule: both sides use the
// highest link version that both accept, and with none they have no version.
// It fails if Choose takes the first common version, the lowest, one only one
// side accepts, or reports a version when none is common.
func TestTheHighestCommonVersionIsUsed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ours, theirs []int
		want         int
		wantAVersion bool
	}{
		{name: "the plan's case", ours: []int{1, 2}, theirs: []int{2, 3}, want: 2, wantAVersion: true},
		{name: "no common version", ours: []int{1}, theirs: []int{2}, want: 0, wantAVersion: false},
		{name: "the highest, not the first", ours: []int{1, 2, 3}, theirs: []int{1, 2, 3}, want: 3, wantAVersion: true},
		{name: "lists in any order", ours: []int{3, 1, 2}, theirs: []int{2, 1}, want: 2, wantAVersion: true},
		{name: "a version only we accept is not used", ours: []int{1, 5}, theirs: []int{1, 4}, want: 1, wantAVersion: true},
		{name: "a side that names no version", ours: []int{1}, theirs: nil, want: 0, wantAVersion: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Choose(tc.ours, tc.theirs)
			if got != tc.want || ok != tc.wantAVersion {
				t.Errorf("Choose(%v, %v) = %d, %v; want %d, %v", tc.ours, tc.theirs, got, ok, tc.want, tc.wantAVersion)
			}
			back, backOK := Choose(tc.theirs, tc.ours)
			if back != got || backOK != ok {
				t.Errorf("Choose(%v, %v) = %d, %v; the other side chose %d, %v", tc.theirs, tc.ours, back, backOK, got, ok)
			}
		})
	}
}
