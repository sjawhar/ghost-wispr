package concierge

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/voice"
)

// earTimeout bounds the rewrite of one answer for the ear.
const earTimeout = 30 * time.Second

// errAskStillSending is a reply that names a question still on its way: its
// task cannot be found until the send returns, so the reply comes again.
var errAskStillSending = errors.New("concierge: the reply names a question still being sent; it comes again")

// Deliver takes one message from the unit's Envoy session: an agent's reply
// to a question the voice asked. The reply names the question's event in
// in_reply_to, which is its task's id (or leads to it, while the task still
// holds its local id, or when it names the request to reply again). A message
// that names nothing is never taken as an answer: its sender is asked once,
// per question, to reply with in_reply_to. The written answer is stored at
// once, and announced on Answers once rewritten for the ear. A message that
// is no answer to a task of a meeting the unit still holds changes nothing.
// An error answers the frame nak, so Envoy delivers it again: a store that
// could not be written, or a reply to a question still being sent.
func (c *Concierge) Deliver(_ context.Context, message *envoy.Message) error {
	task, ok, err := c.taskOf(message)
	if err != nil || !ok {
		return err
	}
	answered, err := c.store.AnswerVoiceTask(task.ID, message.Text)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	c.tellChanged(task.SessionID)
	c.forget(message.SourceSession, task.ID, message.InReplyTo)
	if !answered {
		return nil
	}
	c.logger.Info("concierge took an agent's answer", "task", task.ID, "session", task.SessionID)
	go c.announce(&task, message.Text)
	return nil
}

// taskOf finds the task message answers. A reply that names an event the
// ledger does not hold while a question to its sender is still being sent is
// errAskStillSending: that event is the question's, which its task takes once
// the send returns. A reply that names an event the unit never learned, such
// as a question Envoy stored from a send it did not confirm, is met like a
// message that names nothing.
func (c *Concierge) taskOf(message *envoy.Message) (storage.VoiceTask, bool, error) {
	holder := "session:" + message.SourceSession
	if message.InReplyTo != "" {
		task, err := c.task(message.InReplyTo)
		if errors.Is(err, sql.ErrNoRows) {
			c.mu.Lock()
			sending := c.inflight[message.SourceSession] > 0
			c.mu.Unlock()
			if sending {
				return storage.VoiceTask{}, false, errAskStillSending
			}
			c.logger.Info("concierge took a reply to no question it holds", "in_reply_to", message.InReplyTo, "from", message.SourceSession)
			c.askAgain(message)
			return storage.VoiceTask{}, false, nil
		}
		if err != nil {
			return storage.VoiceTask{}, false, err
		}
		if task.Holder != holder {
			c.logger.Warn("concierge dropped a reply from a session the question did not go to", "task", task.ID, "from", message.SourceSession)
			return storage.VoiceTask{}, false, nil
		}
		return task, true, nil
	}
	c.askAgain(message)
	return storage.VoiceTask{}, false, nil
}

// askAgain meets a message that names no question it answers. It is never
// taken as an answer by guess: the sender gets, once per question still
// waiting on it, a request to reply with in_reply_to. A reply to that request
// names it, and so lands on its question's task.
func (c *Concierge) askAgain(message *envoy.Message) {
	agents := c.currentAgents()
	c.mu.Lock()
	pending := slices.Clone(c.working[message.SourceSession])
	c.mu.Unlock()
	if agents == nil || message.SourceSession == "" || len(pending) == 0 {
		c.logger.Info("concierge took an Envoy message that answers no question", "from", message.SourceSession)
		return
	}
	for _, id := range pending {
		task, err := c.task(id)
		if err != nil {
			c.logger.Warn("concierge could not read a task it may ask about again", "task", id, "error", err)
			continue
		}
		if task.State != storage.VoiceTaskWorking {
			continue
		}
		c.mu.Lock()
		_, asked := c.askedAgain[task.ID]
		if !asked {
			c.askedAgain[task.ID] = ""
		}
		c.mu.Unlock()
		if asked {
			continue
		}
		c.sendAgainRequest(agents, message.SourceSession, &task, message.EventID, c.askAgainMessage(&task))
	}
}

// sendAgainRequest sends agent the request to send task's answer as a reply
// to it, in reply to inReplyTo when set, and leads the request's event to the
// task. A request Envoy did not confirm keeps its key, so its retry is one
// message to the agent. A retry that Envoy answers it held already, under an
// event the unit never learned, is sent once more under a key of its own: the
// agent may have answered the one the unit cannot name.
func (c *Concierge) sendAgainRequest(agents Agents, agent string, task *storage.VoiceTask, inReplyTo, message string) {
	c.mu.Lock()
	key := c.againKey[task.ID]
	c.mu.Unlock()
	reused := key != ""
	if !reused {
		key = uuid.NewString()
	}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
		sent, err := agents.Send(ctx, &envoy.SendRequest{Target: agent, Message: message, IdempotencyKey: key, InReplyTo: inReplyTo})
		cancel()
		c.mu.Lock()
		closed, deleted := c.closedLocked(task.SessionID)
		again := false
		switch {
		case err != nil && !closed:
			delete(c.askedAgain, task.ID)
			c.againKey[task.ID] = key
		case err != nil || deleted:
			// A deleted meeting's request adds nothing back, and an ended
			// one's gets no retry.
			delete(c.askedAgain, task.ID)
			delete(c.againKey, task.ID)
		case sent.Duplicate && reused:
			delete(c.againKey, task.ID)
			again = true
		default:
			delete(c.againKey, task.ID)
			c.askedAgain[task.ID] = sent.EventID
			c.againFor[sent.EventID] = task.ID
		}
		c.mu.Unlock()
		switch {
		case err != nil:
			c.logger.Warn("concierge could not ask an agent to reply with in_reply_to", "task", task.ID, "error", err)
		case again:
			key, reused = uuid.NewString(), false
			continue
		default:
			c.logger.Info("concierge asked an agent to reply with in_reply_to", "task", task.ID, "request", sent.EventID)
		}
		return
	}
}

// askAgainMessage asks the agent to send its answer to task's question again
// as a reply. It names the question by its issue and time, not its words.
func (c *Concierge) askAgainMessage(task *storage.VoiceTask) string {
	return "Ghost Wispr here. I cannot tell which of my questions your message answers, so nobody in the room heard it. " +
		"If it answers " + questionAbout(task) + ", reply to this message (envoy_send with in_reply_to) with that answer, in three sentences or fewer. Your reply is read aloud in the room."
}

// answerAgainMessage asks the agent for its answer to task's question as a
// reply to this message, whether it answered already or not yet: the unit
// cannot name the message that carried the question.
func (c *Concierge) answerAgainMessage(task *storage.VoiceTask) string {
	return "Ghost Wispr here. If you already answered " + questionAbout(task) + ", send that answer again as a reply to this message (envoy_send with in_reply_to); if not, reply to this message when you do. " +
		"Three sentences or fewer: your reply is read aloud in the room."
}

// questionAbout names task's question by its issue and time, not its words.
func questionAbout(task *storage.VoiceTask) string {
	about := "my question"
	if task.IssueKey != "" {
		about = "my question about " + task.IssueKey
	}
	return about + " (asked " + task.AskedAt.UTC().Format("15:04:05") + " UTC)"
}

// forget drops answered task ids, local or event, from the agent's
// unanswered tasks, and the request to reply again that it got for them.
func (c *Concierge) forget(agent string, ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.working[agent] = slices.DeleteFunc(c.working[agent], func(id string) bool { return slices.Contains(ids, id) })
	if len(c.working[agent]) == 0 {
		delete(c.working, agent)
	}
	for _, id := range ids {
		if request, ok := c.askedAgain[id]; ok {
			delete(c.againFor, request)
			delete(c.askedAgain, id)
		}
		delete(c.againKey, id)
	}
}

// announce rewrites an answer for the ear and queues its announcement; the
// queue hands it to Answers unless its meeting ends or is deleted first. An
// answer whose meeting no longer records goes to no model: it will not be
// spoken.
func (c *Concierge) announce(task *storage.VoiceTask, written string) {
	c.mu.Lock()
	closed, _ := c.closedLocked(task.SessionID)
	c.mu.Unlock()
	if closed || !c.stillOwed(&voice.PendingAnswer{TaskID: task.ID}) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), earTimeout)
	defer cancel()
	spoken := c.forEar(ctx, strings.TrimSpace(written), task.IssueKey)
	c.enqueue(&voice.PendingAnswer{TaskID: task.ID, SessionID: task.SessionID, IssueKey: task.IssueKey, Text: spoken, AskedAt: task.AskedAt})
}
