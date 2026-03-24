package gdrive

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/api/drive/v3"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// RestoredSession holds all data parsed from a GDrive session folder.
type RestoredSession struct {
	ID            string
	Title         string
	StartedAt     time.Time
	EndedAt       *time.Time
	Summary       string
	SummaryPreset string
	Segments      []transcribe.Segment
	DriveFolderID string
	HasAudio      bool
}

// RestoreResult summarizes the outcome of a restore operation.
type RestoreResult struct {
	Restored int      `json:"restored"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// ListSessionFolders returns all subfolders in the configured root folder.
func (s *Syncer) ListSessionFolders(ctx context.Context) ([]*drive.File, error) {
	var folders []*drive.File
	pageToken := ""
	for {
		q := fmt.Sprintf("'%s' in parents and mimeType = 'application/vnd.google-apps.folder' and trashed = false", s.folderID)
		call := s.service.Files.List().
			Q(q).
			Fields("nextPageToken, files(id, name, createdTime)").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			PageSize(100).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}

		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list session folders: %w", err)
		}
		folders = append(folders, resp.Files...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	return folders, nil
}

// ListFolderFiles returns files inside a specific folder.
func (s *Syncer) ListFolderFiles(ctx context.Context, folderID string) ([]*drive.File, error) {
	q := fmt.Sprintf("'%s' in parents and trashed = false", folderID)
	var files []*drive.File
	pageToken := ""
	for {
		call := s.service.Files.List().
			Q(q).
			Fields("nextPageToken, files(id, name, mimeType)").
			SupportsAllDrives(true).
			IncludeItemsFromAllDrives(true).
			PageSize(100).
			Context(ctx)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list folder files %s: %w", folderID, err)
		}
		files = append(files, resp.Files...)
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return files, nil
}

// ExportGoogleDoc exports a Google Docs file as plain text.
func (s *Syncer) ExportGoogleDoc(ctx context.Context, fileID string) (string, error) {
	resp, err := s.service.Files.Export(fileID, "text/plain").Context(ctx).Download()
	if err != nil {
		return "", fmt.Errorf("export doc %s: %w", fileID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("read export %s: %w", fileID, err)
	}

	// Strip BOM if present.
	text := strings.TrimPrefix(string(data), "\ufeff")
	return text, nil
}

// RestoreFromDrive lists all session folders, downloads their content,
// parses it, and calls the importFn for each session.
// Uses 10 concurrent workers to parallelize Drive API calls.
func (s *Syncer) RestoreFromDrive(ctx context.Context, importFn func(RestoredSession) error) (*RestoreResult, error) {
	folders, err := s.ListSessionFolders(ctx)
	if err != nil {
		return nil, err
	}

	s.logger.Info("gdrive restore folders discovered", "operation", "restore_list_folders", "folders", len(folders))

	const workers = 10
	type folderResult struct {
		sess *RestoredSession
		err  error
		name string
	}

	jobs := make(chan *drive.File, len(folders))
	results := make(chan folderResult, len(folders))

	// Start workers.
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for folder := range jobs {
				sess, err := s.restoreFolder(ctx, folder)
				results <- folderResult{sess: sess, err: err, name: folder.Name}
			}
		}()
	}

	// Send all folders to workers.
	for _, f := range folders {
		jobs <- f
	}
	close(jobs)

	// Close results when all workers finish.
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results.
	result := &RestoreResult{}
	for r := range results {
		if r.err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", r.name, r.err))
			continue
		}

		if err := importFn(*r.sess); err != nil {
			if strings.Contains(err.Error(), "already exists") {
				result.Skipped++
				continue
			}
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", r.sess.ID, err))
			continue
		}

		result.Restored++
		if result.Restored%50 == 0 {
			s.logger.Info("gdrive restore progress", "operation", "restore_import", "restored", result.Restored)
		}
	}

	s.logger.Info("gdrive restore complete", "operation", "restore_complete", "restored", result.Restored, "skipped", result.Skipped, "errors", len(result.Errors))
	return result, nil
}

func (s *Syncer) restoreFolder(ctx context.Context, folder *drive.File) (*RestoredSession, error) {
	files, err := s.ListFolderFiles(ctx, folder.Id)
	if err != nil {
		return nil, err
	}

	var transcriptFileID, summaryFileID string
	hasAudio := false
	for _, f := range files {
		switch f.Name {
		case "transcript.md":
			transcriptFileID = f.Id
		case "summary.md":
			summaryFileID = f.Id
		default:
			if strings.HasPrefix(f.MimeType, "audio/") {
				hasAudio = true
			}
		}
	}

	if transcriptFileID == "" {
		return nil, fmt.Errorf("no transcript.md found")
	}

	transcriptText, err := s.ExportGoogleDoc(ctx, transcriptFileID)
	if err != nil {
		return nil, fmt.Errorf("export transcript: %w", err)
	}

	meta, segments := ParseTranscriptMarkdown(transcriptText)
	if meta.ID == "" {
		return nil, fmt.Errorf("transcript has no session ID in frontmatter")
	}

	sess := &RestoredSession{
		ID:            meta.ID,
		StartedAt:     meta.StartedAt,
		EndedAt:       meta.EndedAt,
		Segments:      segments,
		DriveFolderID: folder.Id,
		HasAudio:      hasAudio,
	}

	if summaryFileID != "" {
		summaryText, err := s.ExportGoogleDoc(ctx, summaryFileID)
		if err != nil {
			s.logger.Warn("gdrive restore skipped summary", "operation", "restore_summary", "session_id", meta.ID, "error", err)
		} else {
			sumMeta, summaryBody := ParseSummaryMarkdown(summaryText)
			sess.Summary = summaryBody
			sess.SummaryPreset = sumMeta.SummaryPreset
			if sess.Title == "" {
				sess.Title = sumMeta.Title
			}
		}
	}

	return sess, nil
}

// Frontmatter holds parsed YAML-like frontmatter from markdown files.
type Frontmatter struct {
	ID            string
	StartedAt     time.Time
	EndedAt       *time.Time
	SummaryPreset string
	Title         string
}

// parseFrontmatter extracts the YAML block between --- delimiters and returns
// the parsed metadata and the remaining body text.
func parseFrontmatter(text string) (Frontmatter, string) {
	text = strings.TrimSpace(text)
	var fm Frontmatter

	if !strings.HasPrefix(text, "---") {
		return fm, text
	}

	end := strings.Index(text[3:], "---")
	if end < 0 {
		return fm, text
	}

	yamlBlock := text[3 : end+3]
	body := strings.TrimSpace(text[end+6:])

	for _, line := range strings.Split(yamlBlock, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		val = strings.Trim(val, `"`)

		switch key {
		case "id":
			fm.ID = val
		case "started_at":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				fm.StartedAt = t
			}
		case "ended_at":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				fm.EndedAt = &t
			}
		case "summary_preset":
			fm.SummaryPreset = val
		}
	}

	return fm, body
}

var transcriptLineRe = regexp.MustCompile(`^\*\*\[(\d{2}:\d{2}:\d{2})\] Speaker (\d+):\*\* (.+)$`)

// ParseTranscriptMarkdown parses a transcript.md exported from GDrive.
func ParseTranscriptMarkdown(text string) (Frontmatter, []transcribe.Segment) {
	fm, body := parseFrontmatter(text)

	var segments []transcribe.Segment
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		matches := transcriptLineRe.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		timeStr := matches[1]
		speakerStr := matches[2]
		segText := matches[3]

		speaker, _ := strconv.Atoi(speakerStr)
		speaker-- // Convert from 1-indexed display to 0-indexed storage.

		// Reconstruct the full timestamp using the session date + segment time.
		var ts time.Time
		if !fm.StartedAt.IsZero() {
			date := fm.StartedAt.UTC().Format("2006-01-02")
			if t, err := time.Parse("2006-01-02 15:04:05", date+" "+timeStr); err == nil {
				ts = t.UTC()
			}
		}

		segments = append(segments, transcribe.Segment{
			Speaker:   speaker,
			Text:      segText,
			Timestamp: ts,
		})
	}

	return fm, segments
}

// ParseSummaryMarkdown parses a summary.md exported from GDrive.
// Returns frontmatter metadata and the summary body (without the title heading).
func ParseSummaryMarkdown(text string) (Frontmatter, string) {
	fm, body := parseFrontmatter(text)

	// The body starts with "# <title>\n\n<summary content>".
	// Extract the title and return the rest as the summary.
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) > 0 && strings.HasPrefix(lines[0], "# ") {
		fm.Title = strings.TrimPrefix(lines[0], "# ")
		// If title equals the session ID, it's the default — don't keep it.
		if fm.Title == fm.ID {
			fm.Title = ""
		}
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		} else {
			body = ""
		}
	}

	return fm, body
}
