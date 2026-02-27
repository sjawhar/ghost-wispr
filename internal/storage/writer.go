package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

type Writer struct {
	dir string
	mu  sync.Mutex
}

func NewWriter(dir string) *Writer {
	return &Writer{dir: dir}
}

func (w *Writer) Append(seg transcribe.Segment) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", w.dir, err)
	}

	date := seg.Timestamp.Format("2006-01-02")
	path := filepath.Join(w.dir, date+".md")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprintln(f, seg.FormatMarkdown()); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func (w *Writer) CurrentPath() string {
	date := time.Now().Format("2006-01-02")
	return filepath.Join(w.dir, date+".md")
}
