package storage

import (
	"testing"
	"time"
)

// An empty summary keeps the summary the session has, so it keeps the model
// that made it too: a completed write with no summary and a new model neither
// puts the new model's name on the kept summary nor gives it a new summary
// version that queues it for Envoy again.
func TestAnEmptySummaryKeepsTheModelOfTheKeptSummary(t *testing.T) {
	for _, run := range []struct {
		name  string
		start bool
	}{
		{"after StartSummary", true},
		{"with no summary running", false},
	} {
		t.Run(run.name, func(t *testing.T) {
			store := newTestSQLiteStore(t)
			const id = "empty-summary-model"
			if err := store.CreateSession(id, time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			if err := store.UpdateSummaryWithModel(id, "Launch planning", "The kept summary.", SummaryCompleted, "default", "{}", "model-a"); err != nil {
				t.Fatalf("store the summary: %v", err)
			}
			if err := setEnvoyPublishStatus(store, id, EnvoyPublishPublished); err != nil {
				t.Fatalf("publish to Envoy: %v", err)
			}
			if run.start {
				if err := store.StartSummary(id); err != nil {
					t.Fatalf("StartSummary: %v", err)
				}
			}
			before := storeSession(t, store, id)

			if err := store.UpdateSummaryWithModel(id, "", "", SummaryCompleted, "", "{}", "model-b"); err != nil {
				t.Fatalf("store an empty summary: %v", err)
			}

			after := storeSession(t, store, id)
			if after.Summary != "The kept summary." || after.SummaryModel != "model-a" {
				t.Errorf("summary %q from model %q; want the kept summary from %q", after.Summary, after.SummaryModel, "model-a")
			}
			if after.SummaryVersion != before.SummaryVersion || after.EnvoyPublishStatus != before.EnvoyPublishStatus {
				t.Errorf("summary version %d, Envoy status %q; want %d, %q: the summary did not change",
					after.SummaryVersion, after.EnvoyPublishStatus, before.SummaryVersion, before.EnvoyPublishStatus)
			}
		})
	}
}
