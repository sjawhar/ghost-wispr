package concierge

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

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
// holds its local id); a reply that names nothing is matched to its sender's
// one unanswered task. The written answer is stored at once, and announced on
// Answers once rewritten for the ear. A message that is no answer to a task
// of a meeting the unit still holds changes nothing. An error answers the
// frame nak, so Envoy delivers it again: a store that could not be written,
// or a reply to a question still being sent.
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
	c.forget(message.SourceSession, task.ID, message.InReplyTo)
	if !answered {
		return nil
	}
	matchedBy := "sender"
	if message.InReplyTo != "" {
		matchedBy = "in_reply_to"
	}
	c.logger.Info("concierge took an agent's answer", "task", task.ID, "session", task.SessionID, "matched_by", matchedBy)
	go c.announce(&task, message.Text)
	return nil
}

// taskOf finds the task message answers. A reply that names an event the
// ledger does not hold while a question to its sender is still being sent is
// errAskStillSending: that event is the question's, which its task takes once
// the send returns.
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
	c.mu.Lock()
	pending := slices.Clone(c.working[message.SourceSession])
	c.mu.Unlock()
	var found []storage.VoiceTask
	for _, id := range pending {
		task, err := c.task(id)
		if err == nil && task.State == storage.VoiceTaskWorking {
			found = append(found, task)
		}
	}
	if len(found) != 1 {
		if message.SourceSession != "" {
			c.logger.Info("concierge took an Envoy message that answers no question", "from", message.SourceSession, "candidates", len(found))
		}
		return storage.VoiceTask{}, false, nil
	}
	return found[0], true, nil
}

// forget drops answered task ids, local or event, from the agent's
// unanswered tasks.
func (c *Concierge) forget(agent string, ids ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.working[agent] = slices.DeleteFunc(c.working[agent], func(id string) bool { return slices.Contains(ids, id) })
	if len(c.working[agent]) == 0 {
		delete(c.working, agent)
	}
}

// announce rewrites an answer for the ear and puts it on Answers, unless its
// meeting was deleted while it was rewritten. A full channel drops the
// announcement, not the answer: it stays in the ledger, where task_status
// reads it.
func (c *Concierge) announce(task *storage.VoiceTask, written string) {
	ctx, cancel := context.WithTimeout(context.Background(), earTimeout)
	defer cancel()
	spoken := c.forEar(ctx, strings.TrimSpace(written))
	if _, err := c.store.VoiceTask(task.ID); err != nil {
		c.logger.Info("concierge dropped an answer whose task is gone", "task", task.ID, "error", err)
		return
	}
	answer := voice.PendingAnswer{TaskID: task.ID, SessionID: task.SessionID, IssueKey: task.IssueKey, Text: spoken}
	select {
	case c.answers <- answer:
	default:
		c.logger.Warn("concierge dropped the announcement of an answer; nobody reads Answers", "task", task.ID)
	}
}
