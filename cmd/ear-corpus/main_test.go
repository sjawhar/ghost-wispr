package main

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
)

// fakeModel is the Gemini model of the tests. A rewrite for the ear gives
// back the agent's written answer as it is, which passes R8's check against
// it. Marks in a reply steer it: one with "FALLBACK" is rewritten with a
// backtick every time, so the Concierge falls back to the fixed sentence; one
// with "RETRY" is rewritten with a backtick on the first try only. The judge
// answers "no" for a text that holds "LAST", else "yes", unless judgeAnswer
// says what it answers. It keeps each prompt it was sent: asked for the
// rewrites, judged for the judge.
type fakeModel struct {
	mu          sync.Mutex
	asked       []string
	judged      []string
	judgeConfig []*genai.GenerateContentConfig
	models      []string
	judgeAnswer func(text string) string
	// refuse, when set, is the error every call fails with.
	refuse func(prompt string) error
}

func (m *fakeModel) GenerateContentStream(_ context.Context, model string, contents []*genai.Content, cfg *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	prompt := contents[len(contents)-1].Parts[0].Text
	m.mu.Lock()
	m.models = append(m.models, model)
	var answer string
	if cfg.ResponseMIMEType == "application/json" {
		m.judged = append(m.judged, prompt)
		m.judgeConfig = append(m.judgeConfig, cfg)
		switch {
		case m.judgeAnswer != nil:
			answer = m.judgeAnswer(prompt)
		case strings.Contains(prompt, "LAST"):
			answer = `{"conclusion_first":"no"}`
		default:
			answer = `{"conclusion_first":"yes"}`
		}
	} else {
		m.asked = append(m.asked, prompt)
		// The Concierge's prompt is a header line, the written answer, and
		// on a second try a blank line and why the first failed.
		_, written, _ := strings.Cut(prompt, "\n")
		written, _, retry := strings.Cut(written, "\n\n")
		switch {
		case strings.Contains(written, "FALLBACK"), strings.Contains(written, "RETRY") && !retry:
			answer = "Run `make` first."
		default:
			answer = written
		}
	}
	var refused error
	if m.refuse != nil {
		refused = m.refuse(prompt)
	}
	m.mu.Unlock()
	return func(yield func(*genai.GenerateContentResponse, error) bool) {
		if refused != nil {
			yield(nil, refused)
			return
		}
		yield(&genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: genai.NewContentFromText(answer, genai.RoleModel)}}}, nil)
	}
}

// writeReplies writes each text as a .txt file of a new directory, named
// for its index and, when set, its key.
func writeReplies(t *testing.T, keys, texts []string) string {
	t.Helper()
	dir := t.TempDir()
	for i, text := range texts {
		name := string(rune('a'+i/26)) + string(rune('a'+i%26))
		if i < len(keys) && keys[i] != "" {
			name += "-" + keys[i]
		}
		if err := os.WriteFile(filepath.Join(dir, name+".txt"), []byte(text+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// conciergeOn is a config whose Concierge runs model on a Vertex AI provider.
func conciergeOn(model string) string {
	return "providers:\n  vertex: {api: vertex, project: example-project, auth: {kind: google}}\nconcierge:\n  model: vertex/" + model + "\n"
}

// runTool runs the tool on model with args, and a config file of its own
// unless args name one.
func runTool(t *testing.T, model *fakeModel, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	if !containsFlag(args, "-config") {
		path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
		if err := os.WriteFile(path, []byte(conciergeOn("gemini-fake")), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append([]string{"-config", path}, args...)
	}
	var out, errOut bytes.Buffer
	connect := func(context.Context, *config.Config) (concierge.Generator, error) {
		if model == nil {
			return nil, errors.New("no model client in this test")
		}
		return model, nil
	}
	code = run(context.Background(), args, &out, &errOut, connect)
	return code, out.String(), errOut.String()
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}

// TestALinePerReplyAndASummary: each reply of a directory gets a line with
// its index, its tries, whether it passed R8's check and the judge's verdict,
// and a summary follows; all passing and judged conclusion-first exits 0.
func TestALinePerReplyAndASummary(t *testing.T) {
	dir := writeReplies(t, nil, []string{
		"It is merged and deployed.",
		"RETRY It is merged and deployed.",
		"It waits on one review.",
	})
	code, stdout, stderr := runTool(t, &fakeModel{}, "-replies", dir, "-judge")
	want := "reply 1: tries=1 passed=true judge=yes\n" +
		"reply 2: tries=2 passed=true judge=yes\n" +
		"reply 3: tries=1 passed=true judge=yes\n" +
		"summary: replies=3 passed=3 fell_back=0 conclusion_first=3/3 (100.0%) judge_errors=0 result=pass\n"
	if stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	if code != 0 {
		t.Errorf("exit %d, want 0; stderr:\n%s", code, stderr)
	}
}

// invented returns n invented replies, the first marked of which hold mark.
func invented(n, marked int, mark string) []string {
	texts := make([]string, n)
	for i := range texts {
		texts[i] = "It is merged and deployed."
		if i < marked {
			texts[i] = mark + " " + texts[i]
		}
	}
	return texts
}

// TestAFallbackExits1: a reply whose two rewrites both fail R8's check falls
// back to the fixed sentence, and the run exits 1, with or without the judge.
func TestAFallbackExits1(t *testing.T) {
	dir := writeReplies(t, nil, []string{"It is merged.", "FALLBACK It is merged."})
	code, stdout, _ := runTool(t, &fakeModel{}, "-replies", dir, "-judge")
	if !strings.Contains(stdout, "reply 2: tries=2 passed=false judge=") {
		t.Errorf("stdout =\n%s\nwant reply 2 failing after 2 tries", stdout)
	}
	if !strings.Contains(stdout, "summary: replies=2 passed=1 fell_back=1 ") || !strings.HasSuffix(stdout, " result=fail\n") {
		t.Errorf("stdout =\n%s\nwant a failing summary with one fallback", stdout)
	}
	if code != 1 {
		t.Errorf("exit %d with -judge, want 1", code)
	}
	if code, _, _ := runTool(t, &fakeModel{}, "-replies", dir); code != 1 {
		t.Errorf("exit %d without -judge, want 1", code)
	}
}

// TestTwoOfTenJudgedNoExits1: at least 90% must be judged conclusion-first;
// 8 of 10 exits 1, and 9 of 10 exits 0.
func TestTwoOfTenJudgedNoExits1(t *testing.T) {
	code, stdout, _ := runTool(t, &fakeModel{}, "-replies", writeReplies(t, nil, invented(10, 2, "LAST")), "-judge")
	if code != 1 {
		t.Errorf("2 of 10 judged no: exit %d, want 1", code)
	}
	if !strings.Contains(stdout, "reply 1: tries=1 passed=true judge=no\n") || !strings.Contains(stdout, "reply 3: tries=1 passed=true judge=yes\n") {
		t.Errorf("stdout =\n%s\nwant replies 1 and 2 judged no, 3 judged yes", stdout)
	}
	if !strings.Contains(stdout, "summary: replies=10 passed=10 fell_back=0 conclusion_first=8/10 (80.0%) judge_errors=0 result=fail\n") {
		t.Errorf("stdout =\n%s\nwant a failing summary at 8 of 10", stdout)
	}
	if code, stdout, _ := runTool(t, &fakeModel{}, "-replies", writeReplies(t, nil, invented(10, 1, "LAST")), "-judge"); code != 0 {
		t.Errorf("1 of 10 judged no: exit %d, want 0; stdout:\n%s", code, stdout)
	}
}

// TestJudgeOnlyExits0OnlyWhenNineInTenFail: the negative control judges the
// texts as given, with no rewrite, and passes only when at least 90% of them
// are judged not conclusion-first.
func TestJudgeOnlyExits0OnlyWhenNineInTenFail(t *testing.T) {
	model := &fakeModel{}
	code, stdout, stderr := runTool(t, model, "-judge-only", writeReplies(t, nil, invented(10, 9, "LAST")))
	if code != 0 {
		t.Errorf("9 of 10 failing: exit %d, want 0; stderr:\n%s", code, stderr)
	}
	if !strings.HasPrefix(stdout, "text 1: judge=no\n") || !strings.Contains(stdout, "text 10: judge=yes\n") {
		t.Errorf("stdout =\n%s\nwant a line per text with its verdict", stdout)
	}
	if !strings.HasSuffix(stdout, "summary: texts=10 failing=9/10 (90.0%) judge_errors=0 result=pass\n") {
		t.Errorf("stdout =\n%s\nwant a passing summary at 9 of 10 failing", stdout)
	}
	if len(model.asked) != 0 || len(model.judged) != 10 {
		t.Errorf("%d rewrites and %d judged, want none and 10", len(model.asked), len(model.judged))
	}
	code, stdout, _ = runTool(t, &fakeModel{}, "-judge-only", writeReplies(t, nil, invented(10, 8, "LAST")))
	if code != 1 || !strings.HasSuffix(stdout, "summary: texts=10 failing=8/10 (80.0%) judge_errors=0 result=fail\n") {
		t.Errorf("8 of 10 failing: exit %d, stdout:\n%s\nwant 1 and a failing summary", code, stdout)
	}
}

// TestAJudgeWithNoVerdictPassesNothing: an answer that is not the fixed
// shape is no verdict. It counts as no conclusion first in a run, and as no
// failing text in the control, so a broken judge passes neither.
func TestAJudgeWithNoVerdictPassesNothing(t *testing.T) {
	for _, answer := range []string{`{"conclusion_first":"maybe"}`, `yes`, `{"conclusion_first":"no","why":"x"}`, `{"conclusion_first":"no"} {}`, ``} {
		model := &fakeModel{judgeAnswer: func(string) string { return answer }}
		code, stdout, _ := runTool(t, model, "-judge-only", writeReplies(t, nil, invented(10, 10, "LAST")))
		if code != 1 || !strings.Contains(stdout, "text 1: judge=error\n") || !strings.HasSuffix(stdout, "summary: texts=10 failing=0/10 (0.0%) judge_errors=10 result=fail\n") {
			t.Errorf("judge answers %q: -judge-only exit %d, stdout:\n%s\nwant 1, no failing text and 10 judge errors", answer, code, stdout)
		}
	}
	broken := 0
	model := &fakeModel{judgeAnswer: func(string) string {
		broken++
		if broken <= 2 {
			return "yes"
		}
		return `{"conclusion_first":"yes"}`
	}}
	code, stdout, _ := runTool(t, model, "-replies", writeReplies(t, nil, invented(10, 0, "")), "-judge")
	if code != 1 || !strings.Contains(stdout, "conclusion_first=8/10 (80.0%) judge_errors=2 result=fail\n") {
		t.Errorf("two judge errors in 10: exit %d, stdout:\n%s\nwant 1, 8 of 10 and 2 judge errors", code, stdout)
	}
}

// TestNoReplyTextIsPrinted: a reply may be real meeting work, so neither it
// nor its rewrite is printed, on any path: a pass, a retry, a fallback, a
// judge whose answer quotes the text, a model whose refusal quotes it, and
// the control.
func TestNoReplyTextIsPrinted(t *testing.T) {
	const canary = "quokkaplume"
	dir := writeReplies(t, nil, []string{
		"The " + canary + " build is green.",
		"RETRY The " + canary + " build is green.",
		"FALLBACK The " + canary + " build is green.",
	})
	quoting := func(text string) string { return "The text says " + text }
	refusing := func(prompt string) error {
		return genai.APIError{Code: 400, Status: "INVALID_ARGUMENT", Message: "refused: " + prompt}
	}
	for name, tc := range map[string]struct {
		model *fakeModel
		args  []string
	}{
		"rewrite":        {&fakeModel{}, []string{"-replies", dir}},
		"rewrite, judge": {&fakeModel{}, []string{"-replies", dir, "-judge"}},
		"judge quotes":   {&fakeModel{judgeAnswer: quoting}, []string{"-replies", dir, "-judge"}},
		"control":        {&fakeModel{}, []string{"-judge-only", dir}},
		"control quotes": {&fakeModel{judgeAnswer: quoting}, []string{"-judge-only", dir}},
		"model refuses":  {&fakeModel{refuse: refusing}, []string{"-replies", dir, "-judge"}},
		"judge refuses":  {&fakeModel{refuse: refusing}, []string{"-judge-only", dir}},
	} {
		_, stdout, stderr := runTool(t, tc.model, tc.args...)
		if strings.Contains(stdout+stderr, canary) {
			t.Errorf("%s: the output prints a reply's words:\nstdout:\n%s\nstderr:\n%s", name, stdout, stderr)
		}
		// The control of this check: the canary reached the model, so the
		// output could have printed it.
		reached := false
		for _, prompt := range append(tc.model.asked, tc.model.judged...) {
			reached = reached || strings.Contains(prompt, canary)
		}
		if !reached {
			t.Errorf("%s: the canary never reached the model, so this check could not fail", name)
		}
	}
}

// TestRepliesFromJSONL: a JSONL file of {"key","written"} objects is read in
// order, and each key reaches the rewrite as the key the question named,
// which R8 puts in the text. A directory's file names give the key the same
// way. The JSON array of internal/concierge's ear corpus reads too.
func TestRepliesFromJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replies.jsonl")
	lines := `{"key":"ABC-12","written":"It is merged."}` + "\n\n" + `{"key":"","written":"It waits on one review."}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{}
	code, stdout, stderr := runTool(t, model, "-replies", path, "-judge")
	if code != 0 || !strings.Contains(stdout, "summary: replies=2 passed=2 ") {
		t.Fatalf("exit %d, stdout:\n%s\nstderr:\n%s\nwant 2 replies passing", code, stdout, stderr)
	}
	if len(model.judged) != 2 || !strings.Contains(model.judged[0], "ABC-12: It is merged.") || strings.Contains(model.judged[1], "ABC-12") {
		t.Errorf("judged %q, want the first rewrite to hold its key and the second none", model.judged)
	}
	dirModel := &fakeModel{}
	if code, _, _ := runTool(t, dirModel, "-replies", writeReplies(t, []string{"ABC-12"}, []string{"It is merged."}), "-judge"); code != 0 || len(dirModel.judged) != 1 || !strings.Contains(dirModel.judged[0], "ABC-12: It is merged.") {
		t.Errorf("a directory: exit %d, judged %q, want its file name's key in the rewrite", code, dirModel.judged)
	}
	corpus, err := readReplies(filepath.Join("..", "..", "internal", "concierge", "testdata", "ear_replies.json"))
	if err != nil || len(corpus) != 40 || corpus[0].key != "ZORP-12" || corpus[0].text == "" {
		t.Errorf("internal/concierge's corpus: %d replies, %v; want 40, the first keyed ZORP-12", len(corpus), err)
	}
}

// TestUsageAndConfigErrorsExit2: a run that cannot start exits 2, says why
// on stderr and prints no result.
func TestUsageAndConfigErrorsExit2(t *testing.T) {
	dir := writeReplies(t, nil, invented(2, 0, ""))
	scratch := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(scratch, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	badConfig := write("bad.yaml", "indicator_light: lantern\n")
	emptyDir := filepath.Join(scratch, "empty")
	if err := os.Mkdir(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"no input":                  {},
		"both inputs":               {"-replies", dir, "-judge-only", dir},
		"an argument":               {"-replies", dir, "extra"},
		"an unknown flag":           {"-replies", dir, "-judges"},
		"a missing path":            {"-replies", filepath.Join(scratch, "missing")},
		"an empty directory":        {"-replies", emptyDir},
		"an empty file":             {"-judge-only", write("empty.jsonl", "\n")},
		"a bad line":                {"-replies", write("bad.jsonl", `{"key":"ABC-12","written":"It is merged."}`+"\n{\n")},
		"a line with no text":       {"-replies", write("none.jsonl", `{"key":"ABC-12"}`+"\n")},
		"a line with both texts":    {"-replies", write("both.jsonl", `{"written":"It is merged.","reply":"It is merged."}`+"\n")},
		"a line with another field": {"-replies", write("other.jsonl", `{"key":"ABC-12","written":"It is merged.","txt":"x"}`+"\n")},
		"an empty text file":        {"-replies", writeReplies(t, nil, []string{"  "})},
		"a config that fails":       {"-config", badConfig, "-replies", dir},
	} {
		code, stdout, stderr := runTool(t, &fakeModel{}, args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 2, no result and a reason", name, code, stdout, stderr)
		}
	}
	if code, stdout, _ := runTool(t, nil, "-replies", dir); code != 2 || stdout != "" {
		t.Errorf("no model client: exit %d, stdout %q; want 2 and no result", code, stdout)
	}
}

// TestConfigDefaultsToGhostWisprConfig: with no -config, the tool reads the
// unit's config where the recorder does, GHOST_WISPR_CONFIG, and both the
// rewrite and the judge use its concierge.model.
func TestConfigDefaultsToGhostWisprConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit.yaml")
	if err := os.WriteFile(path, []byte(conciergeOn("gemini-of-the-unit")), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvPrefix+"CONFIG", path)
	model := &fakeModel{}
	connect := func(context.Context, *config.Config) (concierge.Generator, error) { return model, nil }
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-replies", writeReplies(t, nil, invented(1, 0, "")), "-judge"}, &stdout, &stderr, connect); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr.String())
	}
	if len(model.models) != 2 || model.models[0] != "gemini-of-the-unit" || model.models[1] != "gemini-of-the-unit" {
		t.Errorf("models called %q, want the rewrite and the judge on gemini-of-the-unit", model.models)
	}
}

// TestTheJudgeAsksAtTemperatureZeroForAFixedShape: the judge's call on a
// Vertex AI provider sets temperature 0 (on an OpenAI API provider it sets
// none, TestTheJudgeRunsOnAnOpenAIAPIProvider) and asks for JSON of one
// field, "yes" or "no", and holds the text it judges.
func TestTheJudgeAsksAtTemperatureZeroForAFixedShape(t *testing.T) {
	model := &fakeModel{}
	if code, _, stderr := runTool(t, model, "-judge-only", writeReplies(t, nil, []string{"It is merged."})); code != 1 {
		t.Fatalf("exit %d, want 1 for a text judged conclusion-first; stderr:\n%s", code, stderr)
	}
	if len(model.judgeConfig) != 1 {
		t.Fatalf("%d judge calls, want 1", len(model.judgeConfig))
	}
	cfg := model.judgeConfig[0]
	if cfg.Temperature == nil || *cfg.Temperature != 0 {
		t.Errorf("temperature %v, want 0", cfg.Temperature)
	}
	schema, _ := cfg.ResponseJsonSchema.(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	field, _ := properties["conclusion_first"].(map[string]any)
	if len(properties) != 1 || field == nil || !slices.Equal(field["enum"].([]any), []any{"yes", "no"}) {
		t.Errorf("schema %v, want one field conclusion_first of yes or no", cfg.ResponseJsonSchema)
	}
	if cfg.SystemInstruction == nil || !strings.Contains(cfg.SystemInstruction.Parts[0].Text, "first sentence") {
		t.Errorf("the judge's instruction does not ask about the first sentence")
	}
	if !strings.Contains(model.judged[0], "It is merged.") {
		t.Errorf("judged %q, want the text as given", model.judged[0])
	}
}

// TestWithoutTheJudge: with no -judge, nothing is judged, and every reply
// passing R8's check exits 0.
func TestWithoutTheJudge(t *testing.T) {
	model := &fakeModel{}
	code, stdout, _ := runTool(t, model, "-replies", writeReplies(t, nil, invented(2, 1, "LAST")))
	want := "reply 1: tries=1 passed=true judge=not-run\n" +
		"reply 2: tries=1 passed=true judge=not-run\n" +
		"summary: replies=2 passed=2 fell_back=0 judge=not-run result=pass\n"
	if code != 0 || stdout != want || len(model.judged) != 0 {
		t.Errorf("exit %d, %d judged, stdout:\n%s\nwant 0, none judged and\n%s", code, len(model.judged), stdout, want)
	}
}

// TestAStoppedRunExits1: a run stopped before its replies are done has no
// result, and exits 1.
func TestAStoppedRunExits1(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model := &fakeModel{}
	connect := func(context.Context, *config.Config) (concierge.Generator, error) { return model, nil }
	path := filepath.Join(t.TempDir(), "ghost-wispr.yaml")
	if err := os.WriteFile(path, []byte(conciergeOn("gemini-fake")), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"-config", path, "-replies", writeReplies(t, nil, invented(2, 0, "")), "-judge"}, &stdout, &stderr, connect); code != 1 || strings.Contains(stdout.String(), "summary:") {
		t.Errorf("exit %d, stdout:\n%s\nwant 1 and no summary", code, stdout.String())
	}
}

// TestTheControlCorpusHoldsTenTexts: testdata/conclusion_last.jsonl is the
// negative control of c-ear-live, ten invented texts with the conclusion put
// last on purpose.
func TestTheControlCorpusHoldsTenTexts(t *testing.T) {
	texts, err := readReplies(filepath.Join("testdata", "conclusion_last.jsonl"))
	if err != nil || len(texts) != 10 {
		t.Fatalf("%d texts, %v; want 10", len(texts), err)
	}
}
