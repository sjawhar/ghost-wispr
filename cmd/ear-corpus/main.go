// ear-corpus rewrites agent replies for the ear with the Concierge's live
// model, on the path a holder's answer takes before the voice says it, and
// checks each result: it must pass R8's check within two tries and, with
// -judge, a model judge must find its conclusion in its first sentence.
//
//	go run ./cmd/ear-corpus -replies PATH -judge
//	go run ./cmd/ear-corpus -judge-only cmd/ear-corpus/testdata/conclusion_last.jsonl
//
// PATH is a directory of .txt files, one reply each, whose file name holds
// the issue key the question named (ABC-12.txt), or a JSONL file of
// {"key": "ABC-12", "written": "..."} objects. -judge-only judges the texts
// at its path as given, with no rewrite: the negative control, whose texts
// put the conclusion last on purpose. The model and its credentials come
// from the unit's config (-config, by default $GHOST_WISPR_CONFIG, else
// ghost-wispr.yaml); nothing is written to the unit's database.
//
// It prints no reply text, since a reply may be real meeting work: only each
// reply's index, tries, check and verdict, then a summary. It exits 0 when
// every result passed within two tries and, with -judge, at least 90% were
// judged conclusion-first; with -judge-only, when at least 90% were judged
// not conclusion-first. It exits 1 otherwise, and 2 when it cannot start.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"google.golang.org/genai"

	"github.com/sjawhar/ghost-wispr/internal/concierge"
	"github.com/sjawhar/ghost-wispr/internal/config"
	"github.com/sjawhar/ghost-wispr/internal/providers"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// The exit status: every result passed, some did not, or the tool could
// not start.
const (
	exitPass  = 0
	exitFail  = 1
	exitUsage = 2
)

// earTries is how many rewrites R8 allows before the fixed sentence.
const earTries = 2

// The judge's verdicts; verdictError is no verdict.
const (
	verdictYes   = "yes"
	verdictNo    = "no"
	verdictError = "error"
)

// connectFunc builds the Gemini model client from the unit's config; a test
// passes a fake.
type connectFunc func(ctx context.Context, cfg *config.Config) (concierge.Generator, error)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, connectConcierge)
	stop()
	os.Exit(code)
}

// run is the tool, with its model client from connect.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, connect connectFunc) int {
	flags := flag.NewFlagSet("ear-corpus", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", config.PathFromEnvironment(), "the unit's config file; by default $"+config.EnvPrefix+"CONFIG, else ghost-wispr.yaml, as the recorder reads it")
	repliesPath := flags.String("replies", "", `the agent replies to rewrite for the ear: a directory of .txt files named for their issue key, or a JSONL file of {"key": "ABC-12", "written": "..."} objects`)
	judge := flags.Bool("judge", false, "ask a model judge whether each result puts its conclusion first")
	judgeOnly := flags.String("judge-only", "", "judge the texts at this path as given, with no rewrite: the negative control")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitPass
		}
		return exitUsage
	}
	switch {
	case flags.NArg() > 0:
		complain(stderr, exitUsage, "unexpected argument %q", flags.Arg(0))
		flags.Usage()
		return exitUsage
	case (*repliesPath == "") == (*judgeOnly == ""):
		complain(stderr, exitUsage, "give one of -replies and -judge-only")
		flags.Usage()
		return exitUsage
	}
	path := *repliesPath
	if *judgeOnly != "" {
		path = *judgeOnly
	}
	texts, err := readReplies(path)
	if err != nil {
		return complain(stderr, exitUsage, "%v", err)
	}
	// The config's warnings are the recorder's (Deepgram, summaries, the
	// light); one that stops the model client shows as connect's error.
	cfgStore, _, err := config.NewStoreWithEnv(*configPath, filepath.Join(filepath.Dir(*configPath), ".env"))
	if err != nil {
		return complain(stderr, exitUsage, "config: %v", err)
	}
	cfg := cfgStore.Get()
	m, err := cfg.ResolveModel(config.RoleConcierge, cfg.Concierge.Model)
	if err != nil {
		return complain(stderr, exitUsage, "the model client: %v", err)
	}
	generator, err := connect(ctx, &cfg)
	if err != nil {
		return complain(stderr, exitUsage, "the model client: %v", err)
	}
	j := &judgeClient{generator: generator, model: m.Model, temperature: judgeTemperature(m.Config.API), stderr: stderr}
	if *judgeOnly != "" {
		return judgeTexts(ctx, stdout, stderr, j, texts)
	}
	if !*judge {
		j = nil
	}
	return rewriteReplies(ctx, stdout, stderr, generator, m.Model, j, texts)
}

// rewriteReplies rewrites each reply for the ear with a Concierge on a
// scratch store, deleted after, and judges each result when j is set.
func rewriteReplies(ctx context.Context, stdout, stderr io.Writer, generator concierge.Generator, model string, j *judgeClient, replies []reply) int {
	scratch, err := os.MkdirTemp("", "ear-corpus-")
	if err != nil {
		return complain(stderr, exitUsage, "%v", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	db, err := storage.NewSQLiteStore(filepath.Join(scratch, "ear.db"))
	if err != nil {
		return complain(stderr, exitUsage, "the scratch store: %v", err)
	}
	defer func() { _ = db.Close() }()
	c, err := concierge.New(&concierge.Config{
		Model:     model,
		Generator: generator,
		Store:     db,
		Logger:    slog.New(slog.NewTextHandler(stderr, nil)),
	})
	if err != nil {
		return complain(stderr, exitUsage, "%v", err)
	}
	passed, yes, judgeErrors := 0, 0, 0
	for i, reply := range replies {
		rewrite, err := c.RewriteForEar(ctx, reply.text, reply.key)
		if err != nil {
			return complain(stderr, exitFail, "stopped at reply %d: %v", i+1, err)
		}
		ok := rewrite.Passed && rewrite.Tries <= earTries
		if ok {
			passed++
		}
		verdict := "not-run"
		if j != nil {
			verdict = j.judge(ctx, fmt.Sprintf("reply %d", i+1), rewrite.Text)
			switch verdict {
			case verdictYes:
				yes++
			case verdictError:
				judgeErrors++
			}
		}
		_, _ = fmt.Fprintf(stdout, "reply %d: tries=%d passed=%t judge=%s\n", i+1, rewrite.Tries, ok, verdict)
	}
	n := len(replies)
	pass := passed == n
	judged := "judge=not-run"
	if j != nil {
		pass = pass && atLeastNinetyPercent(yes, n)
		judged = fmt.Sprintf("conclusion_first=%d/%d (%.1f%%) judge_errors=%d", yes, n, percent(yes, n), judgeErrors)
	}
	_, _ = fmt.Fprintf(stdout, "summary: replies=%d passed=%d fell_back=%d %s result=%s\n", n, passed, n-passed, judged, result(pass))
	return exitCode(pass)
}

// judgeTexts judges each text as given, with no rewrite: the negative
// control, whose texts put the conclusion last on purpose. It passes when at
// least 90% are judged not conclusion-first. A text with no verdict does not
// count as failing, so a judge that answers nothing cannot pass it.
func judgeTexts(ctx context.Context, stdout, stderr io.Writer, j *judgeClient, texts []reply) int {
	failing, judgeErrors := 0, 0
	for i, text := range texts {
		verdict := j.judge(ctx, fmt.Sprintf("text %d", i+1), text.text)
		if err := ctx.Err(); err != nil {
			return complain(stderr, exitFail, "stopped at text %d: %v", i+1, err)
		}
		switch verdict {
		case verdictNo:
			failing++
		case verdictError:
			judgeErrors++
		}
		_, _ = fmt.Fprintf(stdout, "text %d: judge=%s\n", i+1, verdict)
	}
	n := len(texts)
	pass := atLeastNinetyPercent(failing, n)
	_, _ = fmt.Fprintf(stdout, "summary: texts=%d failing=%d/%d (%.1f%%) judge_errors=%d result=%s\n",
		n, failing, n, percent(failing, n), judgeErrors, result(pass))
	return exitCode(pass)
}

// complain writes "ear-corpus: " and the message to w, and returns code.
func complain(w io.Writer, code int, format string, args ...any) int {
	_, _ = fmt.Fprintf(w, "ear-corpus: "+format+"\n", args...)
	return code
}

// reply is one agent reply: its written text and the issue key its question
// named, or "".
type reply struct {
	key  string
	text string
}

// issueKey is an issue key, as R8's check knows one.
var issueKey = regexp.MustCompile(`[A-Z]+-[0-9]+`)

// readReplies reads the replies at path: each .txt file of a directory, in
// name order, keyed by the first issue key its name holds; or each object of
// a JSONL file, or of a JSON array as internal/concierge/testdata's ear
// corpus holds them. A path that holds no reply is an error.
func readReplies(path string) ([]reply, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	read := readReplyFile
	if info.IsDir() {
		read = readReplyDir
	}
	replies, err := read(path)
	if err != nil {
		return nil, err
	}
	if len(replies) == 0 {
		return nil, fmt.Errorf("%s holds no reply", path)
	}
	return replies, nil
}

func readReplyDir(dir string) ([]reply, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var replies []reply
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".txt" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(string(raw))
		if text == "" {
			return nil, fmt.Errorf("%s: no text", filepath.Join(dir, name))
		}
		replies = append(replies, reply{key: issueKey.FindString(strings.TrimSuffix(name, ".txt")), text: text})
	}
	return replies, nil
}

// replyObject is one reply of a JSON file: the key its question named, and
// its text as "written", or as "reply", the name internal/concierge's ear
// corpus gives it.
type replyObject struct {
	Key     string `json:"key"`
	Written string `json:"written"`
	Reply   string `json:"reply"`
}

func (o *replyObject) reply() (reply, error) {
	if o.Written != "" && o.Reply != "" {
		return reply{}, errors.New(`holds both "written" and "reply"`)
	}
	text := strings.TrimSpace(o.Written + o.Reply)
	if text == "" {
		return reply{}, errors.New(`holds no "written" text`)
	}
	return reply{key: strings.TrimSpace(o.Key), text: text}, nil
}

// decodeOne decodes the one JSON value data holds into into, and refuses a
// field into does not have.
func decodeOne(data []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

func readReplyFile(path string) ([]reply, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var replies []reply
	if trimmed := bytes.TrimSpace(raw); bytes.HasPrefix(trimmed, []byte("[")) {
		var objects []replyObject
		if err := decodeOne(trimmed, &objects); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for i := range objects {
			r, err := objects[i].reply()
			if err != nil {
				return nil, fmt.Errorf("%s: object %d %w", path, i+1, err)
			}
			replies = append(replies, r)
		}
		return replies, nil
	}
	for n, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var object replyObject
		if err := decodeOne(line, &object); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		r, err := object.reply()
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n+1, err)
		}
		replies = append(replies, r)
	}
	return replies, nil
}

// judgePrompt asks the judge whether a text puts its conclusion in its first
// sentence, as R8 asks of every text the voice says.
const judgePrompt = `You judge a text that a voice says aloud in a meeting, as the answer to a question about a piece of work. Answer "yes" when its first sentence states the conclusion: the answer itself, such as the state, the result or the decision. Answer "no" when its first sentence gives background, steps, a reason or a lead-in, and the conclusion comes later. Answer only with the JSON object {"conclusion_first": "yes"} or {"conclusion_first": "no"}.`

// judgeSchema is the fixed shape of the judge's answer.
var judgeSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"conclusion_first": map[string]any{"type": "string", "enum": []any{verdictYes, verdictNo}},
	},
	"required":             []any{"conclusion_first"},
	"additionalProperties": false,
}

// judgeClient is the model judge, on the Concierge's model client and model.
type judgeClient struct {
	generator   concierge.Generator
	model       string
	temperature *float32 // judgeTemperature
	stderr      io.Writer
}

// judgeTemperature is the temperature the judge asks at on a provider of
// api: 0, so a text gets the same verdict each run, but none on an OpenAI
// API provider. There the model gateway's reasoning models (gpt-6.1-terra)
// refuse any temperature but 1 (HTTP 400, "Only temperature=1 is
// supported"), and with none each model answers at its own default.
func judgeTemperature(api string) *float32 {
	if api == config.APIOpenAI {
		return nil
	}
	return genai.Ptr[float32](0)
}

// judge asks, at judgeTemperature, whether text puts its conclusion in its
// first sentence, and returns "yes", "no", or, with the reason on stderr
// under label, "error". It never prints the text or the judge's words.
func (j *judgeClient) judge(ctx context.Context, label, text string) string {
	verdict, err := j.ask(ctx, text)
	if err != nil {
		complain(j.stderr, exitFail, "%s: the judge: %v", label, err)
		return verdictError
	}
	return verdict
}

func (j *judgeClient) ask(ctx context.Context, text string) (string, error) {
	request := &genai.GenerateContentConfig{
		SystemInstruction:  genai.NewContentFromText(judgePrompt, genai.RoleUser),
		Temperature:        j.temperature,
		ResponseMIMEType:   "application/json",
		ResponseJsonSchema: judgeSchema,
	}
	var b strings.Builder
	for response, err := range j.generator.GenerateContentStream(ctx, j.model, []*genai.Content{genai.NewContentFromText("The text:\n"+text, genai.RoleUser)}, request) {
		if err != nil {
			return "", quiet(err)
		}
		if response == nil || len(response.Candidates) == 0 || response.Candidates[0].Content == nil {
			continue
		}
		for _, part := range response.Candidates[0].Content.Parts {
			if !part.Thought {
				b.WriteString(part.Text)
			}
		}
	}
	var answer struct {
		ConclusionFirst string `json:"conclusion_first"`
	}
	if err := decodeOne([]byte(b.String()), &answer); err != nil || (answer.ConclusionFirst != verdictYes && answer.ConclusionFirst != verdictNo) {
		return "", errors.New(`its answer is not {"conclusion_first": "yes"} or {"conclusion_first": "no"}`)
	}
	return answer.ConclusionFirst, nil
}

// quiet is a model error without the server's message, which may quote the
// prompt and so the reply's words: an API error keeps its code and status,
// and a transport error loses the URL. It is internal/concierge's quiet,
// which that package does not export.
func quiet(err error) error {
	var refused genai.APIError
	var refusedPointer *genai.APIError
	var transport *url.Error
	switch {
	case errors.As(err, &refused):
		return fmt.Errorf("the model answered %d %s", refused.Code, refused.Status)
	case errors.As(err, &refusedPointer):
		return fmt.Errorf("the model answered %d %s", refusedPointer.Code, refusedPointer.Status)
	case errors.As(err, &transport):
		return transport.Err
	default:
		return err
	}
}

// atLeastNinetyPercent reports whether part is at least 90% of whole; none
// of none is not.
func atLeastNinetyPercent(part, whole int) bool {
	return whole > 0 && part*10 >= whole*9
}

// percent is part of whole, in percent; of none, 0.
func percent(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return 100 * float64(part) / float64(whole)
}

func result(pass bool) string {
	if pass {
		return "pass"
	}
	return "fail"
}

func exitCode(pass bool) int {
	if pass {
		return exitPass
	}
	return exitFail
}

// connectConcierge builds the Concierge's model client on the provider
// concierge.model names, as the recorder's newConcierge does.
func connectConcierge(ctx context.Context, cfg *config.Config) (concierge.Generator, error) {
	p, _, err := providers.New(cfg, providers.Options{}).Model(cfg, config.RoleConcierge, cfg.Concierge.Model)
	if err != nil {
		return nil, err
	}
	return concierge.NewGenerator(ctx, p)
}
