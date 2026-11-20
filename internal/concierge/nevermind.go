package concierge

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/sjawhar/ghost-wispr/internal/envoy"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// NeverMind answers "Wispr, never mind": it cancels meeting sessionID's
// newest working question and tells its holder (R7). With no working
// question it does nothing.
func (c *Concierge) NeverMind(ctx context.Context, sessionID string) error {
	tasks, err := c.store.VoiceTasks(sessionID)
	if err != nil {
		return err
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		task := tasks[i]
		if task.State != storage.VoiceTaskWorking {
			continue
		}
		moved, err := c.store.EndVoiceTask(task.ID, storage.VoiceTaskCancelled)
		if err != nil || !moved {
			return err
		}
		c.tellChanged(sessionID)
		agent := strings.TrimPrefix(task.Holder, "session:")
		c.forget(agent, task.ID)
		c.tellHolderEnded(ctx, &task, agent, storage.VoiceTaskCancelled, "the room no longer needs it")
		return nil
	}
	return nil
}

// tellHolderEnded sends agent a short notice, asking no reply, that task's
// question, now ended (cancelled or replaced), is not to be answered, and
// why. It names the question by its time and issue, never its words, and
// logs one line naming how it ended, the task and the notice's event.
func (c *Concierge) tellHolderEnded(ctx context.Context, task *storage.VoiceTask, agent string, ended storage.VoiceTaskState, why string) {
	agents := c.currentAgents()
	if agents == nil || envoy.ValidSessionID(agent) != nil {
		return
	}
	about := ""
	if task.IssueKey != "" {
		about = " about " + task.IssueKey
	}
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendTimeout)
	defer cancel()
	sent, err := agents.Send(sendCtx, &envoy.SendRequest{
		Target:         agent,
		Message:        "Ghost Wispr here. Please do not answer my question" + about + " of " + task.AskedAt.UTC().Format("15:04:05") + " UTC: " + why + ".",
		NoReply:        true,
		IdempotencyKey: uuid.NewSHA1(uuid.NameSpaceOID, []byte("ghost-wispr ended "+task.ID)).String(),
		InReplyTo:      c.eventOf(task.ID),
	})
	if err != nil {
		c.logger.Warn("concierge could not tell an agent its question ended", "task", task.ID, "error", err)
		return
	}
	c.logger.Info("concierge told an agent its question was "+string(ended), "task", task.ID, "request", sent.EventID)
}

// eventOf is the Envoy event of task id's question: the id itself, or, while
// the task keeps its local id, the event sentAs leads to it from.
func (c *Concierge) eventOf(id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for event, local := range c.sentAs {
		if local == id {
			return event
		}
	}
	return id
}
