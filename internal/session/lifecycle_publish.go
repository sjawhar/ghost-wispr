package session

import (
	"context"

	"github.com/sjawhar/ghost-wispr/internal/stopping"
)

// StopSessionStartPublishes prevents a shutdown from registering a new
// session-started send while WaitForLifecyclePublishes is waiting.
func (m *Manager) StopSessionStartPublishes() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleStartPublishesStopped = true
}

func (m *Manager) publishSessionStarted(sessionID string) {
	m.publishLifecycle(sessionID, "session_started", true, EventPublisher.PublishSessionStarted)
}

// sessionEndedEvent is the session_ended lifecycle event's type, as the Envoy
// publisher names it, and sends it again from its queue
// (envoy.Publisher.PublishPendingLifecycleEvents).
const sessionEndedEvent = "session_ended"

// publishSessionEnded runs the session-ended hook, as work StopBackground
// waits for (Go): the Concierge's part of it writes the store. A session that
// ends after the stop runs no hook.
func (m *Manager) publishSessionEnded(sessionID string) {
	m.mu.Lock()
	hook := m.sessionEnded
	m.mu.Unlock()
	if hook != nil {
		m.Go(func(context.Context) { hook(sessionID) })
	}
	m.publishLifecycle(sessionID, sessionEndedEvent, false, EventPublisher.PublishSessionEnded)
}

// publishLifecycle sends one lifecycle event in the background. The send
// writes the event's delivery state to the store, so it is work StopBackground
// waits for: the stop's lifecycle wait gives it its time first, and
// StopBackground then cuts it off (an event cut off stays pending, for the
// next start's sweep). Once StopBackground has run, no send starts: a
// session_ended event is already queued with its session's end (endSession),
// for that sweep.
func (m *Manager) publishLifecycle(sessionID, eventType string, isStart bool, send func(EventPublisher, context.Context, string) error) {
	m.mu.Lock()
	if isStart && m.lifecycleStartPublishesStopped {
		m.mu.Unlock()
		return
	}
	publisher := m.publisher
	if publisher == nil {
		m.mu.Unlock()
		return
	}
	if m.backgroundStopped {
		m.mu.Unlock()
		m.logger.Info("envoy lifecycle event not sent: the app is stopping; a queued event goes at the next start's sweep", "operation", "publish_"+eventType, "session_id", sessionID)
		return
	}
	// Shutdown first prevents new start sends, then waits. Register the send
	// while holding mu so that sequence cannot race with this Add; it is
	// counted as background work (beginBackground) under the same lock.
	m.lifecyclePublishes.Add(1)
	m.backgroundWork.Add(1)
	m.backgroundRunning++
	m.mu.Unlock()

	go func() {
		defer m.endBackground()
		defer m.lifecyclePublishes.Done()
		if err := send(publisher, m.background, sessionID); err != nil {
			if stopping.CutOff(m.background) {
				m.logger.Info("envoy lifecycle publish cut off by the stop; the next start's sweep sends it", "operation", "publish_"+eventType, "session_id", sessionID)
				return
			}
			m.logger.Error("envoy lifecycle publish failed", "operation", "publish_"+eventType, "session_id", sessionID, "error", err)
		}
	}()
}

// WaitForLifecyclePublishes waits for in-flight session-started and
// session-ended sends, stopping when ctx is cancelled. It first waits for every
// session end under way: an end clears the session before it registers its
// session_ended send, so a shutdown whose own end found no session (a pause's
// end had it) would otherwise find no send to wait for, close the database,
// and leave that send unable to mark itself delivered.
func (m *Manager) WaitForLifecyclePublishes(ctx context.Context) error {
	m.mu.Lock()
	stop := context.AfterFunc(ctx, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.endsDone.Broadcast()
	})
	defer stop()
	for m.endsRunning > 0 {
		if err := ctx.Err(); err != nil {
			m.mu.Unlock()
			return err
		}
		m.endsDone.Wait()
	}
	m.mu.Unlock()

	done := make(chan struct{})
	go func() {
		m.lifecyclePublishes.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}
