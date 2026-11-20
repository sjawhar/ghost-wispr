package earcheck

import "testing"

func TestQuestionKeyUsesTranscriptSpokenForms(t *testing.T) {
	for text, want := range map[string]string{
		"What is happening with INFRA-1027?":             "INFRA-1027",
		"What is happening with infra 1027?":             "INFRA-1027",
		"What is happening with infra ten twenty-seven?": "INFRA-1027",
		"There is no issue key here.":                    "",
	} {
		if got := QuestionKey(text); got != want {
			t.Errorf("QuestionKey(%q) = %q, want %q", text, got, want)
		}
	}
}
