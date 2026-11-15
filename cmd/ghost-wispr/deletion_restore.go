package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/api/drive/v3"

	recordingname "github.com/sjawhar/ghost-wispr/internal/recording"
	"github.com/sjawhar/ghost-wispr/internal/storage"
)

// restoreDeletionRecordings combines the session's locally known recordings
// with every audio file Drive has for it, restoring only missing local files.
// Each entry of the session's audio path is read where recording.Resolve
// finds it (as written, then in audioDir), so the recording spliced is the one
// the app plays and syncs; one found in neither place is restored into
// audioDir.
func restoreDeletionRecordings(ctx context.Context, guard recordingRestoreGuard, session *storage.Session, audioDir string, source deletionRecordingSource, allowPrecutRecording string) (deletionRecordingSet, error) {
	set := deletionRecordingSet{}
	keepDownloads := false
	defer func() {
		if !keepDownloads {
			removeFiles(set.Downloaded)
		}
	}()

	byName := make(map[string]string)
	for _, entry := range recordingname.Entries(session.AudioPath) {
		path := recordingname.Resolve(entry, audioDir)
		byName[filepath.Base(path)] = path
	}
	driveFiles := make(map[string][]*drive.File)
	driveFolders := make(map[string]string)
	driveSourceIDs := make(map[string]string)
	addDriveFiles := func(folderID string, expected map[string]struct{}, sourceID string) error {
		files, err := source.ListFolderFiles(ctx, folderID)
		if err != nil {
			return fmt.Errorf("list Drive recordings in folder %s: %w", folderID, err)
		}
		for _, file := range files {
			name, ok := recordingname.LocalName(file.Name)
			if !strings.HasPrefix(file.MimeType, "audio/") || !ok {
				continue
			}
			if len(expected) != 0 {
				if _, matches := expected[recordingname.Key(name)]; !matches {
					continue
				}
			}
			driveFiles[name] = append(driveFiles[name], file)
			if _, known := byName[name]; !known {
				if strings.TrimSpace(audioDir) == "" {
					return fmt.Errorf("restore Drive recording %s: audio directory is not configured", file.Name)
				}
				byName[name] = filepath.Join(audioDir, name)
			}
			if _, known := driveFolders[name]; !known {
				driveFolders[name] = folderID
				driveSourceIDs[name] = sourceID
			}
		}
		return nil
	}
	// allKnown reports whether byName names a recording for every key in
	// expected.
	allKnown := func(expected map[string]struct{}) bool {
		for key := range expected {
			found := false
			for name := range byName {
				if recordingname.Key(name) == key {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	if source != nil {
		if session.GDriveFolderID != "" {
			if err := addDriveFiles(session.GDriveFolderID, nil, session.ID); err != nil {
				return deletionRecordingSet{}, err
			}
		}
		if session.AudioRemovedAt != "" {
			sources, err := guard.RefinementRecordingSources(session.ID)
			if err != nil {
				return deletionRecordingSet{}, fmt.Errorf("list Drive recording sources for session %s: %w", session.ID, err)
			}
			for _, sourceSession := range sources {
				if sourceSession.GDriveFolderID == "" && sourceSession.FallbackGDriveFolderID == "" {
					continue
				}
				expected := make(map[string]struct{}, len(sourceSession.RecordingPaths))
				for _, path := range sourceSession.RecordingPaths {
					if key := recordingname.Key(path); key != "" && key != "." {
						expected[key] = struct{}{}
					}
				}
				if len(expected) == 0 {
					expected[sourceSession.ID] = struct{}{}
				}
				if allKnown(expected) {
					continue
				}
				if sourceSession.FallbackGDriveFolderID != "" && sourceSession.FallbackGDriveFolderID != sourceSession.GDriveFolderID {
					if err := addDriveFiles(sourceSession.FallbackGDriveFolderID, expected, sourceSession.ID); err != nil {
						return deletionRecordingSet{}, err
					}
				}
				if !allKnown(expected) && sourceSession.GDriveFolderID != "" {
					if err := addDriveFiles(sourceSession.GDriveFolderID, expected, sourceSession.ID); err != nil {
						return deletionRecordingSet{}, err
					}
				}
			}
		}
	}
	if len(byName) == 0 {
		return deletionRecordingSet{}, nil
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	set.Paths = make([]string, 0, len(names))
	for _, name := range names {
		path := byName[name]
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			set.Paths = append(set.Paths, path)
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return deletionRecordingSet{}, fmt.Errorf("stat recording %s: %w", path, err)
		}
		folderID := driveFolders[name]
		if source == nil || folderID == "" {
			return deletionRecordingSet{}, fmt.Errorf("recording %s is missing locally and cannot be restored from Drive", path)
		}
		staged, err := downloadDestination(path)
		if err != nil {
			return deletionRecordingSet{}, err
		}
		sourceID := driveSourceIDs[name]
		if sourceID == "" {
			sourceID = session.ID
		}
		if err := downloadDriveRecording(ctx, guard, sourceID, folderID, source, driveFiles[name], staged, path, recordingname.FileName(path), filepath.Base(path) == allowPrecutRecording); err != nil {
			_ = os.Remove(staged)
			return deletionRecordingSet{}, err
		}
		if err := os.Rename(staged, path); err != nil {
			_ = os.Remove(staged)
			return deletionRecordingSet{}, fmt.Errorf("install restored recording %s: %w", path, err)
		}
		if err := syncRecordingDirectory(path); err != nil {
			_ = os.Remove(path)
			return deletionRecordingSet{}, err
		}
		set.Paths = append(set.Paths, path)
		set.Downloaded = append(set.Downloaded, path)
	}
	keepDownloads = true
	return set, nil
}

func downloadDriveRecording(ctx context.Context, guard recordingRestoreGuard, sessionID, folderID string, source deletionRecordingSource, files []*drive.File, destination, recordingPath, driveName string, allowPrecut bool) error {
	if len(files) == 0 {
		return fmt.Errorf("drive has no recording named %s", driveName)
	}
	expected, cut, err := guard.LatestRecordingSHA256(sessionID, recordingPath)
	if err != nil {
		return fmt.Errorf("read latest cut hash for recording %s: %w", destination, err)
	}
	if cut && !allowPrecut {
		for _, file := range files {
			if file.Sha256Checksum != expected {
				return fmt.Errorf("refuse Drive recording %s: SHA-256 %q does not match the latest cut hash %q", file.Name, file.Sha256Checksum, expected)
			}
		}
	}
	if err := source.DownloadRecording(ctx, folderID, driveName, destination); err != nil {
		return fmt.Errorf("restore recording %s: %w", filepath.Base(destination), err)
	}
	if cut && !allowPrecut {
		hash, err := recordingname.SHA256(destination)
		if err != nil {
			_ = os.Remove(destination)
			return fmt.Errorf("hash restored recording %s: %w", destination, err)
		}
		if hash != expected {
			_ = os.Remove(destination)
			return fmt.Errorf("refuse restored recording %s: SHA-256 %q does not match the latest cut hash %q", destination, hash, expected)
		}
	}
	return nil
}

// ensureDeletionRecordings finds or restores a session's recordings before a
// deletion or a refinement. Drive is opened only for a session that has a
// folder there or whose audio was removed from this unit: a merge whose audio
// was removed before it had a folder of its own restores from its sources'
// folders.
func ensureDeletionRecordings(ctx context.Context, store *storage.SQLiteStore, session *storage.Session, audioDir string, openDrive func(context.Context) (deletionRecordingSource, error)) (deletionRecordingSet, error) {
	var source deletionRecordingSource
	if session.GDriveFolderID != "" || session.AudioRemovedAt != "" {
		opened, err := openDrive(ctx)
		if err != nil {
			return deletionRecordingSet{}, err
		}
		source = opened
	}
	return restoreDeletionRecordings(ctx, store, session, audioDir, source, "")
}

func retryRefinementAfterRestoringRecording(ctx context.Context, sessionID string, restore func(context.Context, string) error, refine func(context.Context, string) error) error {
	if err := restore(ctx, sessionID); err != nil {
		return err
	}
	return refine(ctx, sessionID)
}

// refinementAudioRestoreStore is what restoring a session's recordings for a
// refinement reads and writes.
type refinementAudioRestoreStore interface {
	GetSession(id string) (storage.Session, error)
	RestoreAudioForRefinement(id, beforeAudioPath, restoredAudioPath string) (bool, error)
	DriveRestoreAllowed(sessionID string) (bool, error)
	RefinementRecordingPaths(sessionID string) ([]string, error)
}

func hasLocalRefinementRecording(session *storage.Session, audioDir string) (bool, error) {
	if session.AudioRemovedAt != "" {
		return false, nil
	}
	entries := 0
	for _, entry := range recordingname.Entries(session.AudioPath) {
		entries++
		path := recordingname.Resolve(entry, audioDir)
		info, err := os.Stat(path)
		switch {
		case err == nil && !info.IsDir():
		case err != nil && !errors.Is(err, os.ErrNotExist):
			return false, fmt.Errorf("check recording %s: %w", path, err)
		default:
			return false, nil
		}
	}
	return entries > 0, nil
}

func refinementRecordingAvailable(session *storage.Session, audioDir string) error {
	local, err := hasLocalRefinementRecording(session, audioDir)
	if err != nil {
		return err
	}
	if local {
		return nil
	}
	if session.AudioRemovedAt == "" {
		return errors.New("no recording is available on this unit or Drive")
	}
	if strings.TrimSpace(session.GDriveFolderID) == "" {
		return errors.New("the recording was removed from this unit, but no Drive folder is listed")
	}
	if session.SyncState == storage.SyncStateRemoteDeleted {
		return errors.New("the recording was removed from this unit and its Drive folder is deleted")
	}
	if session.SyncStatus != storage.SyncSynced {
		return errors.New("the recording was removed from this unit and its Drive copy is not synced")
	}
	return nil
}

func missingRefinementRecordings(required, restored []string) []string {
	available := make(map[string]struct{}, len(restored))
	for _, path := range restored {
		if key := recordingname.Key(path); key != "" && key != "." {
			available[key] = struct{}{}
		}
	}
	missing := make([]string, 0, len(required))
	seen := make(map[string]struct{}, len(required))
	for _, path := range required {
		name := filepath.Base(strings.TrimSpace(path))
		key := recordingname.Key(path)
		if key == "" || key == "." {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		if _, found := available[key]; !found {
			missing = append(missing, name)
		}
	}
	return missing
}

// restoreRefinementRecording restores, with restore, the recordings of
// session that this unit lacks and records them for its refinement, under
// the session's deletion lock: no deletion or other restore of the session
// runs meanwhile. It reads session again first; store and audioDir are
// arguments so a retry or a recovery restores from the source it has.
func (d *segmentDeleteRunner) restoreRefinementRecording(ctx context.Context, store refinementAudioRestoreStore, session *storage.Session, audioDir string, restore func(context.Context, storage.Session) (deletionRecordingSet, error)) error {
	unlock := d.locks.lock(session.ID)
	defer unlock()
	current, err := store.GetSession(session.ID)
	if err != nil {
		return fmt.Errorf("read session %s before restoring its recording: %w", session.ID, err)
	}
	*session = current

	local, err := hasLocalRefinementRecording(session, audioDir)
	if err != nil {
		return err
	}
	if local {
		return nil
	}
	allowed, err := store.DriveRestoreAllowed(session.ID)
	if err != nil {
		return fmt.Errorf("check whether Drive restoration is allowed for session %s: %w", session.ID, err)
	}
	if !allowed {
		return fmt.Errorf("drive restoration is blocked while session %s owes a replacement", session.ID)
	}
	recordings, err := restore(ctx, *session)
	if err != nil {
		return fmt.Errorf("restore recording for refinement: %w", err)
	}
	cleanupDownloads := recordings.Downloaded
	defer func() { removeFiles(cleanupDownloads) }()
	if len(recordings.Paths) == 0 {
		return errors.New("no recording is available on this unit or Drive")
	}
	if session.AudioRemovedAt != "" {
		required, err := store.RefinementRecordingPaths(session.ID)
		if err != nil {
			return fmt.Errorf("list recordings known to session %s: %w", session.ID, err)
		}
		if len(required) == 0 {
			required = recordingname.Entries(session.AudioPath)
		}
		if missing := missingRefinementRecordings(required, recordings.Paths); len(missing) > 0 {
			return fmt.Errorf("drive did not restore recording %s for session %s; restore it from Drive's trash, then retry refinement", strings.Join(missing, ", "), session.ID)
		}
	}
	restoredAudioPath := strings.Join(recordings.Paths, ",")
	if restoredAudioPath == session.AudioPath {
		if session.AudioRemovedAt == "" {
			cleanupDownloads = nil
			return nil
		}
		return errors.New("drive did not restore every recording removed from this unit")
	}
	restored, err := store.RestoreAudioForRefinement(session.ID, session.AudioPath, restoredAudioPath)
	if err != nil {
		return fmt.Errorf("record restored audio for refinement: %w", err)
	}
	if !restored {
		if current, err := store.GetSession(session.ID); err == nil {
			cleanupDownloads = downloadsNotNamedBy(current.AudioPath, recordings.Downloaded)
		}
		return fmt.Errorf("recording changed while restoring it for refinement")
	}
	cleanupDownloads = nil
	return nil
}

func downloadsNotNamedBy(audioPath string, downloads []string) []string {
	named := make(map[string]struct{})
	for _, path := range recordingname.Entries(audioPath) {
		if name := filepath.Base(path); name != "." {
			named[name] = struct{}{}
		}
	}
	unclaimed := make([]string, 0, len(downloads))
	for _, path := range downloads {
		if _, found := named[filepath.Base(path)]; !found {
			unclaimed = append(unclaimed, path)
		}
	}
	return unclaimed
}

const refinementRetryWarningKeyPrefix = "retry-refinement-"

func retryRefinementWithFailureWarning(ctx context.Context, registry *warningsRegistry, sessionID string, retry func(context.Context, string) error) error {
	err := retry(ctx, sessionID)
	if err == nil {
		registry.Clear(refinementRetryWarningKeyPrefix + sessionID)
		return nil
	}
	warning := fmt.Sprintf("Retry refinement failed for session %s: %v", sessionID, err)
	log.Printf("warning: %s", warning)
	registry.Set(refinementRetryWarningKeyPrefix+sessionID, warning)
	return err
}

// retryRefinementHook is the "Retry refinement" action: it restores the
// session's recordings this unit lacks (ensureRecordings), then refines it,
// and keeps a status warning while the retry fails.
func (d *segmentDeleteRunner) retryRefinementHook(registry *warningsRegistry) func(context.Context, string) error {
	ensure := func(ctx context.Context, sessionID string) error {
		session, err := d.store.GetSession(sessionID)
		if err != nil {
			return err
		}
		return d.restoreRefinementRecording(ctx, d.store, &session, d.audioDir(), d.ensureRecordings)
	}
	return func(ctx context.Context, sessionID string) error {
		return retryRefinementWithFailureWarning(ctx, registry, sessionID, func(ctx context.Context, sessionID string) error {
			return retryRefinementAfterRestoringRecording(ctx, sessionID, ensure, func(ctx context.Context, sessionID string) error {
				return retryRefinementWithWarningClear(ctx, registry, d.store, sessionID, d.refine)
			})
		})
	}
}
