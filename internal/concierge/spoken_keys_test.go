package concierge

import "testing"

// TestASpelledOutNameIsSearchedAsOneWord (the office Pi on 2026-11-24): a
// name said letter by letter reaches the transcript as separate letters,
// letter names and digit words ("ex v six"), which the search dropped as too
// short or searched as a plain word ("six port"). It fails if such a run is
// not searched as the one word it spells, or if words that spell nothing
// change.
func TestASpelledOutNameIsSearchedAsOneWord(t *testing.T) {
	for turn, want := range map[string]string{
		"what's happening with the ex v six port?": "xv6 port",
		"What's happening with the x v 6 port?":    "xv6 port",
		"what's happening with the ex-v-six port?": "xv6 port",
		"what's happening with the XV6 port?":      "xv6 port",
		"how is the g p u budget":                  "gpu budget",
		// No spelled name: a letter name alone, digit words alone, a key.
		"my ex boss asked about six ports":   "boss six ports",
		"is one or two ports ready":          "ports",
		"what changed on INFRA-4592 today":   "",
		"what is the status of infra 4592 v": "",
	} {
		if got := searchTerms(turn); got != want {
			t.Errorf("searchTerms(%q) = %q, want %q", turn, got, want)
		}
	}
}

// TestDidYouMeanSaysEachKeyAsItIsSpoken (the office Pi on 2026-11-24): the
// question which issue said each key as written and the start of each title,
// 9 to 11 s of speech; the text-to-speech voice spelled "INFRA" letter by
// letter and said each digit. It fails unless the question says each key as
// people say one, its project as a word when it reads as one, else letter by
// letter, and its number in pairs, and nothing else.
func TestDidYouMeanSaysEachKeyAsItIsSpoken(t *testing.T) {
	for _, tc := range []struct {
		a, b candidate
		want string
	}{
		{candidate{"INFRA-1568", "Parser evals: first answers"}, candidate{"INFRA-1597", "Parser eval contracts"},
			"Did you mean Infra fifteen sixty-eight, or Infra fifteen ninety-seven?"},
		{candidate{"INFRA-323", "Overnight tune-up"}, candidate{"INFRA-2829", "Six committed statements"},
			"Did you mean Infra three twenty-three, or Infra twenty-eight twenty-nine?"},
		{candidate{"ABC-12", "Realtime voice"}, candidate{"ABC-13", "Wake word"},
			"Did you mean A B C twelve, or A B C thirteen?"},
		{candidate{"TOPAZ-1005", "Boot"}, candidate{"MOVA-1", "State"},
			"Did you mean Topaz ten oh five, or Mova one?"},
		{candidate{"ARST-1500", "Proofs"}, candidate{"INFRA-2000", "Lights"},
			"Did you mean A R S T fifteen hundred, or Infra two thousand?"},
	} {
		got := didYouMean([]candidate{tc.a, tc.b})
		if got != tc.want {
			t.Errorf("didYouMean(%s, %s) = %q, want %q", tc.a.key, tc.b.key, got, tc.want)
		}
		if reason := earForm(got); reason != "" {
			t.Errorf("didYouMean(%s, %s) = %q fails the ear's form: %s", tc.a.key, tc.b.key, got, reason)
		}
	}
	if got, want := bareNumberConfirmation([]candidate{{"INFRA-4592", "Ghost Wispr"}}), "Did you mean Infra forty-five ninety-two?"; got != want {
		t.Errorf("bareNumberConfirmation = %q, want %q", got, want)
	}
}
