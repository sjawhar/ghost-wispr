// Sol's twenty-fourth review of #191 at 12c293e
// (https://github.com/sjawhar/ghost-wispr/pull/191#pullrequestreview-6084899163):
// Sol's probes, kept as regression tests, unchanged.

package voice

import "testing"

func TestSol191R24KeyMemoUsesOneProjectSnapshot(t *testing.T) {
	for _, change := range []bool{false, true} {
		name := "stable_control"
		if change {
			name = "refresh_between_getters_then_return"
		}
		t.Run(name, func(t *testing.T) {
			keys := []string{"OPS"}
			calls := 0
			s := &Supervisor{cfg: SupervisorConfig{ProjectKeys: func() []string {
				calls++
				snapshot := keys
				if change && calls == 1 {
					keys = []string{"INFRA"}
				}
				return snapshot
			}}}
			ex := &exchange{}
			m := new(keysMemo)
			const raw = "status of infra forty five ninety two?"
			first := s.keysOfLocked(ex, m, raw)
			keys = []string{"OPS"} // a later refresh returns to the original project set
			second := s.keysOfLocked(ex, m, raw)
			expected := s.withKeys(ex, raw)
			t.Logf("first=%q second=%q current-withKeys=%q memo-key=%q getters=%d", first, second, expected, m.keys, calls)
			if second != expected {
				t.Errorf("cached words %q disagree with unchanged current input %q", second, expected)
			}
		})
	}
}
