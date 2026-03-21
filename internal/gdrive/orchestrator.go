package gdrive

import (
	"context"
	"fmt"
	"log"

	"github.com/sjawhar/ghost-wispr/internal/storage"
	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type OrchestratorStore interface {
	GetSession(id string) (storage.Session, error)
	GetSegments(sessionID string) ([]transcribe.Segment, error)
	UpdateSyncStatus(sessionID, status, driveFolderID string) error
}

type Orchestrator struct {
	syncer *Syncer
	store  OrchestratorStore
}

func NewOrchestrator(syncer *Syncer, store OrchestratorStore) *Orchestrator {
	return &Orchestrator{syncer: syncer, store: store}
}

func (o *Orchestrator) SyncSession(ctx context.Context, sessionID string) error {
	sess, err := o.store.GetSession(sessionID)
	if err != nil {
		return fmt.Errorf("get session %s: %w", sessionID, err)
	}

	segments, err := o.store.GetSegments(sessionID)
	if err != nil {
		return fmt.Errorf("get segments %s: %w", sessionID, err)
	}

	syncSess := SyncSession{
		ID:            sess.ID,
		Title:         sess.Title,
		StartedAt:     sess.StartedAt,
		EndedAt:       sess.EndedAt,
		Summary:       sess.Summary,
		SummaryPreset: sess.SummaryPreset,
	}

	files, folderName, err := BuildSyncFiles(syncSess, segments, sess.AudioPath)
	if err != nil {
		_ = o.store.UpdateSyncStatus(sessionID, storage.SyncFailed, "")
		return fmt.Errorf("build sync files %s: %w", sessionID, err)
	}

	driveFolderID, err := o.syncer.Upload(ctx, folderName, files)
	if err != nil {
		_ = o.store.UpdateSyncStatus(sessionID, storage.SyncFailed, "")
		return fmt.Errorf("upload %s: %w", sessionID, err)
	}

	if err := o.store.UpdateSyncStatus(sessionID, storage.SyncSynced, driveFolderID); err != nil {
		return fmt.Errorf("update sync status %s: %w", sessionID, err)
	}

	log.Printf("gdrive: synced session %s to folder %s", sessionID, driveFolderID)
	return nil
}
