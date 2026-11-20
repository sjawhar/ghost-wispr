package voice

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// The room test needs a word-free count of errors in what GPT-Live actually
// said, not the written or spoken fact. The Supervisor logs that count and
// leaves the words out of the journal.
func TestSupervisorLogsSpokenFactErrorCountWithoutWords(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	r := newRigWith(t, newFakeConcierge("ABC-123 is in review."), func(cfg *SupervisorConfig, _ *rig) {
		cfg.Logf = func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		}
	})
	sess := r.ask("what is happening?")
	waitSaid(t, sess)
	sess.events <- Event{Kind: EventFactCheck, Count: 3}
	eventually(t, "spoken fact count", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(strings.Join(logs, "\n"), "GPT-Live spoken fact errors 3")
	})
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "ABC-123") {
			t.Fatalf("fact count log holds words: %q", line)
		}
	}
}
