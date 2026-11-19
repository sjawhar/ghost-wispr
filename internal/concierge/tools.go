package concierge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/dispatch"
	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// cannotReachDispatch is what a tool answers when Dispatch does not answer,
// or the unit has no token for it: words the voice can say as they are.
const cannotReachDispatch = "I cannot reach Dispatch yet."

// cannotReachEnvoy is a tool's answer when the unit has no Envoy session, or
// Envoy does not answer a lookup.
const cannotReachEnvoy = "I cannot reach the agents yet."

// questionNotSent is ask_agent's answer when Envoy did not take the question.
const questionNotSent = "The question did not reach the agent."

const (
	// specSummaryChars is how much of an issue's spec read_issue gives.
	specSummaryChars = 1200
	// recentEvents is how many of an issue's newest events read_issue gives
	// (R6).
	recentEvents = 10
	// eventTextChars bounds each event's text.
	eventTextChars = 240
	// holderEvents is how far back issue_holder looks for a session.
	holderEvents = 50
	// maxQuestionChars bounds an ask_agent question, the room's words that
	// leave the unit (R7).
	maxQuestionChars = 300
)

// sendTimeout bounds one ask_agent send. The send does not stop with the
// turn: the listener may already have stored the question, and its
// idempotency key makes the one send safe to finish.
const sendTimeout = 10 * time.Second

// ToolSpec is one tool as a model declares it: its name, what it does, and
// its arguments as a JSON Schema object.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func object(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringArg(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// ToolSpecs are the Concierge's tools. Voices B and C declare exactly these
// to their vendors, and their Run takes exactly these arguments.
func ToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        "search_issues",
			Description: "Search Dispatch, the team's issue tracker, for the issues a description names. Use it when people describe the work rather than giving an issue key.",
			Parameters:  object([]string{"query"}, map[string]any{"query": stringArg("Two to five keywords from the description, such as 'realtime voice agent'.")}),
		},
		{
			Name:        "read_issue",
			Description: "Read one issue: its status, who it is assigned to, when it last changed, the start of its spec, and its newest events.",
			Parameters:  object([]string{"key"}, map[string]any{"key": stringArg("The issue key, such as ABC-12.")}),
		},
		{
			Name:        "issue_holder",
			Description: "Find who works on an issue: a live agent session that can be asked, an agent session that is not answering, a person, or nobody.",
			Parameters:  object([]string{"key"}, map[string]any{"key": stringArg("The issue key, such as ABC-12.")}),
		},
		{
			Name: "ask_agent",
			Description: "Ask the agent session that holds an issue one read-only question, when the record cannot answer it. " +
				"The answer comes later; the room hears a chime when it lands. Returns a task id for task_status.",
			Parameters: object([]string{"session", "question"}, map[string]any{
				"session":   stringArg("The agent's session id, from issue_holder."),
				"question":  stringArg("The question in the fewest words, quoting the room only where needed."),
				"issue_key": stringArg("The issue the question is about, such as ABC-12."),
			}),
		},
		{
			Name:        "task_status",
			Description: "Read where a question asked with ask_agent in this meeting stands, and its answer once it landed.",
			Parameters:  object([]string{"id"}, map[string]any{"id": stringArg("The task id ask_agent returned.")}),
		},
		{
			Name:        "list_joined_agents",
			Description: "List the agents that joined this meeting.",
			Parameters:  object(nil, map[string]any{}),
		},
	}
}

// runner runs the tools for one exchange: ask_agent's pointer names its
// meeting and the time of its question.
type runner struct {
	c *Concierge
	x *voice.Exchange
	// newest is, per issue read_issue read, its newest event's time.
	newest map[string]time.Time
	// unreachable: a tool's answer said Dispatch or Envoy did not answer
	// (outOfReach).
	unreachable bool
}

// Tools gives x's tools, for x.Tools.
func (c *Concierge) Tools(x *voice.Exchange) voice.ToolRunner {
	return &runner{c: c, x: x}
}

// IssueHolder answers exactly what the issue_holder tool answers for key, for
// a caller with no exchange, such as the acceptance driver. The session it
// names is made askable for no exchange.
func (c *Concierge) IssueHolder(ctx context.Context, key string) (json.RawMessage, error) {
	raw, err := json.Marshal(map[string]string{"key": key})
	if err != nil {
		return nil, err
	}
	return (&runner{c: c, x: &voice.Exchange{}}).Run(ctx, voice.ToolCall{Name: "issue_holder", Args: raw})
}

// Run runs one tool. A tool that fails answers {"error": "..."}, which a
// model can say; Run's error is for an unknown tool or a stop. The
// Supervisor answers voice.AnswerTool itself, through Answer.
func (r *runner) Run(ctx context.Context, call voice.ToolCall) (json.RawMessage, error) {
	var args map[string]any
	if err := json.Unmarshal(orEmpty(call.Args), &args); err != nil {
		return nil, fmt.Errorf("concierge: %s arguments: %w", call.Name, err)
	}
	result, err := r.call(ctx, call.Name, args)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return raw
}

// call runs one tool and answers its result, or {"error": ...}.
func (r *runner) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	arg := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	var (
		result map[string]any
		err    error
	)
	switch name {
	case "search_issues":
		result, err = r.searchIssues(ctx, arg("query"))
	case "read_issue":
		result, err = r.readIssue(ctx, arg("key"))
	case "issue_holder":
		result, err = r.issueHolder(ctx, arg("key"))
	case "ask_agent":
		result, err = r.askAgent(ctx, arg("session"), arg("question"), arg("issue_key"))
	case "task_status":
		result, err = r.taskStatus(arg("id"))
	case "list_joined_agents":
		result = r.listJoinedAgents()
	default:
		return nil, fmt.Errorf("concierge: no tool %q", name)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		// An issue read Dispatch answers not-found is an answer, not a
		// failure: a key said in the room that names no issue is no reason to
		// log. A not-found on any other route is, since a wrong Dispatch URL
		// answers every route that way.
		var refused *dispatch.APIError
		if !errors.As(err, &refused) || refused.Status != http.StatusNotFound || refused.Route != "issue" {
			r.c.logger.Warn("concierge tool failed", "tool", name, "error", err)
		}
		result = map[string]any{"error": toolError(err)}
	}
	if outOfReach(result) {
		r.unreachable = true
	}
	return result, nil
}

// outOfReach reports whether a tool's answer says Dispatch or Envoy did not
// answer: R10's error sound.
func outOfReach(result map[string]any) bool {
	switch result["error"] {
	case cannotReachDispatch, cannotReachEnvoy, questionNotSent:
		return true
	}
	return result["note"] == cannotReachEnvoy
}

// somethingWrong is a tool's answer for a failure on the recorder itself.
const somethingWrong = "Something went wrong on the recorder."

// noSuchIssue is what the model is told of an issue Dispatch does not have.
const noSuchIssue = "Dispatch has no such issue."

// toolError is what a model is told of a failure: a bad key as such,
// Dispatch's not-found as such, a Dispatch that does not answer or refuses
// the unit as out of reach, and a failure on the recorder itself as that.
func toolError(err error) string {
	var told *toldError
	var refused *dispatch.APIError
	switch {
	case errors.As(err, &told):
		return told.text
	case errors.Is(err, dispatch.ErrBadIssueKey):
		return "That is not an issue key; an issue key looks like ABC-12."
	case dispatch.IsNotFound(err):
		return noSuchIssue
	case errors.Is(err, dispatch.ErrUnreachable), errors.As(err, &refused):
		return cannotReachDispatch
	default:
		return somethingWrong
	}
}

// toldError is a failure whose text the model gets as it is.
type toldError struct{ text string }

func (e *toldError) Error() string { return e.text }

func tell(format string, args ...any) error { return &toldError{text: fmt.Sprintf(format, args...)} }

func (r *runner) recordsOrTell() (Records, error) {
	if r.c.records == nil {
		return nil, tell(cannotReachDispatch)
	}
	return r.c.records, nil
}

func (r *runner) searchIssues(ctx context.Context, query string) (map[string]any, error) {
	records, err := r.recordsOrTell()
	if err != nil {
		return nil, err
	}
	// Dispatch gets the issue words of what was said, never the sentence.
	terms := searchTerms(query)
	if terms == "" {
		return nil, tell("Search for what? Give a few keywords.")
	}
	hits, err := records.Search(ctx, terms, 10)
	if err != nil {
		return nil, err
	}
	issues := []map[string]any{}
	best := map[string]float64{}
	for _, hit := range hits {
		if hit.IssueKey == "" {
			continue
		}
		if score, seen := best[hit.IssueKey]; seen {
			best[hit.IssueKey] = max(score, hit.Score)
			continue
		}
		best[hit.IssueKey] = hit.Score
		if len(issues) < 5 {
			issues = append(issues, map[string]any{"key": hit.IssueKey, "title": hit.Title, "status": hit.Status, "match": hit.Snippet})
		}
	}
	scores := make([]float64, 0, len(issues))
	for _, issue := range issues {
		issue["score"] = best[issue["key"].(string)]
		scores = append(scores, best[issue["key"].(string)])
	}
	return map[string]any{"issues": issues, "clear_match": clearMatch(scores)}, nil
}

// clearMatchRatio is how far the best issue's score must lead the second's
// for a search to have one clear match (R5).
const clearMatchRatio = 1.5

// clearMatch reports whether the first of scores, the best issue's, is at
// least clearMatchRatio times every other's. One issue is a clear match; none
// is not.
func clearMatch(scores []float64) bool {
	if len(scores) == 0 {
		return false
	}
	for _, score := range scores[1:] {
		if scores[0] < clearMatchRatio*score {
			return false
		}
	}
	return true
}

func (r *runner) readIssue(ctx context.Context, key string) (map[string]any, error) {
	records, err := r.recordsOrTell()
	if err != nil {
		return nil, err
	}
	key, err = dispatch.NormalizeIssueKey(key)
	if err != nil {
		return nil, err
	}
	issue, err := records.Issue(ctx, key)
	if err != nil {
		return nil, err
	}
	now := r.c.now()
	result := map[string]any{
		"key": issue.Key, "title": issue.Title, "status": issue.Status, "priority": issue.Priority,
		"assignee": issue.Assignee, "parent": issue.Parent, "open_asks": issue.OpenAsks,
	}
	if !issue.UpdatedAt.IsZero() {
		result["updated_ago"] = ago(now, issue.UpdatedAt)
	}
	if slug := issue.PrimaryDocument(); slug != "" {
		if spec, err := records.DocumentText(ctx, issue.Key, slug); err == nil {
			result["spec_summary"] = cut(spec, specSummaryChars)
		} else {
			r.c.logger.Warn("concierge could not read an issue's spec", "issue", issue.Key, "error", err)
		}
	}
	events, err := records.Events(ctx, issue.Key, recentEvents)
	if err != nil {
		return nil, err
	}
	recent := make([]map[string]any, 0, len(events))
	var newest time.Time
	for i := range events {
		event := &events[i]
		if event.CreatedAt.After(newest) {
			newest = event.CreatedAt
		}
		entry := map[string]any{"type": event.Type, "when": ago(now, event.CreatedAt), "by": spokenName(event.Actor.Title())}
		if text := eventText(event.Payload); text != "" {
			entry["text"] = cut(text, eventTextChars)
		}
		recent = append(recent, entry)
	}
	if r.newest == nil {
		r.newest = map[string]time.Time{}
	}
	r.newest[issue.Key] = newest
	result["recent_events"] = recent
	return result, nil
}

// eventText is what an event says in words: a comment's body, an ask's
// question and answer, a document's name.
func eventText(payload json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(payload, &fields) != nil {
		return ""
	}
	var parts []string
	for _, key := range []string{"body", "question", "name", "title", "reason"} {
		if value, ok := fields[key].(string); ok && strings.TrimSpace(value) != "" {
			parts = append(parts, strings.TrimSpace(value))
		}
	}
	if answer, ok := fields["answer"].(map[string]any); ok {
		if text, ok := answer["text"].(string); ok && text != "" {
			parts = append(parts, "Answer: "+text)
		} else if selected, ok := answer["selected"].([]any); ok && len(selected) > 0 {
			var chosen []string
			for _, option := range selected {
				if s, ok := option.(string); ok {
					chosen = append(chosen, s)
				}
			}
			parts = append(parts, "Answer: "+strings.Join(chosen, ", "))
		}
	}
	return strings.Join(parts, " ")
}

// cut keeps the first max bytes of s, at a rune boundary, with an ellipsis
// when it cut anything.
func cut(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	end := max
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func (r *runner) issueHolder(ctx context.Context, key string) (map[string]any, error) {
	records, err := r.recordsOrTell()
	if err != nil {
		return nil, err
	}
	key, err = dispatch.NormalizeIssueKey(key)
	if err != nil {
		return nil, err
	}
	issue, err := records.Issue(ctx, key)
	if err != nil {
		return nil, err
	}
	if issue.Claim != nil {
		if issue.Claim.Actor.Kind == "session" {
			return r.sessionHolder(ctx, issue.Claim.Actor.ID, issue.Claim.Actor.Origin.SessionTitle), nil
		}
		return map[string]any{"holder": "person", "person": issue.Claim.Actor.ID}, nil
	}
	// No claim: the agent on the work is the newest session that wrote to
	// the issue, other than this unit.
	events, err := records.Events(ctx, issue.Key, holderEvents)
	if err != nil {
		return nil, err
	}
	own := ""
	if agents := r.c.currentAgents(); agents != nil {
		own = agents.ID()
	}
	for i := range events {
		if actor := &events[i].Actor; actor.Kind == "session" && actor.ID != "" && actor.ID != own {
			return r.sessionHolder(ctx, actor.ID, actor.Origin.SessionTitle), nil
		}
	}
	return map[string]any{"holder": "none", "assignee": issue.Assignee}, nil
}

// sessionHolder says whether agent session id can be asked now, from Envoy's
// registry.
func (r *runner) sessionHolder(ctx context.Context, id, title string) map[string]any {
	r.c.remember(r.x, id)
	result := map[string]any{"session": id, "title": spokenName(title)}
	agents := r.c.currentAgents()
	if agents == nil {
		result["holder"], result["state"] = "unresponsive", "unknown"
		result["note"] = cannotReachEnvoy
		return result
	}
	info, err := agents.Lookup(ctx, id)
	if err != nil {
		r.c.logger.Warn("concierge could not read an agent's liveness", "session", id, "error", err)
		result["holder"], result["state"] = "unresponsive", "unknown"
		result["note"] = cannotReachEnvoy
		return result
	}
	if title == "" {
		result["title"] = spokenName(info.Title)
	}
	if info.State == envoy.StateLive {
		result["holder"], result["live"] = "session", true
		return result
	}
	result["holder"], result["state"] = "unresponsive", string(info.State)
	if !info.LastSeen.IsZero() {
		result["last_seen"] = ago(r.c.now(), info.LastSeen)
	}
	return result
}

func (r *runner) askAgent(ctx context.Context, session, question, issueKey string) (map[string]any, error) {
	if r.x.SessionID == "" {
		return nil, tell("No meeting is recording, so I cannot ask an agent.")
	}
	agents := r.c.currentAgents()
	if agents == nil {
		return nil, tell(cannotReachEnvoy)
	}
	if envoy.ValidSessionID(session) != nil {
		return nil, tell("That is not an agent session id; get it from issue_holder.")
	}
	if !r.c.wasNamed(r.x, session) {
		return nil, tell("I can ask only an agent that issue_holder found for this question.")
	}
	if question == "" {
		return nil, tell("Ask what? The question is empty.")
	}
	if utf8.RuneCountInString(question) > maxQuestionChars {
		return nil, tell("The question is too long; ask it in fewer words.")
	}
	if issueKey != "" {
		normalized, err := dispatch.NormalizeIssueKey(issueKey)
		if err != nil {
			return nil, err
		}
		issueKey = normalized
	}
	info, err := agents.Lookup(ctx, session)
	if err != nil {
		return nil, tell(cannotReachEnvoy)
	}
	if info.State == envoy.StateGone {
		return nil, tell("That agent's session has ended, so it cannot be asked.")
	}

	// A new question to the same holder in the same exchange replaces the
	// older one (R7).
	asker := [2]string{exchangeKey(r.x), session}
	r.c.mu.Lock()
	supersedes := r.c.lastAsk[asker]
	r.c.mu.Unlock()
	var previous *storage.VoiceTask
	if supersedes != "" {
		if task, err := r.c.task(supersedes); err != nil || task.State != storage.VoiceTaskWorking {
			supersedes = ""
		} else {
			previous = &task
		}
	}

	now := r.c.now()
	localID := uuid.NewString()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.c.mu.Lock()
	closed, _ := r.c.closedLocked(r.x.SessionID)
	r.c.mu.Unlock()
	if closed {
		return nil, tell("This meeting has ended, so I ask no agent for it.")
	}
	if err := r.c.store.CreateVoiceTask(&storage.VoiceTask{
		ID: localID, SessionID: r.x.SessionID, AskedAt: now, IssueKey: issueKey, Holder: "session:" + session,
	}); err != nil {
		return nil, fmt.Errorf("record the question: %w", err)
	}
	// While the question is on its way, a reply that names its event cannot
	// find the task yet; Deliver sends such a reply back to come again.
	r.c.sending(session, +1)
	defer r.c.sending(session, -1)
	// A stop that came before the send keeps the room's question in the
	// unit. A send already begun outlives a stop: the listener may already
	// hold the question, and the idempotency key makes finishing it safe.
	if err := ctx.Err(); err != nil {
		if deleteErr := r.c.store.DeleteVoiceTask(localID); deleteErr != nil {
			r.c.logger.Warn("concierge could not remove the task of a question it did not send", "task", localID, "error", deleteErr)
		}
		r.c.tellChanged(r.x.SessionID)
		return nil, err
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	sent, err := agents.Send(sendCtx, &envoy.SendRequest{
		Target:         session,
		Message:        r.c.askMessage(r.x, now, question, issueKey),
		IdempotencyKey: r.c.askKey(r.x, session, question),
		Supersedes:     supersedes,
	})
	cancel()
	if err != nil {
		if deleteErr := r.c.store.DeleteVoiceTask(localID); deleteErr != nil {
			r.c.logger.Warn("concierge could not remove the task of a question it did not send", "task", localID, "error", deleteErr)
		}
		r.c.tellChanged(r.x.SessionID)
		r.c.logger.Warn("concierge could not send a question", "session", session, "error", err)
		return nil, tell(questionNotSent)
	}
	taskID := sent.EventID
	r.c.mu.Lock()
	r.c.sentAs[taskID] = localID
	r.c.sessionOf[taskID], r.c.sessionOf[localID] = r.x.SessionID, r.x.SessionID
	r.c.mu.Unlock()
	if err := r.c.store.RekeyVoiceTask(localID, taskID); err != nil {
		// The task keeps its local id; sentAs keeps leading the question's
		// event to it, for the reply and for task_status.
		r.c.logger.Warn("concierge could not key a task by its Envoy event", "task", localID, "event", taskID, "error", err)
	} else {
		r.c.mu.Lock()
		delete(r.c.sentAs, taskID)
		delete(r.c.sessionOf, localID)
		r.c.mu.Unlock()
	}
	if previous != nil {
		replaced, err := r.c.store.EndVoiceTask(previous.ID, storage.VoiceTaskReplaced)
		if err != nil {
			r.c.logger.Warn("concierge could not mark a task replaced", "task", previous.ID, "error", err)
		}
		if replaced {
			r.c.forget(session, previous.ID)
			r.c.tellHolderEnded(ctx, previous, session, "a newer question replaces it")
		}
	}
	r.c.mu.Lock()
	closed, deleted := r.c.closedLocked(r.x.SessionID)
	switch {
	case deleted:
		// The meeting was deleted while the question was sent: what it held
		// is gone, and this adds nothing back.
		delete(r.c.sentAs, taskID)
		delete(r.c.sessionOf, taskID)
		delete(r.c.sessionOf, localID)
	case closed:
		// The meeting ended while the question was sent: the lead to its
		// task stays for a late reply, and the question is cancelled like
		// the meeting's others.
	default:
		r.c.lastAsk[asker] = taskID
		r.c.working[session] = append(r.c.working[session], taskID)
	}
	r.c.mu.Unlock()
	if closed && !deleted {
		row := taskID
		if _, err := r.c.store.VoiceTask(taskID); err != nil {
			row = localID
		}
		if _, err := r.c.store.EndVoiceTask(row, storage.VoiceTaskCancelled); err != nil {
			r.c.logger.Warn("concierge could not cancel the question of an ended session", "task", row, "error", err)
		}
	}
	if !deleted {
		r.c.tellChanged(r.x.SessionID)
	}
	result := map[string]any{
		"task_id":         taskID,
		"recipient_state": string(sent.RecipientState),
		"note":            "Asked. The answer comes later, with a chime.",
	}
	if sent.RecipientState != envoy.StateLive {
		result["note"] = "Asked, but the agent is not answering now; its answer may take a while."
	}
	return result, nil
}

// askKey is the idempotency key of question to agent session in exchange x:
// the same question resent in the same exchange, after a send that failed or
// timed out, is one message to the agent.
func (c *Concierge) askKey(x *voice.Exchange, session, question string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("ghost-wispr ask "+exchangeKey(x)+" "+session+" "+question)).String()
}

// askMessage is the question as the agent reads it: who sends it, that it is
// read-only, the question, where in the meeting it was asked, and how to
// answer. It carries the asker's question and nothing else of the room.
func (c *Concierge) askMessage(x *voice.Exchange, now time.Time, question, issueKey string) string {
	var b strings.Builder
	b.WriteString("From the office meeting recorder (Ghost Wispr), on behalf of someone in the room it cannot identify.\n")
	b.WriteString("This is a read-only question. Make no change because of it unless a person confirms the change in Dispatch.\n\n")
	if issueKey != "" {
		fmt.Fprintf(&b, "Question about %s: %s\n\n", issueKey, question)
	} else {
		fmt.Fprintf(&b, "Question: %s\n\n", question)
	}
	start := x.WakeAt
	if start.IsZero() || start.After(now) {
		start = now
	}
	fmt.Fprintf(&b, "Asked in Ghost Wispr session %s, %s to %s UTC", x.SessionID, start.UTC().Format("15:04:05"), now.UTC().Format("15:04:05"))
	if session, err := c.store.GetSession(x.SessionID); err == nil && !session.StartedAt.IsZero() && !start.Before(session.StartedAt) {
		fmt.Fprintf(&b, " (%s to %s into the session's recording)", clock(start.Sub(session.StartedAt)), clock(now.Sub(session.StartedAt)))
	}
	b.WriteString(".")
	if c.publicURL != "" {
		fmt.Fprintf(&b, " Its transcript, which honours deletions: %s/api/sessions/%s", strings.TrimRight(c.publicURL, "/"), x.SessionID)
	}
	b.WriteString("\n\nReply to this message (envoy_send with in_reply_to) in three sentences or fewer. Your reply is read aloud in the room.")
	return b.String()
}

// clock writes d as m:ss, or h:mm:ss from an hour.
func clock(d time.Duration) string {
	seconds := int(d / time.Second)
	if seconds >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", seconds/3600, seconds/60%60, seconds%60)
	}
	return fmt.Sprintf("%d:%02d", seconds/60, seconds%60)
}

func (r *runner) taskStatus(id string) (map[string]any, error) {
	task, err := r.c.task(id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && task.SessionID != r.x.SessionID) {
		return nil, tell("This meeting asked no question with that task id.")
	}
	if err != nil {
		return nil, fmt.Errorf("read task %s: %w", id, err)
	}
	result := map[string]any{
		"task_id": task.ID, "state": string(task.State), "issue_key": task.IssueKey,
		"holder": strings.TrimPrefix(task.Holder, "session:"), "asked": ago(r.c.now(), task.AskedAt),
		"spoken": task.SpokenAt != nil,
	}
	if task.AnswerWritten != "" {
		result["answer"] = task.AnswerWritten
	}
	return result, nil
}

func (r *runner) listJoinedAgents() map[string]any {
	agents := []map[string]any{}
	if r.c.joined != nil {
		for _, agent := range r.c.joined() {
			r.c.remember(r.x, agent.SessionID)
			agents = append(agents, map[string]any{"session": agent.SessionID, "title": spokenName(agent.Title), "issue_key": agent.IssueKey})
		}
	}
	return map[string]any{"agents": agents}
}

// namedFor is how long the sessions issue_holder named stay askable in an
// exchange: longer than any exchange.
const namedFor = time.Hour

// named is the agent sessions the tools named in one exchange.
type named struct {
	at       time.Time
	sessions map[string]bool
}

// exchangeKey names one exchange: its meeting and its wake.
func exchangeKey(x *voice.Exchange) string {
	return x.SessionID + "|" + x.WakeAt.UTC().Format(time.RFC3339Nano)
}

// remember makes session askable in x: issue_holder or list_joined_agents
// named it there.
func (c *Concierge) remember(x *voice.Exchange, session string) {
	if x.SessionID == "" {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if closed, _ := c.closedLocked(x.SessionID); closed {
		return
	}
	for key, entry := range c.named {
		if now.Sub(entry.at) > namedFor {
			delete(c.named, key)
		}
	}
	key := exchangeKey(x)
	entry, ok := c.named[key]
	if !ok {
		entry = &named{at: now, sessions: map[string]bool{}}
		c.named[key] = entry
	}
	entry.sessions[session] = true
}

// wasNamed reports whether the tools named session in x, so ask_agent may ask
// it. Room speech, an issue's text or an agent's answer cannot name a session
// to ask; only the record's holder or a joined agent is asked.
func (c *Concierge) wasNamed(x *voice.Exchange, session string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.named[exchangeKey(x)]
	return ok && entry.sessions[session]
}

// sending counts the questions on their way to agent session session.
func (c *Concierge) sending(session string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight[session] += delta
	if c.inflight[session] <= 0 {
		delete(c.inflight, session)
	}
}

// task reads a task by its id, or by the Envoy event of its question while
// the task still holds its local id, or by the event of the request its
// agent got to reply again with in_reply_to.
func (c *Concierge) task(id string) (storage.VoiceTask, error) {
	task, err := c.store.VoiceTask(id)
	if !errors.Is(err, sql.ErrNoRows) {
		return task, err
	}
	c.mu.Lock()
	local, sent := c.sentAs[id]
	asked, again := c.againFor[id]
	c.mu.Unlock()
	switch {
	case sent:
		return c.store.VoiceTask(local)
	case again:
		return c.task(asked)
	default:
		return task, err
	}
}
