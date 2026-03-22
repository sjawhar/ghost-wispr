package gc

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/ghost-wispr/internal/storage"
)

type Store interface {
	GetGCEligibleSessions(maxAgeDays int, syncGated bool, diskPressure bool) ([]string, error)
	GetSession(id string) (storage.Session, error)
	DeleteSession(id string) error
}

type Config struct {
	MaxAgeDays     int
	MaxAudioSizeMB int
	SyncGated      bool
	AudioDir       string
}

type Collector struct {
	store  Store
	config Config
}

func New(store Store, config Config) *Collector {
	return &Collector{store: store, config: config}
}

func (c *Collector) Run() ([]string, error) {
	ids, err := c.store.GetGCEligibleSessions(c.config.MaxAgeDays, c.config.SyncGated, false)
	if err != nil {
		return nil, fmt.Errorf("query gc eligible: %w", err)
	}

	var deleted []string
	for _, id := range ids {
		if err := c.deleteSession(id); err != nil {
			log.Printf("gc: skip session %s: %v", id, err)
			continue
		}
		deleted = append(deleted, id)
	}

	if c.checkDiskPressure() {
		allSynced, err := c.store.GetGCEligibleSessions(0, c.config.SyncGated, true)
		if err != nil {
			return deleted, fmt.Errorf("query disk-pressure gc: %w", err)
		}
		deletedSet := make(map[string]struct{}, len(deleted))
		for _, id := range deleted {
			deletedSet[id] = struct{}{}
		}
		for _, id := range allSynced {
			if _, already := deletedSet[id]; already {
				continue
			}
			if err := c.deleteSession(id); err != nil {
				log.Printf("gc: disk-pressure skip session %s: %v", id, err)
				continue
			}
			deleted = append(deleted, id)
			if !c.checkDiskPressure() {
				break
			}
		}
	}

	return deleted, nil
}

func (c *Collector) deleteSession(id string) error {
	sess, err := c.store.GetSession(id)
	if err != nil {
		return fmt.Errorf("get session: %w", err)
	}

	// Delete DB record first — if this fails, audio files remain (recoverable).
	// Reverse order (delete files first) risks permanent audio loss on DB failure.
	if err := c.store.DeleteSession(id); err != nil {
		return fmt.Errorf("delete from db: %w", err)
	}

	// Clean up audio files (may be comma-separated for merged sessions).
	if sess.AudioPath != "" {
		for _, p := range strings.Split(sess.AudioPath, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			audioPath := p
			if !filepath.IsAbs(audioPath) && c.config.AudioDir != "" {
				audioPath = filepath.Join(c.config.AudioDir, filepath.Base(audioPath))
			}
			if err := os.Remove(audioPath); err != nil && !os.IsNotExist(err) {
				log.Printf("gc: failed to remove audio %s for session %s: %v", audioPath, id, err)
			}
		}
	}

	return nil
}

func (c *Collector) checkDiskPressure() bool {
	if c.config.MaxAudioSizeMB <= 0 || c.config.AudioDir == "" {
		return false
	}
	size, err := dirSize(c.config.AudioDir)
	if err != nil {
		return false
	}
	return size > int64(c.config.MaxAudioSizeMB)*1024*1024
}

func dirSize(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
