package session

import "context"

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

func (m *Manager) publishSessionEnded(sessionID string) {
	m.publishLifecycle(sessionID, "session_ended", false, EventPublisher.PublishSessionEnded)
}

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
	// Shutdown first prevents new start sends, then waits. Register the send
	// while holding mu so that sequence cannot race with this Add.
	m.lifecyclePublishes.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.lifecyclePublishes.Done()
		if err := send(publisher, context.Background(), sessionID); err != nil {
			m.logger.Error("envoy lifecycle publish failed", "operation", "publish_"+eventType, "session_id", sessionID, "error", err)
		}
	}()
}

// WaitForLifecyclePublishes waits for in-flight session-started and
// session-ended sends, stopping when ctx is cancelled.
func (m *Manager) WaitForLifecyclePublishes(ctx context.Context) error {
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
